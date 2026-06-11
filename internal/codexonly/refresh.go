package codexonly

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	CodexClientID        = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultCodexTokenURL = "https://auth.openai.com/oauth/token"
)

type AuthRefresher interface {
	Refresh(context.Context, *Auth) error
}

type RefresherFunc func(context.Context, *Auth) error

func (f RefresherFunc) Refresh(ctx context.Context, auth *Auth) error {
	return f(ctx, auth)
}

type Refresher struct {
	Client   *http.Client
	TokenURL string
	Now      func() time.Time
}

func (r *Refresher) Refresh(ctx context.Context, auth *Auth) error {
	if auth == nil {
		return fmt.Errorf("auth is nil")
	}
	if strings.TrimSpace(auth.RefreshToken) == "" {
		return fmt.Errorf("codex refresh token is missing")
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	tokenURL := strings.TrimSpace(r.TokenURL)
	if tokenURL == "" {
		tokenURL = DefaultCodexTokenURL
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}

	form := url.Values{
		"client_id":     {CodexClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {auth.RefreshToken},
		"scope":         {"openid profile email"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("refresh codex token: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read refresh response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("refresh codex token: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err = json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("parse refresh response: %w", err)
	}
	if strings.TrimSpace(parsed.AccessToken) == "" {
		return fmt.Errorf("refresh codex token: response missing access_token")
	}
	auth.AccessToken = parsed.AccessToken
	if strings.TrimSpace(parsed.RefreshToken) != "" {
		auth.RefreshToken = parsed.RefreshToken
	}
	if strings.TrimSpace(parsed.IDToken) != "" {
		auth.IDToken = parsed.IDToken
	}
	if parsed.ExpiresIn > 0 {
		auth.ExpiresAt = now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	}
	if err = auth.Save(); err != nil {
		return fmt.Errorf("save refreshed auth: %w", err)
	}
	return nil
}

func NewHTTPClient(proxyURL string, timeout time.Duration) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxyURL = strings.TrimSpace(proxyURL)
	switch {
	case strings.EqualFold(proxyURL, "direct"), strings.EqualFold(proxyURL, "none"):
		transport.Proxy = nil
	case proxyURL != "":
		parsed, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse proxy URL: %w", err)
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}, nil
}
