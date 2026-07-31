package codexonly

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestFatalStorageFailureWinsBeforeSuccessfulResponseCommit(t *testing.T) {
	server := newStorageFailureTestServer(t, "http://127.0.0.1:1/backend-api/codex", "http://127.0.0.1:1/backend-api")
	server.cfg.Debug = true
	writer := newBlockingHeaderResponseWriter()
	req := httptest.NewRequest(http.MethodGet, "/v0/local-admin/users", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	handlerDone := make(chan struct{})
	var logs bytes.Buffer
	restore := captureStandardLogger(t, &logs)
	defer restore()

	go func() {
		defer close(handlerDone)
		server.ServeHTTP(writer, req)
	}()

	select {
	case <-writer.headerStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for successful handler response")
	}

	fatalErr := server.users.databaseError("injected runtime read", errors.New("injected SQLite failure"))
	if !errors.Is(fatalErr, ErrStorageFailure) {
		t.Fatalf("databaseError = %v, want ErrStorageFailure", fatalErr)
	}
	close(writer.allowHeader)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for handler completion")
	}

	if writer.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body: %s", writer.Code, writer.Body.String())
	}
	if bytes.Contains(writer.Body.Bytes(), []byte(`"users"`)) {
		t.Fatalf("fatal response committed successful body: %s", writer.Body.String())
	}
	if !strings.Contains(logs.String(), "status=500") {
		t.Fatalf("debug log did not record fatal status:\n%s", logs.String())
	}
	assertFatalStorageError(t, server.FatalErrors())
}

func TestStorageResponseWriterBlocksImplicitCommitsAfterFatal(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		failures := failedStorageState()
		recorder := httptest.NewRecorder()
		writer := newStorageResponseWriter(recorder, failures)
		successBody := []byte(`{"ok":true}`)

		written, err := writer.Write(successBody)
		if err != nil {
			t.Fatalf("Write returned error: %v", err)
		}
		if written != len(successBody) {
			t.Fatalf("Write count = %d, want %d", written, len(successBody))
		}
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", recorder.Code)
		}
		if bytes.Contains(recorder.Body.Bytes(), successBody) {
			t.Fatalf("fatal response committed successful body: %s", recorder.Body.String())
		}
	})

	t.Run("flush", func(t *testing.T) {
		failures := failedStorageState()
		recorder := httptest.NewRecorder()
		writer := newStorageResponseWriter(recorder, failures)

		writer.Flush()

		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", recorder.Code)
		}
		if !recorder.Flushed {
			t.Fatal("underlying response was not flushed")
		}
	})

	t.Run("hijack", func(t *testing.T) {
		failures := failedStorageState()
		underlying := &trackingHijackResponseWriter{ResponseRecorder: httptest.NewRecorder()}
		writer := newStorageResponseWriter(underlying, failures)

		_, _, err := writer.Hijack()
		if !errors.Is(err, ErrStorageFailure) {
			t.Fatalf("Hijack error = %v, want ErrStorageFailure", err)
		}
		if underlying.hijacked {
			t.Fatal("underlying response was hijacked after fatal storage error")
		}
		if underlying.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", underlying.Code)
		}
	})
}

func TestOpenUserStoreFailsForSQLiteStartupErrors(t *testing.T) {
	t.Run("unavailable path", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(parent, []byte("file"), 0o600); err != nil {
			t.Fatalf("write parent file: %v", err)
		}
		if _, err := OpenUserStore(context.Background(), filepath.Join(parent, "users.db")); err == nil {
			t.Fatal("OpenUserStore returned nil error")
		}
	})

	t.Run("corrupt database", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "users.db")
		if err := os.WriteFile(path, []byte("not a sqlite database"), 0o600); err != nil {
			t.Fatalf("write corrupt database: %v", err)
		}
		if _, err := OpenUserStore(context.Background(), path); err == nil {
			t.Fatal("OpenUserStore returned nil error")
		}
	})

	t.Run("migration conflict", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "users.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("open setup database: %v", err)
		}
		if _, err = db.Exec(`CREATE VIEW users AS SELECT 'usr_1' AS id`); err != nil {
			t.Fatalf("create conflicting view: %v", err)
		}
		if err = db.Close(); err != nil {
			t.Fatalf("close setup database: %v", err)
		}
		if _, err = OpenUserStore(context.Background(), path); err == nil {
			t.Fatal("OpenUserStore returned nil error")
		}
	})

	t.Run("failed initial load", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "users.db")
		store, err := OpenUserStore(context.Background(), path)
		if err != nil {
			t.Fatalf("OpenUserStore setup returned error: %v", err)
		}
		if err = store.Close(); err != nil {
			t.Fatalf("close setup store: %v", err)
		}

		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatalf("reopen setup database: %v", err)
		}
		_, err = db.Exec(
			`INSERT INTO users (id, name, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			"usr_bad",
			"bad timestamp",
			1,
			"not-a-time",
			"not-a-time",
		)
		if err != nil {
			t.Fatalf("insert invalid persisted state: %v", err)
		}
		if err = db.Close(); err != nil {
			t.Fatalf("close setup database: %v", err)
		}

		if _, err = OpenUserStore(context.Background(), path); err == nil {
			t.Fatal("OpenUserStore returned nil error")
		}
	})
}

func TestRuntimeSQLiteReadFailureStopsProxyAndSignalsFatal(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	server := newStorageFailureTestServer(t, upstream.URL+"/backend-api/codex", upstream.URL+"/backend-api")
	if err := server.users.db.Close(); err != nil {
		t.Fatalf("close SQLite database: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/backend-api/files", nil)
	req.Header.Set("Authorization", "Bearer access-1")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body: %s", resp.Code, resp.Body.String())
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
	assertFatalStorageError(t, server.FatalErrors())
}

func TestRuntimeSQLiteWriteFailureSignalsFatal(t *testing.T) {
	server := newStorageFailureTestServer(t, "http://127.0.0.1:1/backend-api/codex", "http://127.0.0.1:1/backend-api")
	if err := server.users.db.Close(); err != nil {
		t.Fatalf("close SQLite database: %v", err)
	}

	_, err := server.users.CreateUser(context.Background(), CreateUserParams{Name: "Alice"})
	if !errors.Is(err, ErrStorageFailure) {
		t.Fatalf("CreateUser error = %v, want ErrStorageFailure", err)
	}
	disabled := false
	err = server.users.RecordUsage(context.Background(), UsageRecordParams{}, UsageConfig{Enabled: &disabled})
	if !errors.Is(err, ErrStorageFailure) {
		t.Fatalf("disabled RecordUsage error = %v, want ErrStorageFailure", err)
	}
	assertFatalStorageError(t, server.FatalErrors())
}

func TestRuntimeSessionAffinitySQLiteFailuresSignalFatal(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		server := newStorageFailureTestServer(t, "http://127.0.0.1:1/backend-api/codex", "http://127.0.0.1:1/backend-api")
		digests := digestSessionAffinitySignals("user:usr_1", []sessionAffinitySignal{
			{Kind: sessionAffinitySignalSessionID, Value: "session-1"},
		})
		if err := server.users.db.Close(); err != nil {
			t.Fatalf("close SQLite database: %v", err)
		}
		if _, _, err := server.users.LookupSessionAffinity(context.Background(), digests); !errors.Is(err, ErrStorageFailure) {
			t.Fatalf("LookupSessionAffinity error = %v, want ErrStorageFailure", err)
		}
		assertFatalStorageError(t, server.FatalErrors())
	})

	t.Run("write", func(t *testing.T) {
		server := newStorageFailureTestServer(t, "http://127.0.0.1:1/backend-api/codex", "http://127.0.0.1:1/backend-api")
		digests := digestSessionAffinitySignals("user:usr_1", []sessionAffinitySignal{
			{Kind: sessionAffinitySignalSessionID, Value: "session-1"},
		})
		if err := server.users.db.Close(); err != nil {
			t.Fatalf("close SQLite database: %v", err)
		}
		if _, err := server.users.BindSessionAffinity(context.Background(), digests, "account:acct_a"); !errors.Is(err, ErrStorageFailure) {
			t.Fatalf("BindSessionAffinity error = %v, want ErrStorageFailure", err)
		}
		assertFatalStorageError(t, server.FatalErrors())
	})
}

func TestRuntimeAuthHealthSQLiteFailuresSignalFatal(t *testing.T) {
	t.Run("read", func(t *testing.T) {
		server := newStorageFailureTestServer(t, "http://127.0.0.1:1/backend-api/codex", "http://127.0.0.1:1/backend-api")
		if err := server.users.db.Close(); err != nil {
			t.Fatalf("close SQLite database: %v", err)
		}
		if _, err := server.users.LoadAuthHealthStates(context.Background()); !errors.Is(err, ErrStorageFailure) {
			t.Fatalf("LoadAuthHealthStates error = %v, want ErrStorageFailure", err)
		}
		assertFatalStorageError(t, server.FatalErrors())
	})

	t.Run("write", func(t *testing.T) {
		server := newStorageFailureTestServer(t, "http://127.0.0.1:1/backend-api/codex", "http://127.0.0.1:1/backend-api")
		auths, err := server.auths.Store.Load(context.Background())
		if err != nil {
			t.Fatalf("load auths: %v", err)
		}
		if err = server.users.db.Close(); err != nil {
			t.Fatalf("close SQLite database: %v", err)
		}
		err = server.health.MarkUnavailable(context.Background(), auths[0], AuthHealthState{
			Kind:          AuthHealthQuota,
			Reason:        "quota",
			RetryAt:       time.Now().Add(time.Minute),
			Authoritative: true,
			StatusCode:    http.StatusTooManyRequests,
		})
		if !errors.Is(err, ErrStorageFailure) {
			t.Fatalf("MarkUnavailable error = %v, want ErrStorageFailure", err)
		}
		assertFatalStorageError(t, server.FatalErrors())
	})
}

func TestRuntimeModelCatalogHealthWriteFailureReturnsFatal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"models": []map[string]any{{
			"slug":         "runtime-model",
			"display_name": "Runtime Model",
		}}})
	}))
	defer upstream.Close()

	server := newStorageFailureTestServer(
		t,
		upstream.URL+"/backend-api/codex",
		upstream.URL+"/backend-api",
	)
	auths, err := server.auths.Store.Load(context.Background())
	if err != nil {
		t.Fatalf("load auths: %v", err)
	}
	err = server.health.MarkUnavailable(context.Background(), auths[0], AuthHealthState{
		Kind:       AuthHealthTransient,
		Reason:     "temporary",
		RetryAt:    time.Now().Add(time.Minute),
		StatusCode: http.StatusServiceUnavailable,
	})
	if err != nil {
		t.Fatalf("seed auth health state: %v", err)
	}
	if err = server.users.db.Close(); err != nil {
		t.Fatalf("close SQLite database: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.146.0", nil)
	req.Header.Set("Authorization", "Bearer access-1")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body: %s", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "runtime-model") {
		t.Fatalf("fatal response included successful model catalog: %s", resp.Body.String())
	}
	assertFatalStorageError(t, server.FatalErrors())
}

func TestExpectedStoreErrorsDoNotSignalFatal(t *testing.T) {
	server := newStorageFailureTestServer(t, "http://127.0.0.1:1/backend-api/codex", "http://127.0.0.1:1/backend-api")
	store := server.users

	if _, err := store.CreateUser(context.Background(), CreateUserParams{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("validation error = %v, want ErrInvalidInput", err)
	}
	if _, err := store.GetUser(context.Background(), "missing"); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("not found error = %v, want ErrUserNotFound", err)
	}
	if _, err := store.CreateUser(context.Background(), CreateUserParams{Name: "Alice"}); err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if _, err := store.CreateUser(context.Background(), CreateUserParams{Name: "alice"}); !errors.Is(err, ErrDuplicateUserName) {
		t.Fatalf("constraint error = %v, want ErrDuplicateUserName", err)
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ListUsers(canceledCtx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read error = %v, want context.Canceled", err)
	}
	if _, err := store.CreateUser(canceledCtx, CreateUserParams{Name: "Bob"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write error = %v, want context.Canceled", err)
	}

	select {
	case err := <-server.FatalErrors():
		t.Fatalf("unexpected fatal error: %v", err)
	default:
	}
}

func TestConcurrentSQLiteFailuresReportOnlyFirstFatalError(t *testing.T) {
	server := newStorageFailureTestServer(t, "http://127.0.0.1:1/backend-api/codex", "http://127.0.0.1:1/backend-api")
	if err := server.users.db.Close(); err != nil {
		t.Fatalf("close SQLite database: %v", err)
	}

	const reporters = 32
	errs := make(chan error, reporters)
	var wg sync.WaitGroup
	for i := 0; i < reporters; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if index%2 == 0 {
				_, err := server.users.ListUsers(context.Background(), nil)
				errs <- err
				return
			}
			_, err := server.users.CreateUser(context.Background(), CreateUserParams{Name: "user"})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)

	var firstMessage string
	for err := range errs {
		if !errors.Is(err, ErrStorageFailure) {
			t.Fatalf("runtime error = %v, want ErrStorageFailure", err)
		}
		if firstMessage == "" {
			firstMessage = err.Error()
		}
		if err.Error() != firstMessage {
			t.Fatalf("runtime error = %q, want first fatal error %q", err.Error(), firstMessage)
		}
	}
	assertFatalStorageError(t, server.FatalErrors())
	select {
	case err := <-server.FatalErrors():
		t.Fatalf("received repeated fatal error: %v", err)
	default:
	}
}

func TestServerShutdownClosesEstablishedWebSocket(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err = conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()

	server := newStorageFailureTestServer(t, upstream.URL+"/backend-api/codex", upstream.URL+"/backend-api")
	created, err := server.users.CreateUser(context.Background(), CreateUserParams{Name: "Alice"})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	proxy := httptest.NewServer(server)
	t.Cleanup(proxy.Close)

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/v1/responses"
	headers := http.Header{"Authorization": []string{"Bearer " + created.PlaintextAPIKey}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("dial proxy websocket: %v", err)
	}
	defer conn.Close()

	start := time.Now()
	server.Shutdown()
	if err = conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, _, err = conn.ReadMessage(); err == nil {
		t.Fatal("ReadMessage returned nil error after shutdown")
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("websocket shutdown took %s, want less than 1s", elapsed)
	}
	proxy.Close()
	select {
	case errFatal := <-server.FatalErrors():
		t.Fatalf("normal shutdown reported fatal storage error: %v", errFatal)
	default:
	}
}

func newStorageFailureTestServer(t *testing.T, codexBaseURL string, chatGPTBaseURL string) *Server {
	t.Helper()
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"expired": "2099-01-01T00:00:00Z"
	}`)
	server, err := NewHandler(context.Background(), &Config{
		AuthDir:        authDir,
		Database:       DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:   codexBaseURL,
		ChatGPTBaseURL: chatGPTBaseURL,
		RequestRetry:   1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	t.Cleanup(func() {
		if errClose := server.Close(); errClose != nil && !strings.Contains(errClose.Error(), "database is closed") {
			t.Fatalf("Close returned error: %v", errClose)
		}
	})
	return server
}

func assertFatalStorageError(t *testing.T, fatalErrors <-chan error) {
	t.Helper()
	select {
	case err := <-fatalErrors:
		if !errors.Is(err, ErrStorageFailure) {
			t.Fatalf("fatal error = %v, want ErrStorageFailure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fatal storage error")
	}
}

func failedStorageState() *storageFailureState {
	failures := newStorageFailureState(nil)
	failures.report(fmt.Errorf("%w: injected SQLite failure", ErrStorageFailure))
	return failures
}

type trackingHijackResponseWriter struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (w *trackingHijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.hijacked = true
	return nil, nil, nil
}

type blockingHeaderResponseWriter struct {
	*httptest.ResponseRecorder
	headerStarted chan struct{}
	allowHeader   chan struct{}
	headerOnce    sync.Once
}

func newBlockingHeaderResponseWriter() *blockingHeaderResponseWriter {
	return &blockingHeaderResponseWriter{
		ResponseRecorder: httptest.NewRecorder(),
		headerStarted:    make(chan struct{}),
		allowHeader:      make(chan struct{}),
	}
}

func (w *blockingHeaderResponseWriter) Header() http.Header {
	w.headerOnce.Do(func() {
		close(w.headerStarted)
		<-w.allowHeader
	})
	return w.ResponseRecorder.Header()
}
