package codexonly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
		AdminAPIKey:    "admin-key",
		Database:       DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:   upstream.URL + "/backend-api/codex",
		CodexUserAgent: "codex-tui/0.139.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.139.0)",
		RequestRetry:   1,
	}
	handler, err := NewHandler(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.3-codex","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+userKey)
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
		AdminAPIKey:  "admin-key",
		Database:     DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL: upstream.URL + "/backend-api/codex",
		RequestRetry: 1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey

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
			req.Header.Set("Authorization", "Bearer "+userKey)
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
		AdminAPIKey:    "admin-key",
		Database:       DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:   codexUpstream.URL + "/backend-api/codex",
		ChatGPTBaseURL: chatGPTUpstream.URL + "/backend-api",
		RequestRetry:   1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey

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
			req.Header.Set("Authorization", "Bearer "+userKey)
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

func TestServerProxiesChatGPTWhamUsageEndpoint(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"account_id": "acct_1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("WHAM endpoint was sent to codex upstream: %s", r.URL.Path)
	}))
	defer codexUpstream.Close()

	var sawPath string
	var sawAuthorization string
	var sawAccount string
	chatGPTUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawAuthorization = r.Header.Get("Authorization")
		sawAccount = r.Header.Get("Chatgpt-Account-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro_lite","rate_limit":{"primary_window":{"used_percent":42,"limit_window_seconds":18000,"reset_at":1893456000},"secondary_window":{"used_percent":12,"limit_window_seconds":604800,"reset_at":1894060800}}}`))
	}))
	defer chatGPTUpstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		Port:           8317,
		AuthDir:        authDir,
		AdminAPIKey:    "admin-key",
		Database:       DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:   codexUpstream.URL + "/backend-api/codex",
		ChatGPTBaseURL: chatGPTUpstream.URL + "/backend-api",
		RequestRetry:   1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey

	req := httptest.NewRequest(http.MethodGet, "/backend-api/wham/usage", nil)
	req.Header.Set("Authorization", "Bearer "+userKey)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	if sawPath != "/backend-api/wham/usage" {
		t.Fatalf("upstream path = %q, want /backend-api/wham/usage", sawPath)
	}
	if sawAuthorization != "Bearer access-1" {
		t.Fatalf("upstream Authorization = %q, want Bearer access-1", sawAuthorization)
	}
	if sawAccount != "acct_1" {
		t.Fatalf("upstream Chatgpt-Account-Id = %q, want acct_1", sawAccount)
	}
}

func TestServerProxiesChatGPTHostedMCPRoutes(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"account_id": "acct_1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	codexUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("hosted MCP endpoint was sent to codex upstream: %s", r.URL.Path)
	}))
	defer codexUpstream.Close()

	var sawPath string
	var sawAuthorization string
	var sawAccount string
	var sawProtocolVersion string
	var sawSessionID string
	var sawAccept string
	var sawBody string
	chatGPTUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		sawAuthorization = r.Header.Get("Authorization")
		sawAccount = r.Header.Get("Chatgpt-Account-Id")
		sawProtocolVersion = r.Header.Get("MCP-Protocol-Version")
		sawSessionID = r.Header.Get("Mcp-Session-Id")
		sawAccept = r.Header.Get("Accept")
		body, _ := io.ReadAll(r.Body)
		sawBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"codex_apps","version":"test"}}}`))
	}))
	defer chatGPTUpstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		Port:           8317,
		AuthDir:        authDir,
		AdminAPIKey:    "admin-key",
		Database:       DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:   codexUpstream.URL + "/backend-api/codex",
		ChatGPTBaseURL: chatGPTUpstream.URL + "/backend-api",
		RequestRetry:   1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey

	tests := []struct {
		name     string
		path     string
		wantPath string
	}{
		{
			name:     "legacy codex apps MCP",
			path:     "/backend-api/wham/apps",
			wantPath: "/backend-api/wham/apps",
		},
		{
			name:     "hosted plugin runtime MCP",
			path:     "/backend-api/ps/mcp",
			wantPath: "/backend-api/ps/mcp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sawPath = ""
			sawAuthorization = ""
			sawAccount = ""
			sawProtocolVersion = ""
			sawSessionID = ""
			sawAccept = ""
			sawBody = ""

			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
			req.Header.Set("Authorization", "Bearer "+userKey)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("MCP-Protocol-Version", "2025-06-18")
			req.Header.Set("Mcp-Session-Id", "session-1")
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
			if sawProtocolVersion != "2025-06-18" {
				t.Fatalf("upstream MCP-Protocol-Version = %q, want 2025-06-18", sawProtocolVersion)
			}
			if sawSessionID != "session-1" {
				t.Fatalf("upstream Mcp-Session-Id = %q, want session-1", sawSessionID)
			}
			if sawAccept != "application/json, text/event-stream" {
				t.Fatalf("upstream Accept = %q, want application/json, text/event-stream", sawAccept)
			}
			if sawBody != `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` {
				t.Fatalf("upstream body = %q", sawBody)
			}
		})
	}
}

func TestServerAcceptsCodexOAuthForChatGPTBackendRoutes(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "auth.json", `{
		"tokens": {
			"access_token": "access-1",
			"refresh_token": "refresh-1",
			"account_id": "acct_1"
		}
	}`)

	var sawPath string
	chatGPTUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"pro_lite"}`))
	}))
	defer chatGPTUpstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		Port:           8317,
		AuthDir:        authDir,
		CodexBaseURL:   chatGPTUpstream.URL + "/backend-api/codex",
		ChatGPTBaseURL: chatGPTUpstream.URL + "/backend-api",
		RequestRetry:   1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/backend-api/wham/usage", nil)
	req.Header.Set("Authorization", "Bearer access-1")
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	if sawPath != "/backend-api/wham/usage" {
		t.Fatalf("upstream path = %q, want /backend-api/wham/usage", sawPath)
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
		AdminAPIKey:  "admin-key",
		Database:     DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL: "http://127.0.0.1:1/backend-api/codex",
		RequestRetry: 1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey

	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.139.0", nil)
	req.Header.Set("Authorization", "Bearer "+userKey)
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
		AdminAPIKey:  "admin-key",
		Database:     DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
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

func TestServerAcceptsUserAPIKeyFromXAPIKeyWithUnrelatedAuthorization(t *testing.T) {
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
		AdminAPIKey:  "admin-key",
		Database:     DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL: upstream.URL + "/backend-api/codex",
		RequestRetry: 1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer chatgpt-token")
	req.Header.Set("X-API-Key", userKey)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
}

func TestManagementAPIDisabledWithoutAdminAPIKey(t *testing.T) {
	handler := newUserManagementTestHandler(t, &Config{})

	req := httptest.NewRequest(http.MethodGet, "/v0/management/users", nil)
	resp := httptest.NewRecorder()

	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", resp.Code, resp.Body.String())
	}
}

func TestManagementAPICreatesAndListsUsers(t *testing.T) {
	handler := newUserManagementTestHandler(t, &Config{
		AdminAPIKey: "admin-key",
	})

	createdResp := doJSONRequest(t, handler, http.MethodPost, "/v0/management/users", `{"name":" Alice "}`, "admin-key")
	if createdResp.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201, body: %s", createdResp.Code, createdResp.Body.String())
	}
	var created CreatedUserAPIKey
	decodeResponse(t, createdResp, &created)
	if created.User.Name != "Alice" {
		t.Fatalf("created user name = %q, want Alice", created.User.Name)
	}
	if created.PlaintextAPIKey == "" || !strings.HasPrefix(created.PlaintextAPIKey, "cop_") {
		t.Fatalf("plaintext API key = %q, want cop_ prefix", created.PlaintextAPIKey)
	}

	duplicateResp := doJSONRequest(t, handler, http.MethodPost, "/v0/management/users", `{"name":"alice"}`, "admin-key")
	if duplicateResp.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409, body: %s", duplicateResp.Code, duplicateResp.Body.String())
	}

	listResp := doJSONRequest(t, handler, http.MethodGet, "/v0/management/users", "", "admin-key")
	if listResp.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200, body: %s", listResp.Code, listResp.Body.String())
	}
	if strings.Contains(listResp.Body.String(), created.PlaintextAPIKey) {
		t.Fatalf("list response leaked plaintext API key: %s", listResp.Body.String())
	}
	var list struct {
		Users []UserWithAPIKey `json:"users"`
	}
	decodeResponse(t, listResp, &list)
	if len(list.Users) != 1 {
		t.Fatalf("list user count = %d, want 1", len(list.Users))
	}
	if list.Users[0].APIKey == nil || list.Users[0].APIKey.MaskedKey == "" {
		t.Fatalf("listed API key metadata = %#v, want masked key", list.Users[0].APIKey)
	}

	getResp := doJSONRequest(t, handler, http.MethodGet, "/v0/management/users/"+created.User.ID, "", "admin-key")
	if getResp.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200, body: %s", getResp.Code, getResp.Body.String())
	}
	var got UserWithAPIKey
	decodeResponse(t, getResp, &got)
	if got.User.ID != created.User.ID {
		t.Fatalf("got user ID = %q, want %q", got.User.ID, created.User.ID)
	}

	userKeyManagementResp := doJSONRequest(t, handler, http.MethodGet, "/v0/management/users", "", created.PlaintextAPIKey)
	if userKeyManagementResp.Code != http.StatusUnauthorized {
		t.Fatalf("user key management status = %d, want 401", userKeyManagementResp.Code)
	}
}

func TestUserAPIKeySelfService(t *testing.T) {
	handler := newUserManagementTestHandler(t, &Config{
		AdminAPIKey: "admin-key",
	})
	created := createManagedUser(t, handler, "admin-key", "Alice")

	adminResp := doJSONRequest(t, handler, http.MethodGet, "/v0/user/api-key", "", "admin-key")
	if adminResp.Code != http.StatusUnauthorized {
		t.Fatalf("admin key user API status = %d, want 401", adminResp.Code)
	}

	getResp := doJSONRequest(t, handler, http.MethodGet, "/v0/user/api-key", "", created.PlaintextAPIKey)
	if getResp.Code != http.StatusOK {
		t.Fatalf("user get status = %d, want 200, body: %s", getResp.Code, getResp.Body.String())
	}
	if strings.Contains(getResp.Body.String(), created.PlaintextAPIKey) {
		t.Fatalf("user get response leaked plaintext API key: %s", getResp.Body.String())
	}
	var current UserWithAPIKey
	decodeResponse(t, getResp, &current)
	if current.User.ID != created.User.ID {
		t.Fatalf("current user ID = %q, want %q", current.User.ID, created.User.ID)
	}

	resetResp := doJSONRequest(t, handler, http.MethodPost, "/v0/user/api-key/reset", "", created.PlaintextAPIKey)
	if resetResp.Code != http.StatusOK {
		t.Fatalf("user reset status = %d, want 200, body: %s", resetResp.Code, resetResp.Body.String())
	}
	var reset CreatedUserAPIKey
	decodeResponse(t, resetResp, &reset)
	if reset.PlaintextAPIKey == "" || reset.PlaintextAPIKey == created.PlaintextAPIKey {
		t.Fatalf("reset plaintext API key = %q, want new non-empty key", reset.PlaintextAPIKey)
	}

	oldResp := doJSONRequest(t, handler, http.MethodGet, "/v0/user/api-key", "", created.PlaintextAPIKey)
	if oldResp.Code != http.StatusUnauthorized {
		t.Fatalf("old key status = %d, want 401", oldResp.Code)
	}
	newResp := doJSONRequest(t, handler, http.MethodGet, "/v0/user/api-key", "", reset.PlaintextAPIKey)
	if newResp.Code != http.StatusOK {
		t.Fatalf("new key status = %d, want 200, body: %s", newResp.Code, newResp.Body.String())
	}
}

func TestUserAPIDisabledUserForbidden(t *testing.T) {
	handler := newUserManagementTestHandler(t, &Config{AdminAPIKey: "admin-key"})
	created := createManagedUser(t, handler, "admin-key", "Alice")

	disableResp := doJSONRequest(t, handler, http.MethodPatch, "/v0/management/users/"+created.User.ID, `{"enabled":false}`, "admin-key")
	if disableResp.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200, body: %s", disableResp.Code, disableResp.Body.String())
	}
	disabledResp := doJSONRequest(t, handler, http.MethodGet, "/v0/user/api-key", "", created.PlaintextAPIKey)
	if disabledResp.Code != http.StatusForbidden {
		t.Fatalf("disabled user status = %d, want 403, body: %s", disabledResp.Code, disabledResp.Body.String())
	}

	enableResp := doJSONRequest(t, handler, http.MethodPatch, "/v0/management/users/"+created.User.ID, `{"enabled":true}`, "admin-key")
	if enableResp.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200, body: %s", enableResp.Code, enableResp.Body.String())
	}
	enabledResp := doJSONRequest(t, handler, http.MethodGet, "/v0/user/api-key", "", created.PlaintextAPIKey)
	if enabledResp.Code != http.StatusOK {
		t.Fatalf("re-enabled user status = %d, want 200, body: %s", enabledResp.Code, enabledResp.Body.String())
	}
}

func TestProxyAcceptsStoredUserAPIKeyAndRejectsDisabledUser(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	var sawAuthorization string
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		sawAuthorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:        authDir,
		AdminAPIKey:    "admin-key",
		Database:       DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:   upstream.URL + "/backend-api/codex",
		RequestRetry:   1,
		ChatGPTBaseURL: upstream.URL + "/backend-api",
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	created := createManagedUser(t, handler, "admin-key", "Alice")

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+created.PlaintextAPIKey)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("stored user key proxy status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	if sawAuthorization != "Bearer access-1" {
		t.Fatalf("upstream Authorization = %q, want Bearer access-1", sawAuthorization)
	}

	disableResp := doJSONRequest(t, handler, http.MethodPatch, "/v0/management/users/"+created.User.ID, `{"enabled":false}`, "admin-key")
	if disableResp.Code != http.StatusOK {
		t.Fatalf("disable status = %d, want 200, body: %s", disableResp.Code, disableResp.Body.String())
	}
	disabledReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	disabledReq.Header.Set("Authorization", "Bearer "+created.PlaintextAPIKey)
	disabledResp := httptest.NewRecorder()
	handler.ServeHTTP(disabledResp, disabledReq)
	if disabledResp.Code != http.StatusForbidden {
		t.Fatalf("disabled user proxy status = %d, want 403, body: %s", disabledResp.Code, disabledResp.Body.String())
	}
	if upstreamCalls != 1 {
		t.Fatalf("upstream calls after disabled proxy = %d, want 1", upstreamCalls)
	}

	enableResp := doJSONRequest(t, handler, http.MethodPatch, "/v0/management/users/"+created.User.ID, `{"enabled":true}`, "admin-key")
	if enableResp.Code != http.StatusOK {
		t.Fatalf("enable status = %d, want 200, body: %s", enableResp.Code, enableResp.Body.String())
	}
	enabledReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	enabledReq.Header.Set("Authorization", "Bearer "+created.PlaintextAPIKey)
	enabledResp := httptest.NewRecorder()
	handler.ServeHTTP(enabledResp, enabledReq)
	if enabledResp.Code != http.StatusOK {
		t.Fatalf("re-enabled user proxy status = %d, want 200, body: %s", enabledResp.Code, enabledResp.Body.String())
	}
}

func TestProxyRejectsUnauthenticatedRequests(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("unauthenticated proxy request reached upstream")
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:        authDir,
		AdminAPIKey:    "admin-key",
		Database:       DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:   upstream.URL + "/backend-api/codex",
		ChatGPTBaseURL: upstream.URL + "/backend-api",
		RequestRetry:   1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body: %s", resp.Code, resp.Body.String())
	}
}

func TestDebugLogsSuccessfulProxyRequestWithoutSecrets(t *testing.T) {
	var logs bytes.Buffer
	restore := captureStandardLogger(t, &logs)
	defer restore()

	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:      authDir,
		AdminAPIKey:  "admin-key",
		Database:     DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL: upstream.URL + "/backend-api/codex",
		RequestRetry: 1,
		Debug:        true,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+userKey)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	got := logs.String()
	for _, want := range []string{
		"debug enabled",
		"request received method=POST path=/v1/responses",
		"proxy auth ok method=POST path=/v1/responses auth=user_api_key",
		"proxy upstream request method=POST path=/v1/responses",
		"target_path=/backend-api/codex/responses",
		"proxy upstream response method=POST path=/v1/responses status=200",
		"request completed method=POST path=/v1/responses status=200",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("debug log missing %q\nlogs:\n%s", want, got)
		}
	}
	for _, secret := range []string{userKey, "access-1", "refresh-1"} {
		if strings.Contains(got, secret) {
			t.Fatalf("debug log leaked secret %q\nlogs:\n%s", secret, got)
		}
	}
}

func TestDebugLogsUnauthorizedProxyRequestWithoutSecrets(t *testing.T) {
	var logs bytes.Buffer
	restore := captureStandardLogger(t, &logs)
	defer restore()

	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:      authDir,
		AdminAPIKey:  "admin-key",
		Database:     DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL: "http://127.0.0.1:1/backend-api/codex",
		RequestRetry: 1,
		Debug:        true,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer cop_missing")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body: %s", resp.Code, resp.Body.String())
	}
	got := logs.String()
	for _, want := range []string{
		"request received method=POST path=/v1/responses",
		"proxy auth failed method=POST path=/v1/responses",
		"token_sources=authorization:bearer",
		"request completed method=POST path=/v1/responses status=401",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("debug log missing %q\nlogs:\n%s", want, got)
		}
	}
	for _, secret := range []string{"cop_missing", "access-1", "refresh-1"} {
		if strings.Contains(got, secret) {
			t.Fatalf("debug log leaked secret %q\nlogs:\n%s", secret, got)
		}
	}
}

func captureStandardLogger(t *testing.T, buf *bytes.Buffer) func() {
	t.Helper()
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	oldPrefix := log.Prefix()
	log.SetOutput(buf)
	log.SetFlags(0)
	log.SetPrefix("")
	return func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
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

func newUserManagementTestHandler(t *testing.T, cfg *Config) http.Handler {
	t.Helper()
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"expired": "2099-01-01T00:00:00Z"
	}`)
	if cfg == nil {
		cfg = &Config{}
	}
	cfg.AuthDir = authDir
	cfg.Database.Path = filepath.Join(t.TempDir(), "users.db")
	cfg.CodexBaseURL = "http://127.0.0.1:1/backend-api/codex"
	cfg.RequestRetry = 1
	handler, err := NewHandler(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	return handler
}

func doJSONRequest(t *testing.T, handler http.Handler, method string, path string, body string, token string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = bytes.NewBufferString(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

func decodeResponse(t *testing.T, resp *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(resp.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response %q: %v", resp.Body.String(), err)
	}
}

func createManagedUser(t *testing.T, handler http.Handler, adminKey string, name string) CreatedUserAPIKey {
	t.Helper()
	resp := doJSONRequest(t, handler, http.MethodPost, "/v0/management/users", fmt.Sprintf(`{"name":%q}`, name), adminKey)
	if resp.Code != http.StatusCreated {
		t.Fatalf("create managed user status = %d, want 201, body: %s", resp.Code, resp.Body.String())
	}
	var created CreatedUserAPIKey
	decodeResponse(t, resp, &created)
	return created
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
