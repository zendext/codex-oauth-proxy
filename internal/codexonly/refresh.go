package codexonly

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	CodexClientID              = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultCodexTokenURL       = "https://auth.openai.com/oauth/token"
	DefaultOAuthRefreshTimeout = 30 * time.Second
	maxOAuthRefreshAttempts    = 3
	maxOAuthRefreshBodyBytes   = 1 << 20
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
		return newOAuthRefreshError("invalid auth", 0, "", nil)
	}
	if strings.TrimSpace(auth.RefreshToken) == "" {
		return newOAuthRefreshError("missing refresh token", 0, "", nil)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultOAuthRefreshTimeout)
	defer cancel()

	client := r.Client
	if client == nil {
		client = &http.Client{}
	}
	tokenURL := strings.TrimSpace(r.TokenURL)
	if tokenURL == "" {
		tokenURL = DefaultCodexTokenURL
	}
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}

	candidate := cloneAuth(auth)
	for attempt := 1; attempt <= maxOAuthRefreshAttempts; attempt++ {
		parsed, retryAfter, err := refreshCodexTokenAttempt(ctx, client, tokenURL, candidate.RefreshToken)
		if err != nil {
			if attempt == maxOAuthRefreshAttempts || !oauthRefreshRetryable(err) {
				return err
			}
			if err = waitOAuthRefreshRetry(ctx, retryAfter, attempt); err != nil {
				return newOAuthRefreshError("canceled", 0, "", err)
			}
			continue
		}

		candidate.AccessToken = parsed.AccessToken
		if parsed.RefreshToken != "" {
			candidate.RefreshToken = parsed.RefreshToken
		}
		if parsed.IDToken != "" {
			candidate.IDToken = parsed.IDToken
		}
		switch {
		case parsed.ExpiresIn > 0:
			candidate.ExpiresAt = now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
		case !tokenExpiry(parsed.AccessToken).IsZero():
			candidate.ExpiresAt = tokenExpiry(parsed.AccessToken)
		}
		candidate.ReparseIdentity()
		if err = candidate.Save(); err != nil {
			return newOAuthRefreshError("persistence failed", 0, "", err)
		}
		copyAuth(auth, candidate)
		return nil
	}
	return newOAuthRefreshError("attempt limit reached", 0, "", nil)
}

type oauthRefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

type oauthRefreshError struct {
	reason    string
	status    int
	code      string
	cause     error
	retryable bool
}

func newOAuthRefreshError(reason string, status int, code string, cause error) error {
	return &oauthRefreshError{
		reason: reason,
		status: status,
		code:   safeOAuthErrorCode(code),
		cause:  cause,
	}
}

func newRetryableOAuthRefreshError(reason string, status int, code string, cause error) error {
	err := &oauthRefreshError{
		reason: reason,
		status: status,
		code:   safeOAuthErrorCode(code),
		cause:  cause,
	}
	err.retryable = true
	return err
}

func (e *oauthRefreshError) Error() string {
	message := "codex OAuth refresh failed"
	if e.reason != "" {
		message += ": " + e.reason
	}
	if e.status != 0 {
		message += fmt.Sprintf(" status=%d", e.status)
	}
	if e.code != "" {
		message += " code=" + e.code
	}
	return message
}

func (e *oauthRefreshError) Unwrap() error {
	return e.cause
}

func refreshCodexTokenAttempt(ctx context.Context, client *http.Client, tokenURL string, refreshToken string) (oauthRefreshResponse, time.Duration, error) {
	form := url.Values{
		"client_id":     {CodexClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthRefreshResponse{}, -1, newOAuthRefreshError("request creation failed", 0, "", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		refreshErr := newOAuthRefreshError
		if transientOAuthNetworkError(err) {
			refreshErr = newRetryableOAuthRefreshError
		}
		return oauthRefreshResponse{}, -1, refreshErr("request failed", 0, "", err)
	}
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, maxOAuthRefreshBodyBytes+1))
	_ = resp.Body.Close()
	if errRead != nil {
		refreshErr := newOAuthRefreshError
		if transientOAuthNetworkError(errRead) {
			refreshErr = newRetryableOAuthRefreshError
		}
		return oauthRefreshResponse{}, -1, refreshErr("response read failed", resp.StatusCode, "", errRead)
	}
	if len(body) > maxOAuthRefreshBodyBytes {
		return oauthRefreshResponse{}, -1, newOAuthRefreshError("response is too large", resp.StatusCode, "", nil)
	}

	retryAfter := parseOAuthRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var oauthError struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &oauthError)
		refreshErr := newOAuthRefreshError
		if oauthRefreshStatusRetryable(resp.StatusCode) && safeOAuthErrorCode(oauthError.Error) != "invalid_grant" {
			refreshErr = newRetryableOAuthRefreshError
		}
		return oauthRefreshResponse{}, retryAfter, refreshErr("token endpoint rejected request", resp.StatusCode, oauthError.Error, nil)
	}

	var parsed oauthRefreshResponse
	if err = json.Unmarshal(body, &parsed); err != nil {
		return oauthRefreshResponse{}, -1, newOAuthRefreshError("malformed response", resp.StatusCode, "", nil)
	}
	parsed.AccessToken = strings.TrimSpace(parsed.AccessToken)
	parsed.RefreshToken = strings.TrimSpace(parsed.RefreshToken)
	parsed.IDToken = strings.TrimSpace(parsed.IDToken)
	if parsed.AccessToken == "" {
		return oauthRefreshResponse{}, -1, newOAuthRefreshError("response missing access token", resp.StatusCode, "", nil)
	}
	const maxExpiresIn = int64((1<<63 - 1) / int64(time.Second))
	if parsed.ExpiresIn < 0 || parsed.ExpiresIn > maxExpiresIn {
		return oauthRefreshResponse{}, -1, newOAuthRefreshError("invalid token expiry", resp.StatusCode, "", nil)
	}
	return parsed, -1, nil
}

func oauthRefreshRetryable(err error) bool {
	var refreshErr *oauthRefreshError
	if !errors.As(err, &refreshErr) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return refreshErr.retryable
}

func oauthRefreshStatusRetryable(status int) bool {
	switch status {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func transientOAuthNetworkError(err error) bool {
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return true
	}
	var temporary interface {
		Temporary() bool
	}
	if errors.As(err, &temporary) && temporary.Temporary() {
		return true
	}
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH)
}

func waitOAuthRefreshRetry(ctx context.Context, retryAfter time.Duration, attempt int) error {
	delay := retryAfter
	if delay < 0 {
		delay = time.Duration(attempt) * 100 * time.Millisecond
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseOAuthRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return -1
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		const maxSeconds = uint64((1<<63 - 1) / int64(time.Second))
		if seconds > maxSeconds {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(seconds) * time.Second
	}
	if asciiDigits(value) {
		return time.Duration(1<<63 - 1)
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return -1
	}
	if !retryAt.After(now) {
		return 0
	}
	return retryAt.Sub(now)
}

func asciiDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func safeOAuthErrorCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 64 {
		return ""
	}
	for _, char := range code {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '_',
			char == '-',
			char == '.':
		default:
			return ""
		}
	}
	return code
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
