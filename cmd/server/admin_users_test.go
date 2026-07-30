package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

func TestAdminUsersCreatePrintsPlaintextKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/local-admin/users" {
			t.Fatalf("request = %s %s, want POST /v0/local-admin/users", r.Method, r.URL.Path)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req["name"] != "Alice" {
			t.Fatalf("name = %#v, want Alice", req["name"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(codexonly.CreatedUserAPIKey{
			User:            codexonly.UserRecord{ID: "usr_1", Name: "Alice", Enabled: true},
			APIKey:          codexonly.APIKeyRecord{ID: "key_1", UserID: "usr_1", MaskedKey: "cop_****abcd", Enabled: true},
			PlaintextAPIKey: "cop_plain",
		})
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL)
	if err != nil {
		t.Fatalf("newAdminClient returned error: %v", err)
	}
	var out bytes.Buffer
	if err = runAdminUsersCreate(context.Background(), client, adminOptions{}, "Alice", &out); err != nil {
		t.Fatalf("runAdminUsersCreate returned error: %v", err)
	}
	if !strings.Contains(out.String(), "cop_plain") {
		t.Fatalf("output = %q, want plaintext key", out.String())
	}
}

func TestAdminUsersListJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/local-admin/users" {
			t.Fatalf("request = %s %s, want GET /v0/local-admin/users", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"users":[{"user":{"id":"usr_1","name":"Alice","enabled":true},"api_key":{"id":"key_1","user_id":"usr_1","masked_key":"cop_****abcd","enabled":true}}]}`))
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL)
	if err != nil {
		t.Fatalf("newAdminClient returned error: %v", err)
	}
	var out bytes.Buffer
	if err = runAdminUsersList(context.Background(), client, adminOptions{json: true}, "", &out); err != nil {
		t.Fatalf("runAdminUsersList returned error: %v", err)
	}
	if !strings.Contains(out.String(), `"users"`) || !strings.Contains(out.String(), `"Alice"`) {
		t.Fatalf("json output = %q, want users payload", out.String())
	}
}

func TestAdminUsersDisableSendsPatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/v0/local-admin/users/usr_1" {
			t.Fatalf("request = %s %s, want PATCH /v0/local-admin/users/usr_1", r.Method, r.URL.Path)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req["enabled"] != false {
			t.Fatalf("enabled = %#v, want false", req["enabled"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"usr_1","name":"Alice","enabled":false},"api_key":{"id":"key_1","user_id":"usr_1","masked_key":"cop_****abcd","enabled":true}}`))
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL)
	if err != nil {
		t.Fatalf("newAdminClient returned error: %v", err)
	}
	var out bytes.Buffer
	if err = runAdminUsersSetEnabled(context.Background(), client, adminOptions{}, "usr_1", &out, false); err != nil {
		t.Fatalf("runAdminUsersSetEnabled returned error: %v", err)
	}
	if !strings.Contains(out.String(), "false") {
		t.Fatalf("output = %q, want disabled state", out.String())
	}
}
