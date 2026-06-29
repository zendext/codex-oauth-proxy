package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSplitAdminFlagsAllowsURLAfterLeafCommand(t *testing.T) {
	opts, rest, err := splitAdminFlags([]string{"users", "list", "--url", "http://127.0.0.1:8318", "--json"})
	if err != nil {
		t.Fatalf("splitAdminFlags returned error: %v", err)
	}
	if opts.baseURL != "http://127.0.0.1:8318" || !opts.json {
		t.Fatalf("opts = %#v, want custom URL and json", opts)
	}
	if strings.Join(rest, " ") != "users list" {
		t.Fatalf("rest = %q, want users list", strings.Join(rest, " "))
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
