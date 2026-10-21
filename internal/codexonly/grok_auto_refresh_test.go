package codexonly

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func autoRefreshGrokFixture(t *testing.T, issuer, upstream string, expires time.Time) (*Server, CreatedUserAPIKey, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	writeGrokAuthFile(t, path, map[string]any{
		"session": map[string]any{
			"key": "old-access", "refresh_token": "old-refresh", "auth_mode": "oidc",
			"user_id": "b", "oidc_issuer": issuer, "oidc_client_id": "test-client",
			"expires_at": expires.UTC().Format(time.RFC3339),
		},
		"unrelated": map[string]any{"key": "leave-me"},
	})
	handler := newGrokTestHandler(t, t.TempDir(), path, upstream+"/v1")
	server := handler.Codex.(*Server)
	server.cfg.Grok.RefreshTokenURL = issuer + "/token"
	return server, createManagedUser(t, server, "admin-key", "auto-refresh-user"), path
}

func assertGrokAutoUsage(t *testing.T, server *Server, wantRequests, wantFailed int) {
	t.Helper()
	var requests, failed int
	err := server.users.db.QueryRow(`SELECT COALESCE(SUM(request_count),0), COALESCE(SUM(failed_request_count),0) FROM usage_buckets`).Scan(&requests, &failed)
	if err != nil {
		t.Fatal(err)
	}
	if requests != wantRequests || failed != wantFailed {
		t.Fatalf("usage requests=%d failed=%d, want %d/%d", requests, failed, wantRequests, wantFailed)
	}
}

func TestGrokAutoRefreshBeforeRequest(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/v1/models"} {
		for _, remaining := range []time.Duration{-time.Minute, time.Minute} {
			t.Run(fmt.Sprintf("%s/%s", path, remaining), func(t *testing.T) {
				var refreshes, calls atomic.Int32
				auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					refreshes.Add(1)
					_ = r.ParseForm()
					if r.PostForm.Get("refresh_token") != "old-refresh" || r.PostForm.Get("client_id") != "test-client" {
						t.Error("refresh grant did not use the selected credential")
					}
					_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`)
				}))
				defer auth.Close()
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Header.Get("Authorization") != "Bearer fresh-access" {
						t.Error("inference used an expired credential")
					}
					_, _ = io.WriteString(w, `{"data":[],"usage":{"total_tokens":1}}`)
				}))
				defer upstream.Close()
				server, user, file := autoRefreshGrokFixture(t, auth.URL, upstream.URL, time.Now().Add(remaining))
				var authDeadline, inferenceDeadline atomic.Bool
				transport := server.grokHTTP.Transport
				server.grokHTTP.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					_, bounded := r.Context().Deadline()
					if r.URL.Path == "/token" {
						authDeadline.Store(bounded)
					} else {
						inferenceDeadline.Store(bounded)
					}
					return transport.RoundTrip(r)
				})
				method := http.MethodPost
				if path == "/v1/models" {
					method = http.MethodGet
				}
				response := doJSONRequest(t, server.GrokHandler(), method, path, `{"model":"grok-test"}`, user.PlaintextAPIKey)
				if response.Code != http.StatusOK || refreshes.Load() != 1 || calls.Load() != 1 {
					t.Fatalf("status=%d refreshes=%d calls=%d, want 200/1/1", response.Code, refreshes.Load(), calls.Load())
				}
				if !authDeadline.Load() || inferenceDeadline.Load() {
					t.Fatalf("refresh bounded=%t inference bounded=%t", authDeadline.Load(), inferenceDeadline.Load())
				}
				record, err := grokAuthScopeRecord(file, "session")
				if err != nil || stringField(record, "key") != "fresh-access" || stringField(record, "refresh_token") != "fresh-refresh" {
					t.Fatalf("rotated credential not persisted: %v", err)
				}
			})
		}
	}
}

func TestGrokAutoRefreshUnauthorizedRetryBound(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/models"} {
		for _, rejectFresh := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reject=%t", path, rejectFresh), func(t *testing.T) {
				var refreshes, calls atomic.Int32
				auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					refreshes.Add(1)
					_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`)
				}))
				defer auth.Close()
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if rejectFresh || r.Header.Get("Authorization") == "Bearer old-access" {
						w.WriteHeader(http.StatusUnauthorized)
						return
					}
					_, _ = io.WriteString(w, `{"data":[],"usage":{"total_tokens":1}}`)
				}))
				defer upstream.Close()
				server, user, _ := autoRefreshGrokFixture(t, auth.URL, upstream.URL, time.Now().Add(time.Hour))
				method := http.MethodPost
				if path == "/v1/models" {
					method = http.MethodGet
				}
				response := doJSONRequest(t, server.GrokHandler(), method, path, `{"model":"grok-test"}`, user.PlaintextAPIKey)
				want := http.StatusOK
				if rejectFresh {
					want = http.StatusBadGateway
					if path == "/v1/models" {
						want = http.StatusServiceUnavailable
					}
				}
				if response.Code != want || refreshes.Load() != 1 || calls.Load() != 2 {
					t.Fatalf("status=%d refreshes=%d calls=%d, want %d/1/2", response.Code, refreshes.Load(), calls.Load(), want)
				}
				if method == http.MethodPost {
					failed := 0
					if rejectFresh {
						failed = 1
					}
					assertGrokAutoUsage(t, server, 1, failed)
				}
			})
		}
	}
}

func TestGrokAutoRefreshUnauthorizedAfterProactiveRefresh(t *testing.T) {
	var refreshes atomic.Int32
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := refreshes.Add(1)
		fmt.Fprintf(w, `{"access_token":"fresh-%d","refresh_token":"rotated-%d","expires_in":3600}`, attempt, attempt)
	}))
	defer auth.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh-2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"usage":{"total_tokens":1}}`)
	}))
	defer upstream.Close()
	server, user, _ := autoRefreshGrokFixture(t, auth.URL, upstream.URL, time.Now().Add(-time.Minute))
	response := doJSONRequest(t, server.GrokHandler(), http.MethodPost, "/v1/responses", `{"model":"grok-test"}`, user.PlaintextAPIKey)
	if response.Code != http.StatusOK || refreshes.Load() != 2 {
		t.Fatalf("status=%d refreshes=%d, want proactive refresh plus one 401 refresh", response.Code, refreshes.Load())
	}
	assertGrokAutoUsage(t, server, 1, 0)
}

func TestGrokAutoRefreshDoesNotWaitForUnauthorizedBody(t *testing.T) {
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"fresh-access","expires_in":3600}`)
	}))
	defer auth.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") == "Bearer old-access" {
			w.WriteHeader(http.StatusUnauthorized)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, `{"usage":{"total_tokens":1}}`)
	}))
	defer upstream.Close()
	server, user, _ := autoRefreshGrokFixture(t, auth.URL, upstream.URL, time.Now().Add(time.Hour))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-test"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+user.PlaintextAPIKey)
	response := httptest.NewRecorder()
	server.GrokHandler().ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d: automatic refresh waited for the rejected response body", response.Code)
	}
}

func TestGrokAutoRefreshConcurrentUnauthorized(t *testing.T) {
	const count = 12
	var oldCalls, freshCalls, refreshes atomic.Int32
	allOld := make(chan struct{})
	release := make(chan struct{})
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`)
	}))
	defer auth.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer old-access" {
			if oldCalls.Add(1) == count {
				close(allOld)
			}
			<-release
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		freshCalls.Add(1)
		_, _ = io.WriteString(w, `{"usage":{"total_tokens":1}}`)
	}))
	defer upstream.Close()
	server, user, _ := autoRefreshGrokFixture(t, auth.URL, upstream.URL, time.Now().Add(time.Hour))
	results := make(chan int, count)
	for i := 0; i < count; i++ {
		go func() {
			response := doJSONRequest(t, server.GrokHandler(), http.MethodPost, "/v1/responses", `{"model":"grok-test"}`, user.PlaintextAPIKey)
			results <- response.Code
		}()
	}
	select {
	case <-allOld:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("concurrent requests did not reach the upstream")
	}
	close(release)
	for i := 0; i < count; i++ {
		if status := <-results; status != http.StatusOK {
			t.Errorf("concurrent request status=%d", status)
		}
	}
	if refreshes.Load() != 1 || freshCalls.Load() != count {
		t.Fatalf("refreshes=%d fresh attempts=%d, want 1/%d", refreshes.Load(), freshCalls.Load(), count)
	}
	assertGrokAutoUsage(t, server, count, 0)
}

func TestGrokAutoRefreshFailureFallsBack(t *testing.T) {
	var refreshes atomic.Int32
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	defer auth.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fallback-access" {
			t.Error("failed refresh credential was sent upstream")
		}
		_, _ = io.WriteString(w, `{"usage":{"total_tokens":1}}`)
	}))
	defer upstream.Close()
	server, user, file := autoRefreshGrokFixture(t, auth.URL, upstream.URL, time.Now().Add(-time.Minute))
	writeGrokAuthFile(t, file, map[string]any{
		"expired":  map[string]any{"key": "old-access", "refresh_token": "old-refresh", "auth_mode": "oidc", "user_id": "b", "oidc_issuer": auth.URL, "oidc_client_id": "test-client", "expires_at": "2000-01-01T00:00:00Z"},
		"fallback": map[string]any{"key": "fallback-access", "auth_mode": "oidc", "user_id": "a", "oidc_issuer": auth.URL, "expires_at": "2099-01-01T00:00:00Z"},
	})
	response := doJSONRequest(t, server.GrokHandler(), http.MethodPost, "/v1/responses", `{"model":"grok-test"}`, user.PlaintextAPIKey)
	if response.Code != http.StatusOK || refreshes.Load() != 1 {
		t.Fatalf("status=%d refreshes=%d, want 200/1", response.Code, refreshes.Load())
	}
	assertGrokAutoUsage(t, server, 1, 0)
}

func TestGrokAutoRefreshWaiterCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var refreshes atomic.Int32
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if refreshes.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			_, _ = io.WriteString(w, `{"access_token":"fresh-access","expires_in":3600}`)
		case <-r.Context().Done():
		}
	}))
	defer auth.Close()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"usage":{"total_tokens":1}}`)
	}))
	defer upstream.Close()
	server, user, _ := autoRefreshGrokFixture(t, auth.URL, upstream.URL, time.Now().Add(-time.Minute))
	owner := make(chan int, 1)
	go func() {
		owner <- doJSONRequest(t, server.GrokHandler(), http.MethodPost, "/v1/responses", `{"model":"grok-test"}`, user.PlaintextAPIKey).Code
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("automatic refresh never started")
	}
	credentials, err := server.grokAuths.Load(context.Background())
	if err != nil || len(credentials) != 1 {
		t.Fatalf("load current credential: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	waiter := make(chan error, 1)
	go func() {
		_, errRefresh := server.grokAuths.Refresh(ctx, credentials[0].ID, server.grokHTTP, server.cfg.Grok.RefreshTokenURL)
		waiter <- errRefresh
	}()
	select {
	case errWait := <-waiter:
		if !errors.Is(errWait, context.DeadlineExceeded) {
			t.Fatalf("waiter error=%v, want its own deadline", errWait)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter remained blocked behind refresh")
	}
	if refreshes.Load() != 1 {
		t.Fatalf("concurrent admin waiter started %d token exchanges", refreshes.Load())
	}
	close(release)
	released = true
	select {
	case status := <-owner:
		if status != http.StatusOK {
			t.Fatalf("waiter cancellation aborted owner: status=%d", status)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh owner did not finish after release")
	}
}

func TestGrokRefreshPreservesOtherConcurrentAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		entered <- struct{}{}
		<-release
		fmt.Fprintf(w, `{"access_token":%q,"expires_in":3600}`, "new-"+r.PostForm.Get("refresh_token"))
	}))
	defer auth.Close()
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	records := map[string]any{}
	for _, account := range []string{"a", "b"} {
		records[account] = map[string]any{
			"key": "access-" + account, "refresh_token": "refresh-" + account, "auth_mode": "oidc",
			"user_id": account, "oidc_issuer": auth.URL, "oidc_client_id": "test-client", "expires_at": "2099-01-01T00:00:00Z",
		}
	}
	writeGrokAuthFile(t, path, records)
	store := NewGrokAuthStore([]string{path})
	credentials, err := store.Load(context.Background())
	if err != nil || len(credentials) != 2 {
		t.Fatalf("fixture credentials: %v", err)
	}
	results := make(chan error, 2)
	for _, credential := range credentials {
		go func(id string) {
			_, errRefresh := store.Refresh(context.Background(), id, auth.Client(), auth.URL+"/token")
			results <- errRefresh
		}(credential.ID)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("one account's token exchange blocked the other account")
		}
	}
	close(release)
	released = true
	for i := 0; i < 2; i++ {
		if errRefresh := <-results; errRefresh != nil {
			t.Fatal(errRefresh)
		}
	}
	for _, account := range []string{"a", "b"} {
		record, errRead := grokAuthScopeRecord(path, account)
		if errRead != nil || stringField(record, "key") != "new-refresh-"+account {
			t.Fatalf("concurrent refresh lost account %s: %v", account, errRead)
		}
	}
}

func TestGrokAutoRefreshCancellation(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		close(entered)
		<-r.Context().Done()
		close(canceled)
	}))
	defer auth.Close()
	server, user, file := autoRefreshGrokFixture(t, auth.URL, "http://127.0.0.1:1", time.Now().Add(-time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-test"}`)).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+user.PlaintextAPIKey)
		server.GrokHandler().ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("expired credential never entered automatic refresh")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request survived cancellation during refresh")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("OAuth request survived caller cancellation")
	}
	record, err := grokAuthScopeRecord(file, "session")
	if err != nil || stringField(record, "key") != "old-access" {
		t.Fatalf("canceled refresh changed the credential: %v", err)
	}
}
