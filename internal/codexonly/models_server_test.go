package codexonly

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServerRuntimeModelsAggregatePerAuthAndClientVersion(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var mu sync.Mutex
	var requests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/models" {
			t.Errorf("upstream path = %q, want /backend-api/codex/models", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		authorization := r.Header.Get("Authorization")
		version := r.URL.Query().Get("client_version")
		mu.Lock()
		requests = append(requests, authorization+"|"+version+"|"+r.Header.Get("User-Agent"))
		mu.Unlock()
		switch authorization {
		case "Bearer access-a":
			writeJSON(w, http.StatusOK, map[string]any{"models": []map[string]any{
				{
					"slug":         "shared",
					"display_name": "canonical-a",
					"a_only":       true,
					"priority":     1,
				},
				{
					"slug":         "only-a",
					"display_name": "only-a",
					"priority":     2,
				},
			}})
		case "Bearer access-b":
			writeJSON(w, http.StatusOK, map[string]any{"models": []map[string]any{
				{
					"slug":         "shared",
					"display_name": "candidate-b",
					"b_only":       true,
					"priority":     0,
				},
				{
					"slug":         "only-b",
					"display_name": "only-b",
					"priority":     3,
				},
			}})
		default:
			t.Errorf("unexpected upstream authorization %q", authorization)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	server, apiKey := newRuntimeModelsServer(t, authDir, upstream.URL, func(cfg *Config) {
		cfg.CodexUserAgent = "codex_cli_rs/0.145.0 (Linux; x86_64)"
	})

	explicit := doModelRequest(t, server, apiKey, "/v1/models?client_version=0.144.1")
	if explicit.Code != http.StatusOK {
		t.Fatalf("explicit version status = %d, want 200, body: %s", explicit.Code, explicit.Body.String())
	}
	var codexPayload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(explicit.Body.Bytes(), &codexPayload); err != nil {
		t.Fatalf("decode Codex models: %v", err)
	}
	if got := modelSlugs(codexPayload.Models); !slices.Equal(got, []string{"shared", "only-a", "only-b"}) {
		t.Fatalf("Codex model slugs = %v, want deterministic union", got)
	}
	shared := findCodexClientModel(codexPayload.Models, "shared")
	if shared["display_name"] != "canonical-a" || shared["a_only"] != true {
		t.Fatalf("shared canonical model = %#v, want complete account A object", shared)
	}
	if _, ok := shared["b_only"]; ok {
		t.Fatalf("shared model merged account B fields: %#v", shared)
	}

	openAI := doModelRequest(t, server, apiKey, "/v1/models")
	if openAI.Code != http.StatusOK {
		t.Fatalf("OpenAI models status = %d, want 200, body: %s", openAI.Code, openAI.Body.String())
	}
	var openAIPayload struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(openAI.Body.Bytes(), &openAIPayload); err != nil {
		t.Fatalf("decode OpenAI models: %v", err)
	}
	if openAIPayload.Object != "list" || findOpenAIModel(openAIPayload.Data, "only-b") == nil {
		t.Fatalf("OpenAI model payload = %#v, want runtime aggregate", openAIPayload)
	}

	mu.Lock()
	gotRequests := slices.Clone(requests)
	mu.Unlock()
	if len(gotRequests) != 4 {
		t.Fatalf("upstream model requests = %v, want two auths for two versions", gotRequests)
	}
	for _, auth := range []string{"Bearer access-a", "Bearer access-b"} {
		if !slices.Contains(gotRequests, auth+"|0.144.1|codex_cli_rs/0.145.0 (Linux; x86_64)") {
			t.Fatalf("explicit version request missing for %s: %v", auth, gotRequests)
		}
		if !slices.Contains(gotRequests, auth+"|0.145.0|codex_cli_rs/0.145.0 (Linux; x86_64)") {
			t.Fatalf("configured User-Agent version request missing for %s: %v", auth, gotRequests)
		}
	}
}

func TestServerRuntimeModelsPartialFailureAndEmbeddedFallback(t *testing.T) {
	t.Run("one auth failure keeps successful catalog", func(t *testing.T) {
		authDir := t.TempDir()
		writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
		writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/backend-api/codex/models" {
				http.NotFound(w, r)
				return
			}
			if r.Header.Get("Authorization") == "Bearer access-a" {
				http.Error(w, "temporary", http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"models": []map[string]any{{
				"slug":         "only-b",
				"display_name": "only-b",
			}}})
		}))
		defer upstream.Close()

		server, apiKey := newRuntimeModelsServer(t, authDir, upstream.URL, nil)
		blockModelBackgroundRetries(server)
		resp := doModelRequest(t, server, apiKey, "/v1/models?client_version=0.146.0")
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
		}
		var payload struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode models: %v", err)
		}
		if got := modelSlugs(payload.Models); !slices.Equal(got, []string{"only-b"}) {
			t.Fatalf("partial-failure models = %v, want only successful auth contribution", got)
		}
	})

	t.Run("all auth failure uses embedded catalog", func(t *testing.T) {
		authDir := t.TempDir()
		writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
		writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/backend-api/codex/models" {
				http.Error(w, "temporary", http.StatusServiceUnavailable)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		}))
		defer upstream.Close()

		server, apiKey := newRuntimeModelsServer(t, authDir, upstream.URL, nil)
		blockModelBackgroundRetries(server)
		resp := doModelRequest(t, server, apiKey, "/v1/models?client_version=0.146.0")
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
		}
		var payload struct {
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode models: %v", err)
		}
		if findCodexClientModel(payload.Models, "gpt-5.6-sol") == nil {
			t.Fatalf("embedded fallback missing gpt-5.6-sol: %#v", payload.Models)
		}

		proxyReq := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(`{"model":"not-in-embedded-catalog","input":"hello"}`),
		)
		proxyReq.Header.Set("Authorization", "Bearer "+apiKey)
		proxyReq.Header.Set("Content-Type", "application/json")
		proxyReq.Header.Set("User-Agent", "codex_cli_rs/0.146.0")
		proxyResp := httptest.NewRecorder()
		server.ServeHTTP(proxyResp, proxyReq)
		if proxyResp.Code != http.StatusOK {
			t.Fatalf("embedded fallback incorrectly restricted routing: status=%d body=%s", proxyResp.Code, proxyResp.Body.String())
		}
	})
}

func TestServerModelsClientVersionValidationAndEmptyDefault(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	var versionsMu sync.Mutex
	var versions []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		versionsMu.Lock()
		versions = append(versions, r.URL.Query().Get("client_version"))
		versionsMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"models": []map[string]any{}})
	}))
	defer upstream.Close()

	server, apiKey := newRuntimeModelsServer(t, authDir, upstream.URL, func(cfg *Config) {
		cfg.CodexUserAgent = "codex_cli_rs/0.145.0 (Linux; x86_64)"
	})
	empty := doModelRequest(t, server, apiKey, "/v1/models?client_version=")
	if empty.Code != http.StatusOK {
		t.Fatalf("empty version status = %d, want 200, body: %s", empty.Code, empty.Body.String())
	}
	invalid := doModelRequest(t, server, apiKey, "/v1/models?client_version=0.146.0%0Anext")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid version status = %d, want 400, body: %s", invalid.Code, invalid.Body.String())
	}

	versionsMu.Lock()
	gotVersions := slices.Clone(versions)
	versionsMu.Unlock()
	if !slices.Equal(gotVersions, []string{"0.145.0"}) {
		t.Fatalf("upstream versions = %v, want configured default only", gotVersions)
	}
}

func TestServerModelFetchUnauthorizedRefreshDoesNotRebindSession(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-old", false)
	var modelCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls.Add(1)
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"code": "unauthorized"},
		})
	}))
	defer upstream.Close()
	var refreshCalls atomic.Int32
	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  "access-new",
			"refresh_token": "refresh-acct_a",
			"expires_in":    3600,
		})
	}))
	defer refresh.Close()

	server, apiKey := newRuntimeModelsServer(t, authDir, upstream.URL, func(cfg *Config) {
		cfg.CodexRefreshTokenURL = refresh.URL
	})
	blockModelBackgroundRetries(server)
	binding := bindRuntimeModelsSession(t, server, apiKey, "sticky-unauthorized", "account:acct_a")

	resp := doModelRequest(t, server, apiKey, "/v1/models?client_version=0.146.0")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want embedded fallback 200, body: %s", resp.Code, resp.Body.String())
	}
	if modelCalls.Load() != 2 || refreshCalls.Load() != 1 {
		t.Fatalf("model calls=%d refresh calls=%d, want 2 and 1", modelCalls.Load(), refreshCalls.Load())
	}
	state, ok := server.health.State("account:acct_a", "")
	if !ok || state.Kind != AuthHealthUnauthorized {
		t.Fatalf("auth health = %#v, %t, want continued unauthorized", state, ok)
	}
	assertRuntimeModelsBindingUnchanged(t, server, binding)
}

func TestServerModelFetchRateLimitSetsCooldownWithoutRebindingSession(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	now := time.Now().UTC()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{"code": "rate_limit_exceeded"},
		})
	}))
	defer upstream.Close()

	server, apiKey := newRuntimeModelsServer(t, authDir, upstream.URL, nil)
	blockModelBackgroundRetries(server)
	binding := bindRuntimeModelsSession(t, server, apiKey, "sticky-rate-limit", "account:acct_a")

	resp := doModelRequest(t, server, apiKey, "/v1/models?client_version=0.146.0")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want embedded fallback 200, body: %s", resp.Code, resp.Body.String())
	}
	state, ok := server.health.State("account:acct_a", "")
	if !ok || state.Kind != AuthHealthQuota || !state.Authoritative {
		t.Fatalf("auth health = %#v, %t, want authoritative quota", state, ok)
	}
	if state.RetryAt.Before(now.Add(119 * time.Second)) {
		t.Fatalf("quota retry time = %s, want Retry-After near %s", state.RetryAt, now.Add(120*time.Second))
	}
	assertRuntimeModelsBindingUnchanged(t, server, binding)
}

func TestServerModelSupportRebindsWholeSession(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
	var responseAuthsMu sync.Mutex
	var responseAuths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		if r.URL.Path == "/backend-api/codex/models" {
			models := []map[string]any{{"slug": "common", "display_name": "common"}}
			if authorization == "Bearer access-b" {
				models = append(models, map[string]any{"slug": "special", "display_name": "special"})
			}
			writeJSON(w, http.StatusOK, map[string]any{"models": models})
			return
		}
		responseAuthsMu.Lock()
		responseAuths = append(responseAuths, authorization)
		responseAuthsMu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer upstream.Close()

	server, apiKey := newRuntimeModelsServer(t, authDir, upstream.URL, func(cfg *Config) {
		cfg.RequestRetry = 0
		cfg.requestRetrySet = true
		cfg.MaxRetryInterval = 0
		cfg.maxRetryIntervalSet = true
	})
	models := doModelRequest(t, server, apiKey, "/v1/models?client_version=0.146.0")
	if models.Code != http.StatusOK {
		t.Fatalf("model synchronization status=%d, want 200, body=%s", models.Code, models.Body.String())
	}
	for _, model := range []string{"common", "special", "common"} {
		req := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello"}`, model)),
		)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Session-Id", "sticky-model-support")
		req.Header.Set("User-Agent", "codex_cli_rs/0.146.0")
		resp := httptest.NewRecorder()
		server.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("model %s status=%d, want 200, body=%s", model, resp.Code, resp.Body.String())
		}
	}
	unsupported := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"unsupported","input":"hello"}`),
	)
	unsupported.Header.Set("Authorization", "Bearer "+apiKey)
	unsupported.Header.Set("Content-Type", "application/json")
	unsupported.Header.Set("Session-Id", "sticky-model-support")
	unsupported.Header.Set("User-Agent", "codex_cli_rs/0.146.0")
	unsupportedResp := httptest.NewRecorder()
	server.ServeHTTP(unsupportedResp, unsupported)
	if unsupportedResp.Code != http.StatusNotFound {
		t.Fatalf("unsupported model status=%d, want 404, body=%s", unsupportedResp.Code, unsupportedResp.Body.String())
	}

	responseAuthsMu.Lock()
	gotAuths := slices.Clone(responseAuths)
	responseAuthsMu.Unlock()
	wantAuths := []string{"Bearer access-a", "Bearer access-b", "Bearer access-b"}
	if !slices.Equal(gotAuths, wantAuths) {
		t.Fatalf("response auths = %v, want whole-session rebind %v", gotAuths, wantAuths)
	}
}

func newRuntimeModelsServer(
	t *testing.T,
	authDir string,
	upstreamURL string,
	configure func(*Config),
) (*Server, string) {
	t.Helper()
	cfg := &Config{
		AuthDir:        authDir,
		AdminAPIKey:    "admin-key",
		Database:       DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:   upstreamURL + "/backend-api/codex",
		ChatGPTBaseURL: upstreamURL + "/backend-api",
	}
	if configure != nil {
		configure(cfg)
	}
	server, err := NewHandler(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	apiKey := createManagedUser(t, server, "admin-key", "Alice").PlaintextAPIKey
	return server, apiKey
}

func doModelRequest(t *testing.T, server *Server, apiKey string, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	return resp
}

func blockModelBackgroundRetries(server *Server) {
	server.models.waitRetry = func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}
}

func bindRuntimeModelsSession(
	t *testing.T,
	server *Server,
	apiKey string,
	sessionID string,
	authID string,
) SessionAffinityBinding {
	t.Helper()
	credential, err := server.authenticateUserAPIKeyFromTokens(context.Background(), []string{apiKey})
	if err != nil {
		t.Fatalf("authenticate managed key: %v", err)
	}
	authorization := proxyAuthorization{Credential: &credential}
	digests := sessionAffinityDigestsForRequest(authorization, []sessionAffinitySignal{{
		Kind:  sessionAffinitySignalSessionID,
		Value: sessionID,
	}})
	binding, err := server.users.BindSessionAffinity(context.Background(), digests, authID)
	if err != nil {
		t.Fatalf("bind session affinity: %v", err)
	}
	return binding
}

func assertRuntimeModelsBindingUnchanged(
	t *testing.T,
	server *Server,
	want SessionAffinityBinding,
) {
	t.Helper()
	got, found, err := server.users.LookupSessionAffinity(context.Background(), want.Digests)
	if err != nil {
		t.Fatalf("lookup session affinity: %v", err)
	}
	if !found || got.AuthID != want.AuthID || got.BindingDigest != want.BindingDigest {
		t.Fatalf("session binding changed after catalog refresh: got=%#v found=%t want=%#v", got, found, want)
	}
}

func TestConfiguredClientVersionFromUserAgent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  *Config
		want string
	}{
		{name: "default", cfg: &Config{}, want: "0.146.0"},
		{name: "configured", cfg: &Config{CodexUserAgent: "codex_cli_rs/0.145.0 (Linux)"}, want: "0.145.0"},
		{name: "configured custom product", cfg: &Config{CodexUserAgent: "custom-client/1.2.3"}, want: "1.2.3"},
		{name: "invalid configured fallback", cfg: &Config{CodexUserAgent: "custom-client"}, want: "0.146.0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := configuredClientVersion(test.cfg); got != test.want {
				t.Fatalf("configuredClientVersion() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRequestClientVersionUsesCodexUserAgentOnly(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		userAgent string
		want      string
	}{
		{userAgent: "codex_cli_rs/0.144.1 (Linux)", want: "0.144.1"},
		{userAgent: "codex-tui/0.143.0 (Mac OS)", want: "0.143.0"},
		{userAgent: "curl/8.10.1", want: "0.146.0"},
	} {
		req := &http.Request{Header: make(http.Header), URL: &url.URL{}}
		req.Header.Set("User-Agent", test.userAgent)
		if got := requestClientVersion(req, &Config{}); got != test.want {
			t.Fatalf("requestClientVersion(%q) = %q, want %q", test.userAgent, got, test.want)
		}
	}
}
