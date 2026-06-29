package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseCLILegacyServeFlags(t *testing.T) {
	opts, err := parseCLI([]string{"--config", "config.yaml", "--local-model"})
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if opts.command != commandServe {
		t.Fatalf("command = %s, want %s", opts.command, commandServe)
	}
	if opts.configPath != "config.yaml" {
		t.Fatalf("configPath = %q, want config.yaml", opts.configPath)
	}
	if !opts.localModel {
		t.Fatalf("localModel = false, want true")
	}
}

func TestParseCLIExplicitServe(t *testing.T) {
	opts, err := parseCLI([]string{"serve", "--config", "config.yaml"})
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if opts.command != commandServe || opts.configPath != "config.yaml" {
		t.Fatalf("opts = %#v, want serve with config.yaml", opts)
	}
}

func TestParseCLIAdminKeepsRemainingArgs(t *testing.T) {
	opts, err := parseCLI([]string{"admin", "users", "list", "--url", "http://127.0.0.1:8318"})
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if opts.command != commandAdmin {
		t.Fatalf("command = %s, want %s", opts.command, commandAdmin)
	}
	if got := len(opts.adminArgs); got != 4 {
		t.Fatalf("adminArgs length = %d, want 4: %#v", got, opts.adminArgs)
	}
}

func TestRunAdminJSONDoesNotPrintVersionBanner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"users":[]}`))
	}))
	defer server.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"admin", "users", "list", "--url", server.URL, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit = %d, want 0, stderr: %s", code, stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("admin json output is not valid JSON: %q: %v", stdout.String(), err)
	}
	if _, ok := payload["users"]; !ok {
		t.Fatalf("payload = %#v, want users field", payload)
	}
}
