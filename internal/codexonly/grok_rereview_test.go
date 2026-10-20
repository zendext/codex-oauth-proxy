package codexonly

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReReviewGrokRefreshReplacementDuringDiscovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	var issuer string
	var tokenGrantRefresh string
	record := func(account, token string) map[string]any {
		return map[string]any{"https://auth.x.ai": map[string]any{
			"key": token, "refresh_token": "synthetic-refresh-" + account,
			"auth_mode": "oidc", "user_id": account,
			"oidc_issuer": issuer, "oidc_client_id": "synthetic-client",
			"create_time": "2026-10-20T01:00:00Z", "expires_at": "2099-01-01T00:00:00Z",
		}}
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			writeGrokAuthFile(t, path, record("replacement-account", "replacement-access"))
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"token_endpoint":%q}`, issuer+"/oauth/token")
		case "/oauth/token":
			_ = r.ParseForm()
			tokenGrantRefresh = r.PostForm.Get("refresh_token")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"refreshed-old-account-access","refresh_token":"refreshed-old-account-refresh","expires_in":3600}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer endpoint.Close()
	issuer = endpoint.URL
	writeGrokAuthFile(t, path, record("old-account", "old-access"))
	store := NewGrokAuthStore([]string{path})
	creds, err := store.Load(context.Background())
	if err != nil || len(creds) != 1 {
		t.Fatalf("fixture Load: %v", err)
	}
	_, refreshErr := store.Refresh(context.Background(), creds[0].ID, grokHTTPClient(endpoint.Client()), "")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]map[string]any
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	got := saved["https://auth.x.ai"]
	if got["key"] != "replacement-access" || got["refresh_token"] != "synthetic-refresh-replacement-account" {
		t.Fatalf("replacement login overwritten: user=%v key=%v refresh=%v; old grant=%q refreshErr=%v", got["user_id"], got["key"], got["refresh_token"], tokenGrantRefresh, refreshErr)
	}
}

func TestReReviewGrokUpstreamPrefix(t *testing.T) {
	for _, prefix := range []string{"/v1", "/gateway/v1"} {
		for _, endpoint := range []string{"/responses", "/chat/completions"} {
			t.Run(prefix+endpoint, func(t *testing.T) {
				gotPath := ""
				wantPath := prefix + endpoint
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotPath = r.URL.Path
					if r.URL.Path != wantPath {
						http.NotFound(w, r)
						return
					}
					_, _ = io.WriteString(w, `{}`)
				}))
				defer upstream.Close()
				server, user := reviewGrokFixture(t, upstream.URL+prefix, 1)
				rec := doJSONRequest(t, server.GrokHandler(), http.MethodPost, "/v1"+endpoint, `{"model":"grok-test"}`, user.PlaintextAPIKey)
				if gotPath != wantPath || rec.Code != 200 {
					t.Fatalf("base path %q: upstream path=%q status=%d; want %q and 200", prefix, gotPath, rec.Code, wantPath)
				}
			})
		}
	}
}

func TestReReviewGrokManagementNoStore(t *testing.T) {
	server, _ := reviewGrokFixture(t, "http://127.0.0.1:1/v1", 1)
	for _, path := range []string{"/v0/local-admin/grok/auths", "/v0/local-admin/users", "/v0/management/users"} {
		t.Run(path, func(t *testing.T) {
			rec := doJSONRequestFromRemote(t, server.GrokHandler(), "127.0.0.1:12345", http.MethodGet, path, "", "admin-key")
			if rec.Code != 200 {
				t.Fatalf("fixture status=%d, body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
				t.Fatalf("management response Cache-Control=%q; want no-store", rec.Header().Get("Cache-Control"))
			}
		})
	}
}
