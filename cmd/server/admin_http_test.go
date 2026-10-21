package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

func runAdminTestCommand(t *testing.T, baseURL string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"admin"}, args...)
	commandArgs = append(commandArgs, "--url", baseURL)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run(context.Background(), commandArgs, &stdout, &stderr); code != 0 {
		t.Fatalf("run exit = %d, want 0, stderr: %s", code, stderr.String())
	}
	return stdout.String()
}

func TestParseCLIAdminUsesDefaultURL(t *testing.T) {
	parsed, err := parseCLI([]string{"admin", "users", "list"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if parsed.app.Admin.URL != defaultAdminBaseURL {
		t.Fatalf("url = %q, want %q", parsed.app.Admin.URL, defaultAdminBaseURL)
	}
}

func TestAdminGrokStatusReachesLoopbackHandler(t *testing.T) {
	authDir := t.TempDir()
	grokPath := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(grokPath, []byte(`{"https://auth.x.ai":{"key":"synthetic","auth_mode":"oidc","user_id":"review-user","oidc_issuer":"https://auth.x.ai","expires_at":"2099-01-01T00:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := codexonly.NewHandler(context.Background(), &codexonly.Config{
		AuthDir:      authDir,
		Database:     codexonly.DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL: "http://127.0.0.1:1/backend-api/codex",
		Grok:         codexonly.GrokConfig{AuthFiles: []string{grokPath}, BaseURL: "http://127.0.0.1:1/v1"},
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	server := httptest.NewServer(handler.GrokHandler())
	defer server.Close()

	stdout := runAdminTestCommand(t, server.URL, "grok", "status", "--grok-url", server.URL)
	if !strings.Contains(stdout, "review-user") {
		t.Fatalf("stdout = %q, want Grok login from the real loopback handler", stdout)
	}
}

func TestGrokAdminCommandsDefaultToGrokListener(t *testing.T) {
	var sawPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"auths":[]}`))
	}))
	defer server.Close()
	stdout := runAdminTestCommand(t, server.URL, "grok", "status", "--grok-url", server.URL)
	if sawPath != "/v0/local-admin/grok/auths" {
		t.Fatalf("path = %q, want Grok local admin path", sawPath)
	}
	if !strings.Contains(stdout, "ID") {
		t.Fatalf("stdout = %q, want Grok auth table", stdout)
	}
}

func TestAdminClientUsesLocalAdminPrefix(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("Authorization header = %q, want empty", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"users": []any{}})
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL)
	if err != nil {
		t.Fatalf("newAdminClient returned error: %v", err)
	}
	var payload map[string]any
	if err = client.doJSON(context.Background(), http.MethodGet, "/users", nil, nil, &payload); err != nil {
		t.Fatalf("doJSON returned error: %v", err)
	}
	if gotPath != "/v0/local-admin/users" {
		t.Fatalf("path = %q, want /v0/local-admin/users", gotPath)
	}
}

func TestAdminClientReportsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL)
	if err != nil {
		t.Fatalf("newAdminClient returned error: %v", err)
	}
	err = client.doJSON(context.Background(), http.MethodGet, "/users", nil, nil, nil)
	if err == nil {
		t.Fatalf("doJSON returned nil, want error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %q, want status code", err.Error())
	}
}
