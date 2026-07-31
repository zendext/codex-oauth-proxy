package codexonly

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthManagerCoordinatesConcurrentProactiveRefresh(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "auth.json", `{
		"type": "codex",
		"account_id": "acct_1",
		"access_token": "old-access",
		"refresh_token": "old-refresh",
		"expired": "2000-01-01T00:00:00Z"
	}`)

	var refreshCalls atomic.Int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refresher := RefresherFunc(func(_ context.Context, auth *Auth) error {
		if refreshCalls.Add(1) == 1 {
			close(refreshStarted)
		}
		<-releaseRefresh
		auth.AccessToken = "new-access"
		auth.RefreshToken = "new-refresh"
		auth.ExpiresAt = time.Now().Add(time.Hour)
		return auth.Save()
	})
	manager := &AuthManager{
		Store:     NewFileAuthStore(authDir),
		Refresher: refresher,
		Now: func() time.Time {
			return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
		},
	}

	const callers = 24
	start := make(chan struct{})
	results := make(chan *Auth, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			auth, err := manager.Select(context.Background())
			results <- auth
			errs <- err
		}()
	}
	close(start)
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for refresh")
	}
	time.Sleep(25 * time.Millisecond)
	close(releaseRefresh)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("Select returned error: %v", err)
		}
	}
	for auth := range results {
		if auth == nil || auth.AccessToken != "new-access" || auth.RefreshToken != "new-refresh" {
			t.Fatalf("selected auth = %#v, want refreshed tokens", auth)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestAuthManagerReactiveRefreshReusesCompletedRefreshForOldToken(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "auth.json", `{
		"type": "codex",
		"account_id": "acct_1",
		"access_token": "old-access",
		"refresh_token": "old-refresh",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	var refreshCalls atomic.Int32
	manager := &AuthManager{
		Store: NewFileAuthStore(authDir),
		Refresher: RefresherFunc(func(_ context.Context, auth *Auth) error {
			refreshCalls.Add(1)
			auth.AccessToken = "new-access"
			auth.RefreshToken = "new-refresh"
			auth.ExpiresAt = time.Now().Add(time.Hour)
			return auth.Save()
		}),
	}
	first, err := manager.Select(context.Background())
	if err != nil {
		t.Fatalf("Select returned error: %v", err)
	}
	stale := cloneAuth(first)

	if err = manager.RefreshAfterUnauthorized(context.Background(), first, "old-access"); err != nil {
		t.Fatalf("first RefreshAfterUnauthorized returned error: %v", err)
	}
	if err = manager.RefreshAfterUnauthorized(context.Background(), stale, "old-access"); err != nil {
		t.Fatalf("stale RefreshAfterUnauthorized returned error: %v", err)
	}

	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	if first.AccessToken != "new-access" || stale.AccessToken != "new-access" {
		t.Fatalf("refreshed access tokens = %q and %q, want new-access", first.AccessToken, stale.AccessToken)
	}
}

func TestRefresherRetriesTransientFailuresAtMostThreeAttempts(t *testing.T) {
	var attempts atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch attempts.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "0")
			http.Error(w, "temporary secret response", http.StatusServiceUnavailable)
		case 2:
			w.Header().Set("Retry-After", "0")
			http.Error(w, "rate limited secret response", http.StatusTooManyRequests)
		default:
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token":  "new-access",
				"refresh_token": "rotated-refresh",
				"expires_in":    3600,
			})
		}
	}))
	defer tokenServer.Close()

	auth := newRefreshTestAuth(t)
	refresher := &Refresher{Client: tokenServer.Client(), TokenURL: tokenServer.URL}
	if err := refresher.Refresh(context.Background(), auth); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if auth.AccessToken != "new-access" || auth.RefreshToken != "rotated-refresh" {
		t.Fatalf("refreshed tokens = %q/%q, want new-access/rotated-refresh", auth.AccessToken, auth.RefreshToken)
	}

	t.Run("attempt ceiling", func(t *testing.T) {
		var failedAttempts atomic.Int32
		failingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			failedAttempts.Add(1)
			w.Header().Set("Retry-After", "0")
			http.Error(w, "temporary", http.StatusServiceUnavailable)
		}))
		defer failingServer.Close()

		err := (&Refresher{Client: failingServer.Client(), TokenURL: failingServer.URL}).Refresh(context.Background(), newRefreshTestAuth(t))
		if err == nil {
			t.Fatal("Refresh returned nil error")
		}
		if got := failedAttempts.Load(); got != maxOAuthRefreshAttempts {
			t.Fatalf("attempts = %d, want %d", got, maxOAuthRefreshAttempts)
		}
	})
}

func TestRefresherRetriesTransientNetworkFailure(t *testing.T) {
	var attempts atomic.Int32
	client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			return nil, temporaryNetworkError{}
		}
		return jsonHTTPResponse(req, http.StatusOK, `{"access_token":"new-access","expires_in":3600}`), nil
	})}

	auth := newRefreshTestAuth(t)
	if err := (&Refresher{Client: client, TokenURL: "https://auth.example/token"}).Refresh(context.Background(), auth); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}

	t.Run("permanent network error", func(t *testing.T) {
		var failedAttempts atomic.Int32
		failingClient := &http.Client{Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			failedAttempts.Add(1)
			return nil, permanentNetworkError{}
		})}
		err := (&Refresher{Client: failingClient, TokenURL: "https://auth.example/token"}).Refresh(context.Background(), newRefreshTestAuth(t))
		if err == nil {
			t.Fatal("Refresh returned nil error")
		}
		if got := failedAttempts.Load(); got != 1 {
			t.Fatalf("attempts = %d, want 1", got)
		}
	})
}

func TestRefresherDoesNotRetryTerminalResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "invalid grant", status: http.StatusBadRequest, body: `{"error":"invalid_grant","error_description":"contains secret-old-refresh"}`},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":"unauthorized","access_token":"secret-access"}`},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":"forbidden","refresh_token":"secret-refresh"}`},
		{name: "malformed success", status: http.StatusOK, body: `{"access_token":`},
		{name: "missing access token", status: http.StatusOK, body: `{"refresh_token":"secret-rotated"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer tokenServer.Close()

			auth := newRefreshTestAuth(t)
			err := (&Refresher{Client: tokenServer.Client(), TokenURL: tokenServer.URL}).Refresh(context.Background(), auth)
			if err == nil {
				t.Fatal("Refresh returned nil error")
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("attempts = %d, want 1", got)
			}
			for _, secret := range []string{"secret-old-refresh", "secret-access", "secret-refresh", "secret-rotated"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("Refresh error exposed %q: %v", secret, err)
				}
			}
		})
	}
}

func TestRefresherRejectsNegativeExpiryWithoutChangingAuth(t *testing.T) {
	auth := newRefreshTestAuth(t)
	before := cloneAuth(auth)
	beforeFile, err := os.ReadFile(auth.Path)
	if err != nil {
		t.Fatalf("read auth before refresh: %v", err)
	}

	var attempts atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		_, _ = io.WriteString(w, `{
			"access_token":"secret-new-access",
			"refresh_token":"secret-new-refresh",
			"expires_in":-1
		}`)
	}))
	defer tokenServer.Close()

	err = (&Refresher{Client: tokenServer.Client(), TokenURL: tokenServer.URL}).Refresh(context.Background(), auth)
	if err == nil {
		t.Fatal("Refresh returned nil error")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	if !strings.Contains(err.Error(), "invalid token expiry") {
		t.Fatalf("Refresh error = %v, want invalid token expiry", err)
	}
	for _, secret := range []string{"secret-new-access", "secret-new-refresh"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Refresh error exposed %q: %v", secret, err)
		}
	}
	if auth.AccessToken != before.AccessToken ||
		auth.RefreshToken != before.RefreshToken ||
		auth.IDToken != before.IDToken ||
		auth.AccountID != before.AccountID ||
		auth.Email != before.Email ||
		!auth.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("auth changed after rejected expiry: before=%#v after=%#v", before, auth)
	}
	afterFile, err := os.ReadFile(auth.Path)
	if err != nil {
		t.Fatalf("read auth after refresh: %v", err)
	}
	if !bytes.Equal(afterFile, beforeFile) {
		t.Fatalf("auth file changed after rejected expiry:\nbefore: %s\nafter: %s", beforeFile, afterFile)
	}
}

func TestRefresherAppliesOverallDeadlineAndCancellation(t *testing.T) {
	t.Run("default deadline", func(t *testing.T) {
		client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			deadline, ok := req.Context().Deadline()
			if !ok {
				t.Fatal("refresh request has no deadline")
			}
			remaining := time.Until(deadline)
			if remaining < 29*time.Second || remaining > 31*time.Second {
				t.Fatalf("refresh deadline remaining = %s, want about 30s", remaining)
			}
			return jsonHTTPResponse(req, http.StatusOK, `{"access_token":"new-access","expires_in":3600}`), nil
		})}
		if err := (&Refresher{Client: client, TokenURL: "https://auth.example/token"}).Refresh(context.Background(), newRefreshTestAuth(t)); err != nil {
			t.Fatalf("Refresh returned error: %v", err)
		}
	})

	t.Run("caller cancellation", func(t *testing.T) {
		requestStarted := make(chan struct{})
		client := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			close(requestStarted)
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			errCh <- (&Refresher{Client: client, TokenURL: "https://auth.example/token"}).Refresh(ctx, newRefreshTestAuth(t))
		}()
		<-requestStarted
		cancel()
		select {
		case err := <-errCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Refresh error = %v, want context canceled", err)
			}
		case <-time.After(time.Second):
			t.Fatal("Refresh did not stop after cancellation")
		}
	})

	t.Run("retry after beyond caller deadline", func(t *testing.T) {
		var attempts atomic.Int32
		tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.Header().Set("Retry-After", "120")
			http.Error(w, "temporary", http.StatusServiceUnavailable)
		}))
		defer tokenServer.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		err := (&Refresher{Client: tokenServer.Client(), TokenURL: tokenServer.URL}).Refresh(ctx, newRefreshTestAuth(t))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Refresh error = %v, want deadline exceeded", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("Refresh took %s, want prompt deadline cancellation", elapsed)
		}
		if got := attempts.Load(); got != 1 {
			t.Fatalf("attempts = %d, want 1", got)
		}
	})
}

func TestRefresherAtomicSaveFailureLeavesPreviousAuthIntact(t *testing.T) {
	auth := newRefreshTestAuth(t)
	original, err := os.ReadFile(auth.Path)
	if err != nil {
		t.Fatalf("read original auth: %v", err)
	}
	auth.renameFile = func(_, _ string) error {
		return errors.New("injected rename failure")
	}
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  "new-access",
			"refresh_token": "new-refresh",
			"expires_in":    3600,
		})
	}))
	defer tokenServer.Close()

	err = (&Refresher{Client: tokenServer.Client(), TokenURL: tokenServer.URL}).Refresh(context.Background(), auth)
	if err == nil {
		t.Fatal("Refresh returned nil error")
	}
	if auth.AccessToken != "old-access" || auth.RefreshToken != "old-refresh" {
		t.Fatalf("in-memory tokens changed after failed save: %q/%q", auth.AccessToken, auth.RefreshToken)
	}
	current, err := os.ReadFile(auth.Path)
	if err != nil {
		t.Fatalf("read auth after failed save: %v", err)
	}
	if !bytes.Equal(current, original) {
		t.Fatalf("auth file changed after failed save:\n%s", current)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(auth.Path), "."+filepath.Base(auth.Path)+".tmp-*"))
	if err != nil {
		t.Fatalf("glob auth temp files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary auth files remain after failed save: %v", matches)
	}
}

func TestRefresherAtomicSaveEnforcesPrivatePermissions(t *testing.T) {
	auth := newRefreshTestAuth(t)
	if err := os.Chmod(auth.Path, 0o644); err != nil {
		t.Fatalf("chmod auth: %v", err)
	}
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "new-access",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	if err := (&Refresher{Client: tokenServer.Client(), TokenURL: tokenServer.URL}).Refresh(context.Background(), auth); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	info, err := os.Stat(auth.Path)
	if err != nil {
		t.Fatalf("stat refreshed auth: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("auth permissions = %04o, want 0600", got)
	}
}

func TestReactiveUnauthorizedRetriesSameAuthBeforeProxyCommit(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "auth.json", `{
		"type": "codex",
		"account_id": "acct_1",
		"access_token": "old-access",
		"refresh_token": "old-refresh",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	var tokenCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token":  "new-access",
			"refresh_token": "rotated-refresh",
			"expires_in":    3600,
		})
	}))
	defer tokenServer.Close()

	var upstreamCalls atomic.Int32
	var bodiesMu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodiesMu.Lock()
		bodies = append(bodies, string(body))
		bodiesMu.Unlock()
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") == "Bearer old-access" {
			http.Error(w, `{"error":"expired old-access"}`, http.StatusUnauthorized)
			return
		}
		if r.Header.Get("Authorization") != "Bearer new-access" {
			t.Errorf("Authorization = %q, want refreshed token", r.Header.Get("Authorization"))
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:              authDir,
		AdminAPIKey:          "admin-key",
		Database:             DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:         upstream.URL + "/backend-api/codex",
		CodexRefreshTokenURL: tokenServer.URL,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hello"}`))
	resetRequestBody(req, []byte(`{"input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+userKey)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("token calls = %d, want 1", got)
	}
	bodiesMu.Lock()
	defer bodiesMu.Unlock()
	if len(bodies) != 2 || bodies[0] != `{"input":"hello"}` || bodies[1] != bodies[0] {
		t.Fatalf("upstream bodies = %#v, want identical replay", bodies)
	}
}

func TestReactiveUnauthorizedDoesNotReuseReplacementAccountAtSamePath(t *testing.T) {
	authDir := t.TempDir()
	authPath := filepath.Join(authDir, "auth.json")
	writeAuthFile(t, authDir, "auth.json", `{
		"type": "codex",
		"account_id": "acct_a",
		"access_token": "access-a",
		"refresh_token": "refresh-a",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	var tokenCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "refreshed-a",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	var upstreamCalls atomic.Int32
	var sawReplacementToken atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") == "Bearer access-b" {
			sawReplacementToken.Store(true)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		if err := os.WriteFile(authPath, []byte(`{
			"type": "codex",
			"account_id": "acct_b",
			"access_token": "access-b",
			"refresh_token": "refresh-b",
			"expired": "2099-01-01T00:00:00Z"
		}`), 0o600); err != nil {
			t.Errorf("replace auth file: %v", err)
		}
		http.Error(w, "expired", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:              authDir,
		AdminAPIKey:          "admin-key",
		Database:             DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:         upstream.URL + "/backend-api/codex",
		CodexRefreshTokenURL: tokenServer.URL,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hello"}`))
	resetRequestBody(req, []byte(`{"input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+userKey)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body: %s", resp.Code, resp.Body.String())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	if got := tokenCalls.Load(); got != 0 {
		t.Fatalf("token calls = %d, want 0", got)
	}
	if sawReplacementToken.Load() {
		t.Fatal("request retried with replacement account token")
	}
}

func TestReactiveUnauthorizedForwardsNonReplayableBodyOnce(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "auth.json", `{
		"type": "codex",
		"account_id": "acct_1",
		"access_token": "old-access",
		"refresh_token": "old-refresh",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	var tokenCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "new-access",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	var upstreamCalls atomic.Int32
	var sawBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		sawBody = string(body)
		http.Error(w, "expired", http.StatusUnauthorized)
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:              authDir,
		AdminAPIKey:          "admin-key",
		Database:             DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:         upstream.URL + "/backend-api/codex",
		CodexRefreshTokenURL: tokenServer.URL,
		AllowFastMode:        true,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Body = io.NopCloser(strings.NewReader("unknown-length-body"))
	req.ContentLength = -1
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+userKey)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body: %s", resp.Code, resp.Body.String())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	if got := tokenCalls.Load(); got != 0 {
		t.Fatalf("token calls = %d, want 0", got)
	}
	if sawBody != "unknown-length-body" {
		t.Fatalf("upstream body = %q, want unknown-length-body", sawBody)
	}
}

func TestChatCompletionsReactiveUnauthorizedRefreshesAndRetries(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "auth.json", `{
		"type": "codex",
		"account_id": "acct_1",
		"access_token": "old-access",
		"refresh_token": "old-refresh",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	var tokenCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "new-access",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") == "Bearer old-access" {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","delta":"ok"}`+"\n\n"+
			"event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp_ok","model":"gpt-5.3-codex"}}`+"\n\n")
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:              authDir,
		AdminAPIKey:          "admin-key",
		Database:             DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:         upstream.URL + "/backend-api/codex",
		CodexRefreshTokenURL: tokenServer.URL,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[{"role":"user","content":"say ok"}]
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("token calls = %d, want 1", got)
	}
}

func TestOAuthRefreshFailuresAreSanitizedForClientsAndDebugLogs(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "auth.json", `{
		"type": "codex",
		"account_id": "acct_1",
		"access_token": "secret-old-access",
		"refresh_token": "secret-old-refresh",
		"expired": "2000-01-01T00:00:00Z"
	}`)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"secret-old-access secret-old-refresh secret-endpoint-detail"}`)
	}))
	defer tokenServer.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:              authDir,
		AdminAPIKey:          "admin-key",
		Database:             DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:         "http://127.0.0.1:1/backend-api/codex",
		CodexRefreshTokenURL: tokenServer.URL,
		Debug:                true,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	t.Cleanup(func() { _ = handler.Close() })
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey

	var logs bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+userKey)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body: %s", resp.Code, resp.Body.String())
	}
	combined := resp.Body.String() + logs.String()
	for _, secret := range []string{"secret-old-access", "secret-old-refresh", "secret-endpoint-detail"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("client response or logs exposed %q:\n%s", secret, combined)
		}
	}
	if !strings.Contains(resp.Body.String(), "upstream authentication unavailable") {
		t.Fatalf("client response = %s, want sanitized auth error", resp.Body.String())
	}
	if !strings.Contains(logs.String(), "status=400") || !strings.Contains(logs.String(), "code=invalid_grant") {
		t.Fatalf("debug logs lack safe refresh context:\n%s", logs.String())
	}
}

func newRefreshTestAuth(t *testing.T) *Auth {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	raw := []byte(`{
		"type": "codex",
		"account_id": "acct_1",
		"access_token": "old-access",
		"refresh_token": "old-refresh",
		"expired": "2000-01-01T00:00:00Z"
	}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	auths, err := NewFileAuthStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("load auth: %v", err)
	}
	return auths[0]
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonHTTPResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

type temporaryNetworkError struct{}

func (temporaryNetworkError) Error() string   { return "temporary network failure with secret detail" }
func (temporaryNetworkError) Timeout() bool   { return false }
func (temporaryNetworkError) Temporary() bool { return true }

type permanentNetworkError struct{}

func (permanentNetworkError) Error() string { return "permanent network failure with secret detail" }
func (permanentNetworkError) Timeout() bool { return false }
