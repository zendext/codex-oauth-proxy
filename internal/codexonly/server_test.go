package codexonly

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServerProxiesResponsesWithCodexAuthHeaders(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"account_id": "acct_1",
		"email": "user@example.com",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	var sawPath string
	var sawAuthorization string
	var sawAccount string
	var sawBody string
	var sawUserAgent string
	var sawSessionID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawAuthorization = r.Header.Get("Authorization")
		sawAccount = r.Header.Get("Chatgpt-Account-Id")
		sawUserAgent = r.Header.Get("User-Agent")
		sawSessionID = r.Header.Get("Session_id")
		body, _ := io.ReadAll(r.Body)
		sawBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Port:           8317,
		AuthDir:        authDir,
		APIKeys:        []string{"proxy-key"},
		CodexBaseURL:   upstream.URL + "/backend-api/codex",
		CodexUserAgent: "codex-tui/0.139.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.139.0)",
		RequestRetry:   1,
	}
	handler, err := NewHandler(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.3-codex","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer proxy-key")
	req.Header.Set("User-Agent", "generic-responses-client/1.0")
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	if sawPath != "/backend-api/codex/responses" {
		t.Fatalf("upstream path = %q, want /backend-api/codex/responses", sawPath)
	}
	if sawAuthorization != "Bearer access-1" {
		t.Fatalf("upstream Authorization = %q, want Bearer access-1", sawAuthorization)
	}
	if sawAccount != "acct_1" {
		t.Fatalf("upstream Chatgpt-Account-Id = %q, want acct_1", sawAccount)
	}
	if sawUserAgent != cfg.CodexUserAgent {
		t.Fatalf("upstream User-Agent = %q, want configured Codex User-Agent %q", sawUserAgent, cfg.CodexUserAgent)
	}
	if strings.TrimSpace(sawSessionID) == "" {
		t.Fatalf("upstream Session_id is empty")
	}
	if sawBody != `{"model":"gpt-5.3-codex","input":"hello"}` {
		t.Fatalf("upstream body = %q", sawBody)
	}
	if got := resp.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestServerProxiesAdditionalCodexEndpoints(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"account_id": "acct_1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	var sawPath string
	var sawQuery string
	var sawAuthorization string
	var sawContentType string
	var sawOpenAIBeta string
	var sawBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawQuery = r.URL.RawQuery
		sawAuthorization = r.Header.Get("Authorization")
		sawContentType = r.Header.Get("Content-Type")
		sawOpenAIBeta = r.Header.Get("OpenAI-Beta")
		body, _ := io.ReadAll(r.Body)
		sawBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		Port:         8317,
		AuthDir:      authDir,
		APIKeys:      []string{"proxy-key"},
		CodexBaseURL: upstream.URL + "/backend-api/codex",
		RequestRetry: 1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}

	tests := []struct {
		name            string
		method          string
		path            string
		body            string
		contentType     string
		wantPath        string
		wantQuery       string
		wantContentType string
		wantOpenAIBeta  string
	}{
		{
			name:            "web search",
			method:          http.MethodPost,
			path:            "/v1/alpha/search?region=us",
			body:            `{"q":"codex"}`,
			wantPath:        "/backend-api/codex/alpha/search",
			wantQuery:       "region=us",
			wantContentType: "application/json",
		},
		{
			name:            "image generation",
			method:          http.MethodPost,
			path:            "/v1/images/generations",
			body:            `{"prompt":"diagram"}`,
			wantPath:        "/backend-api/codex/images/generations",
			wantContentType: "application/json",
		},
		{
			name:            "image edit preserves multipart content type",
			method:          http.MethodPost,
			path:            "/v1/images/edits",
			body:            "--boundary\r\ncontent\r\n--boundary--\r\n",
			contentType:     "multipart/form-data; boundary=boundary",
			wantPath:        "/backend-api/codex/images/edits",
			wantContentType: "multipart/form-data; boundary=boundary",
		},
		{
			name:            "memory summarize",
			method:          http.MethodPost,
			path:            "/v1/memories/trace_summarize",
			body:            `{"input":[]}`,
			wantPath:        "/backend-api/codex/memories/trace_summarize",
			wantContentType: "application/json",
		},
		{
			name:            "realtime call preserves sdp content type",
			method:          http.MethodPost,
			path:            "/v1/realtime/calls",
			body:            "v=0",
			contentType:     "application/sdp",
			wantPath:        "/backend-api/codex/realtime/calls",
			wantContentType: "application/sdp",
		},
		{
			name:           "realtime websocket does not add responses beta",
			method:         http.MethodGet,
			path:           "/v1/realtime?intent=conversation&model=gpt-realtime",
			wantPath:       "/backend-api/codex/realtime",
			wantQuery:      "intent=conversation&model=gpt-realtime",
			wantOpenAIBeta: "",
		},
		{
			name:            "direct backend codex alias",
			method:          http.MethodPost,
			path:            "/backend-api/codex/images/generations",
			body:            `{"prompt":"diagram"}`,
			wantPath:        "/backend-api/codex/images/generations",
			wantContentType: "application/json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sawPath = ""
			sawQuery = ""
			sawAuthorization = ""
			sawContentType = ""
			sawOpenAIBeta = ""
			sawBody = ""

			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer proxy-key")
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			if tt.path == "/v1/realtime?intent=conversation&model=gpt-realtime" {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
			}
			resp := httptest.NewRecorder()

			handler.ServeHTTP(resp, req)

			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
			}
			if sawPath != tt.wantPath {
				t.Fatalf("upstream path = %q, want %q", sawPath, tt.wantPath)
			}
			if sawQuery != tt.wantQuery {
				t.Fatalf("upstream query = %q, want %q", sawQuery, tt.wantQuery)
			}
			if sawAuthorization != "Bearer access-1" {
				t.Fatalf("upstream Authorization = %q, want Bearer access-1", sawAuthorization)
			}
			if sawContentType != tt.wantContentType {
				t.Fatalf("upstream Content-Type = %q, want %q", sawContentType, tt.wantContentType)
			}
			if sawOpenAIBeta != tt.wantOpenAIBeta {
				t.Fatalf("upstream OpenAI-Beta = %q, want %q", sawOpenAIBeta, tt.wantOpenAIBeta)
			}
			if sawBody != tt.body {
				t.Fatalf("upstream body = %q, want %q", sawBody, tt.body)
			}
		})
	}
}

func TestServerProxiesChatGPTFileEndpoints(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"account_id": "acct_1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("file endpoint was sent to codex upstream: %s", r.URL.Path)
	}))
	defer codexUpstream.Close()

	var sawPath string
	var sawAuthorization string
	var sawAccount string
	var sawBody string
	chatGPTUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawAuthorization = r.Header.Get("Authorization")
		sawAccount = r.Header.Get("Chatgpt-Account-Id")
		body, _ := io.ReadAll(r.Body)
		sawBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer chatGPTUpstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		Port:           8317,
		AuthDir:        authDir,
		APIKeys:        []string{"proxy-key"},
		CodexBaseURL:   codexUpstream.URL + "/backend-api/codex",
		ChatGPTBaseURL: chatGPTUpstream.URL + "/backend-api",
		RequestRetry:   1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}

	tests := []struct {
		name     string
		path     string
		body     string
		wantPath string
	}{
		{
			name:     "create file",
			path:     "/backend-api/files",
			body:     `{"file_name":"a.txt","file_size":1,"use_case":"codex"}`,
			wantPath: "/backend-api/files",
		},
		{
			name:     "finalize file upload",
			path:     "/backend-api/files/file_123/uploaded",
			body:     `{}`,
			wantPath: "/backend-api/files/file_123/uploaded",
		},
		{
			name:     "root files alias",
			path:     "/files",
			body:     `{"file_name":"a.txt","file_size":1,"use_case":"codex"}`,
			wantPath: "/backend-api/files",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sawPath = ""
			sawAuthorization = ""
			sawAccount = ""
			sawBody = ""

			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer proxy-key")
			resp := httptest.NewRecorder()

			handler.ServeHTTP(resp, req)

			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
			}
			if sawPath != tt.wantPath {
				t.Fatalf("upstream path = %q, want %q", sawPath, tt.wantPath)
			}
			if sawAuthorization != "Bearer access-1" {
				t.Fatalf("upstream Authorization = %q, want Bearer access-1", sawAuthorization)
			}
			if sawAccount != "acct_1" {
				t.Fatalf("upstream Chatgpt-Account-Id = %q, want acct_1", sawAccount)
			}
			if sawBody != tt.body {
				t.Fatalf("upstream body = %q, want %q", sawBody, tt.body)
			}
		})
	}
}

func TestCodexClientModelsIncludeFullCodexMetadata(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	handler, err := NewHandler(context.Background(), &Config{
		Port:         8317,
		AuthDir:      authDir,
		APIKeys:      []string{"proxy-key"},
		CodexBaseURL: "http://127.0.0.1:1/backend-api/codex",
		RequestRetry: 1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.139.0", nil)
	req.Header.Set("Authorization", "Bearer proxy-key")
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	model := findCodexClientModel(payload.Models, "gpt-5.5")
	if model == nil {
		t.Fatalf("gpt-5.5 model metadata not found in %#v", payload.Models)
	}
	if got := model["display_name"]; got != "GPT-5.5" {
		t.Fatalf("display_name = %#v, want GPT-5.5", got)
	}
	if got := model["context_window"]; got != float64(272000) {
		t.Fatalf("context_window = %#v, want 272000", got)
	}
	if got := model["prefer_websockets"]; got != true {
		t.Fatalf("prefer_websockets = %#v, want true", got)
	}
	if !stringSliceFieldContains(model, "available_in_plans", "prolite") {
		t.Fatalf("available_in_plans does not include prolite: %#v", model["available_in_plans"])
	}
	if !reasoningLevelsContain(model, "xhigh") {
		t.Fatalf("supported_reasoning_levels does not include xhigh: %#v", model["supported_reasoning_levels"])
	}
}

func TestServerRejectsInvalidProxyAPIKey(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	handler, err := NewHandler(context.Background(), &Config{
		Port:         8317,
		AuthDir:      authDir,
		APIKeys:      []string{"proxy-key"},
		CodexBaseURL: "http://127.0.0.1:1/backend-api/codex",
		RequestRetry: 1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong-key")
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.Code)
	}
}

func TestServerAcceptsXAPIKeyWithUnrelatedAuthorization(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		Port:         8317,
		AuthDir:      authDir,
		APIKeys:      []string{"proxy-key"},
		CodexBaseURL: upstream.URL + "/backend-api/codex",
		RequestRetry: 1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer chatgpt-token")
	req.Header.Set("X-API-Key", "proxy-key")
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
}

func findCodexClientModel(models []map[string]any, slug string) map[string]any {
	for _, model := range models {
		if model["slug"] == slug {
			return model
		}
	}
	return nil
}

func stringSliceFieldContains(model map[string]any, key string, want string) bool {
	values, ok := model[key].([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func reasoningLevelsContain(model map[string]any, want string) bool {
	values, ok := model["supported_reasoning_levels"].([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		entry, ok := value.(map[string]any)
		if ok && entry["effort"] == want {
			return true
		}
	}
	return false
}

func TestAuthManagerRefreshesExpiredAuthBeforeProxy(t *testing.T) {
	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "codex.json")
	if err := os.WriteFile(authPath, []byte(`{
		"type": "codex",
		"access_token": "old-access",
		"refresh_token": "refresh-1",
		"expired": "2000-01-01T00:00:00Z"
	}`), 0o600); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	refresher := RefresherFunc(func(ctx context.Context, auth *Auth) error {
		auth.AccessToken = "new-access"
		auth.ExpiresAt = time.Now().Add(time.Hour)
		return auth.Save()
	})
	manager := &AuthManager{
		Store:     NewFileAuthStore(authDir),
		Refresher: refresher,
		Now: func() time.Time {
			return time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
		},
	}

	auth, err := manager.Select(context.Background())
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	if auth.AccessToken != "new-access" {
		t.Fatalf("AccessToken = %q, want new-access", auth.AccessToken)
	}
}
