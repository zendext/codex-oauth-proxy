package codexonly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestServerSessionAffinityPersistsAcrossTenantKeyRotationAndRestart(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
	databasePath := filepath.Join(t.TempDir(), "users.db")

	var mu sync.Mutex
	var authorizations []string
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	server := newSessionAffinityServer(t, authDir, databasePath, upstream.URL)
	alice := createManagedUser(t, server, "admin-key", "Alice")
	bob := createManagedUser(t, server, "admin-key", "Bob")

	sendResponseRequest(t, server, alice.PlaintextAPIKey, "", `{"model":"gpt-5.3-codex","input":"no-session-1"}`)
	sendResponseRequest(t, server, alice.PlaintextAPIKey, "", `{"model":"gpt-5.3-codex","input":"no-session-2"}`)
	aliceBody := `{"model":"gpt-5.3-codex","prompt_cache_key":"shared-cache","conversation":{"id":"alice-conversation"},"input":"alice-1"}`
	sendResponseRequest(t, server, alice.PlaintextAPIKey, "", aliceBody)
	sendResponseRequest(t, server, alice.PlaintextAPIKey, "", `{"model":"gpt-5.6","conversation_id":"alice-conversation","input":"alice-2"}`)
	sendResponseRequest(t, server, bob.PlaintextAPIKey, "", `{"model":"gpt-5.3-codex","prompt_cache_key":"shared-cache","conversation":{"id":"alice-conversation"},"input":"bob"}`)

	resetResp := doJSONRequest(
		t,
		server,
		http.MethodPost,
		"/v0/management/users/"+alice.User.ID+"/api-key/reset",
		"",
		"admin-key",
	)
	if resetResp.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200, body: %s", resetResp.Code, resetResp.Body.String())
	}
	var reset CreatedUserAPIKey
	decodeResponse(t, resetResp, &reset)
	sendResponseRequest(t, server, reset.PlaintextAPIKey, "", `{"model":"gpt-5.5","prompt_cache_key":"shared-cache","input":"alice-rotated"}`)

	if err := server.Close(); err != nil {
		t.Fatalf("close server before restart: %v", err)
	}
	server = newSessionAffinityServer(t, authDir, databasePath, upstream.URL)
	t.Cleanup(func() {
		if errClose := server.Close(); errClose != nil {
			t.Fatalf("close restarted server: %v", errClose)
		}
	})
	sendResponseRequest(t, server, reset.PlaintextAPIKey, "", `{"model":"gpt-5.4","conversation_id":"alice-conversation","input":"alice-restart"}`)

	mu.Lock()
	gotAuth := slices.Clone(authorizations)
	gotBodies := slices.Clone(bodies)
	mu.Unlock()
	wantAuth := []string{
		"Bearer access-a",
		"Bearer access-b",
		"Bearer access-a",
		"Bearer access-a",
		"Bearer access-b",
		"Bearer access-a",
		"Bearer access-a",
	}
	if !slices.Equal(gotAuth, wantAuth) {
		t.Fatalf("upstream authorizations = %v, want %v", gotAuth, wantAuth)
	}
	if gotBodies[2] != aliceBody {
		t.Fatalf("restored upstream body = %q, want %q", gotBodies[2], aliceBody)
	}
}

func TestServerCompatibilitySessionAffinityUsesMatchedAuthTenant(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var mu sync.Mutex
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	server := newSessionAffinityServer(t, authDir, filepath.Join(t.TempDir(), "users.db"), upstream.URL)
	defer server.Close()
	for _, token := range []string{"access-a", "access-b", "access-a", "access-b"} {
		req := httptest.NewRequest(http.MethodGet, "/backend-api/wham/usage", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Session-Id", "shared-session")
		resp := httptest.NewRecorder()
		server.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("compatibility token %q status = %d, want 200, body: %s", token, resp.Code, resp.Body.String())
		}
	}
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a-new", false)
	req := httptest.NewRequest(http.MethodGet, "/backend-api/wham/usage", nil)
	req.Header.Set("Authorization", "Bearer access-a-new")
	req.Header.Set("Session-Id", "shared-session")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("rotated compatibility token status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}

	mu.Lock()
	got := slices.Clone(authorizations)
	mu.Unlock()
	want := []string{
		"Bearer access-a",
		"Bearer access-b",
		"Bearer access-a",
		"Bearer access-b",
		"Bearer access-a-new",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("compatibility tenant authorizations = %v, want %v", got, want)
	}
}

func TestServerConcurrentFirstSessionRequestsUseDatabaseWinner(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var mu sync.Mutex
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	server := newSessionAffinityServer(t, authDir, filepath.Join(t.TempDir(), "users.db"), upstream.URL)
	defer server.Close()
	user := createManagedUser(t, server, "admin-key", "Alice")

	const requests = 32
	start := make(chan struct{})
	errs := make(chan string, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.3-codex","session_id":"race-session","input":"hello"}`))
			req.Header.Set("Authorization", "Bearer "+user.PlaintextAPIKey)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			server.ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				errs <- resp.Body.String()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for failure := range errs {
		t.Fatalf("concurrent request failed: %s", failure)
	}

	mu.Lock()
	got := slices.Clone(authorizations)
	mu.Unlock()
	if len(got) != requests {
		t.Fatalf("upstream request count = %d, want %d", len(got), requests)
	}
	for _, authorization := range got[1:] {
		if authorization != got[0] {
			t.Fatalf("concurrent first requests split across auths: %v", got)
		}
	}
}

func TestServerSessionAffinityUsesSignalFromLargeJSONBody(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var mu sync.Mutex
	var authorizations []string
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	server := newSessionAffinityServer(t, authDir, filepath.Join(t.TempDir(), "users.db"), upstream.URL)
	defer server.Close()
	user := createManagedUser(t, server, "admin-key", "Alice")
	body := largeSessionAffinityJSON("large-sticky-session")
	if len(body) <= sessionAffinityReplayMemoryBytes {
		t.Fatalf("large body length = %d, want more than %d", len(body), sessionAffinityReplayMemoryBytes)
	}

	sendResponseRequest(t, server, user.PlaintextAPIKey, "", body)
	sendResponseRequest(t, server, user.PlaintextAPIKey, "", body)

	mu.Lock()
	gotAuths := slices.Clone(authorizations)
	gotBodies := slices.Clone(bodies)
	mu.Unlock()
	if len(gotAuths) != 2 || gotAuths[0] != gotAuths[1] {
		t.Fatalf("large-body authorizations = %v, want one sticky auth", gotAuths)
	}
	for i, gotBody := range gotBodies {
		if gotBody != body {
			t.Fatalf("upstream body #%d length = %d, want exact %d-byte body", i+1, len(gotBody), len(body))
		}
	}
}

func TestServerSessionAffinitySpillFailuresRemainBoundedAndDoNotBind(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*sessionAffinityReplayStore)
	}{
		{
			name: "create",
			configure: func(store *sessionAffinityReplayStore) {
				store.createTemp = func() (*os.File, error) {
					return nil, errors.New("injected create failure")
				}
			},
		},
		{
			name: "write",
			configure: func(store *sessionAffinityReplayStore) {
				writes := 0
				store.writeFile = func(file *os.File, p []byte) (int, error) {
					writes++
					if writes == 2 {
						return 0, errors.New("injected write failure")
					}
					return file.Write(p)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authDir := t.TempDir()
			writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
			writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

			var upstreamBody string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				upstreamBody = string(body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer upstream.Close()

			server := newSessionAffinityServer(t, authDir, filepath.Join(t.TempDir(), "users.db"), upstream.URL)
			server.cfg.AllowFastMode = true
			defer server.Close()
			var replayStore *sessionAffinityReplayStore
			server.sessionAffinityReplayStore = func() *sessionAffinityReplayStore {
				replayStore = &sessionAffinityReplayStore{}
				tt.configure(replayStore)
				return replayStore
			}
			user := createManagedUser(t, server, "admin-key", "Alice")
			body := largeSessionAffinityJSON("must-not-bind")

			sendResponseRequest(t, server, user.PlaintextAPIKey, "", body)

			if upstreamBody != body {
				t.Fatalf("upstream body length = %d, want exact %d-byte body", len(upstreamBody), len(body))
			}
			var bindings int
			if err := server.users.db.QueryRow(`SELECT COUNT(*) FROM session_affinity_bindings`).Scan(&bindings); err != nil {
				t.Fatalf("count session affinity bindings: %v", err)
			}
			if bindings != 0 {
				t.Fatalf("session affinity bindings = %d, want 0 after incomplete inspection", bindings)
			}
			if replayStore == nil {
				t.Fatal("session affinity replay store was not created")
			}
			if got := replayStore.memory.Len() + len(replayStore.tail); got > sessionAffinityReplayMemoryBytes {
				t.Fatalf("replay heap bytes = %d, want at most %d", got, sessionAffinityReplayMemoryBytes)
			}
			if replayStore.file != nil || replayStore.path != "" {
				t.Fatalf("spill failure leaked replay store: file=%v path=%q", replayStore.file, replayStore.path)
			}
		})
	}
}

func TestServerSessionAffinityDisableReenableAndRemoval(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var mu sync.Mutex
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	server := newSessionAffinityServer(t, authDir, filepath.Join(t.TempDir(), "users.db"), upstream.URL)
	defer server.Close()
	user := createManagedUser(t, server, "admin-key", "Alice")

	sendResponseRequest(t, server, user.PlaintextAPIKey, "idle-session", `{"model":"gpt-5.3-codex","input":"initial"}`)
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", true)
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	sendResponseRequest(t, server, user.PlaintextAPIKey, "idle-session", `{"model":"gpt-5.4","input":"re-enabled"}`)

	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", true)
	sendResponseRequest(t, server, user.PlaintextAPIKey, "idle-session", `{"model":"gpt-5.5","input":"disabled-failover"}`)
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	sendResponseRequest(t, server, user.PlaintextAPIKey, "idle-session", `{"model":"gpt-5.6","input":"sticky-replacement"}`)

	sendResponseRequest(t, server, user.PlaintextAPIKey, "removed-session", `{"model":"gpt-5.3-codex","input":"before-removal"}`)
	if err := os.Remove(filepath.Join(authDir, "a.json")); err != nil {
		t.Fatalf("remove auth a: %v", err)
	}
	sendResponseRequest(t, server, user.PlaintextAPIKey, "removed-session", `{"model":"gpt-5.3-codex","input":"after-removal"}`)

	var removedRows int
	if err := server.users.db.QueryRow(
		`SELECT COUNT(*) FROM session_affinity_bindings WHERE auth_id = 'account:acct_a'`,
	).Scan(&removedRows); err != nil {
		t.Fatalf("count removed auth bindings: %v", err)
	}
	if removedRows != 0 {
		t.Fatalf("removed auth binding rows = %d, want 0", removedRows)
	}

	mu.Lock()
	got := slices.Clone(authorizations)
	mu.Unlock()
	want := []string{
		"Bearer access-a",
		"Bearer access-a",
		"Bearer access-b",
		"Bearer access-b",
		"Bearer access-a",
		"Bearer access-b",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("disable/removal authorizations = %v, want %v", got, want)
	}
}

func TestServerSessionAffinityInvalidatesStableIdentityReplacement(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var mu sync.Mutex
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	server := newSessionAffinityServer(t, authDir, filepath.Join(t.TempDir(), "users.db"), upstream.URL)
	defer server.Close()
	user := createManagedUser(t, server, "admin-key", "Alice")

	sendResponseRequest(t, server, user.PlaintextAPIKey, "replacement-session", `{"model":"gpt-5.3-codex","input":"before"}`)
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_c", "access-c", false)
	sendResponseRequest(t, server, user.PlaintextAPIKey, "replacement-session", `{"model":"gpt-5.3-codex","input":"after"}`)

	var oldRows int
	if err := server.users.db.QueryRow(
		`SELECT COUNT(*) FROM session_affinity_bindings WHERE auth_id = 'account:acct_a'`,
	).Scan(&oldRows); err != nil {
		t.Fatalf("count replaced auth rows: %v", err)
	}
	if oldRows != 0 {
		t.Fatalf("replaced auth binding rows = %d, want 0", oldRows)
	}
	mu.Lock()
	got := slices.Clone(authorizations)
	mu.Unlock()
	if len(got) != 2 || got[0] != "Bearer access-a" || got[1] == "Bearer access-a" {
		t.Fatalf("identity replacement authorizations = %v, want old auth then replacement selection", got)
	}
}

func TestServerInvalidOrMissingSessionSignalsFallBackWithoutBinding(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var mu sync.Mutex
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	server := newSessionAffinityServer(t, authDir, filepath.Join(t.TempDir(), "users.db"), upstream.URL)
	defer server.Close()
	user := createManagedUser(t, server, "admin-key", "Alice")

	sendResponseRequest(t, server, user.PlaintextAPIKey, "", `{"model":"gpt-5.3-codex","input":"missing"}`)
	sendResponseRequest(t, server, user.PlaintextAPIKey, "", `{"model":"gpt-5.3-codex","session_id":"bad\u0000value","input":"control"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.3-codex","input":"oversized"}`))
	req.Header.Set("Authorization", "Bearer "+user.PlaintextAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session-Id", strings.Repeat("x", maxSessionAffinitySignalBytes+1))
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("oversized signal status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}

	var rows int
	if err := server.users.db.QueryRow(`SELECT COUNT(*) FROM session_affinity_bindings`).Scan(&rows); err != nil {
		t.Fatalf("count session affinity rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("invalid or missing signals created %d affinity rows, want 0", rows)
	}
	mu.Lock()
	got := slices.Clone(authorizations)
	mu.Unlock()
	want := []string{"Bearer access-a", "Bearer access-b", "Bearer access-a"}
	if !slices.Equal(got, want) {
		t.Fatalf("fallback authorizations = %v, want %v", got, want)
	}
	select {
	case err := <-server.FatalErrors():
		t.Fatalf("invalid session signal reported fatal storage error: %v", err)
	default:
	}
}

func TestServerInvalidBodyUnicodeDoesNotBindAndForwardsExactBytes(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var mu sync.Mutex
	var authorizations []string
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		bodies = append(bodies, slices.Clone(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	server := newSessionAffinityServer(t, authDir, filepath.Join(t.TempDir(), "users.db"), upstream.URL)
	defer server.Close()
	user := createManagedUser(t, server, "admin-key", "Alice")
	invalidUTF8 := append([]byte(`{"model":"gpt-5.3-codex","session_id":"bad-`), 0xff)
	invalidUTF8 = append(invalidUTF8, []byte(`","input":"utf8"}`)...)
	requestBodies := [][]byte{
		invalidUTF8,
		[]byte(`{"model":"gpt-5.3-codex","conversation":{"id":"bad-\uD800"},"input":"surrogate"}`),
	}

	for _, body := range requestBodies {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+user.PlaintextAPIKey)
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		server.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("invalid Unicode signal status = %d, want 200, body: %s", resp.Code, resp.Body.String())
		}
	}

	var rows int
	if err := server.users.db.QueryRow(`SELECT COUNT(*) FROM session_affinity_bindings`).Scan(&rows); err != nil {
		t.Fatalf("count session affinity rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("invalid Unicode signals created %d affinity rows, want 0", rows)
	}
	mu.Lock()
	gotAuths := slices.Clone(authorizations)
	gotBodies := slices.Clone(bodies)
	mu.Unlock()
	if want := []string{"Bearer access-a", "Bearer access-b"}; !slices.Equal(gotAuths, want) {
		t.Fatalf("fallback authorizations = %v, want %v", gotAuths, want)
	}
	if len(gotBodies) != len(requestBodies) {
		t.Fatalf("upstream body count = %d, want %d", len(gotBodies), len(requestBodies))
	}
	for i := range requestBodies {
		if !bytes.Equal(gotBodies[i], requestBodies[i]) {
			t.Fatalf("upstream body #%d = %q, want exact %q", i+1, gotBodies[i], requestBodies[i])
		}
	}
}

func TestServerSessionAffinityPreservesChatStreamingAndWebSockets(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var mu sync.Mutex
	var httpAuths []string
	var websocketAuths []string
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocketRequested(r) {
			mu.Lock()
			websocketAuths = append(websocketAuths, r.Header.Get("Authorization"))
			mu.Unlock()
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			messageType, payload, err := conn.ReadMessage()
			if err == nil {
				_ = conn.WriteMessage(messageType, payload)
			}
			return
		}
		mu.Lock()
		httpAuths = append(httpAuths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("event: response.output_text.delta\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","delta":"ok"}` + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp","model":"gpt-5.3-codex","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
	}))
	defer upstream.Close()

	server := newSessionAffinityServer(t, authDir, filepath.Join(t.TempDir(), "users.db"), upstream.URL)
	defer server.Close()
	user := createManagedUser(t, server, "admin-key", "Alice")

	for range 2 {
		resp := doJSONRequest(
			t,
			server,
			http.MethodPost,
			"/v1/chat/completions",
			`{"model":"gpt-5.3-codex","session_id":"chat-stream","messages":[{"role":"user","content":"hello"}],"stream":true}`,
			user.PlaintextAPIKey,
		)
		if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "[DONE]") {
			t.Fatalf("chat stream status = %d body = %s", resp.Code, resp.Body.String())
		}
	}

	proxy := httptest.NewServer(server)
	defer proxy.Close()
	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/v1/responses"
	for range 2 {
		headers := http.Header{
			"Authorization": []string{"Bearer " + user.PlaintextAPIKey},
			"Session-Id":    []string{"websocket-session"},
		}
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
		if err != nil {
			t.Fatalf("dial proxy websocket: %v", err)
		}
		frame := []byte(`{"type":"response.create","model":"gpt-5.3-codex","input":"hello"}`)
		if err = conn.WriteMessage(websocket.TextMessage, frame); err != nil {
			conn.Close()
			t.Fatalf("write websocket frame: %v", err)
		}
		_, echoed, err := conn.ReadMessage()
		if err != nil {
			conn.Close()
			t.Fatalf("read websocket frame: %v", err)
		}
		if string(echoed) != string(frame) {
			conn.Close()
			t.Fatalf("echoed frame = %q, want %q", string(echoed), string(frame))
		}
		if err = conn.Close(); err != nil {
			t.Fatalf("close websocket: %v", err)
		}
	}

	mu.Lock()
	gotHTTP := slices.Clone(httpAuths)
	gotWebSocket := slices.Clone(websocketAuths)
	mu.Unlock()
	if len(gotHTTP) != 2 || gotHTTP[0] != gotHTTP[1] {
		t.Fatalf("chat streaming auths = %v, want one sticky auth", gotHTTP)
	}
	if len(gotWebSocket) != 2 || gotWebSocket[0] != gotWebSocket[1] {
		t.Fatalf("websocket auths = %v, want one sticky auth", gotWebSocket)
	}
}

func TestServerConcurrentCASRebindFollowsWinner(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
	writeSessionAffinityAuth(t, authDir, "c.json", "acct_c", "access-c", false)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	server := newSessionAffinityServer(t, authDir, filepath.Join(t.TempDir(), "users.db"), upstream.URL)
	defer server.Close()
	user := createManagedUser(t, server, "admin-key", "Alice")
	authorization := proxyAuthorization{Credential: &AuthenticatedAPIKey{User: user.User, APIKey: user.APIKey}}
	signals := []sessionAffinitySignal{{Kind: sessionAffinitySignalSessionID, Value: "cas-session"}}
	initial, err := server.selectProxyAuth(context.Background(), authorization, signals)
	if err != nil {
		t.Fatalf("selectProxyAuth returned error: %v", err)
	}

	start := make(chan struct{})
	results := make(chan AuthSelection, 2)
	errs := make(chan error, 2)
	for _, candidate := range []string{"account:acct_b", "account:acct_c"} {
		go func(candidate string) {
			<-start
			selection, errRebind := server.RebindAuthSelection(context.Background(), initial, candidate)
			results <- selection
			errs <- errRebind
		}(candidate)
	}
	close(start)
	first := <-results
	second := <-results
	if err = <-errs; err != nil {
		t.Fatalf("first RebindAuthSelection error: %v", err)
	}
	if err = <-errs; err != nil {
		t.Fatalf("second RebindAuthSelection error: %v", err)
	}
	if first.Auth.ID != second.Auth.ID || first.Binding.AuthID != second.Binding.AuthID {
		t.Fatalf("CAS selections did not converge: first=%#v second=%#v", first, second)
	}
	if first.Auth.ID == initial.Auth.ID {
		t.Fatalf("CAS selection stayed on initial auth %q", initial.Auth.ID)
	}
}

func newSessionAffinityServer(t *testing.T, authDir string, databasePath string, upstreamURL string) *Server {
	t.Helper()
	server, err := NewHandler(context.Background(), &Config{
		AuthDir:        authDir,
		AdminAPIKey:    "admin-key",
		Database:       DatabaseConfig{Path: databasePath},
		CodexBaseURL:   upstreamURL + "/backend-api/codex",
		ChatGPTBaseURL: upstreamURL + "/backend-api",
		RequestRetry:   1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	return server
}

func writeSessionAffinityAuth(t *testing.T, dir string, name string, accountID string, accessToken string, disabled bool) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type":          "codex",
		"account_id":    accountID,
		"access_token":  accessToken,
		"refresh_token": "refresh-" + accountID,
		"expired":       "2099-01-01T00:00:00Z",
		"disabled":      disabled,
	})
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	if err = os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatalf("write auth: %v", err)
	}
}

func sendResponseRequest(t *testing.T, server *Server, apiKey string, sessionID string, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("Session-Id", sessionID)
	}
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("response request status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
}

func TestSessionAffinityDebugLogsDoNotExposeRawSignals(t *testing.T) {
	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	oldPrefix := log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	})

	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()
	server := newSessionAffinityServer(t, authDir, filepath.Join(t.TempDir(), "users.db"), upstream.URL)
	server.cfg.Debug = true
	defer server.Close()
	user := createManagedUser(t, server, "admin-key", "Alice")

	rawSession := "raw-session-log-secret"
	sendResponseRequest(
		t,
		server,
		user.PlaintextAPIKey,
		rawSession,
		`{"model":"gpt-5.3-codex","conversation_id":"raw-conversation-log-secret","input":"hello"}`,
	)
	for _, secret := range []string{rawSession, "raw-conversation-log-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("debug logs exposed raw session signal %q:\n%s", secret, logs.String())
		}
	}
}

func TestSessionAffinityCleanupUsesUTCClock(t *testing.T) {
	store := openTestUserStore(t)
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.FixedZone("offset", 8*60*60))
	store.now = func() time.Time { return now }
	if got := store.storeNow(); got.Location() != time.UTC {
		t.Fatalf("storeNow location = %s, want UTC", got.Location())
	}
}
