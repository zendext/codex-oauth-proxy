package codexonly

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRefresherUpdatesAuthAndWritesTokenFile(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "codex.json")
	if err := os.WriteFile(authPath, []byte(`{
		"type": "codex",
		"access_token": "old-access",
		"refresh_token": "old-refresh",
		"account_id": "acct_1",
		"email": "user@example.com",
		"expired": "2000-01-01T00:00:00Z"
	}`), 0o600); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	var sawRefreshToken string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		sawRefreshToken = r.PostForm.Get("refresh_token")
		if got := r.PostForm.Get("grant_type"); got != "refresh_token" {
			t.Fatalf("grant_type = %q, want refresh_token", got)
		}
		if got := r.PostForm.Get("client_id"); got != CodexClientID {
			t.Fatalf("client_id = %q, want %q", got, CodexClientID)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"id_token":      "new-id",
			"expires_in":    3600,
		})
	}))
	defer tokenServer.Close()

	auth := &Auth{
		Path:         authPath,
		AccessToken:  "old-access",
		RefreshToken: "old-refresh",
		AccountID:    "acct_1",
		Email:        "user@example.com",
		ExpiresAt:    time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	refresher := &Refresher{
		Client:   tokenServer.Client(),
		TokenURL: tokenServer.URL,
		Now: func() time.Time {
			return time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
		},
	}

	if err := refresher.Refresh(context.Background(), auth); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}

	if sawRefreshToken != "old-refresh" {
		t.Fatalf("refresh_token = %q, want old-refresh", sawRefreshToken)
	}
	if auth.AccessToken != "new-access" {
		t.Fatalf("AccessToken = %q, want new-access", auth.AccessToken)
	}
	if auth.RefreshToken != "new-refresh" {
		t.Fatalf("RefreshToken = %q, want new-refresh", auth.RefreshToken)
	}
	if !auth.ExpiresAt.After(time.Date(2026, 6, 11, 12, 30, 0, 0, time.UTC)) {
		t.Fatalf("ExpiresAt = %s, want roughly one hour after refresh", auth.ExpiresAt)
	}

	raw, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read refreshed auth: %v", err)
	}
	var saved map[string]any
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("unmarshal refreshed auth: %v", err)
	}
	if saved["access_token"] != "new-access" {
		t.Fatalf("saved access_token = %#v, want new-access", saved["access_token"])
	}
	if saved["refresh_token"] != "new-refresh" {
		t.Fatalf("saved refresh_token = %#v, want new-refresh", saved["refresh_token"])
	}
	if saved["type"] != "codex" {
		t.Fatalf("saved type = %#v, want codex", saved["type"])
	}
}

func TestRefresherRejectsMissingRefreshToken(t *testing.T) {
	refresher := &Refresher{TokenURL: "http://127.0.0.1/token"}
	auth := &Auth{Path: filepath.Join(t.TempDir(), "codex.json")}

	err := refresher.Refresh(context.Background(), auth)
	if err == nil {
		t.Fatal("Refresh returned nil error, want missing refresh token error")
	}
}

func TestProxyURLTransportUsesExplicitProxy(t *testing.T) {
	proxyURL, err := url.Parse("http://127.0.0.1:8181")
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	client, err := NewHTTPClient(proxyURL.String(), 0)
	if err != nil {
		t.Fatalf("NewHTTPClient returned error: %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	req := httptest.NewRequest(http.MethodGet, "https://example.com", nil)
	gotURL, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("Proxy returned error: %v", err)
	}
	if gotURL.String() != proxyURL.String() {
		t.Fatalf("proxy URL = %s, want %s", gotURL.String(), proxyURL.String())
	}
}

func TestProxyURLDirectDisablesProxy(t *testing.T) {
	client, err := NewHTTPClient("direct", 0)
	if err != nil {
		t.Fatalf("NewHTTPClient returned error: %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("transport.Proxy is configured, want nil for direct proxy mode")
	}
}
