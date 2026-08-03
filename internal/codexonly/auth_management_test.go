package codexonly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuthManagementStatusAdminAndLocalEquivalentAndRedacted(t *testing.T) {
	authDir := t.TempDir()
	identifiedPath := filepath.Join(authDir, "identified-secret-path.json")
	writeAuthFile(t, authDir, filepath.Base(identifiedPath), `{
		"type": "codex",
		"account_id": "acct_status",
		"email": "operator@example.com",
		"access_token": "secret-access-status",
		"refresh_token": "secret-refresh-status",
		"id_token": "secret-id-status",
		"expired": "2099-01-01T00:00:00Z",
		"last_refresh": "2026-07-30T12:00:00Z"
	}`)
	writeAuthFile(t, authDir, "duplicate.json", `{
		"type": "codex",
		"account_id": "acct_status",
		"email": "operator@example.com",
		"access_token": "secret-access-duplicate",
		"refresh_token": "secret-refresh-duplicate",
		"expired": "2099-01-01T00:00:00Z",
		"disabled": true
	}`)
	writeAuthFile(t, authDir, "unidentified-secret-path.json", `{
		"type": "codex",
		"email": "unknown@example.net",
		"access_token": "secret-access-unidentified",
		"refresh_token": "secret-refresh-unidentified",
		"expired": "2099-01-01T00:00:00Z"
	}`)
	server := newAuthManagementTestServer(t, authDir, nil)

	result, err := server.reconcileAuths(context.Background())
	if err != nil {
		t.Fatalf("reconcile auths: %v", err)
	}
	identified := authByAccountID(result.Auths, "acct_status")
	if identified == nil {
		t.Fatal("identified auth not found")
	}
	created, err := server.users.CreateUser(context.Background(), CreateUserParams{Name: "Alice"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	authorization := proxyAuthorization{Credential: &AuthenticatedAPIKey{User: created.User, APIKey: created.APIKey}}
	if _, err = server.selectProxyAuth(context.Background(), authorization, []sessionAffinitySignal{
		{Kind: sessionAffinitySignalSessionID, Value: "secret-status-session"},
	}); err != nil {
		t.Fatalf("create session binding: %v", err)
	}
	if err = server.health.MarkUnavailable(context.Background(), identified, AuthHealthState{
		Kind:          AuthHealthQuota,
		Reason:        "quota",
		RetryAt:       time.Now().Add(time.Hour),
		Authoritative: true,
		StatusCode:    http.StatusTooManyRequests,
		ErrorCode:     "rate_limit_exceeded",
	}); err != nil {
		t.Fatalf("seed auth health: %v", err)
	}
	server.models.Close()
	server.models = newRuntimeModelCatalog(context.Background(), func(_ context.Context, auth *Auth, _ string) (modelCatalogFetchResult, error) {
		if auth.AccountID == "acct_status" {
			return modelCatalogFetchResult{Models: []map[string]any{{"slug": "gpt-status"}}}, nil
		}
		return modelCatalogFetchResult{}, errors.New("no catalog")
	}, server.health.HealthyEpoch)
	if _, err = server.models.Catalog(context.Background(), configuredClientVersion(server.cfg), result.Active); err != nil {
		t.Fatalf("prime model catalog: %v", err)
	}

	admin := doJSONRequest(t, server, http.MethodGet, "/v0/management/auths", "", "admin-secret-value")
	local := doJSONRequestFromRemote(t, server, "127.0.0.1:43123", http.MethodGet, "/v0/local-admin/auths", "", "")
	if admin.Code != http.StatusOK || local.Code != http.StatusOK {
		t.Fatalf("status codes admin=%d local=%d, bodies admin=%s local=%s", admin.Code, local.Code, admin.Body.String(), local.Body.String())
	}
	if admin.Body.String() != local.Body.String() {
		t.Fatalf("admin/local status differ:\nadmin=%s\nlocal=%s", admin.Body.String(), local.Body.String())
	}
	if got := admin.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}

	var payload struct {
		Auths []struct {
			AccountID             string    `json:"account_id"`
			IdentityState         string    `json:"identity_state"`
			Manageable            bool      `json:"manageable"`
			Email                 string    `json:"email"`
			SourceFileCount       int       `json:"source_file_count"`
			Enabled               bool      `json:"enabled"`
			RuntimeState          string    `json:"runtime_state"`
			TokenExpiresAt        time.Time `json:"token_expires_at"`
			LastRefreshAt         time.Time `json:"last_refresh_at"`
			CooldownUntil         time.Time `json:"cooldown_until"`
			CooldownReason        string    `json:"cooldown_reason"`
			KnownSupportedModels  []string  `json:"known_supported_models"`
			SessionBindingCount   int64     `json:"session_binding_count"`
			ActiveConnectionCount int64     `json:"active_connection_count"`
			LastError             *struct {
				Code   string `json:"code"`
				Status int    `json:"status"`
			} `json:"last_error"`
		} `json:"auths"`
	}
	decodeResponse(t, admin, &payload)
	if len(payload.Auths) != 2 {
		t.Fatalf("auth count = %d, want 2: %s", len(payload.Auths), admin.Body.String())
	}
	var identifiedStatus, unidentifiedStatus *struct {
		AccountID             string    `json:"account_id"`
		IdentityState         string    `json:"identity_state"`
		Manageable            bool      `json:"manageable"`
		Email                 string    `json:"email"`
		SourceFileCount       int       `json:"source_file_count"`
		Enabled               bool      `json:"enabled"`
		RuntimeState          string    `json:"runtime_state"`
		TokenExpiresAt        time.Time `json:"token_expires_at"`
		LastRefreshAt         time.Time `json:"last_refresh_at"`
		CooldownUntil         time.Time `json:"cooldown_until"`
		CooldownReason        string    `json:"cooldown_reason"`
		KnownSupportedModels  []string  `json:"known_supported_models"`
		SessionBindingCount   int64     `json:"session_binding_count"`
		ActiveConnectionCount int64     `json:"active_connection_count"`
		LastError             *struct {
			Code   string `json:"code"`
			Status int    `json:"status"`
		} `json:"last_error"`
	}
	for index := range payload.Auths {
		entry := &payload.Auths[index]
		if entry.AccountID == "acct_status" {
			identifiedStatus = entry
		} else if entry.IdentityState == "unidentified" {
			unidentifiedStatus = entry
		}
	}
	if identifiedStatus == nil || unidentifiedStatus == nil {
		t.Fatalf("missing identified or unidentified status: %#v", payload.Auths)
	}
	if !identifiedStatus.Manageable || identifiedStatus.Email != "o***@example.com" ||
		identifiedStatus.SourceFileCount != 2 || !identifiedStatus.Enabled ||
		identifiedStatus.RuntimeState != "cooling" || identifiedStatus.CooldownReason != "quota" ||
		identifiedStatus.SessionBindingCount != 1 || identifiedStatus.ActiveConnectionCount != 0 ||
		identifiedStatus.LastError == nil || identifiedStatus.LastError.Code != "rate_limit_exceeded" ||
		identifiedStatus.LastError.Status != http.StatusTooManyRequests ||
		!slices.Equal(identifiedStatus.KnownSupportedModels, []string{"gpt-status"}) {
		t.Fatalf("identified status = %#v", identifiedStatus)
	}
	if identifiedStatus.TokenExpiresAt.IsZero() || identifiedStatus.LastRefreshAt.IsZero() ||
		identifiedStatus.CooldownUntil.IsZero() {
		t.Fatalf("identified timestamps = expiry %s refresh %s cooldown %s", identifiedStatus.TokenExpiresAt, identifiedStatus.LastRefreshAt, identifiedStatus.CooldownUntil)
	}
	if unidentifiedStatus.AccountID != "" || unidentifiedStatus.Manageable ||
		unidentifiedStatus.IdentityState != "unidentified" ||
		unidentifiedStatus.Email != "u***@example.net" {
		t.Fatalf("unidentified status = %#v", unidentifiedStatus)
	}

	for _, secret := range []string{
		"secret-access-status",
		"secret-refresh-status",
		"secret-id-status",
		"secret-access-duplicate",
		"secret-refresh-duplicate",
		"secret-access-unidentified",
		"secret-refresh-unidentified",
		identifiedPath,
		"identified-secret-path.json",
		"unidentified-secret-path.json",
		"secret-status-session",
		"admin-secret-value",
		"operator@example.com",
		"unknown@example.net",
	} {
		if strings.Contains(admin.Body.String(), secret) {
			t.Fatalf("status response leaked %q: %s", secret, admin.Body.String())
		}
	}

	unauthorized := doJSONRequest(t, server, http.MethodGet, "/v0/management/auths", "", "wrong-admin")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", unauthorized.Code)
	}
	if got := unauthorized.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("unauthorized Cache-Control = %q, want no-store", got)
	}
}

func TestAuthManagementForceRefreshConcurrentDisabledAndHealthSemantics(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "auth.json", `{
		"type": "codex",
		"account_id": "acct_refresh",
		"email": "refresh@example.com",
		"access_token": "old-access",
		"refresh_token": "old-refresh",
		"expired": "2099-01-01T00:00:00Z",
		"disabled": true
	}`)
	server := newAuthManagementTestServer(t, authDir, nil)
	auth := requireAuthByAccountID(t, server, "acct_refresh")
	if err := server.health.MarkUnavailable(context.Background(), auth, AuthHealthState{
		Kind:       AuthHealthInvalidGrant,
		Reason:     "invalid_grant",
		StatusCode: http.StatusBadRequest,
		ErrorCode:  "invalid_grant",
	}); err != nil {
		t.Fatalf("seed invalid grant: %v", err)
	}

	var refreshCalls atomic.Int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var releaseRefreshOnce sync.Once
	releaseForceRefresh := func() {
		releaseRefreshOnce.Do(func() {
			close(releaseRefresh)
		})
	}
	t.Cleanup(releaseForceRefresh)
	server.auths.Refresher = RefresherFunc(func(ctx context.Context, candidate *Auth) error {
		if _, ok := ctx.Deadline(); !ok {
			return errors.New("force refresh context has no deadline")
		}
		if refreshCalls.Add(1) == 1 {
			close(refreshStarted)
		}
		<-releaseRefresh
		candidate.AccessToken = "new-access"
		candidate.RefreshToken = "new-refresh"
		candidate.ExpiresAt = time.Now().Add(time.Hour)
		return candidate.Save()
	})

	const callers = 16
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			responses <- doJSONRequest(t, server, http.MethodPost, "/v0/management/auths/refresh", `{"account_id":"acct_refresh"}`, "admin-secret-value")
		}()
	}
	close(start)
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for force refresh")
	}
	time.Sleep(25 * time.Millisecond)
	releaseForceRefresh()
	wg.Wait()
	close(responses)
	for resp := range responses {
		if resp.Code != http.StatusOK {
			t.Fatalf("force refresh status = %d, want 200, body: %s", resp.Code, resp.Body.String())
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	refreshed := requireAuthByAccountID(t, server, "acct_refresh")
	if !refreshed.Disabled || refreshed.AccessToken != "new-access" || refreshed.RefreshToken != "new-refresh" {
		t.Fatalf("refreshed auth = disabled %t access %q refresh %q", refreshed.Disabled, refreshed.AccessToken, refreshed.RefreshToken)
	}
	if _, ok := server.health.State(refreshed.ID, ""); ok {
		t.Fatal("force refresh did not clear credential-related health")
	}

	if err := server.health.MarkUnavailable(context.Background(), refreshed, AuthHealthState{
		Kind:          AuthHealthQuota,
		Reason:        "quota",
		RetryAt:       time.Now().Add(time.Hour),
		Authoritative: true,
		StatusCode:    http.StatusTooManyRequests,
	}); err != nil {
		t.Fatalf("seed quota: %v", err)
	}
	server.auths.Refresher = RefresherFunc(func(_ context.Context, candidate *Auth) error {
		refreshCalls.Add(1)
		candidate.AccessToken = "newer-access"
		candidate.ExpiresAt = time.Now().Add(2 * time.Hour)
		return candidate.Save()
	})
	resp := doJSONRequest(t, server, http.MethodPost, "/v0/management/auths/refresh", `{"account_id":"acct_refresh"}`, "admin-secret-value")
	if resp.Code != http.StatusOK {
		t.Fatalf("quota-preserving refresh status = %d, body: %s", resp.Code, resp.Body.String())
	}
	state, ok := server.health.State(refreshed.ID, "")
	if !ok || state.Kind != AuthHealthQuota {
		t.Fatalf("health after successful refresh = %#v, %t, want quota", state, ok)
	}
}

func TestAuthManagementForceRefreshFailureIsSanitizedAndUnidentifiedIsReadOnly(t *testing.T) {
	authDir := t.TempDir()
	fullPath := filepath.Join(authDir, "auth-secret-path.json")
	writeAuthFile(t, authDir, filepath.Base(fullPath), `{
		"type": "codex",
		"account_id": "acct_failure",
		"access_token": "secret-old-access",
		"refresh_token": "secret-old-refresh",
		"expired": "2099-01-01T00:00:00Z"
	}`)
	writeAuthFile(t, authDir, "unidentified.json", `{
		"type": "codex",
		"access_token": "secret-unidentified-access",
		"refresh_token": "secret-unidentified-refresh"
	}`)
	server := newAuthManagementTestServer(t, authDir, nil)
	var refreshCalls atomic.Int32
	server.auths.Refresher = RefresherFunc(func(context.Context, *Auth) error {
		refreshCalls.Add(1)
		return newOAuthRefreshError("secret upstream body", http.StatusBadRequest, "invalid_grant", errors.New("secret cause"))
	})
	var logs bytes.Buffer
	restore := captureStandardLogger(t, &logs)
	defer restore()

	resp := doJSONRequest(t, server, http.MethodPost, "/v0/management/auths/refresh", `{"account_id":"acct_failure"}`, "admin-secret-value")
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("refresh failure status = %d, want 502, body: %s", resp.Code, resp.Body.String())
	}
	state, ok := server.health.State("account:acct_failure", "")
	if !ok || state.Kind != AuthHealthInvalidGrant || state.ErrorCode != "invalid_grant" {
		t.Fatalf("health after refresh failure = %#v, %t", state, ok)
	}

	unidentified := doJSONRequest(t, server, http.MethodPost, "/v0/management/auths/disable", `{"account_id":""}`, "admin-secret-value")
	if unidentified.Code != http.StatusBadRequest {
		t.Fatalf("unidentified mutation status = %d, want 400, body: %s", unidentified.Code, unidentified.Body.String())
	}
	pathTarget := doJSONRequest(t, server, http.MethodPost, "/v0/management/auths/disable", `{"account_id":"path:unidentified.json"}`, "admin-secret-value")
	if pathTarget.Code != http.StatusConflict {
		t.Fatalf("path-identified mutation status = %d, want 409, body: %s", pathTarget.Code, pathTarget.Body.String())
	}
	pathRefresh := doJSONRequest(t, server, http.MethodPost, "/v0/management/auths/refresh", `{"account_id":"path:unidentified.json"}`, "admin-secret-value")
	if pathRefresh.Code != http.StatusConflict {
		t.Fatalf("path-identified refresh status = %d, want 409, body: %s", pathRefresh.Code, pathRefresh.Body.String())
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want only the identified auth refresh", got)
	}
	for _, output := range []string{resp.Body.String(), logs.String()} {
		for _, secret := range []string{
			"secret upstream body",
			"secret cause",
			"secret-old-access",
			"secret-old-refresh",
			"secret-unidentified-access",
			"secret-unidentified-refresh",
			fullPath,
			"auth-secret-path.json",
			"admin-secret-value",
		} {
			if strings.Contains(output, secret) {
				t.Fatalf("management output leaked %q:\n%s", secret, output)
			}
		}
	}
}

func TestAuthManagementForceRefreshMissingCredentialsFailsWithoutSecrets(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "auth.json", `{
		"type": "codex",
		"account_id": "acct_missing_refresh",
		"access_token": "secret-access-without-refresh",
		"expired": "2099-01-01T00:00:00Z"
	}`)
	server := newAuthManagementTestServer(t, authDir, nil)

	resp := doJSONRequest(t, server, http.MethodPost, "/v0/management/auths/refresh", `{"account_id":"acct_missing_refresh"}`, "admin-secret-value")
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("missing refresh status = %d, want 502, body: %s", resp.Code, resp.Body.String())
	}
	state, ok := server.health.State("account:acct_missing_refresh", "")
	if !ok || state.Kind != AuthHealthCredentialInvalid {
		t.Fatalf("health after missing refresh = %#v, %t", state, ok)
	}
	for _, secret := range []string{"secret-access-without-refresh", "admin-secret-value"} {
		if strings.Contains(resp.Body.String(), secret) {
			t.Fatalf("missing refresh response leaked %q: %s", secret, resp.Body.String())
		}
	}
}

func TestAuthManagementActionsAdminAndLocalEquivalent(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		body  string
		setup func(*testing.T, *Server)
	}{
		{
			name: "force refresh",
			path: "/auths/refresh",
			body: `{"account_id":"acct_equivalent"}`,
			setup: func(t *testing.T, server *Server) {
				t.Helper()
				server.auths.Refresher = RefresherFunc(func(_ context.Context, auth *Auth) error {
					auth.AccessToken = "refreshed-access"
					auth.ExpiresAt = time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
					return auth.Save()
				})
			},
		},
		{
			name: "enable",
			path: "/auths/enable",
			body: `{"account_id":"acct_equivalent"}`,
		},
		{
			name: "disable",
			path: "/auths/disable",
			body: `{"account_id":"acct_equivalent"}`,
		},
		{
			name: "clear cooldown",
			path: "/auths/cooldown/clear",
			body: `{"account_id":"acct_equivalent"}`,
			setup: func(t *testing.T, server *Server) {
				t.Helper()
				auth := requireAuthByAccountID(t, server, "acct_equivalent")
				server.health.now = func() time.Time {
					return time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
				}
				if err := server.health.MarkUnavailable(context.Background(), auth, AuthHealthState{
					Kind:       AuthHealthTransient,
					Reason:     "network",
					RetryAt:    time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC),
					StatusCode: http.StatusServiceUnavailable,
				}); err != nil {
					t.Fatalf("seed cooldown: %v", err)
				}
			},
		},
		{
			name: "clear bindings",
			path: "/session-bindings/clear",
			body: `{"account_id":"acct_equivalent"}`,
			setup: func(t *testing.T, server *Server) {
				t.Helper()
				auth := requireAuthByAccountID(t, server, "acct_equivalent")
				digests := digestSessionAffinitySignals("user:usr_equivalent", []sessionAffinitySignal{
					{Kind: sessionAffinitySignalSessionID, Value: "equivalent-session"},
				})
				if _, err := server.users.BindScopedSessionAffinity(
					context.Background(),
					"user:usr_equivalent",
					digests,
					auth.ID,
				); err != nil {
					t.Fatalf("seed binding: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			newServer := func() *Server {
				authDir := t.TempDir()
				writeAuthFile(t, authDir, "auth.json", `{
					"type": "codex",
					"account_id": "acct_equivalent",
					"access_token": "access-equivalent",
					"refresh_token": "refresh-equivalent",
					"expired": "2099-01-01T00:00:00Z",
					"disabled": true
				}`)
				server := newAuthManagementTestServer(t, authDir, nil)
				if test.setup != nil {
					test.setup(t, server)
				}
				return server
			}
			adminServer := newServer()
			localServer := newServer()
			admin := doJSONRequest(t, adminServer, http.MethodPost, "/v0/management"+test.path, test.body, "admin-secret-value")
			local := doJSONRequestFromRemote(t, localServer, "127.0.0.1:43123", http.MethodPost, "/v0/local-admin"+test.path, test.body, "")
			if admin.Code != http.StatusOK || local.Code != http.StatusOK {
				t.Fatalf("action statuses admin=%d local=%d bodies admin=%s local=%s", admin.Code, local.Code, admin.Body.String(), local.Body.String())
			}
			if admin.Body.String() != local.Body.String() {
				t.Fatalf("admin/local action responses differ:\nadmin=%s\nlocal=%s", admin.Body.String(), local.Body.String())
			}
			if admin.Header().Get("Cache-Control") != "no-store" || local.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("action Cache-Control admin=%q local=%q", admin.Header().Get("Cache-Control"), local.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestAuthManagementDisableDuplicateFilesPreservesActiveRequestAndBindings(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "a.json", flatAuthJSON("acct_live", "access-live", false))
	writeAuthFile(t, authDir, "b.json", flatAuthJSON("acct_live", "access-live-duplicate", true))
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	var releaseRequestOnce sync.Once
	releaseActiveRequest := func() {
		releaseRequestOnce.Do(func() {
			close(releaseRequest)
		})
	}
	t.Cleanup(releaseActiveRequest)
	upstreamCalls := atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		close(requestStarted)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-releaseRequest
		_, _ = io.WriteString(w, "data: done\n\n")
	}))
	defer upstream.Close()
	server := newAuthManagementTestServer(t, authDir, func(cfg *Config) {
		cfg.CodexBaseURL = upstream.URL + "/backend-api/codex"
		cfg.ChatGPTBaseURL = upstream.URL + "/backend-api"
	})
	created, err := server.users.CreateUser(context.Background(), CreateUserParams{Name: "Alice"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	proxyDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/backend-api/wham/usage", nil)
		req.Header.Set("Authorization", "Bearer "+created.PlaintextAPIKey)
		req.Header.Set("Session-Id", "secret-live-session")
		resp := httptest.NewRecorder()
		server.ServeHTTP(resp, req)
		proxyDone <- resp
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active request")
	}

	before := doJSONRequest(t, server, http.MethodGet, "/v0/management/auths", "", "admin-secret-value")
	if !strings.Contains(before.Body.String(), `"active_connection_count":1`) {
		t.Fatalf("status before disable lacks active connection: %s", before.Body.String())
	}
	disable := doJSONRequest(t, server, http.MethodPost, "/v0/management/auths/disable", `{"account_id":"acct_live"}`, "admin-secret-value")
	if disable.Code != http.StatusOK {
		t.Fatalf("disable status = %d, body: %s", disable.Code, disable.Body.String())
	}
	for _, name := range []string{"a.json", "b.json"} {
		meta := readAuthJSON(t, filepath.Join(authDir, name))
		if meta["disabled"] != true {
			t.Fatalf("%s disabled = %#v, want true", name, meta["disabled"])
		}
	}
	select {
	case resp := <-proxyDone:
		t.Fatalf("active request ended during disable with status %d body %s", resp.Code, resp.Body.String())
	default:
	}
	var bindings int
	if err = server.users.db.QueryRow(`SELECT COUNT(DISTINCT binding_digest) FROM session_affinity_bindings`).Scan(&bindings); err != nil {
		t.Fatalf("count bindings after disable: %v", err)
	}
	if bindings != 1 {
		t.Fatalf("binding count after disable = %d, want 1", bindings)
	}

	newReq := httptest.NewRequest(http.MethodGet, "/backend-api/wham/usage", nil)
	newReq.Header.Set("Authorization", "Bearer "+created.PlaintextAPIKey)
	newResp := httptest.NewRecorder()
	server.ServeHTTP(newResp, newReq)
	if newResp.Code != http.StatusServiceUnavailable {
		t.Fatalf("new request after disable status = %d, want 503, body: %s", newResp.Code, newResp.Body.String())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls after disable = %d, want 1", got)
	}

	releaseActiveRequest()
	select {
	case resp := <-proxyDone:
		if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "data: done") {
			t.Fatalf("active request result = status %d body %s", resp.Code, resp.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active request completion")
	}
	after := doJSONRequest(t, server, http.MethodGet, "/v0/management/auths", "", "admin-secret-value")
	if !strings.Contains(after.Body.String(), `"active_connection_count":0`) {
		t.Fatalf("status after request lacks zero active count: %s", after.Body.String())
	}
}

func TestAuthManagementEnableDoesNotRefreshClearCooldownOrBindings(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "a.json", flatAuthJSON("acct_enable", "access-enable", true))
	writeAuthFile(t, authDir, "b.json", flatAuthJSON("acct_enable", "access-enable-duplicate", true))
	server := newAuthManagementTestServer(t, authDir, nil)
	auth := requireAuthByAccountID(t, server, "acct_enable")
	if err := server.health.MarkUnavailable(context.Background(), auth, AuthHealthState{
		Kind:          AuthHealthQuota,
		Reason:        "quota",
		RetryAt:       time.Now().Add(time.Hour),
		Authoritative: true,
		StatusCode:    http.StatusTooManyRequests,
	}); err != nil {
		t.Fatalf("seed quota: %v", err)
	}
	created, err := server.users.CreateUser(context.Background(), CreateUserParams{Name: "Alice"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	digests := digestSessionAffinitySignals("user:"+created.User.ID, []sessionAffinitySignal{
		{Kind: sessionAffinitySignalSessionID, Value: "enable-session"},
	})
	if _, err = server.users.BindSessionAffinity(context.Background(), digests, auth.ID); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	var refreshCalls atomic.Int32
	server.auths.Refresher = RefresherFunc(func(context.Context, *Auth) error {
		refreshCalls.Add(1)
		return nil
	})

	resp := doJSONRequest(t, server, http.MethodPost, "/v0/management/auths/enable", `{"account_id":"acct_enable"}`, "admin-secret-value")
	if resp.Code != http.StatusOK {
		t.Fatalf("enable status = %d, body: %s", resp.Code, resp.Body.String())
	}
	if got := refreshCalls.Load(); got != 0 {
		t.Fatalf("refresh calls = %d, want 0", got)
	}
	for _, name := range []string{"a.json", "b.json"} {
		meta := readAuthJSON(t, filepath.Join(authDir, name))
		if meta["disabled"] != false {
			t.Fatalf("%s disabled = %#v, want false", name, meta["disabled"])
		}
	}
	state, ok := server.health.State(auth.ID, "")
	if !ok || state.Kind != AuthHealthQuota {
		t.Fatalf("health after enable = %#v, %t, want quota", state, ok)
	}
	binding, found, err := server.users.LookupSessionAffinity(context.Background(), digests)
	if err != nil || !found || binding.AuthID != auth.ID {
		t.Fatalf("binding after enable = %#v found=%t error=%v", binding, found, err)
	}
}

func TestAuthManagementEnableValidationFailureLeavesDuplicateFilesUnchanged(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "a.json", flatAuthJSON("acct_atomic", "access-a", true))
	writeAuthFile(t, authDir, "b.json", flatAuthJSON("acct_atomic", "access-b", true))
	server := newAuthManagementTestServer(t, authDir, nil)
	beforeA, err := os.ReadFile(filepath.Join(authDir, "a.json"))
	if err != nil {
		t.Fatalf("read a before: %v", err)
	}
	beforeB, err := os.ReadFile(filepath.Join(authDir, "b.json"))
	if err != nil {
		t.Fatalf("read b before: %v", err)
	}
	if err = os.WriteFile(filepath.Join(authDir, "b.json"), []byte(`{"type":"codex","access_token":`), 0o600); err != nil {
		t.Fatalf("corrupt duplicate: %v", err)
	}

	resp := doJSONRequest(t, server, http.MethodPost, "/v0/management/auths/enable", `{"account_id":"acct_atomic"}`, "admin-secret-value")
	if resp.Code != http.StatusConflict {
		t.Fatalf("enable validation status = %d, want 409, body: %s", resp.Code, resp.Body.String())
	}
	afterA, err := os.ReadFile(filepath.Join(authDir, "a.json"))
	if err != nil {
		t.Fatalf("read a after: %v", err)
	}
	if !bytes.Equal(afterA, beforeA) {
		t.Fatalf("first duplicate changed after validation failure:\nbefore=%s\nafter=%s", beforeA, afterA)
	}
	afterB, err := os.ReadFile(filepath.Join(authDir, "b.json"))
	if err != nil {
		t.Fatalf("read b after: %v", err)
	}
	if bytes.Equal(afterB, beforeB) {
		t.Fatal("test setup did not corrupt second duplicate")
	}
}

func TestAuthManagementDuplicateFileSaveFailureRollsBack(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "a.json", flatAuthJSON("acct_rollback", "access-a", true))
	writeAuthFile(t, authDir, "b.json", flatAuthJSON("acct_rollback", "access-b", true))
	server := newAuthManagementTestServer(t, authDir, nil)
	var failed atomic.Bool
	server.auths.Store.renameFile = func(oldPath string, newPath string) error {
		if strings.HasSuffix(newPath, "b.json") && failed.CompareAndSwap(false, true) {
			return errors.New("injected rename failure")
		}
		return os.Rename(oldPath, newPath)
	}

	resp := doJSONRequest(t, server, http.MethodPost, "/v0/management/auths/enable", `{"account_id":"acct_rollback"}`, "admin-secret-value")
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("enable save failure status = %d, want 500, body: %s", resp.Code, resp.Body.String())
	}
	for _, name := range []string{"a.json", "b.json"} {
		meta := readAuthJSON(t, filepath.Join(authDir, name))
		if meta["disabled"] != true {
			t.Fatalf("%s disabled = %#v, want rollback to true", name, meta["disabled"])
		}
	}
}

func TestAuthManagementSerializesRefreshAndDisablePersistence(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "a.json", flatAuthJSON("acct_serial", "access-a", false))
	writeAuthFile(t, authDir, "b.json", flatAuthJSON("acct_serial", "access-b", false))
	server := newAuthManagementTestServer(t, authDir, nil)
	auth := requireAuthByAccountID(t, server, "acct_serial")

	refreshStarted := make(chan struct{})
	allowRefresh := make(chan struct{})
	var allowRefreshOnce sync.Once
	releaseRefresh := func() {
		allowRefreshOnce.Do(func() {
			close(allowRefresh)
		})
	}
	t.Cleanup(releaseRefresh)
	server.auths.Refresher = RefresherFunc(func(_ context.Context, candidate *Auth) error {
		close(refreshStarted)
		<-allowRefresh
		candidate.AccessToken = "rotated-access"
		candidate.RefreshToken = "rotated-refresh"
		candidate.ExpiresAt = time.Now().Add(time.Hour)
		return candidate.Save()
	})

	disableSaveStarted := make(chan struct{})
	var disableSaveOnce sync.Once
	server.auths.Store.renameFile = func(oldPath string, newPath string) error {
		disableSaveOnce.Do(func() {
			close(disableSaveStarted)
		})
		return os.Rename(oldPath, newPath)
	}

	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- server.auths.ForceRefresh(context.Background(), auth)
	}()
	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for refresh")
	}

	disableDone := make(chan error, 1)
	go func() {
		_, err := server.auths.Store.SetAccountDisabled(context.Background(), "acct_serial", true)
		disableDone <- err
	}()
	select {
	case <-disableSaveStarted:
		t.Fatal("disable persistence started while refresh still owned the auth-file boundary")
	case <-time.After(50 * time.Millisecond):
	}

	releaseRefresh()
	if err := <-refreshDone; err != nil {
		t.Fatalf("ForceRefresh returned error: %v", err)
	}
	if err := <-disableDone; err != nil {
		t.Fatalf("SetAccountDisabled returned error: %v", err)
	}

	first := readAuthJSON(t, filepath.Join(authDir, "a.json"))
	if first["access_token"] != "rotated-access" || first["refresh_token"] != "rotated-refresh" {
		t.Fatalf("refreshed tokens were overwritten: access=%#v refresh=%#v", first["access_token"], first["refresh_token"])
	}
	for _, name := range []string{"a.json", "b.json"} {
		meta := readAuthJSON(t, filepath.Join(authDir, name))
		if meta["disabled"] != true {
			t.Fatalf("%s disabled = %#v, want true", name, meta["disabled"])
		}
	}
}

func TestFileAuthStoreFailedMultiFileEnableHidesIntermediateState(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "a.json", flatAuthJSON("acct_visibility", "access-a", true))
	writeAuthFile(t, authDir, "b.json", flatAuthJSON("acct_visibility", "access-b", true))
	store := NewFileAuthStore(authDir)
	if _, err := store.Reconcile(context.Background()); err != nil {
		t.Fatalf("initial Reconcile returned error: %v", err)
	}

	secondSaveStarted := make(chan struct{})
	releaseSecondSave := make(chan struct{})
	var releaseSecondOnce sync.Once
	releaseSecond := func() {
		releaseSecondOnce.Do(func() {
			close(releaseSecondSave)
		})
	}
	t.Cleanup(releaseSecond)
	var renameCalls atomic.Int32
	store.renameFile = func(oldPath string, newPath string) error {
		switch renameCalls.Add(1) {
		case 1:
			return os.Rename(oldPath, newPath)
		case 2:
			close(secondSaveStarted)
			<-releaseSecondSave
			return errors.New("injected second source failure")
		default:
			return os.Rename(oldPath, newPath)
		}
	}

	enableDone := make(chan error, 1)
	go func() {
		_, err := store.SetAccountDisabled(context.Background(), "acct_visibility", false)
		enableDone <- err
	}()
	select {
	case <-secondSaveStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for partial enable state")
	}

	type loadResult struct {
		auths []*Auth
		err   error
	}
	loadDone := make(chan loadResult, 1)
	go func() {
		auths, err := store.Load(context.Background())
		loadDone <- loadResult{auths: auths, err: err}
	}()
	select {
	case result := <-loadDone:
		t.Fatalf("Load observed intermediate state before rollback: auths=%#v error=%v", result.auths, result.err)
	case <-time.After(50 * time.Millisecond):
	}

	releaseSecond()
	if err := <-enableDone; err == nil {
		t.Fatal("SetAccountDisabled returned nil error, want injected save failure")
	}
	result := <-loadDone
	if result.err != nil {
		t.Fatalf("Load after rollback returned error: %v", result.err)
	}
	if len(result.auths) != 0 {
		t.Fatalf("active auths after failed enable = %#v, want none", result.auths)
	}
	for _, name := range []string{"a.json", "b.json"} {
		meta := readAuthJSON(t, filepath.Join(authDir, name))
		if meta["disabled"] != true {
			t.Fatalf("%s disabled = %#v, want rollback to true", name, meta["disabled"])
		}
	}
}

func TestAuthManagementClearCooldownSafety(t *testing.T) {
	tests := []struct {
		name        string
		state       AuthHealthState
		disabled    bool
		model       string
		wantCleared bool
		wantState   AuthHealthKind
	}{
		{
			name: "transient",
			state: AuthHealthState{
				Kind:       AuthHealthTransient,
				Reason:     "network",
				RetryAt:    time.Now().Add(time.Hour),
				StatusCode: http.StatusServiceUnavailable,
			},
			wantCleared: true,
		},
		{
			name: "disabled transient",
			state: AuthHealthState{
				Kind:       AuthHealthTransient,
				Reason:     "network",
				RetryAt:    time.Now().Add(time.Hour),
				StatusCode: http.StatusServiceUnavailable,
			},
			disabled:    true,
			wantCleared: true,
		},
		{
			name: "quota",
			state: AuthHealthState{
				Kind:          AuthHealthQuota,
				Reason:        "quota",
				RetryAt:       time.Now().Add(time.Hour),
				Authoritative: true,
				StatusCode:    http.StatusTooManyRequests,
			},
			wantCleared: true,
		},
		{
			name: "invalid grant",
			state: AuthHealthState{
				Kind:       AuthHealthInvalidGrant,
				Reason:     "invalid_grant",
				StatusCode: http.StatusBadRequest,
				ErrorCode:  "invalid_grant",
			},
			wantState: AuthHealthInvalidGrant,
		},
		{
			name: "continued unauthorized",
			state: AuthHealthState{
				Kind:       AuthHealthUnauthorized,
				Reason:     "unauthorized",
				StatusCode: http.StatusUnauthorized,
				ErrorCode:  "unauthorized",
			},
			wantState: AuthHealthUnauthorized,
		},
		{
			name: "credential invalid",
			state: AuthHealthState{
				Kind:       AuthHealthCredentialInvalid,
				Reason:     "credential_invalid",
				StatusCode: http.StatusServiceUnavailable,
			},
			wantState: AuthHealthCredentialInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authDir := t.TempDir()
			writeAuthFile(t, authDir, "auth.json", flatAuthJSON("acct_cooldown", "access-cooldown", test.disabled))
			server := newAuthManagementTestServer(t, authDir, nil)
			auth := requireAuthByAccountID(t, server, "acct_cooldown")
			if err := server.health.MarkUnavailable(context.Background(), auth, test.state); err != nil {
				t.Fatalf("seed health: %v", err)
			}

			resp := doJSONRequest(t, server, http.MethodPost, "/v0/management/auths/cooldown/clear", `{"account_id":"acct_cooldown"}`, "admin-secret-value")
			if resp.Code != http.StatusOK {
				t.Fatalf("clear cooldown status = %d, body: %s", resp.Code, resp.Body.String())
			}
			var payload struct {
				Cleared bool `json:"cleared"`
			}
			decodeResponse(t, resp, &payload)
			if payload.Cleared != test.wantCleared {
				t.Fatalf("cleared = %t, want %t", payload.Cleared, test.wantCleared)
			}
			state, ok := server.health.State(auth.ID, "")
			if test.wantState == "" {
				if ok {
					t.Fatalf("health remained after clear: %#v", state)
				}
			} else if !ok || state.Kind != test.wantState {
				t.Fatalf("health after protected clear = %#v, %t, want %s", state, ok, test.wantState)
			}
			if got := requireAuthByAccountID(t, server, "acct_cooldown").Disabled; got != test.disabled {
				t.Fatalf("disabled after clear = %t, want %t", got, test.disabled)
			}
		})
	}

	t.Run("model exclusion remains", func(t *testing.T) {
		authDir := t.TempDir()
		writeAuthFile(t, authDir, "auth.json", flatAuthJSON("acct_model", "access-model", false))
		server := newAuthManagementTestServer(t, authDir, nil)
		auth := requireAuthByAccountID(t, server, "acct_model")
		if err := server.health.MarkModelUnsupported(context.Background(), auth, "gpt-unsupported", upstreamFailure{
			Kind:       upstreamFailureModelUnsupported,
			StatusCode: http.StatusNotFound,
			ErrorCode:  "model_not_found",
		}); err != nil {
			t.Fatalf("seed model exclusion: %v", err)
		}
		resp := doJSONRequest(t, server, http.MethodPost, "/v0/management/auths/cooldown/clear", `{"account_id":"acct_model"}`, "admin-secret-value")
		if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"cleared":false`) {
			t.Fatalf("model clear response = status %d body %s", resp.Code, resp.Body.String())
		}
		if !strings.Contains(resp.Body.String(), `"model":"gpt-unsupported"`) {
			t.Fatalf("model exclusion missing from status after clear: %s", resp.Body.String())
		}
		state, ok := server.health.State(auth.ID, "gpt-unsupported")
		if !ok || state.Kind != AuthHealthModelUnsupported {
			t.Fatalf("model exclusion after clear = %#v, %t", state, ok)
		}
	})
}

func TestAuthManagementClearBindingsScopesAndRedaction(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "a.json", flatAuthJSON("acct_bind_a", "access-a", false))
	writeAuthFile(t, authDir, "b.json", flatAuthJSON("acct_bind_b", "access-b", false))
	server := newAuthManagementTestServer(t, authDir, nil)
	alice, err := server.users.CreateUser(context.Background(), CreateUserParams{Name: "Alice"})
	if err != nil {
		t.Fatalf("create Alice: %v", err)
	}
	bob, err := server.users.CreateUser(context.Background(), CreateUserParams{Name: "Bob"})
	if err != nil {
		t.Fatalf("create Bob: %v", err)
	}
	aliceAuth := proxyAuthorization{Credential: &AuthenticatedAPIKey{User: alice.User, APIKey: alice.APIKey}}
	bobAuth := proxyAuthorization{Credential: &AuthenticatedAPIKey{User: bob.User, APIKey: bob.APIKey}}
	bind := func(authorization proxyAuthorization, session string) AuthSelection {
		t.Helper()
		selection, errSelect := server.selectProxyAuth(context.Background(), authorization, []sessionAffinitySignal{
			{Kind: sessionAffinitySignalSessionID, Value: session},
			{Kind: sessionAffinitySignalConversationID, Value: session + "-conversation"},
		})
		if errSelect != nil {
			t.Fatalf("bind session %q: %v", session, errSelect)
		}
		return selection
	}
	aliceOne := bind(aliceAuth, "secret-shared-session")
	aliceTwo := bind(aliceAuth, "secret-alice-session")
	_ = bind(bobAuth, "secret-shared-session")

	var logs bytes.Buffer
	restore := captureStandardLogger(t, &logs)
	defer restore()

	exact := doJSONRequest(t, server, http.MethodPost, "/v0/management/session-bindings/clear",
		fmt.Sprintf(`{"user_id":%q,"session_key":"secret-shared-session"}`, alice.User.ID),
		"admin-secret-value",
	)
	if exact.Code != http.StatusOK {
		t.Fatalf("exact clear status = %d, body: %s", exact.Code, exact.Body.String())
	}
	assertDeletedCount(t, exact, 1)
	aliceOneDigests := digestSessionAffinitySignals("user:"+alice.User.ID, []sessionAffinitySignal{
		{Kind: sessionAffinitySignalSessionID, Value: "secret-shared-session"},
	})
	if _, found, errLookup := server.users.LookupSessionAffinity(context.Background(), aliceOneDigests); errLookup != nil || found {
		t.Fatalf("Alice exact binding found=%t error=%v after clear", found, errLookup)
	}
	bobDigests := digestSessionAffinitySignals("user:"+bob.User.ID, []sessionAffinitySignal{
		{Kind: sessionAffinitySignalSessionID, Value: "secret-shared-session"},
	})
	if binding, found, errLookup := server.users.LookupSessionAffinity(context.Background(), bobDigests); errLookup != nil || !found || binding.AuthID == "" {
		t.Fatalf("Bob same-key binding = %#v found=%t error=%v", binding, found, errLookup)
	}

	userClear := doJSONRequest(t, server, http.MethodPost, "/v0/management/session-bindings/clear",
		fmt.Sprintf(`{"user_id":%q}`, alice.User.ID),
		"admin-secret-value",
	)
	if userClear.Code != http.StatusOK {
		t.Fatalf("user clear status = %d, body: %s", userClear.Code, userClear.Body.String())
	}
	assertDeletedCount(t, userClear, 1)
	aliceTwoDigests := digestSessionAffinitySignals("user:"+alice.User.ID, []sessionAffinitySignal{
		{Kind: sessionAffinitySignalSessionID, Value: "secret-alice-session"},
	})
	if _, found, errLookup := server.users.LookupSessionAffinity(context.Background(), aliceTwoDigests); errLookup != nil || found {
		t.Fatalf("Alice user binding found=%t error=%v after clear", found, errLookup)
	}

	accountClear := doJSONRequest(t, server, http.MethodPost, "/v0/management/session-bindings/clear",
		fmt.Sprintf(`{"account_id":%q}`, strings.TrimPrefix(aliceOne.Auth.ID, accountAuthIDPrefix)),
		"admin-secret-value",
	)
	if accountClear.Code != http.StatusOK {
		t.Fatalf("account clear status = %d, body: %s", accountClear.Code, accountClear.Body.String())
	}
	if deletedCount(accountClear) < 1 {
		t.Fatalf("account clear deleted_count = %d, want at least 1", deletedCount(accountClear))
	}

	noScope := doJSONRequest(t, server, http.MethodPost, "/v0/management/session-bindings/clear", `{}`, "admin-secret-value")
	if noScope.Code != http.StatusBadRequest {
		t.Fatalf("global clear status = %d, want 400, body: %s", noScope.Code, noScope.Body.String())
	}
	mixedScope := doJSONRequest(t, server, http.MethodPost, "/v0/management/session-bindings/clear",
		fmt.Sprintf(`{"user_id":%q,"account_id":"acct_bind_a"}`, bob.User.ID),
		"admin-secret-value",
	)
	if mixedScope.Code != http.StatusBadRequest {
		t.Fatalf("mixed clear status = %d, want 400, body: %s", mixedScope.Code, mixedScope.Body.String())
	}
	missingUser := doJSONRequest(t, server, http.MethodPost, "/v0/management/session-bindings/clear",
		`{"user_id":"usr_missing"}`,
		"admin-secret-value",
	)
	if missingUser.Code != http.StatusNotFound {
		t.Fatalf("missing user clear status = %d, want 404, body: %s", missingUser.Code, missingUser.Body.String())
	}
	invalidExact := doJSONRequest(t, server, http.MethodPost, "/v0/management/session-bindings/clear",
		fmt.Sprintf("{\"user_id\":%q,\"session_key\":\"bad\\u0000session\"}", bob.User.ID),
		"admin-secret-value",
	)
	if invalidExact.Code != http.StatusBadRequest {
		t.Fatalf("invalid exact clear status = %d, want 400, body: %s", invalidExact.Code, invalidExact.Body.String())
	}
	unknownAccount := doJSONRequest(t, server, http.MethodPost, "/v0/management/session-bindings/clear",
		`{"account_id":"missing-account"}`,
		"admin-secret-value",
	)
	if unknownAccount.Code != http.StatusNotFound {
		t.Fatalf("unknown account clear status = %d, want 404, body: %s", unknownAccount.Code, unknownAccount.Body.String())
	}

	for _, output := range []string{exact.Body.String(), userClear.Body.String(), accountClear.Body.String(), logs.String()} {
		for _, secret := range []string{"secret-shared-session", "secret-alice-session", "secret-shared-session-conversation", "admin-secret-value"} {
			if strings.Contains(output, secret) {
				t.Fatalf("binding clear output leaked %q:\n%s", secret, output)
			}
		}
	}
	if aliceTwo.Auth.ID == "" {
		t.Fatal("Alice second selection has empty auth ID")
	}
}

func TestAuthManagementRejectsPresentEmptySessionKeyWithoutDeletingBindings(t *testing.T) {
	tests := []struct {
		name       string
		sessionKey string
	}{
		{name: "empty", sessionKey: ""},
		{name: "whitespace", sessionKey: " \t "},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authDir := t.TempDir()
			writeAuthFile(t, authDir, "auth.json", flatAuthJSON("acct_empty_session", "access-empty-session", false))
			server := newAuthManagementTestServer(t, authDir, nil)
			user, err := server.users.CreateUser(context.Background(), CreateUserParams{Name: "Alice"})
			if err != nil {
				t.Fatalf("create user: %v", err)
			}
			authorization := proxyAuthorization{Credential: &AuthenticatedAPIKey{User: user.User, APIKey: user.APIKey}}
			if _, err = server.selectProxyAuth(context.Background(), authorization, []sessionAffinitySignal{
				{Kind: sessionAffinitySignalSessionID, Value: "binding-that-must-remain"},
			}); err != nil {
				t.Fatalf("create binding: %v", err)
			}
			before := countLogicalSessionBindings(t, server.users)

			body, err := json.Marshal(map[string]string{
				"user_id":     user.User.ID,
				"session_key": test.sessionKey,
			})
			if err != nil {
				t.Fatalf("encode request: %v", err)
			}
			resp := doJSONRequest(
				t,
				server,
				http.MethodPost,
				"/v0/management/session-bindings/clear",
				string(body),
				"admin-secret-value",
			)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("clear status = %d, want 400, body: %s", resp.Code, resp.Body.String())
			}
			if after := countLogicalSessionBindings(t, server.users); after != before {
				t.Fatalf("binding count after rejected clear = %d, want unchanged %d", after, before)
			}
		})
	}
}

func TestAuthManagementFatalStorageErrorsCannotReturnSuccess(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		setup  func(*testing.T, *Server)
	}{
		{
			name:   "status",
			method: http.MethodGet,
			path:   "/v0/management/auths",
		},
		{
			name:   "disable",
			method: http.MethodPost,
			path:   "/v0/management/auths/disable",
			body:   `{"account_id":"acct_storage"}`,
		},
		{
			name:   "force refresh",
			method: http.MethodPost,
			path:   "/v0/management/auths/refresh",
			body:   `{"account_id":"acct_storage"}`,
		},
		{
			name:   "clear cooldown",
			method: http.MethodPost,
			path:   "/v0/management/auths/cooldown/clear",
			body:   `{"account_id":"acct_storage"}`,
			setup: func(t *testing.T, server *Server) {
				t.Helper()
				auth := requireAuthByAccountID(t, server, "acct_storage")
				if err := server.health.MarkUnavailable(context.Background(), auth, AuthHealthState{
					Kind:          AuthHealthQuota,
					Reason:        "quota",
					RetryAt:       time.Now().Add(time.Hour),
					Authoritative: true,
					StatusCode:    http.StatusTooManyRequests,
				}); err != nil {
					t.Fatalf("seed quota: %v", err)
				}
			},
		},
		{
			name:   "clear bindings",
			method: http.MethodPost,
			path:   "/v0/management/session-bindings/clear",
			body:   `{"account_id":"acct_storage"}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authDir := t.TempDir()
			authPath := filepath.Join(authDir, "auth.json")
			writeAuthFile(t, authDir, "auth.json", flatAuthJSON("acct_storage", "access-storage", false))
			server := newAuthManagementTestServer(t, authDir, nil)
			if test.setup != nil {
				test.setup(t, server)
			}
			before, err := os.ReadFile(authPath)
			if err != nil {
				t.Fatalf("read auth before storage failure: %v", err)
			}
			if err = server.users.db.Close(); err != nil {
				t.Fatalf("close SQLite database: %v", err)
			}

			resp := doJSONRequest(t, server, test.method, test.path, test.body, "admin-secret-value")
			if resp.Code != http.StatusInternalServerError {
				t.Fatalf("storage failure status = %d, want 500, body: %s", resp.Code, resp.Body.String())
			}
			if strings.Contains(resp.Body.String(), `"auths"`) ||
				strings.Contains(resp.Body.String(), `"deleted_count"`) ||
				strings.Contains(resp.Body.String(), `"enabled"`) {
				t.Fatalf("storage failure returned successful management data: %s", resp.Body.String())
			}
			after, err := os.ReadFile(authPath)
			if err != nil {
				t.Fatalf("read auth after storage failure: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("auth file changed after fatal storage preflight:\nbefore=%s\nafter=%s", before, after)
			}
			assertFatalStorageError(t, server.FatalErrors())
		})
	}
}

func newAuthManagementTestServer(t *testing.T, authDir string, configure func(*Config)) *Server {
	t.Helper()
	cfg := &Config{
		AuthDir:        authDir,
		AdminAPIKey:    "admin-secret-value",
		Database:       DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:   "http://127.0.0.1:1/backend-api/codex",
		ChatGPTBaseURL: "http://127.0.0.1:1/backend-api",
		RequestRetry:   0,
	}
	if configure != nil {
		configure(cfg)
	}
	server, err := NewHandler(context.Background(), cfg)
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

func requireAuthByAccountID(t *testing.T, server *Server, accountID string) *Auth {
	t.Helper()
	result, err := server.reconcileAuths(context.Background())
	if err != nil {
		t.Fatalf("reconcile auths: %v", err)
	}
	auth := authByAccountID(result.Auths, accountID)
	if auth == nil {
		t.Fatalf("auth account %q not found", accountID)
	}
	return auth
}

func authByAccountID(auths []*Auth, accountID string) *Auth {
	for _, auth := range auths {
		if auth != nil && auth.AccountID == accountID {
			return auth
		}
	}
	return nil
}

func readAuthJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read auth JSON %s: %v", path, err)
	}
	var result map[string]any
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode auth JSON %s: %v", path, err)
	}
	return result
}

func assertDeletedCount(t *testing.T, resp *httptest.ResponseRecorder, want int64) {
	t.Helper()
	if got := deletedCount(resp); got != want {
		t.Fatalf("deleted_count = %d, want %d, body: %s", got, want, resp.Body.String())
	}
}

func deletedCount(resp *httptest.ResponseRecorder) int64 {
	var payload struct {
		DeletedCount int64 `json:"deleted_count"`
	}
	_ = json.Unmarshal(resp.Body.Bytes(), &payload)
	return payload.DeletedCount
}

func countLogicalSessionBindings(t *testing.T, store *UserStore) int64 {
	t.Helper()
	var count int64
	if err := store.db.QueryRow(`SELECT COUNT(DISTINCT binding_digest) FROM session_affinity_bindings`).Scan(&count); err != nil {
		t.Fatalf("count logical session bindings: %v", err)
	}
	return count
}
