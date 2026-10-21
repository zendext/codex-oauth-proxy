package codexonly

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func reviewGrokFixture(t *testing.T, up string, count int) (*Server, CreatedUserAPIKey) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	records := map[string]any{}
	for i := 0; i < count; i++ {
		records[fmt.Sprint(i)] = map[string]any{
			"key": fmt.Sprintf("synthetic-grok-token-%d", i), "auth_mode": "oidc",
			"user_id": fmt.Sprintf("user-%d", i), "oidc_issuer": "https://auth.x.ai",
			"expires_at": "2099-01-01T00:00:00Z",
		}
	}
	writeGrokAuthFile(t, path, records)
	handler := newGrokTestHandler(t, t.TempDir(), path, up)
	server := handler.Codex.(*Server)
	return server, createManagedUser(t, server, "admin-key", "review-user")
}

func TestReviewGrokInferencePaths(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			gotPath := ""
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				if r.URL.Path != path {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
			}))
			defer up.Close()
			server, user := reviewGrokFixture(t, up.URL+"/v1", 1)
			rec := doJSONRequest(t, server.GrokHandler(), http.MethodPost, path, `{"model":"grok-test"}`, user.PlaintextAPIKey)
			if gotPath != path || rec.Code != 200 {
				t.Fatalf("upstream path=%q, client status=%d; want %q and 200", gotPath, rec.Code, path)
			}
		})
	}
}

func TestReviewGrokLocalAdmin(t *testing.T) {
	server, _ := reviewGrokFixture(t, "http://127.0.0.1:1/v1", 1)
	rec := doJSONRequestFromRemote(t, server.GrokHandler(), "127.0.0.1:32100", http.MethodGet, "/v0/local-admin/grok/auths", "", "")
	if rec.Code != 200 {
		t.Fatalf("loopback Grok admin status=%d, body=%s; want 200 without admin key", rec.Code, rec.Body.String())
	}
}

func TestReviewGrokFailoverStatus(t *testing.T) {
	for _, status := range []int{401, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write([]byte(`{"usage":{"total_tokens":1}}`))
			}))
			defer up.Close()
			server, user := reviewGrokFixture(t, up.URL+"/v1", 2)
			rec := doJSONRequest(t, server.GrokHandler(), http.MethodPost, "/v1/responses", `{"model":"grok-test"}`, user.PlaintextAPIKey)
			if rec.Code != 200 || calls.Load() != 2 {
				t.Fatalf("status=%d, upstream calls=%d; want 200 with second credential", rec.Code, calls.Load())
			}
		})
	}
}

func TestReviewGrokOrdinaryErrorDoesNotFailover(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer up.Close()
	server, user := reviewGrokFixture(t, up.URL+"/v1", 2)
	rec := doJSONRequest(t, server.GrokHandler(), http.MethodPost, "/v1/responses", `{"model":"grok-test"}`, user.PlaintextAPIKey)
	if rec.Code != http.StatusBadRequest || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d; ordinary request error must not switch credentials", rec.Code, calls.Load())
	}
}

func TestReviewGrokModelDiscoveryFailover(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer up.Close()
	server, user := reviewGrokFixture(t, up.URL+"/v1", 2)
	rec := doJSONRequest(t, server.GrokHandler(), http.MethodGet, "/v1/models", "", user.PlaintextAPIKey)
	if rec.Code != 200 || calls.Load() != 2 {
		t.Fatalf("model discovery status=%d, calls=%d; want fallback to second credential", rec.Code, calls.Load())
	}
}

func TestReviewGrokExhaustedRequestCounted(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer up.Close()
	server, user := reviewGrokFixture(t, up.URL+"/v1", 2)
	rec := doJSONRequest(t, server.GrokHandler(), http.MethodPost, "/v1/responses", `{"model":"grok-test"}`, user.PlaintextAPIKey)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", rec.Code)
	}
	var requests, failed int
	err := server.users.db.QueryRow(`SELECT COALESCE(SUM(request_count),0), COALESCE(SUM(failed_request_count),0) FROM usage_buckets`).Scan(&requests, &failed)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || failed != 1 {
		t.Fatalf("usage requests=%d failed=%d; want exactly one failed inference", requests, failed)
	}
}

func TestReviewGrokSSEFlushed(t *testing.T) {
	emitted := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"delta\":\"hello\"}\n\n")
		w.(http.Flusher).Flush()
		close(emitted)
		<-release
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer up.Close()
	server, user := reviewGrokFixture(t, up.URL+"/v1", 1)
	proxy := httptest.NewServer(server.GrokHandler())
	defer proxy.Close()
	first := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", strings.NewReader(`{"model":"grok-test","stream":true}`))
		req.Header.Set("Authorization", "Bearer "+user.PlaintextAPIKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			first <- err
			return
		}
		defer resp.Body.Close()
		var b [1]byte
		_, err = resp.Body.Read(b[:])
		first <- err
	}()
	<-emitted
	select {
	case err := <-first:
		close(release)
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		close(release)
		<-first
		t.Fatal("client got neither headers nor first SSE bytes until upstream completed")
	}
}

func TestReviewGrokShutdownCancelsRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer up.Close()
	server, user := reviewGrokFixture(t, up.URL+"/v1", 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-test"}`))
		req.Header.Set("Authorization", "Bearer "+user.PlaintextAPIKey)
		server.GrokHandler().ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-entered
	server.Shutdown()
	select {
	case <-done:
		close(release)
	case <-time.After(500 * time.Millisecond):
		close(release)
		<-done
		t.Fatal("Grok in-flight request survived Server.Shutdown context cancellation")
	}
}

func TestReviewGrokStorageFailureBeforeCommit(t *testing.T) {
	var server *Server
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = server.users.databaseError("review injected read failure", errors.New("synthetic SQLite failure"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer up.Close()
	server, user := reviewGrokFixture(t, up.URL+"/v1", 1)
	rec := doJSONRequest(t, server.GrokHandler(), http.MethodGet, "/v1/models", "", user.PlaintextAPIKey)
	if rec.Code != 500 {
		t.Fatalf("client status=%d after fatal SQLite error, body=%s; want 500", rec.Code, rec.Body.String())
	}
}

func TestReviewGrokRefreshDoesNotOverwriteReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	record := func(user, key string) map[string]any {
		return map[string]any{"https://auth.x.ai": map[string]any{
			"key": key, "refresh_token": "synthetic-refresh-" + user, "auth_mode": "oidc",
			"user_id": user, "oidc_issuer": "https://auth.x.ai", "oidc_client_id": "test-client",
			"expires_at": "2099-01-01T00:00:00Z",
		}}
	}
	writeGrokAuthFile(t, path, record("old-user", "old-token"))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeGrokAuthFile(t, path, record("replacement-user", "replacement-token"))
		_, _ = io.WriteString(w, `{"access_token":"refreshed-old-user-token","expires_in":3600}`)
	}))
	defer up.Close()
	store := NewGrokAuthStore([]string{path})
	creds, err := store.Load(context.Background())
	if err != nil || len(creds) != 1 {
		t.Fatalf("fixture load failed: %v", err)
	}
	_, _ = store.Refresh(context.Background(), creds[0].ID, up.Client(), up.URL)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "refreshed-old-user-token") {
		t.Fatalf("new account record overwritten by refresh of old account: %s", data)
	}
}

func TestReviewGrokDoesNotFollowRedirects(t *testing.T) {
	var leaked atomic.Bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store(strings.HasPrefix(r.Header.Get("Authorization"), "Bearer synthetic-grok-token-"))
		_, _ = io.WriteString(w, `{}`)
	}))
	defer destination.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", destination.URL+"/outside")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer up.Close()
	server, user := reviewGrokFixture(t, up.URL+"/v1", 1)
	_ = doJSONRequest(t, server.GrokHandler(), http.MethodPost, "/v1/responses", `{"model":"grok-test"}`, user.PlaintextAPIKey)
	if leaked.Load() {
		t.Fatal("upstream 307 followed outside configured endpoint; OAuth bearer forwarded to redirected listener")
	}
}
