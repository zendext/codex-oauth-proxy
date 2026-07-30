package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
