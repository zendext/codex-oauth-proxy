package codexonly

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrokListenerUsesManagedKeyAndStableAuth(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "codex-access",
		"refresh_token": "codex-refresh",
		"expired": "2099-01-01T00:00:00Z"
	}`)
	grokPath := filepath.Join(t.TempDir(), "auth.json")
	writeGrokAuthFile(t, grokPath, map[string]any{
		"https://auth.x.ai": map[string]any{
			"key": "grok-access", "auth_mode": "oidc", "user_id": "user-1",
			"oidc_issuer": "https://auth.x.ai", "oidc_client_id": "client-1",
			"create_time": "2026-07-01T00:00:00Z", "expires_at": "2099-01-01T00:00:00Z",
		},
	})

	var sawAuthorization, sawTokenAuth, sawModel, sawClientKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		sawTokenAuth = r.Header.Get("X-XAI-Token-Auth")
		sawModel = r.Header.Get("x-grok-model-override")
		sawClientKey = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`))
	}))
	defer upstream.Close()

	handler := newGrokTestHandler(t, authDir, grokPath, upstream.URL+"/v1")
	user := createManagedUser(t, handler.Codex, "admin-key", "Alice")
	body := `{"model":"grok-4.6","input":[{"role":"user","content":"hello"}]}`
	resp := doJSONRequest(t, handler.Grok, http.MethodPost, "/v1/responses", body, user.PlaintextAPIKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	if sawAuthorization != "Bearer grok-access" || sawTokenAuth != "xai-grok-cli" || sawModel != "grok-4.6" {
		t.Fatalf("upstream auth/model = %q %q %q", sawAuthorization, sawTokenAuth, sawModel)
	}
	if sawClientKey != "" {
		t.Fatalf("managed key leaked upstream: %q", sawClientKey)
	}

	usage := doJSONRequest(t, handler.Codex, http.MethodGet, "/v0/management/usage?user_id="+user.User.ID, "", "admin-key")
	if usage.Code != http.StatusOK || !strings.Contains(usage.Body.String(), `"total_tokens":8`) {
		t.Fatalf("shared usage = %d %s", usage.Code, usage.Body.String())
	}
	isolated := doJSONRequest(t, handler.Grok, http.MethodPost, "/v1/responses", body, "")
	if isolated.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want 401", isolated.Code)
	}
	codexModels := doJSONRequest(t, handler.Codex, http.MethodGet, "/v1/models", "", user.PlaintextAPIKey)
	if strings.Contains(codexModels.Body.String(), "grok-access") {
		t.Fatal("Codex models response exposed a Grok credential")
	}
}

func TestGrokListenerRejectsCodexOnlyRoutes(t *testing.T) {
	handler := newGrokTestHandler(t, t.TempDir(), filepath.Join(t.TempDir(), "missing.json"), "http://127.0.0.1:1/v1")
	user := createManagedUser(t, handler.Codex, "admin-key", "Alice")
	for _, path := range []string{"/backend-api/codex/responses", "/v0/management/auths", "/v0/user/api-key"} {
		resp := doJSONRequest(t, handler.Grok, http.MethodGet, path, "", user.PlaintextAPIKey)
		if resp.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, resp.Code)
		}
	}
	shared := doJSONRequest(t, handler.Grok, http.MethodGet, "/v0/management/users", "", "admin-key")
	if shared.Code != http.StatusOK {
		t.Fatalf("shared management status = %d, want 200", shared.Code)
	}
}

func TestGrokManagementRefreshesLoginAndReadsSubscriptionUsage(t *testing.T) {
	var sawRefreshClient, sawUsageAuth string
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"token_endpoint":%q}`, "http://"+r.Host+"/oauth/token")
		case "/oauth/token":
			_ = r.ParseForm()
			sawRefreshClient = r.PostForm.Get("client_id")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer authServer.Close()
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUsageAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/billing" || r.URL.Query().Get("format") != "credits" {
			t.Fatalf("usage request = %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":41}}`))
	}))
	defer usageServer.Close()

	path := filepath.Join(t.TempDir(), "auth.json")
	writeGrokAuthFile(t, path, map[string]any{
		"https://auth.x.ai": map[string]any{
			"key": "old-access", "refresh_token": "old-refresh", "auth_mode": "oidc",
			"user_id": "user-1", "oidc_issuer": authServer.URL, "oidc_client_id": "client-1",
			"create_time": "2026-07-01T00:00:00Z", "expires_at": "2099-01-01T00:00:00Z",
		},
		"other": map[string]any{"key": "leave-me"},
	})
	handler := newGrokTestHandler(t, t.TempDir(), path, usageServer.URL)
	status := doJSONRequest(t, handler.Grok, http.MethodGet, "/v0/management/grok/auths", "", "admin-key")
	if status.Code != http.StatusOK {
		t.Fatalf("status = %d %s", status.Code, status.Body.String())
	}
	var listed struct {
		Auths []GrokAuthStatus `json:"auths"`
	}
	decodeResponse(t, status, &listed)
	if len(listed.Auths) != 1 {
		t.Fatalf("auths = %#v, want one login", listed.Auths)
	}
	refreshed := doJSONRequest(t, handler.Grok, http.MethodPost, "/v0/management/grok/auths/refresh", fmt.Sprintf(`{"id":%q}`, listed.Auths[0].ID), "admin-key")
	if refreshed.Code != http.StatusOK || sawRefreshClient != "client-1" {
		t.Fatalf("refresh = %d client %q body %s", refreshed.Code, sawRefreshClient, refreshed.Body.String())
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "fresh-access") || !strings.Contains(string(raw), "leave-me") {
		t.Fatalf("auth file was not safely updated: %v %s", err, raw)
	}
	usage := doJSONRequest(t, handler.Grok, http.MethodGet, "/v0/management/grok/usage?id="+listed.Auths[0].ID, "", "admin-key")
	if usage.Code != http.StatusOK || sawUsageAuth != "Bearer fresh-access" {
		t.Fatalf("usage = %d auth %q body %s", usage.Code, sawUsageAuth, usage.Body.String())
	}
}

func TestGrokAuthSelectionSkipsUnavailableIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeGrokAuthFile(t, path, map[string]any{
		"https://auth.x.ai": map[string]any{
			"key": "current", "auth_mode": "oidc", "user_id": "same-user",
			"oidc_issuer": "https://auth.x.ai", "create_time": "2026-07-02T00:00:00Z",
			"expires_at": "2099-01-01T00:00:00Z",
		},
		"old": map[string]any{
			"key": "old", "auth_mode": "oidc", "user_id": "same-user",
			"oidc_issuer": "https://auth.x.ai", "create_time": "2026-07-01T00:00:00Z",
		},
		"https://accounts.x.ai/sign-in": map[string]any{
			"key": "legacy", "auth_mode": "web_login", "user_id": "legacy-user",
			"create_time": "2099-01-01T00:00:00Z",
		},
		"xai::api_key": map[string]any{"key": "xai-secret", "auth_mode": "api_key", "user_id": "metered"},
	})
	store := NewGrokAuthStore([]string{path})
	first, err := store.Select(context.Background(), nil)
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	second, err := store.Select(context.Background(), map[string]struct{}{first.ID: {}})
	if err != nil {
		t.Fatalf("second Select returned error: %v", err)
	}
	if first.ID == second.ID || first.AccessToken == "" || second.AccessToken == "" {
		t.Fatalf("selection did not rotate stable identities: %#v %#v", first, second)
	}
	if strings.Contains(first.AccessToken+second.AccessToken, "xai-secret") || strings.Contains(first.AccessToken+second.AccessToken, "old") {
		t.Fatalf("selected excluded credential: %q %q", first.AccessToken, second.AccessToken)
	}
}

type grokTestHandler struct {
	Codex http.Handler
	Grok  http.Handler
}

func newGrokTestHandler(t *testing.T, authDir string, grokPath string, upstream string) *grokTestHandler {
	t.Helper()
	cfg := &Config{
		AuthDir:      authDir,
		AdminAPIKey:  "admin-key",
		Database:     DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL: "http://127.0.0.1:1/backend-api/codex",
		Grok:         GrokConfig{AuthFiles: []string{grokPath}, BaseURL: upstream, ClientVersion: "0.2.93"},
	}
	server, err := NewHandler(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return &grokTestHandler{Codex: server, Grok: server.GrokHandler()}
}

func writeGrokAuthFile(t *testing.T, path string, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal Grok auth: %v", err)
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write Grok auth: %v", err)
	}
}
