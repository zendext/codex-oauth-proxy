package codexonly

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigAppliesCodexOnlyDefaults(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("port: 9123\napi-keys:\n  - local-key\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.Port != 9123 {
		t.Fatalf("Port = %d, want 9123", cfg.Port)
	}
	if cfg.Host != "" {
		t.Fatalf("Host = %q, want empty bind host", cfg.Host)
	}
	if cfg.AuthDir != DefaultAuthDir {
		t.Fatalf("AuthDir = %q, want %q", cfg.AuthDir, DefaultAuthDir)
	}
	if len(cfg.APIKeys) != 1 || cfg.APIKeys[0] != "local-key" {
		t.Fatalf("APIKeys = %#v, want [local-key]", cfg.APIKeys)
	}
	if cfg.RequestRetry != 3 {
		t.Fatalf("RequestRetry = %d, want 3", cfg.RequestRetry)
	}
	if cfg.CodexUserAgent != "" {
		t.Fatalf("CodexUserAgent = %q, want empty default", cfg.CodexUserAgent)
	}
	if cfg.ChatGPTBaseURL != DefaultChatGPTBaseURL {
		t.Fatalf("ChatGPTBaseURL = %q, want %q", cfg.ChatGPTBaseURL, DefaultChatGPTBaseURL)
	}
}

func TestLoadAuthsOnlyReturnsActiveCodexAuths(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "codex.json", `{
		"type": "codex",
		"email": "user@example.com",
		"account_id": "acct_1",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"expired": "2099-01-02T03:04:05Z"
	}`)
	writeAuthFile(t, dir, "disabled.json", `{
		"type": "codex",
		"access_token": "disabled-access",
		"refresh_token": "disabled-refresh",
		"disabled": true
	}`)
	writeAuthFile(t, dir, "gemini.json", `{
		"type": "gemini",
		"access_token": "gemini-access"
	}`)

	store := NewFileAuthStore(dir)
	auths, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Email != "user@example.com" {
		t.Fatalf("Email = %q, want user@example.com", auth.Email)
	}
	if auth.AccountID != "acct_1" {
		t.Fatalf("AccountID = %q, want acct_1", auth.AccountID)
	}
	if auth.AccessToken != "access-1" {
		t.Fatalf("AccessToken = %q, want access-1", auth.AccessToken)
	}
	if auth.RefreshToken != "refresh-1" {
		t.Fatalf("RefreshToken = %q, want refresh-1", auth.RefreshToken)
	}
	wantExpiry := time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
	if !auth.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("ExpiresAt = %s, want %s", auth.ExpiresAt, wantExpiry)
	}
}

func TestLoadAuthsSupportsCodexCLIAuthJSON(t *testing.T) {
	dir := t.TempDir()
	wantExpiry := time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
	writeAuthFile(t, dir, "auth.json", fmt.Sprintf(`{
		"OPENAI_API_KEY": "sk-hidden",
		"last_refresh": "2026-06-11T00:00:00Z",
		"tokens": {
			"access_token": %q,
			"refresh_token": "refresh-cli",
			"id_token": "id-cli",
			"account_id": "acct_cli"
		}
	}`, jwtWithExpiry(t, wantExpiry)))

	store := NewFileAuthStore(dir)
	auths, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.AccountID != "acct_cli" {
		t.Fatalf("AccountID = %q, want acct_cli", auth.AccountID)
	}
	if auth.RefreshToken != "refresh-cli" {
		t.Fatalf("RefreshToken = %q, want refresh-cli", auth.RefreshToken)
	}
	if auth.IDToken != "id-cli" {
		t.Fatalf("IDToken = %q, want id-cli", auth.IDToken)
	}
	if !auth.ExpiresAt.Equal(wantExpiry) {
		t.Fatalf("ExpiresAt = %s, want %s", auth.ExpiresAt, wantExpiry)
	}
}

func TestAuthSavePreservesCodexCLIAuthJSON(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{
		"OPENAI_API_KEY": "sk-hidden",
		"last_refresh": "2026-06-11T00:00:00Z",
		"tokens": {
			"access_token": "old-access",
			"refresh_token": "old-refresh",
			"id_token": "old-id",
			"account_id": "acct_cli"
		}
	}`), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}

	store := NewFileAuthStore(dir)
	auths, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}

	auth := auths[0]
	auth.AccessToken = "new-access"
	auth.RefreshToken = "new-refresh"
	auth.IDToken = "new-id"
	auth.AccountID = "acct_new"
	if err = auth.Save(); err != nil {
		t.Fatalf("Save returned error: %v", err)
	}

	raw, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read saved auth: %v", err)
	}
	var saved map[string]any
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("unmarshal saved auth: %v", err)
	}
	if _, ok := saved["access_token"]; ok {
		t.Fatal("saved top-level access_token, want Codex CLI tokens shape")
	}
	if _, ok := saved["type"]; ok {
		t.Fatal("saved top-level type, want Codex CLI shape preserved")
	}
	tokens, ok := saved["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens = %#v, want object", saved["tokens"])
	}
	if tokens["access_token"] != "new-access" {
		t.Fatalf("tokens.access_token = %#v, want new-access", tokens["access_token"])
	}
	if tokens["refresh_token"] != "new-refresh" {
		t.Fatalf("tokens.refresh_token = %#v, want new-refresh", tokens["refresh_token"])
	}
	if tokens["id_token"] != "new-id" {
		t.Fatalf("tokens.id_token = %#v, want new-id", tokens["id_token"])
	}
	if tokens["account_id"] != "acct_new" {
		t.Fatalf("tokens.account_id = %#v, want acct_new", tokens["account_id"])
	}
}

func writeAuthFile(t *testing.T, dir string, name string, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("write auth file %s: %v", name, err)
	}
}

func jwtWithExpiry(t *testing.T, expiry time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]int64{"exp": expiry.Unix()})
	if err != nil {
		t.Fatalf("marshal jwt payload: %v", err)
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	return "header." + encodedPayload + ".signature"
}
