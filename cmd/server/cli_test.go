package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

func TestParseCLILegacyServeFlags(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	parsed, err := parseCLI([]string{"--config", "config.yaml", "--local-model"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if parsed.context.Command() != "serve" {
		t.Fatalf("command = %s, want serve", parsed.context.Command())
	}
	if parsed.app.Serve.ConfigPath != "config.yaml" {
		t.Fatalf("configPath = %q, want config.yaml", parsed.app.Serve.ConfigPath)
	}
	if !parsed.app.Serve.LocalModel {
		t.Fatalf("localModel = false, want true")
	}
}

func TestParseCLIExplicitServe(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	parsed, err := parseCLI([]string{"serve", "--config", "config.yaml"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if parsed.context.Command() != "serve" || parsed.app.Serve.ConfigPath != "config.yaml" {
		t.Fatalf("parsed = %#v, want serve with config.yaml", parsed)
	}
}

func TestParseCLINoSubcommandSelectsServe(t *testing.T) {
	parsed, err := parseCLI(nil, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if parsed.context.Command() != "serve" {
		t.Fatalf("command = %s, want serve", parsed.context.Command())
	}
}

func TestParseCLIUsesDefaultConfigPath(t *testing.T) {
	original := DefaultConfigPath
	DefaultConfigPath = "/etc/codex-oauth-proxy/config.yaml"
	t.Cleanup(func() {
		DefaultConfigPath = original
	})

	parsed, err := parseCLI(nil, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if parsed.app.Serve.ConfigPath != DefaultConfigPath {
		t.Fatalf("configPath = %q, want %q", parsed.app.Serve.ConfigPath, DefaultConfigPath)
	}
}

func TestParseCLIAdminCommandForms(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "list users", args: []string{"admin", "users", "list"}, want: "admin users list"},
		{name: "create user", args: []string{"admin", "users", "create", "Alice"}, want: "admin users create <name>"},
		{name: "get user", args: []string{"admin", "users", "get", "usr_1"}, want: "admin users get <user_id>"},
		{name: "update user", args: []string{"admin", "users", "update", "usr_1", "--name", "Alice 2"}, want: "admin users update <user_id>"},
		{name: "enable user", args: []string{"admin", "users", "enable", "usr_1"}, want: "admin users enable <user_id>"},
		{name: "disable user", args: []string{"admin", "users", "disable", "usr_1"}, want: "admin users disable <user_id>"},
		{name: "reset key", args: []string{"admin", "users", "reset-key", "usr_1"}, want: "admin users reset-key <user_id>"},
		{name: "usage snapshot", args: []string{"admin", "usage", "snapshot", "--user-id", "usr_1"}, want: "admin usage snapshot"},
		{name: "usage timeseries", args: []string{"admin", "usage", "timeseries", "--window", "7d"}, want: "admin usage timeseries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := parseCLI(tt.args, io.Discard, io.Discard)
			if err != nil {
				t.Fatalf("parseCLI returned error: %v", err)
			}
			if parsed.context.Command() != tt.want {
				t.Fatalf("command = %q, want %q", parsed.context.Command(), tt.want)
			}
		})
	}
}

func TestParseCLIAdminFlagsAfterLeafCommand(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	parsed, err := parseCLI(
		[]string{"admin", "users", "list", "--url", "http://127.0.0.1:8318", "--json"},
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if parsed.context.Command() != "admin users list" {
		t.Fatalf("command = %s, want admin users list", parsed.context.Command())
	}
	if parsed.app.Admin.URL != "http://127.0.0.1:8318" || !parsed.app.Admin.JSON {
		t.Fatalf("admin = %#v, want custom URL and JSON", parsed.app.Admin)
	}
}

func TestParseCLIAdminFlagsBeforeResource(t *testing.T) {
	parsed, err := parseCLI(
		[]string{"admin", "--url", "http://127.0.0.1:8318", "--json", "users", "list"},
		io.Discard,
		io.Discard,
	)
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if parsed.context.Command() != "admin users list" {
		t.Fatalf("command = %s, want admin users list", parsed.context.Command())
	}
	if parsed.app.Admin.URL != "http://127.0.0.1:8318" || !parsed.app.Admin.JSON {
		t.Fatalf("admin = %#v, want custom URL and JSON", parsed.app.Admin)
	}
}

func TestRunAdminJSONDoesNotPrintVersionBanner(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"users":[]}`))
	}))
	defer server.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"admin", "users", "list", "--url", server.URL, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit = %d, want 0, stderr: %s", code, stderr.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("admin json output is not valid JSON: %q: %v", stdout.String(), err)
	}
	if _, ok := payload["users"]; !ok {
		t.Fatalf("payload = %#v, want users field", payload)
	}
}

func TestRunAdminUsersUpdateSupportsPositionalAndTrailingFlags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/v0/local-admin/users/usr_1" {
			t.Fatalf("request = %s %s, want PATCH /v0/local-admin/users/usr_1", r.Method, r.URL.Path)
		}
		var req userUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Name == nil || *req.Name != "Alice 2" {
			t.Fatalf("name = %#v, want Alice 2", req.Name)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"usr_1","name":"Alice 2","enabled":true},"api_key":null}`))
	}))
	defer server.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(context.Background(), []string{
		"admin", "users", "update", "usr_1",
		"--name", "Alice 2",
		"--url=" + server.URL,
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit = %d, want 0, stderr: %s", code, stderr.String())
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("stdout = %q, want valid JSON", stdout.String())
	}
}

func TestRunAdminUsageTimeseriesSupportsLeafFlags(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/local-admin/usage/timeseries" {
			t.Fatalf("request = %s %s, want GET /v0/local-admin/usage/timeseries", r.Method, r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("window") != "7d" || query.Get("step") != "1h" {
			t.Fatalf("query = %s, want window=7d step=1h", r.URL.RawQuery)
		}
		if got := strings.Join(query["group_by"], ","); got != "user,model" {
			t.Fatalf("group_by = %q, want user,model", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"window":"7d","step":"1h","group_by":["user","model"],"series":[]}`))
	}))
	defer server.Close()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(context.Background(), []string{
		"admin", "usage", "timeseries",
		"--window", "7d",
		"--step", "1h",
		"--group-by", "user,model",
		"--url", server.URL,
		"--json",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit = %d, want 0, stderr: %s", code, stderr.String())
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("stdout = %q, want valid JSON", stdout.String())
	}
}

func TestRunInvalidAdminCommandWritesOnlyStderr(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"admin", "unknown"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run exit = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "unexpected argument unknown") {
		t.Fatalf("stderr = %q, want unknown resource error", stderr.String())
	}
}

func TestRunHelpPrintsUsage(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exit = %d, want 0, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Usage: codex-oauth-proxy") {
		t.Fatalf("stdout = %q, want generated usage", stdout.String())
	}
	normalized := strings.Join(strings.Fields(stdout.String()), " ")
	if !strings.Contains(normalized, "Omit a command to run the proxy server.") {
		t.Fatalf("stdout = %q, want default command explanation", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestWaitForServerReturnsFatalStorageError(t *testing.T) {
	fatalErr := errors.New("injected SQLite failure")
	fatalErrors := make(chan error, 1)
	fatalErrors <- fmt.Errorf("%w: %w", codexonly.ErrStorageFailure, fatalErr)
	lifecycle := &testServerLifecycle{fatalErrors: fatalErrors}

	err := waitForServer(
		&commandRuntime{ctx: context.Background(), stdout: io.Discard},
		&http.Server{},
		lifecycle,
		make(chan error),
		make(chan os.Signal),
		time.Second,
	)
	if !errors.Is(err, fatalErr) {
		t.Fatalf("waitForServer error = %v, want injected fatal error", err)
	}
	if got := lifecycle.shutdowns.Load(); got != 1 {
		t.Fatalf("shutdown calls = %d, want 1", got)
	}
}

func TestShutdownHTTPServerIsBounded(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, errGet := http.Get("http://" + listener.Addr().String())
		if errGet == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request handler")
	}

	lifecycle := &testServerLifecycle{fatalErrors: make(chan error)}
	start := time.Now()
	err = shutdownHTTPServer(server, lifecycle, 20*time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdownHTTPServer error = %v, want context deadline exceeded", err)
	}
	if elapsed >= time.Second {
		t.Fatalf("shutdownHTTPServer took %s, want less than 1s", elapsed)
	}
	if got := lifecycle.shutdowns.Load(); got != 1 {
		t.Fatalf("shutdown calls = %d, want 1", got)
	}

	close(release)
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for client request")
	}
	select {
	case errServe := <-serveErr:
		if !errors.Is(errServe, http.ErrServerClosed) {
			t.Fatalf("Serve error = %v, want http.ErrServerClosed", errServe)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for HTTP server")
	}
}

type testServerLifecycle struct {
	fatalErrors <-chan error
	shutdowns   atomic.Int32
}

func (l *testServerLifecycle) FatalErrors() <-chan error {
	return l.fatalErrors
}

func (l *testServerLifecycle) Shutdown() {
	l.shutdowns.Add(1)
}
