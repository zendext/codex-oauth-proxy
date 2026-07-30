package main

import (
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

	output := runAdminTestCommand(t, server.URL, "users", "create", "Alice")
	if !strings.Contains(output, "cop_plain") {
		t.Fatalf("output = %q, want plaintext key", output)
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

	output := runAdminTestCommand(t, server.URL, "users", "list", "--json")
	if !strings.Contains(output, `"users"`) || !strings.Contains(output, `"Alice"`) {
		t.Fatalf("json output = %q, want users payload", output)
	}
}

func TestAdminUsersGetUsesUserPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/local-admin/users/usr_1" {
			t.Fatalf("request = %s %s, want GET /v0/local-admin/users/usr_1", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"usr_1","name":"Alice","enabled":true},"api_key":null}`))
	}))
	defer server.Close()

	output := runAdminTestCommand(t, server.URL, "users", "get", "usr_1")
	if !strings.Contains(output, "Alice") {
		t.Fatalf("output = %q, want user", output)
	}
}

func TestAdminUsersSetEnabledSendsPatch(t *testing.T) {
	for _, tt := range []struct {
		command string
		enabled bool
		output  string
	}{
		{command: "enable", enabled: true, output: "true"},
		{command: "disable", enabled: false, output: "false"},
	} {
		t.Run(tt.command, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch || r.URL.Path != "/v0/local-admin/users/usr_1" {
					t.Fatalf("request = %s %s, want PATCH /v0/local-admin/users/usr_1", r.Method, r.URL.Path)
				}
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				if req["enabled"] != tt.enabled {
					t.Fatalf("enabled = %#v, want %t", req["enabled"], tt.enabled)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"user": map[string]any{"id": "usr_1", "name": "Alice", "enabled": tt.enabled},
				})
			}))
			defer server.Close()

			output := runAdminTestCommand(t, server.URL, "users", tt.command, "usr_1")
			if !strings.Contains(output, tt.output) {
				t.Fatalf("output = %q, want enabled state", output)
			}
		})
	}
}

func TestAdminUsersResetKeyUsesResetPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/local-admin/users/usr_1/api-key/reset" {
			t.Fatalf("request = %s %s, want POST /v0/local-admin/users/usr_1/api-key/reset", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(codexonly.CreatedUserAPIKey{
			User:            codexonly.UserRecord{ID: "usr_1", Name: "Alice", Enabled: true},
			APIKey:          codexonly.APIKeyRecord{ID: "key_2", UserID: "usr_1", MaskedKey: "cop_****efgh", Enabled: true},
			PlaintextAPIKey: "cop_replaced",
		})
	}))
	defer server.Close()

	output := runAdminTestCommand(t, server.URL, "users", "reset-key", "usr_1")
	if !strings.Contains(output, "cop_replaced") {
		t.Fatalf("output = %q, want replacement key", output)
	}
}
