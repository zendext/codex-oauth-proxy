# Local Admin CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a zero-config `admin` CLI that manages the running local proxy service through a loopback-only local admin route.

**Architecture:** Keep one `codex-oauth-proxy` binary. Server mode stays backward compatible, while `serve` becomes an explicit alias and `admin` commands call `http://127.0.0.1:8317/v0/local-admin` by default. The local admin route reuses existing management handlers but authorizes only loopback clients, keeping `/v0/management` protected by `admin-api-key`.

**Tech Stack:** Go standard library `flag`, `net/http`, `encoding/json`, `net/url`, `text/tabwriter`, existing `internal/codexonly` server and response structs.

---

## File Structure

- Modify `internal/codexonly/server.go`: add loopback-only local admin route handling and refactor management path dispatch so both `/v0/management` and `/v0/local-admin` reuse the same handler logic.
- Modify `internal/codexonly/server_test.go`: add local admin route tests and helper for setting `RemoteAddr`.
- Modify `cmd/server/main.go`: delegate argument handling to new CLI runner while preserving existing signal-based server shutdown.
- Create `cmd/server/cli.go`: parse top-level command shape and run `serve` or `admin`.
- Create `cmd/server/cli_test.go`: cover legacy server parsing, explicit `serve`, and `admin`.
- Create `cmd/server/admin_http.go`: small HTTP client for `/v0/local-admin` requests, JSON encode/decode, and API error reporting.
- Create `cmd/server/admin_http_test.go`: cover local admin URL construction and HTTP error handling.
- Create `cmd/server/admin_users.go`: implement `admin users` subcommands and table/JSON output.
- Create `cmd/server/admin_users_test.go`: cover user command requests and output.
- Create `cmd/server/admin_usage.go`: implement `admin usage` subcommands and table/JSON output.
- Create `cmd/server/admin_usage_test.go`: cover usage command query parameters and output.
- Modify `README.md`: document `serve`, `admin` commands, default URL, `--url`, and loopback-only behavior.

---

### Task 1: Local Admin Server Route

**Files:**
- Modify: `internal/codexonly/server.go`
- Modify: `internal/codexonly/server_test.go`

- [ ] **Step 1: Write failing local admin route tests**

Add these tests near the existing management API tests in `internal/codexonly/server_test.go`:

```go
func TestLocalAdminAPIBypassesAdminKeyForLoopback(t *testing.T) {
	handler := newUserManagementTestHandler(t, &Config{})

	resp := doJSONRequestFromRemote(t, handler, "127.0.0.1:43123", http.MethodPost, "/v0/local-admin/users", `{"name":"Alice"}`, "")
	if resp.Code != http.StatusCreated {
		t.Fatalf("local create status = %d, want 201, body: %s", resp.Code, resp.Body.String())
	}

	var created CreatedUserAPIKey
	decodeResponse(t, resp, &created)
	if created.User.Name != "Alice" {
		t.Fatalf("created user name = %q, want Alice", created.User.Name)
	}
	if created.PlaintextAPIKey == "" || !strings.HasPrefix(created.PlaintextAPIKey, "cop_") {
		t.Fatalf("plaintext API key = %q, want cop_ prefix", created.PlaintextAPIKey)
	}

	listResp := doJSONRequestFromRemote(t, handler, "[::1]:43123", http.MethodGet, "/v0/local-admin/users", "", "")
	if listResp.Code != http.StatusOK {
		t.Fatalf("local list status = %d, want 200, body: %s", listResp.Code, listResp.Body.String())
	}
}

func TestLocalAdminAPIRejectsNonLoopback(t *testing.T) {
	handler := newUserManagementTestHandler(t, &Config{})

	resp := doJSONRequestFromRemote(t, handler, "203.0.113.10:43123", http.MethodGet, "/v0/local-admin/users", "", "")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("remote local-admin status = %d, want 404, body: %s", resp.Code, resp.Body.String())
	}
}

func TestManagementAPIStillRequiresAdminAPIKeyWithLocalAdminEnabled(t *testing.T) {
	handler := newUserManagementTestHandler(t, &Config{})

	resp := doJSONRequestFromRemote(t, handler, "127.0.0.1:43123", http.MethodGet, "/v0/management/users", "", "")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("management status = %d, want 404, body: %s", resp.Code, resp.Body.String())
	}
}

func doJSONRequestFromRemote(t *testing.T, handler http.Handler, remoteAddr string, method string, path string, body string, token string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = remoteAddr
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}
```

- [ ] **Step 2: Run the focused server tests and verify failure**

Run:

```bash
go test -run 'TestLocalAdminAPI|TestManagementAPIStillRequiresAdminAPIKeyWithLocalAdminEnabled' ./internal/codexonly
```

Expected: the new local admin tests fail with `404` because `/v0/local-admin` is not wired.

- [ ] **Step 3: Add local admin routing and shared management dispatch**

In `internal/codexonly/server.go`, add `net` to the imports. In `serveHTTP`, add the local admin case before the `/v0/management` case:

```go
	case strings.HasPrefix(r.URL.Path, "/v0/local-admin/"):
		if !isLoopbackRemoteAddr(r.RemoteAddr) {
			s.debugf("local admin auth failed method=%s path=%s remote=%s reason=non_loopback", r.Method, r.URL.Path, r.RemoteAddr)
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		s.debugf("local admin auth ok method=%s path=%s remote=%s", r.Method, r.URL.Path, r.RemoteAddr)
		s.handleManagementPath(w, r, strings.TrimPrefix(r.URL.Path, "/v0/local-admin"))
```

Replace the current `handleManagement` body with a wrapper plus a shared path dispatcher:

```go
func (s *Server) handleManagement(w http.ResponseWriter, r *http.Request) {
	s.handleManagementPath(w, r, strings.TrimPrefix(r.URL.Path, "/v0/management"))
}

func (s *Server) handleManagementPath(w http.ResponseWriter, r *http.Request, path string) {
	switch {
	case path == "/usage/timeseries" && r.Method == http.MethodGet:
		timeseries, err := s.users.GetUsageTimeseries(r.Context(), usageTimeseriesParamsFromRequest(r), s.cfg.Usage)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, timeseries)
	case path == "/usage" && r.Method == http.MethodGet:
		filter := UsageSnapshotFilter{
			UserID:   strings.TrimSpace(r.URL.Query().Get("user_id")),
			APIKeyID: strings.TrimSpace(r.URL.Query().Get("api_key_id")),
		}
		usage, err := s.users.GetUsageSnapshot(r.Context(), filter, time.Now().UTC(), s.cfg.Usage)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"usage": usage})
	case path == "/users" && r.Method == http.MethodPost:
		var req createUserRequest
		if !decodeJSONRequest(w, r, &req) {
			return
		}
		created, err := s.users.CreateUser(r.Context(), CreateUserParams{Name: req.Name, Enabled: req.Enabled})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, created)
	case path == "/users" && r.Method == http.MethodGet:
		enabled, ok := enabledFilter(w, r)
		if !ok {
			return
		}
		users, err := s.users.ListUsers(r.Context(), enabled)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": users})
	case strings.HasPrefix(path, "/users/"):
		s.handleManagementUser(w, r, strings.TrimPrefix(path, "/users/"))
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}
```

Add the loopback helper near `authorizedAdmin`:

```go
func isLoopbackRemoteAddr(remoteAddr string) bool {
	host := strings.TrimSpace(remoteAddr)
	if host == "" {
		return false
	}
	if splitHost, _, err := net.SplitHostPort(host); err == nil {
		host = splitHost
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
```

- [ ] **Step 4: Run the focused server tests and verify pass**

Run:

```bash
go test -run 'TestLocalAdminAPI|TestManagementAPIStillRequiresAdminAPIKeyWithLocalAdminEnabled' ./internal/codexonly
```

Expected: all focused tests pass.

- [ ] **Step 5: Commit local admin route**

Run:

```bash
git add internal/codexonly/server.go internal/codexonly/server_test.go
git commit -m "feat: add loopback local admin route"
```

---

### Task 2: Top-Level CLI Parsing And Server Compatibility

**Files:**
- Modify: `cmd/server/main.go`
- Create: `cmd/server/cli.go`
- Create: `cmd/server/cli_test.go`

- [ ] **Step 1: Write failing parser tests**

Create `cmd/server/cli_test.go`:

```go
package main

import "testing"

func TestParseCLILegacyServeFlags(t *testing.T) {
	opts, err := parseCLI([]string{"--config", "config.yaml", "--local-model"})
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if opts.command != commandServe {
		t.Fatalf("command = %s, want %s", opts.command, commandServe)
	}
	if opts.configPath != "config.yaml" {
		t.Fatalf("configPath = %q, want config.yaml", opts.configPath)
	}
	if !opts.localModel {
		t.Fatalf("localModel = false, want true")
	}
}

func TestParseCLIExplicitServe(t *testing.T) {
	opts, err := parseCLI([]string{"serve", "--config", "config.yaml"})
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if opts.command != commandServe || opts.configPath != "config.yaml" {
		t.Fatalf("opts = %#v, want serve with config.yaml", opts)
	}
}

func TestParseCLIAdminKeepsRemainingArgs(t *testing.T) {
	opts, err := parseCLI([]string{"admin", "users", "list", "--url", "http://127.0.0.1:8318"})
	if err != nil {
		t.Fatalf("parseCLI returned error: %v", err)
	}
	if opts.command != commandAdmin {
		t.Fatalf("command = %s, want %s", opts.command, commandAdmin)
	}
	if got := len(opts.adminArgs); got != 4 {
		t.Fatalf("adminArgs length = %d, want 4: %#v", got, opts.adminArgs)
	}
}
```

- [ ] **Step 2: Run parser tests and verify failure**

Run:

```bash
go test -run TestParseCLI ./cmd/server
```

Expected: fail because `parseCLI`, `commandServe`, and `commandAdmin` do not exist.

- [ ] **Step 3: Implement top-level CLI parsing and runner**

Create `cmd/server/cli.go`:

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

type commandKind string

const (
	commandServe commandKind = "serve"
	commandAdmin commandKind = "admin"
)

type cliOptions struct {
	command    commandKind
	configPath string
	localModel bool
	adminArgs  []string
}

func parseCLI(args []string) (cliOptions, error) {
	if len(args) > 0 && args[0] == "admin" {
		return cliOptions{command: commandAdmin, adminArgs: append([]string(nil), args[1:]...)}, nil
	}
	if len(args) > 0 && args[0] == "serve" {
		opts, err := parseServeFlags(args[1:])
		if err != nil {
			return cliOptions{}, err
		}
		return opts, nil
	}
	return parseServeFlags(args)
}

func parseServeFlags(args []string) (cliOptions, error) {
	fs := flag.NewFlagSet("codex-oauth-proxy serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	opts := cliOptions{command: commandServe}
	fs.StringVar(&opts.configPath, "config", DefaultConfigPath, "Configuration file path")
	fs.BoolVar(&opts.localModel, "local-model", false, "Accepted for compatibility; codex-oauth-proxy uses embedded models")
	if err := fs.Parse(args); err != nil {
		return cliOptions{}, err
	}
	return opts, nil
}

func run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) int {
	fmt.Fprintf(stdout, "codex-oauth-proxy Version: %s, Commit: %s, BuiltAt: %s\n", Version, Commit, BuildDate)
	opts, err := parseCLI(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	switch opts.command {
	case commandAdmin:
		if err = runAdmin(ctx, opts.adminArgs, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		return 0
	default:
		if err = runServe(ctx, opts, stdout, stderr); err != nil {
			fmt.Fprintf(stderr, "%v\n", err)
			return 1
		}
		return 0
	}
}

func runServe(ctx context.Context, opts cliOptions, stdout io.Writer, stderr io.Writer) error {
	_ = opts.localModel
	_ = stderr
	configPath := opts.configPath
	if configPath == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get working directory: %w", err)
		}
		configPath = filepath.Join(wd, "config.yaml")
	}
	cfg, err := codexonly.LoadConfig(configPath)
	if err != nil {
		return err
	}
	handler, err := codexonly.NewHandler(ctx, cfg)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              codexonly.ListenAddr(cfg),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(stdout, "codex-oauth-proxy listening on %s\n", server.Addr)
		errCh <- server.ListenAndServe()
	}()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	select {
	case sig := <-sigCh:
		fmt.Fprintf(stdout, "received %s, shutting down\n", sig)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown failed: %w", err)
		}
	case err = <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server failed: %w", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown failed: %w", err)
		}
	}
	return nil
}
```

When adding this file, include `errors` in the import block because `runServe` uses `errors.Is`.

Replace `cmd/server/main.go` with a small process entrypoint:

```go
package main

import (
	"context"
	"os"
)

var (
	Version           = "dev"
	Commit            = "none"
	BuildDate         = "unknown"
	DefaultConfigPath = ""
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}
```

- [ ] **Step 4: Add temporary admin stub for parser compilation**

Create a temporary stub in `cmd/server/cli.go` below `runServe` if `runAdmin` is not yet implemented:

```go
func runAdmin(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	return fmt.Errorf("admin command requires local admin HTTP client: %v", args)
}
```

This stub will be replaced in Task 3.

- [ ] **Step 5: Run parser tests and verify pass**

Run:

```bash
go test -run TestParseCLI ./cmd/server
```

Expected: parser tests pass.

- [ ] **Step 6: Commit parser and serve compatibility**

Run:

```bash
git add cmd/server/main.go cmd/server/cli.go cmd/server/cli_test.go
git commit -m "feat(cli): add serve command parsing"
```

---

### Task 3: Admin HTTP Client And Global Admin Flags

**Files:**
- Modify: `cmd/server/cli.go`
- Create: `cmd/server/admin_http.go`
- Create: `cmd/server/admin_http_test.go`

- [ ] **Step 1: Write failing admin client tests**

Create `cmd/server/admin_http_test.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSplitAdminFlagsAllowsURLAfterLeafCommand(t *testing.T) {
	opts, rest, err := splitAdminFlags([]string{"users", "list", "--url", "http://127.0.0.1:8318", "--json"})
	if err != nil {
		t.Fatalf("splitAdminFlags returned error: %v", err)
	}
	if opts.baseURL != "http://127.0.0.1:8318" || !opts.json {
		t.Fatalf("opts = %#v, want custom URL and json", opts)
	}
	if strings.Join(rest, " ") != "users list" {
		t.Fatalf("rest = %q, want users list", strings.Join(rest, " "))
	}
}

func TestAdminClientUsesLocalAdminPrefix(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		if r.Header.Get("Authorization") != "" {
			t.Fatalf("Authorization header = %q, want empty", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"users": []any{}})
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL)
	if err != nil {
		t.Fatalf("newAdminClient returned error: %v", err)
	}
	var payload map[string]any
	if err = client.doJSON(context.Background(), http.MethodGet, "/users", nil, nil, &payload); err != nil {
		t.Fatalf("doJSON returned error: %v", err)
	}
	if gotPath != "/v0/local-admin/users" {
		t.Fatalf("path = %q, want /v0/local-admin/users", gotPath)
	}
}

func TestAdminClientReportsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL)
	if err != nil {
		t.Fatalf("newAdminClient returned error: %v", err)
	}
	err = client.doJSON(context.Background(), http.MethodGet, "/users", nil, nil, nil)
	if err == nil {
		t.Fatalf("doJSON returned nil, want error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error = %q, want status code", err.Error())
	}
}
```

- [ ] **Step 2: Run admin client tests and verify failure**

Run:

```bash
go test -run 'TestSplitAdminFlags|TestAdminClient' ./cmd/server
```

Expected: fail because admin flag and client helpers do not exist.

- [ ] **Step 3: Implement admin HTTP client**

Create `cmd/server/admin_http.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultAdminBaseURL = "http://127.0.0.1:8317"

type adminOptions struct {
	baseURL string
	json    bool
}

type adminClient struct {
	baseURL    *url.URL
	httpClient *http.Client
}

func splitAdminFlags(args []string) (adminOptions, []string, error) {
	opts := adminOptions{baseURL: defaultAdminBaseURL}
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			opts.json = true
		case arg == "--url":
			if i+1 >= len(args) {
				return adminOptions{}, nil, fmt.Errorf("--url requires a value")
			}
			i++
			opts.baseURL = args[i]
		case strings.HasPrefix(arg, "--url="):
			opts.baseURL = strings.TrimPrefix(arg, "--url=")
		default:
			rest = append(rest, arg)
		}
	}
	return opts, rest, nil
}

func newAdminClient(rawURL string) (*adminClient, error) {
	rawURL = strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if rawURL == "" {
		rawURL = defaultAdminBaseURL
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse admin url: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("admin url must include scheme and host")
	}
	return &adminClient{
		baseURL: parsed,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}, nil
}

func (c *adminClient) doJSON(ctx context.Context, method string, path string, query url.Values, body any, target any) error {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/v0/local-admin" + path
	endpoint.RawQuery = query.Encode()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", c.baseURL.String(), err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("admin request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if target == nil {
		return nil
	}
	if err = json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func writeJSONOutput(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
```

- [ ] **Step 4: Replace temporary admin stub with dispatcher skeleton**

In `cmd/server/cli.go`, replace the temporary `runAdmin` stub with:

```go
func runAdmin(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	opts, rest, err := splitAdminFlags(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("admin requires a resource: users or usage")
	}
	client, err := newAdminClient(opts.baseURL)
	if err != nil {
		return err
	}
	switch rest[0] {
	case "users":
		return runAdminUsers(ctx, client, opts, rest[1:], stdout)
	case "usage":
		return runAdminUsage(ctx, client, opts, rest[1:], stdout)
	default:
		return fmt.Errorf("unknown admin resource %q", rest[0])
	}
}
```

Add temporary stubs below it until later tasks replace them:

```go
func runAdminUsers(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	return fmt.Errorf("admin users command requires user command handlers: %v", args)
}

func runAdminUsage(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	return fmt.Errorf("admin usage command requires usage command handlers: %v", args)
}
```

- [ ] **Step 5: Run admin client tests and parser tests**

Run:

```bash
go test -run 'TestParseCLI|TestSplitAdminFlags|TestAdminClient' ./cmd/server
```

Expected: all focused tests pass.

- [ ] **Step 6: Commit admin client foundation**

Run:

```bash
git add cmd/server/cli.go cmd/server/admin_http.go cmd/server/admin_http_test.go
git commit -m "feat(cli): add local admin HTTP client"
```

---

### Task 4: Admin User Commands

**Files:**
- Modify: `cmd/server/cli.go`
- Create: `cmd/server/admin_users.go`
- Create: `cmd/server/admin_users_test.go`

- [ ] **Step 1: Write failing user command tests**

Create `cmd/server/admin_users_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

func TestAdminUsersCreatePrintsPlaintextKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/local-admin/users" {
			t.Fatalf("request = %s %s, want POST /v0/local-admin/users", r.Method, r.URL.Path)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req["name"] != "Alice" {
			t.Fatalf("name = %#v, want Alice", req["name"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(codexonly.CreatedUserAPIKey{
			User: codexonly.UserRecord{ID: "usr_1", Name: "Alice", Enabled: true},
			APIKey: codexonly.APIKeyRecord{ID: "key_1", UserID: "usr_1", MaskedKey: "cop_****abcd", Enabled: true},
			PlaintextAPIKey: "cop_plain",
		})
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL)
	if err != nil {
		t.Fatalf("newAdminClient returned error: %v", err)
	}
	var out bytes.Buffer
	if err = runAdminUsers(context.Background(), client, adminOptions{}, []string{"create", "Alice"}, &out); err != nil {
		t.Fatalf("runAdminUsers returned error: %v", err)
	}
	if !strings.Contains(out.String(), "cop_plain") {
		t.Fatalf("output = %q, want plaintext key", out.String())
	}
}

func TestAdminUsersListJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/local-admin/users" {
			t.Fatalf("request = %s %s, want GET /v0/local-admin/users", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"users":[{"user":{"id":"usr_1","name":"Alice","enabled":true},"api_key":{"id":"key_1","user_id":"usr_1","masked_key":"cop_****abcd","enabled":true}}]}`))
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL)
	if err != nil {
		t.Fatalf("newAdminClient returned error: %v", err)
	}
	var out bytes.Buffer
	if err = runAdminUsers(context.Background(), client, adminOptions{json: true}, []string{"list"}, &out); err != nil {
		t.Fatalf("runAdminUsers returned error: %v", err)
	}
	if !strings.Contains(out.String(), `"users"`) || !strings.Contains(out.String(), `"Alice"`) {
		t.Fatalf("json output = %q, want users payload", out.String())
	}
}

func TestAdminUsersDisableSendsPatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/v0/local-admin/users/usr_1" {
			t.Fatalf("request = %s %s, want PATCH /v0/local-admin/users/usr_1", r.Method, r.URL.Path)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req["enabled"] != false {
			t.Fatalf("enabled = %#v, want false", req["enabled"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user":{"id":"usr_1","name":"Alice","enabled":false},"api_key":{"id":"key_1","user_id":"usr_1","masked_key":"cop_****abcd","enabled":true}}`))
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL)
	if err != nil {
		t.Fatalf("newAdminClient returned error: %v", err)
	}
	var out bytes.Buffer
	if err = runAdminUsers(context.Background(), client, adminOptions{}, []string{"disable", "usr_1"}, &out); err != nil {
		t.Fatalf("runAdminUsers returned error: %v", err)
	}
	if !strings.Contains(out.String(), "false") {
		t.Fatalf("output = %q, want disabled state", out.String())
	}
}
```

- [ ] **Step 2: Run user command tests and verify failure**

Run:

```bash
go test -run TestAdminUsers ./cmd/server
```

Expected: fail because `runAdminUsers` is still a stub.

- [ ] **Step 3: Implement user commands and output**

Create `cmd/server/admin_users.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

type usersListResponse struct {
	Users []codexonly.UserWithAPIKey `json:"users"`
}

type userCreateRequest struct {
	Name string `json:"name"`
}

type userUpdateRequest struct {
	Name    *string `json:"name,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
}

func runAdminUsers(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("admin users requires a command")
	}
	switch args[0] {
	case "list":
		return runAdminUsersList(ctx, client, opts, args[1:], stdout)
	case "create":
		return runAdminUsersCreate(ctx, client, opts, args[1:], stdout)
	case "get":
		return runAdminUsersGet(ctx, client, opts, args[1:], stdout)
	case "update":
		return runAdminUsersUpdate(ctx, client, opts, args[1:], stdout)
	case "enable":
		return runAdminUsersSetEnabled(ctx, client, opts, args[1:], stdout, true)
	case "disable":
		return runAdminUsersSetEnabled(ctx, client, opts, args[1:], stdout, false)
	case "reset-key":
		return runAdminUsersResetKey(ctx, client, opts, args[1:], stdout)
	default:
		return fmt.Errorf("unknown admin users command %q", args[0])
	}
}

func runAdminUsersList(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("admin users list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	enabled := fs.String("enabled", "", "Filter by enabled state")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := url.Values{}
	if strings.TrimSpace(*enabled) != "" {
		query.Set("enabled", strings.TrimSpace(*enabled))
	}
	var payload usersListResponse
	if err := client.doJSON(ctx, http.MethodGet, "/users", query, nil, &payload); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, payload)
	}
	return writeUsersTable(stdout, payload.Users)
}

func runAdminUsersCreate(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: admin users create <name>")
	}
	var created codexonly.CreatedUserAPIKey
	if err := client.doJSON(ctx, http.MethodPost, "/users", nil, userCreateRequest{Name: args[0]}, &created); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, created)
	}
	fmt.Fprintf(stdout, "USER_ID\tNAME\tENABLED\tAPI_KEY_ID\tMASKED_KEY\tAPI_KEY_VALUE\n")
	fmt.Fprintf(stdout, "%s\t%s\t%t\t%s\t%s\t%s\n", created.User.ID, created.User.Name, created.User.Enabled, created.APIKey.ID, created.APIKey.MaskedKey, created.PlaintextAPIKey)
	return nil
}

func runAdminUsersGet(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: admin users get <user_id>")
	}
	var user codexonly.UserWithAPIKey
	if err := client.doJSON(ctx, http.MethodGet, "/users/"+url.PathEscape(args[0]), nil, nil, &user); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, user)
	}
	return writeUsersTable(stdout, []codexonly.UserWithAPIKey{user})
}

func runAdminUsersUpdate(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	name, rest, err := extractStringFlag(args, "--name")
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: admin users update <user_id> --name <name>")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("--name is required")
	}
	req := userUpdateRequest{Name: &name}
	var user codexonly.UserWithAPIKey
	if err := client.doJSON(ctx, http.MethodPatch, "/users/"+url.PathEscape(rest[0]), nil, req, &user); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, user)
	}
	return writeUsersTable(stdout, []codexonly.UserWithAPIKey{user})
}

func runAdminUsersSetEnabled(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer, enabled bool) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: admin users enable|disable <user_id>")
	}
	req := userUpdateRequest{Enabled: &enabled}
	var user codexonly.UserWithAPIKey
	if err := client.doJSON(ctx, http.MethodPatch, "/users/"+url.PathEscape(args[0]), nil, req, &user); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, user)
	}
	return writeUsersTable(stdout, []codexonly.UserWithAPIKey{user})
}

func runAdminUsersResetKey(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: admin users reset-key <user_id>")
	}
	var created codexonly.CreatedUserAPIKey
	if err := client.doJSON(ctx, http.MethodPost, "/users/"+url.PathEscape(args[0])+"/api-key/reset", nil, nil, &created); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, created)
	}
	fmt.Fprintf(stdout, "USER_ID\tNAME\tENABLED\tAPI_KEY_ID\tMASKED_KEY\tAPI_KEY_VALUE\n")
	fmt.Fprintf(stdout, "%s\t%s\t%t\t%s\t%s\t%s\n", created.User.ID, created.User.Name, created.User.Enabled, created.APIKey.ID, created.APIKey.MaskedKey, created.PlaintextAPIKey)
	return nil
}

func writeUsersTable(stdout io.Writer, users []codexonly.UserWithAPIKey) error {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tUSER_ID\tENABLED\tAPI_KEY\tMASKED_KEY\tKEY_ENABLED")
	for _, item := range users {
		keyID := ""
		masked := ""
		keyEnabled := ""
		if item.APIKey != nil {
			keyID = item.APIKey.ID
			masked = item.APIKey.MaskedKey
			keyEnabled = fmt.Sprintf("%t", item.APIKey.Enabled)
		}
		fmt.Fprintf(tw, "%s\t%s\t%t\t%s\t%s\t%s\n", item.User.Name, item.User.ID, item.User.Enabled, keyID, masked, keyEnabled)
	}
	return tw.Flush()
}

func extractStringFlag(args []string, name string) (string, []string, error) {
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == name:
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("%s requires a value", name)
			}
			i++
			return args[i], append(rest, args[i+1:]...), nil
		case strings.HasPrefix(arg, name+"="):
			return strings.TrimPrefix(arg, name+"="), append(rest, args[i+1:]...), nil
		default:
			rest = append(rest, arg)
		}
	}
	return "", rest, nil
}
```

Remove the temporary `runAdminUsers` stub from `cmd/server/cli.go`.

- [ ] **Step 4: Run user command tests and focused parser tests**

Run:

```bash
go test -run 'TestAdminUsers|TestParseCLI|TestSplitAdminFlags|TestAdminClient' ./cmd/server
```

Expected: all focused tests pass.

- [ ] **Step 5: Commit user commands**

Run:

```bash
git add cmd/server/cli.go cmd/server/admin_users.go cmd/server/admin_users_test.go
git commit -m "feat(cli): add admin user commands"
```

---

### Task 5: Admin Usage Commands

**Files:**
- Modify: `cmd/server/cli.go`
- Create: `cmd/server/admin_usage.go`
- Create: `cmd/server/admin_usage_test.go`

- [ ] **Step 1: Write failing usage command tests**

Create `cmd/server/admin_usage_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminUsageSnapshotTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/local-admin/usage" {
			t.Fatalf("request = %s %s, want GET /v0/local-admin/usage", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("user_id") != "usr_1" {
			t.Fatalf("user_id query = %q, want usr_1", r.URL.Query().Get("user_id"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":[{"user_id":"usr_1","name":"Alice","api_key_id":"key_1","key_hash":"hash","masked_key":"cop_****abcd","windows":{"5h":{"request_count":1,"total_tokens":20},"7d":{"request_count":2,"total_tokens":50}}}]}`))
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL)
	if err != nil {
		t.Fatalf("newAdminClient returned error: %v", err)
	}
	var out bytes.Buffer
	if err = runAdminUsage(context.Background(), client, adminOptions{}, []string{"snapshot", "--user-id", "usr_1"}, &out); err != nil {
		t.Fatalf("runAdminUsage returned error: %v", err)
	}
	if !strings.Contains(out.String(), "TOKENS_5H") || !strings.Contains(out.String(), "50") {
		t.Fatalf("output = %q, want snapshot table", out.String())
	}
}

func TestAdminUsageTimeseriesQueryAndJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/local-admin/usage/timeseries" {
			t.Fatalf("request = %s %s, want GET /v0/local-admin/usage/timeseries", r.Method, r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("window") != "7d" || query.Get("step") != "1h" || query.Get("fill") != "zero" {
			t.Fatalf("query = %s, want window=7d step=1h fill=zero", r.URL.RawQuery)
		}
		if got := strings.Join(query["group_by"], ","); got != "user,model" {
			t.Fatalf("group_by = %q, want user,model", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"window":"7d","step":"1h","group_by":["user","model"],"series":[{"bucket_start":"2026-06-29T00:00:00Z","user_id":"usr_1","name":"Alice","model":"gpt-5.3-codex","request_count":1,"total_tokens":20}]}`))
	}))
	defer server.Close()

	client, err := newAdminClient(server.URL)
	if err != nil {
		t.Fatalf("newAdminClient returned error: %v", err)
	}
	var out bytes.Buffer
	if err = runAdminUsage(context.Background(), client, adminOptions{json: true}, []string{"timeseries", "--window", "7d", "--step", "1h", "--group-by", "user,model", "--fill", "zero"}, &out); err != nil {
		t.Fatalf("runAdminUsage returned error: %v", err)
	}
	if !strings.Contains(out.String(), `"series"`) || !strings.Contains(out.String(), `"gpt-5.3-codex"`) {
		t.Fatalf("json output = %q, want timeseries payload", out.String())
	}
}
```

- [ ] **Step 2: Run usage command tests and verify failure**

Run:

```bash
go test -run TestAdminUsage ./cmd/server
```

Expected: fail because `runAdminUsage` is still a stub.

- [ ] **Step 3: Implement usage commands and output**

Create `cmd/server/admin_usage.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/zendext/codex-oauth-proxy/internal/codexonly"
)

type usageSnapshotResponse struct {
	Usage []codexonly.ManagementUsageEntry `json:"usage"`
}

func runAdminUsage(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("admin usage requires a command")
	}
	switch args[0] {
	case "snapshot":
		return runAdminUsageSnapshot(ctx, client, opts, args[1:], stdout)
	case "timeseries":
		return runAdminUsageTimeseries(ctx, client, opts, args[1:], stdout)
	default:
		return fmt.Errorf("unknown admin usage command %q", args[0])
	}
}

func runAdminUsageSnapshot(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("admin usage snapshot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	userID := fs.String("user-id", "", "Filter by user ID")
	apiKeyID := fs.String("api-key-id", "", "Filter by API key ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := url.Values{}
	if strings.TrimSpace(*userID) != "" {
		query.Set("user_id", strings.TrimSpace(*userID))
	}
	if strings.TrimSpace(*apiKeyID) != "" {
		query.Set("api_key_id", strings.TrimSpace(*apiKeyID))
	}
	var payload usageSnapshotResponse
	if err := client.doJSON(ctx, http.MethodGet, "/usage", query, nil, &payload); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, payload)
	}
	return writeUsageSnapshotTable(stdout, payload.Usage)
}

func runAdminUsageTimeseries(ctx context.Context, client *adminClient, opts adminOptions, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("admin usage timeseries", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	window := fs.String("window", "", "Usage window")
	step := fs.String("step", "", "Aggregation step")
	groupBy := fs.String("group-by", "", "Comma-separated dimensions")
	fill := fs.String("fill", "", "Fill mode")
	userID := fs.String("user-id", "", "Filter by user ID")
	apiKeyID := fs.String("api-key-id", "", "Filter by API key ID")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := url.Values{}
	addQueryValue(query, "window", *window)
	addQueryValue(query, "step", *step)
	addQueryValue(query, "fill", *fill)
	addQueryValue(query, "user_id", *userID)
	addQueryValue(query, "api_key_id", *apiKeyID)
	for _, group := range strings.Split(*groupBy, ",") {
		group = strings.TrimSpace(group)
		if group != "" {
			query.Add("group_by", group)
		}
	}
	var payload codexonly.UsageTimeseries
	if err := client.doJSON(ctx, http.MethodGet, "/usage/timeseries", query, nil, &payload); err != nil {
		return err
	}
	if opts.json {
		return writeJSONOutput(stdout, payload)
	}
	return writeUsageTimeseriesTable(stdout, payload.Series)
}

func addQueryValue(query url.Values, key string, value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		query.Set(key, value)
	}
}

func writeUsageSnapshotTable(stdout io.Writer, usage []codexonly.ManagementUsageEntry) error {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tUSER_ID\tAPI_KEY\tREQ_5H\tTOKENS_5H\tREQ_7D\tTOKENS_7D")
	for _, entry := range usage {
		fiveHour := entry.Windows["5h"]
		sevenDay := entry.Windows["7d"]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\n",
			entry.Name,
			entry.UserID,
			entry.APIKeyID,
			fiveHour.RequestCount,
			fiveHour.TotalTokens,
			sevenDay.RequestCount,
			sevenDay.TotalTokens,
		)
	}
	return tw.Flush()
}

func writeUsageTimeseriesTable(stdout io.Writer, series []codexonly.UsageTimeseriesPoint) error {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BUCKET_START\tUSER\tUSER_ID\tAPI_KEY\tMODEL\tREASONING\tTIER\tREQUESTS\tTOKENS")
	for _, point := range series {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\n",
			point.BucketStart.Format(time.RFC3339),
			point.Name,
			point.UserID,
			point.APIKeyID,
			point.Model,
			point.ReasoningEffort,
			point.ServiceTier,
			point.RequestCount,
			point.TotalTokens,
		)
	}
	return tw.Flush()
}
```

Remove the temporary `runAdminUsage` stub from `cmd/server/cli.go`.

- [ ] **Step 4: Run usage command tests and command package tests**

Run:

```bash
go test ./cmd/server
```

Expected: all `cmd/server` tests pass.

- [ ] **Step 5: Commit usage commands**

Run:

```bash
git add cmd/server/cli.go cmd/server/admin_usage.go cmd/server/admin_usage_test.go
git commit -m "feat(cli): add admin usage commands"
```

---

### Task 6: Documentation

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update README CLI docs**

In `README.md`, update the Run section to include:

````markdown
## Run

```bash
./codex-oauth-proxy --config config.yaml
./codex-oauth-proxy serve --config config.yaml
```

## Admin CLI

The same binary includes a local admin CLI. It calls the running proxy at
`http://127.0.0.1:8317` by default through a loopback-only local admin route and
does not require `--config` or the configured `admin-api-key`.

```bash
./codex-oauth-proxy admin users list
./codex-oauth-proxy admin users create alice
./codex-oauth-proxy admin users reset-key usr_xxx
./codex-oauth-proxy admin usage snapshot
./codex-oauth-proxy admin usage timeseries --window 7d --step 1h --group-by user
```

Use `--url` for a non-default local instance:

```bash
./codex-oauth-proxy admin users list --url http://127.0.0.1:8318
```

All admin commands support `--json` for scripting. User creation and key reset
print the plaintext user API key once.
````

- [ ] **Step 2: Run a Markdown sanity check**

Run:

```bash
rg -n "Admin CLI|local admin|--url" README.md
```

Expected: the new section is present and mentions the default URL, local admin route behavior, `--url`, and `--json`.

- [ ] **Step 3: Commit documentation**

Run:

```bash
git add README.md
git commit -m "docs: document local admin CLI"
```

---

### Task 7: Final Verification

**Files:**
- No planned source edits.

- [ ] **Step 1: Run gofmt**

Run:

```bash
gofmt -w cmd/server internal/codexonly
```

Expected: command exits with status 0.

- [ ] **Step 2: Run all tests**

Run:

```bash
go test ./...
```

Expected: all packages pass.

- [ ] **Step 3: Run required compile verification**

Run:

```bash
go build -o test-output ./cmd/server && rm test-output
```

Expected: command exits with status 0 and leaves no `test-output` file.

- [ ] **Step 4: Inspect final diff**

Run:

```bash
git status --short
git diff --stat HEAD
```

Expected: `git status --short` prints nothing and `git diff --stat HEAD` prints nothing if the implementation tasks were committed as written.
