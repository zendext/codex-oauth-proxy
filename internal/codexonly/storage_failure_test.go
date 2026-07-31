package codexonly

import (
	"context"
	"database/sql"
	"errors"
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
