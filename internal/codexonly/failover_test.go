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
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestRequestReplayCandidateAllowlist(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		websocket   bool
		want        bool
	}{
		{name: "chat completions", method: http.MethodPost, path: "/v1/chat/completions", contentType: "application/json", want: true},
		{name: "responses", method: http.MethodPost, path: "/v1/responses", contentType: "application/json", want: true},
		{name: "responses compact", method: http.MethodPost, path: "/v1/responses/compact", contentType: "application/json", want: true},
		{name: "alpha search", method: http.MethodPost, path: "/v1/alpha/search", contentType: "application/json", want: true},
		{name: "json image generation", method: http.MethodPost, path: "/v1/images/generations", contentType: "application/json", want: true},
		{name: "trace summarization", method: http.MethodPost, path: "/v1/memories/trace_summarize", contentType: "application/json", want: true},
		{name: "read only get", method: http.MethodGet, path: "/backend-api/wham/usage", want: true},
		{name: "read only head", method: http.MethodHead, path: "/v1/responses", want: true},
		{name: "responses websocket handshake", method: http.MethodGet, path: "/v1/responses", websocket: true, want: true},
		{name: "multipart image edit", method: http.MethodPost, path: "/v1/images/edits", contentType: "multipart/form-data; boundary=x", want: false},
		{name: "file upload", method: http.MethodPost, path: "/backend-api/files", contentType: "application/json", want: false},
		{name: "file finalization", method: http.MethodPost, path: "/backend-api/files/file_1/uploaded", contentType: "application/json", want: false},
		{name: "realtime call", method: http.MethodPost, path: "/v1/realtime/calls", contentType: "application/sdp", want: false},
		{name: "realtime connection", method: http.MethodGet, path: "/v1/realtime", websocket: true, want: false},
		{name: "side effecting wham", method: http.MethodPost, path: "/backend-api/wham/accounts/send_add_credits_nudge_email", contentType: "application/json", want: false},
		{name: "legacy hosted mcp", method: http.MethodPost, path: "/backend-api/wham/apps", contentType: "application/json", want: false},
		{name: "hosted mcp", method: http.MethodPost, path: "/backend-api/ps/mcp", contentType: "application/json", want: false},
		{name: "unknown write route", method: http.MethodPost, path: "/v1/unknown", contentType: "application/json", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := &http.Request{
				Method:        test.method,
				URL:           &url.URL{Path: test.path},
				Header:        make(http.Header),
				Body:          http.NoBody,
				ContentLength: 0,
			}
			if test.contentType != "" {
				req.Header.Set("Content-Type", test.contentType)
			}
			if test.websocket {
				req.Header.Set("Connection", "Upgrade")
				req.Header.Set("Upgrade", "websocket")
			}
			if got := requestReplayCandidate(req); got != test.want {
				t.Fatalf("requestReplayCandidate() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestPrepareReplayBodyBoundaries(t *testing.T) {
	t.Run("exactly 32 MiB", func(t *testing.T) {
		body := bytes.Repeat([]byte("x"), maxReplayBodyBytes)
		req := &http.Request{
			Method:        http.MethodPost,
			URL:           &url.URL{Path: "/v1/responses"},
			Header:        http.Header{"Content-Type": {"application/json"}},
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
		}
		if !prepareReplayBody(req, true) {
			t.Fatal("exact 32 MiB body was not replayable")
		}
		replayed, err := req.GetBody()
		if err != nil {
			t.Fatalf("GetBody returned error: %v", err)
		}
		got, err := io.ReadAll(replayed)
		if err != nil {
			t.Fatalf("read replayed body: %v", err)
		}
		_ = replayed.Close()
		if !bytes.Equal(got, body) {
			t.Fatalf("replayed body length = %d, want %d", len(got), len(body))
		}
	})

	t.Run("known over limit", func(t *testing.T) {
		req := &http.Request{
			Method:        http.MethodPost,
			URL:           &url.URL{Path: "/v1/responses"},
			Header:        http.Header{"Content-Type": {"application/json"}},
			Body:          io.NopCloser(strings.NewReader("body")),
			ContentLength: maxReplayBodyBytes + 1,
		}
		if prepareReplayBody(req, true) {
			t.Fatal("known body over 32 MiB was replayable")
		}
		if req.GetBody != nil {
			t.Fatal("known body over 32 MiB gained GetBody")
		}
	})

	t.Run("unknown length", func(t *testing.T) {
		req := &http.Request{
			Method:        http.MethodPost,
			URL:           &url.URL{Path: "/v1/responses"},
			Header:        http.Header{"Content-Type": {"application/json"}},
			Body:          io.NopCloser(strings.NewReader("body")),
			ContentLength: -1,
		}
		if prepareReplayBody(req, true) {
			t.Fatal("unknown-length body was replayable")
		}
		if req.GetBody != nil {
			t.Fatal("unknown-length body gained GetBody")
		}
	})
}

func TestClassifyUpstreamResponseTransitions(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		status    int
		headers   http.Header
		body      string
		model     string
		wantKind  upstreamFailureKind
		wantRetry time.Time
	}{
		{name: "unauthorized", status: 401, body: `{"error":{"code":"unauthorized"}}`, wantKind: upstreamFailureUnauthorized},
		{
			name:      "retry after header",
			status:    429,
			headers:   http.Header{"Retry-After": {"12"}},
			body:      `{"error":{"type":"rate_limit_error"}}`,
			wantKind:  upstreamFailureQuota,
			wantRetry: now.Add(12 * time.Second),
		},
		{
			name:      "explicit codex reset",
			status:    429,
			body:      `{"error":{"type":"usage_limit_reached","resets_in_seconds":90}}`,
			wantKind:  upstreamFailureQuota,
			wantRetry: now.Add(90 * time.Second),
		},
		{
			name:     "model unsupported",
			status:   400,
			body:     `{"error":{"type":"invalid_request_error","code":"model_not_supported","message":"The requested model is not supported."}}`,
			model:    "gpt-test",
			wantKind: upstreamFailureModelUnsupported,
		},
		{
			name:     "request scoped invalid input",
			status:   400,
			body:     `{"error":{"type":"invalid_request_error","code":"invalid_value","message":"input is invalid"}}`,
			model:    "gpt-test",
			wantKind: upstreamFailureRequestScoped,
		},
		{
			name:     "control model is not cached as capability",
			status:   400,
			body:     `{"error":{"code":"model_not_supported","message":"The requested model is not supported."}}`,
			model:    "gpt-5\nsecret",
			wantKind: upstreamFailureRequestScoped,
		},
		{
			name:     "oversized model is not cached as capability",
			status:   400,
			body:     `{"error":{"code":"model_not_supported","message":"The requested model is not supported."}}`,
			model:    strings.Repeat("m", maxModelIdentifierBytes+1),
			wantKind: upstreamFailureRequestScoped,
		},
		{name: "request timeout", status: 408, wantKind: upstreamFailureTransient},
		{name: "retryable upstream", status: 503, wantKind: upstreamFailureTransient},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp := &http.Response{
				StatusCode: test.status,
				Header:     test.headers,
				Body:       io.NopCloser(strings.NewReader(test.body)),
			}
			failure, err := classifyUpstreamResponse(resp, test.model, now)
			if err != nil {
				t.Fatalf("classifyUpstreamResponse returned error: %v", err)
			}
			if failure.Kind != test.wantKind {
				t.Fatalf("failure kind = %q, want %q", failure.Kind, test.wantKind)
			}
			if !failure.RetryAt.Equal(test.wantRetry) {
				t.Fatalf("retry at = %s, want %s", failure.RetryAt, test.wantRetry)
			}
			restored, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read restored response body: %v", err)
			}
			if string(restored) != test.body {
				t.Fatalf("restored response body = %q, want %q", restored, test.body)
			}
		})
	}
}

func TestClassifyUpstreamResponseUsesPrefixBeforeBodyReadError(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	prefix := `{"error":{"type":"usage_limit_reached","resets_in_seconds":90}}`
	readErr := errors.New("injected upstream body read failure")
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     make(http.Header),
		Body:       newFailingResponseBody(prefix, readErr),
	}

	failure, err := classifyUpstreamResponse(resp, "gpt-test", now)
	if err != nil {
		t.Fatalf("classifyUpstreamResponse returned error: %v", err)
	}
	if failure.Kind != upstreamFailureQuota || !failure.RetryAt.Equal(now.Add(90*time.Second)) {
		t.Fatalf("failure = %#v, want quota with prefix recovery deadline", failure)
	}
	restored, err := io.ReadAll(resp.Body)
	if !errors.Is(err, readErr) {
		t.Fatalf("restored body error = %v, want %v", err, readErr)
	}
	if string(restored) != prefix {
		t.Fatalf("restored body = %q, want prefix %q", restored, prefix)
	}
}

func TestServerReplayableResponseBodyReadErrorStillFailsOver(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	server, apiKey := newFailoverTestServer(t, authDir, "http://127.0.0.1:1", func(cfg *Config) {
		cfg.RequestRetry = 0
		cfg.requestRetrySet = true
	})
	var mu sync.Mutex
	var authorizations []string
	server.httpClient.Transport = failoverRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		authorization := req.Header.Get("Authorization")
		mu.Lock()
		authorizations = append(authorizations, authorization)
		mu.Unlock()
		if authorization == "Bearer access-a" {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{"Content-Type": {"text/plain"}},
				Body: newFailingResponseBody(
					"temporary-prefix",
					errors.New("injected upstream body read failure"),
				),
				Request: req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    req,
		}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	mu.Lock()
	got := slices.Clone(authorizations)
	mu.Unlock()
	want := []string{"Bearer access-a", "Bearer access-b"}
	if !slices.Equal(got, want) {
		t.Fatalf("upstream auths = %v, want failover %v", got, want)
	}
	state, ok := server.health.State("account:acct_a", "gpt-test")
	if !ok || state.Kind != AuthHealthTransient {
		t.Fatalf("first auth health = %#v, ok=%t, want transient", state, ok)
	}
}

func TestServerNonReplayableResponseBodyReadErrorPreservesUpstreamResponse(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	server, apiKey := newFailoverTestServer(t, authDir, "http://127.0.0.1:1", nil)
	var calls atomic.Int32
	server.httpClient.Transport = failoverRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header: http.Header{
				"Content-Type":      {"text/plain"},
				"X-Upstream-Result": {"preserved"},
			},
			Body: newFailingResponseBody(
				"temporary-prefix",
				errors.New("injected upstream body read failure"),
			),
			Request: req,
		}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", nil)
	req.Body = io.NopCloser(strings.NewReader(`{"model":"gpt-test","stream":true,"input":"unknown"}`))
	req.ContentLength = -1
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want upstream 503, body: %s", resp.Code, resp.Body.String())
	}
	if resp.Header().Get("X-Upstream-Result") != "preserved" {
		t.Fatalf("upstream header = %q, want preserved", resp.Header().Get("X-Upstream-Result"))
	}
	if resp.Body.String() != "temporary-prefix" {
		t.Fatalf("body = %q, want preserved upstream prefix", resp.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want one-shot response", calls.Load())
	}
}

func TestServerHealthAwareFailoverRebindsStickySession(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var mu sync.Mutex
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		mu.Lock()
		authorizations = append(authorizations, authorization)
		mu.Unlock()
		if authorization == "Bearer access-a" {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, func(cfg *Config) {
		cfg.RequestRetry = 0
		cfg.requestRetrySet = true
		cfg.MaxRetryInterval = 0
		cfg.maxRetryIntervalSet = true
	})

	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hello"}`))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Session-Id", "sticky-failover")
		resp := httptest.NewRecorder()
		server.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
		}
	}

	mu.Lock()
	got := slices.Clone(authorizations)
	mu.Unlock()
	want := []string{"Bearer access-a", "Bearer access-b", "Bearer access-b"}
	if !slices.Equal(got, want) {
		t.Fatalf("upstream authorizations = %v, want %v", got, want)
	}
}

func TestServerNonReplayableRequestProxiesOnceAndUpdatesHealth(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var calls atomic.Int32
	var bodiesMu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		bodiesMu.Lock()
		bodies = append(bodies, string(body))
		bodiesMu.Unlock()
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", nil)
	req.Body = io.NopCloser(strings.NewReader("unknown-length-body"))
	req.ContentLength = -1
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want upstream 503, body: %s", resp.Code, resp.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	bodiesMu.Lock()
	defer bodiesMu.Unlock()
	if len(bodies) != 1 || bodies[0] != "unknown-length-body" {
		t.Fatalf("upstream bodies = %v, want exact one-shot body", bodies)
	}
	state, ok := server.health.State("account:acct_a", "")
	if !ok || state.Kind != AuthHealthTransient {
		t.Fatalf("auth health = %#v, %t, want transient cooldown", state, ok)
	}
}

func TestServerAllowlistedUnknownLengthBodyProxiesOnce(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var calls atomic.Int32
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Body = io.NopCloser(strings.NewReader(`{"model":"gpt-test","stream":true,"input":"unknown"}`))
	req.ContentLength = -1
	req.GetBody = nil
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want upstream 503, body: %s", resp.Code, resp.Body.String())
	}
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	decodeResponse(t, resp, &envelope)
	if envelope.Error.Type != "upstream_error" {
		t.Fatalf("error type = %q, want upstream_error, body: %s", envelope.Error.Type, resp.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want one-shot allowlisted body", calls.Load())
	}
	if gotBody != `{"model":"gpt-test","stream":true,"input":"unknown"}` {
		t.Fatalf("upstream body = %q, want original unknown-length body", gotBody)
	}
}

func TestServerBufferedNetworkRetryAcceptsDuplicateExecutionRisk(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer access-a" {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer does not support hijacking")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack upstream response: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, nil)
	payload := `{"model":"gpt-test","stream":true,"input":"duplicate-risk"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	mu.Lock()
	got := slices.Clone(bodies)
	mu.Unlock()
	if !slices.Equal(got, []string{payload, payload}) {
		t.Fatalf("upstream bodies = %v, want duplicate replay on ambiguous network failure", got)
	}
}

func TestServerDeterministicFinalErrors(t *testing.T) {
	tests := []struct {
		name           string
		handler        func(http.ResponseWriter, *http.Request)
		wantStatus     int
		wantCode       string
		wantRetryAfter bool
	}{
		{
			name: "all model unsupported",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{
						"type":    "invalid_request_error",
						"code":    "model_not_supported",
						"message": "The requested model is not supported.",
					},
				})
			},
			wantStatus: http.StatusNotFound,
			wantCode:   proxyErrorCodeModelNotFound,
		},
		{
			name: "all quota limited",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer access-a" {
					w.Header().Set("Retry-After", "20")
				} else {
					w.Header().Set("Retry-After", "10")
				}
				writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"type": "rate_limit_error"}})
			},
			wantStatus:     http.StatusTooManyRequests,
			wantCode:       proxyErrorCodeRateLimited,
			wantRetryAfter: true,
		},
		{
			name: "all transient failures",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "temporary", http.StatusServiceUnavailable)
			},
			wantStatus: http.StatusBadGateway,
			wantCode:   proxyErrorCodeUpstream,
		},
		{
			name: "mixed near quota and transient",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer access-a" {
					w.Header().Set("Retry-After", "10")
					writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"type": "rate_limit_error"}})
					return
				}
				http.Error(w, "temporary", http.StatusServiceUnavailable)
			},
			wantStatus:     http.StatusTooManyRequests,
			wantCode:       proxyErrorCodeRateLimited,
			wantRetryAfter: true,
		},
		{
			name: "mixed far quota and transient",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer access-a" {
					w.Header().Set("Retry-After", "120")
					writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"type": "rate_limit_error"}})
					return
				}
				http.Error(w, "temporary", http.StatusServiceUnavailable)
			},
			wantStatus: http.StatusBadGateway,
			wantCode:   proxyErrorCodeUpstream,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authDir := t.TempDir()
			writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
			writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
			upstream := httptest.NewServer(http.HandlerFunc(test.handler))
			defer upstream.Close()

			server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, func(cfg *Config) {
				cfg.RequestRetry = 0
				cfg.requestRetrySet = true
			})
			for range 2 {
				req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hello"}`))
				req.Header.Set("Authorization", "Bearer "+apiKey)
				req.Header.Set("Content-Type", "application/json")
				resp := httptest.NewRecorder()
				server.ServeHTTP(resp, req)

				if resp.Code != test.wantStatus {
					t.Fatalf("status = %d, want %d, body: %s", resp.Code, test.wantStatus, resp.Body.String())
				}
				var payload struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				}
				if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
					t.Fatalf("decode final error: %v", err)
				}
				if payload.Error.Code != test.wantCode {
					t.Fatalf("error.code = %q, want %q, body: %s", payload.Error.Code, test.wantCode, resp.Body.String())
				}
				if test.wantRetryAfter && strings.TrimSpace(resp.Header().Get("Retry-After")) == "" {
					t.Fatalf("Retry-After is empty, body: %s", resp.Body.String())
				}
			}
		})
	}
}

func TestServerRetryLayersUseIndependentBudgets(t *testing.T) {
	tests := []struct {
		name                string
		requestRetry        int
		maxRetryCredentials int
		wantStatus          int
		wantAuths           []string
		wantWaits           int
	}{
		{
			name:                "credential budget ends request without cooldown round",
			requestRetry:        0,
			maxRetryCredentials: 1,
			wantStatus:          http.StatusBadGateway,
			wantAuths:           []string{"Bearer access-a"},
		},
		{
			name:                "cooldown round gets a fresh credential budget",
			requestRetry:        1,
			maxRetryCredentials: 1,
			wantStatus:          http.StatusOK,
			wantAuths:           []string{"Bearer access-a", "Bearer access-b"},
			wantWaits:           1,
		},
		{
			name:                "zero credential limit tries all eligible in one round",
			requestRetry:        0,
			maxRetryCredentials: 0,
			wantStatus:          http.StatusOK,
			wantAuths:           []string{"Bearer access-a", "Bearer access-b"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			authDir := t.TempDir()
			writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
			writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

			var mu sync.Mutex
			var authorizations []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authorization := r.Header.Get("Authorization")
				mu.Lock()
				authorizations = append(authorizations, authorization)
				mu.Unlock()
				if authorization == "Bearer access-a" {
					http.Error(w, "temporary", http.StatusServiceUnavailable)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			}))
			defer upstream.Close()

			server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, func(cfg *Config) {
				cfg.RequestRetry = test.requestRetry
				cfg.requestRetrySet = true
				cfg.MaxRetryCredentials = test.maxRetryCredentials
				cfg.MaxRetryInterval = 60
			})
			now := time.Date(2026, 7, 31, 13, 0, 0, 0, time.UTC)
			server.health.now = func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return now
			}
			waits := 0
			server.waitRetry = func(_ context.Context, delay time.Duration) error {
				mu.Lock()
				defer mu.Unlock()
				waits++
				now = now.Add(delay)
				return nil
			}

			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hello"}`))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			server.ServeHTTP(resp, req)

			if resp.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d, body: %s", resp.Code, test.wantStatus, resp.Body.String())
			}
			mu.Lock()
			gotAuths := slices.Clone(authorizations)
			gotWaits := waits
			mu.Unlock()
			if !slices.Equal(gotAuths, test.wantAuths) {
				t.Fatalf("upstream auths = %v, want %v", gotAuths, test.wantAuths)
			}
			if gotWaits != test.wantWaits {
				t.Fatalf("cooldown waits = %d, want %d", gotWaits, test.wantWaits)
			}
		})
	}
}

func TestServerCooldownWaitStopsOnClientCancellationWithoutProxyError(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, func(cfg *Config) {
		cfg.RequestRetry = 1
		cfg.requestRetrySet = true
		cfg.MaxRetryInterval = 60
	})
	waiting := make(chan struct{})
	server.waitRetry = func(ctx context.Context, _ time.Duration) error {
		close(waiting)
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hello"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.ServeHTTP(resp, req)
		close(done)
	}()
	<-waiting
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not stop after cancellation")
	}
	if resp.Body.Len() != 0 {
		t.Fatalf("canceled request synthesized proxy body: %s", resp.Body.String())
	}
}

func TestServerConcurrentStickyFailoverConverges(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer access-a" {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, nil)
	const requests = 20
	start := make(chan struct{})
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hello"}`))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Session-Id", "concurrent-failover")
			resp := httptest.NewRecorder()
			server.ServeHTTP(resp, req)
			if resp.Code != http.StatusOK {
				errs <- fmt.Errorf("status=%d body=%s", resp.Code, resp.Body.String())
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	var distinct int
	if err := server.users.db.QueryRow(
		`SELECT COUNT(DISTINCT auth_id) FROM session_affinity_bindings`,
	).Scan(&distinct); err != nil {
		t.Fatalf("count affinity auths: %v", err)
	}
	if distinct != 1 {
		t.Fatalf("distinct affinity auths = %d, want 1", distinct)
	}
}

func TestServerDoesNotReplayAfterStreamingResponseAccepted(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: partial\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "partial") {
		t.Fatalf("stream response status=%d body=%q", resp.Code, resp.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1 after stream commitment", calls.Load())
	}
}

func TestServerChatCompletionsDoesNotReplayAfterStreamingResponseAccepted(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"partial"}

`)
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-test",
		"messages":[{"role":"user","content":"hello"}],
		"stream":true
	}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "partial") {
		t.Fatalf("chat stream response status=%d body=%q", resp.Code, resp.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("chat upstream calls = %d, want 1 after stream commitment", calls.Load())
	}
}

func TestServerSameAuthUnauthorizedRepairDoesNotConsumeCredentialBudget(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "refreshed-a",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	var mu sync.Mutex
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		mu.Lock()
		authorizations = append(authorizations, authorization)
		mu.Unlock()
		if authorization == "Bearer access-a" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "unauthorized"}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, func(cfg *Config) {
		cfg.CodexRefreshTokenURL = tokenServer.URL
		cfg.RequestRetry = 0
		cfg.requestRetrySet = true
		cfg.MaxRetryCredentials = 1
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	mu.Lock()
	got := slices.Clone(authorizations)
	mu.Unlock()
	want := []string{"Bearer access-a", "Bearer refreshed-a"}
	if !slices.Equal(got, want) {
		t.Fatalf("upstream auths = %v, want same-auth repair %v", got, want)
	}
}

func TestServerContinuedUnauthorizedPersistsAcrossRestart(t *testing.T) {
	authDir := t.TempDir()
	databasePath := filepath.Join(t.TempDir(), "users.db")
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "refreshed-a",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": "unauthorized"}})
	}))
	defer upstream.Close()

	cfg := func() *Config {
		return &Config{
			AuthDir:              authDir,
			AdminAPIKey:          "admin-key",
			Database:             DatabaseConfig{Path: databasePath},
			CodexBaseURL:         upstream.URL + "/backend-api/codex",
			ChatGPTBaseURL:       upstream.URL + "/backend-api",
			CodexRefreshTokenURL: tokenServer.URL,
			RequestRetry:         0,
			requestRetrySet:      true,
		}
	}
	first, err := NewHandler(context.Background(), cfg())
	if err != nil {
		t.Fatalf("first NewHandler returned error: %v", err)
	}
	apiKey := createManagedUser(t, first, "admin-key", "Alice").PlaintextAPIKey
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	first.ServeHTTP(resp, req)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("first status = %d, want 503, body: %s", resp.Code, resp.Body.String())
	}
	if err = first.Close(); err != nil {
		t.Fatalf("close first server: %v", err)
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("first upstream calls = %d, want initial and same-auth retry", got)
	}

	second, err := NewHandler(context.Background(), cfg())
	if err != nil {
		t.Fatalf("second NewHandler returned error: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp = httptest.NewRecorder()
	second.ServeHTTP(resp, req)

	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("restart status = %d, want 503, body: %s", resp.Code, resp.Body.String())
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("restart bypassed persisted unauthorized state: upstream calls = %d, want 2", got)
	}
}

func TestServerExplicitQuotaDeadlinePersistsAcrossRestart(t *testing.T) {
	authDir := t.TempDir()
	databasePath := filepath.Join(t.TempDir(), "users.db")
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Retry-After", "120")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"type": "rate_limit_error"}})
	}))
	defer upstream.Close()

	cfg := func() *Config {
		return &Config{
			AuthDir:         authDir,
			AdminAPIKey:     "admin-key",
			Database:        DatabaseConfig{Path: databasePath},
			CodexBaseURL:    upstream.URL + "/backend-api/codex",
			ChatGPTBaseURL:  upstream.URL + "/backend-api",
			RequestRetry:    0,
			requestRetrySet: true,
		}
	}
	first, err := NewHandler(context.Background(), cfg())
	if err != nil {
		t.Fatalf("first NewHandler returned error: %v", err)
	}
	apiKey := createManagedUser(t, first, "admin-key", "Alice").PlaintextAPIKey
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	first.ServeHTTP(resp, req)
	if resp.Code != http.StatusTooManyRequests || strings.TrimSpace(resp.Header().Get("Retry-After")) == "" {
		t.Fatalf("first quota response status=%d retry-after=%q body=%s", resp.Code, resp.Header().Get("Retry-After"), resp.Body.String())
	}
	if err = first.Close(); err != nil {
		t.Fatalf("close first server: %v", err)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("first upstream calls = %d, want 1", got)
	}

	second, err := NewHandler(context.Background(), cfg())
	if err != nil {
		t.Fatalf("second NewHandler returned error: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp = httptest.NewRecorder()
	second.ServeHTTP(resp, req)

	if resp.Code != http.StatusTooManyRequests || strings.TrimSpace(resp.Header().Get("Retry-After")) == "" {
		t.Fatalf("restart quota response status=%d retry-after=%q body=%s", resp.Code, resp.Header().Get("Retry-After"), resp.Body.String())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("restart bypassed persisted quota deadline: upstream calls = %d, want 1", got)
	}
}

func TestServerRequestScopedFailureDoesNotPenalizeAuth(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"code":    "invalid_value",
				"message": "input is invalid",
			},
		})
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"bad"}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", resp.Code, resp.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want request-scoped stop after one", calls.Load())
	}
	for _, authID := range []string{"account:acct_a", "account:acct_b"} {
		if state, ok := server.health.State(authID, "gpt-test"); ok {
			t.Fatalf("auth %s was penalized by request-scoped failure: %#v", authID, state)
		}
	}
}

func TestServerModelExclusionIsNotAuthGlobal(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"model":"unsupported-model"`)) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": map[string]any{
					"code":    "model_not_supported",
					"message": "The requested model is not supported.",
				},
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, func(cfg *Config) {
		cfg.RequestRetry = 0
		cfg.requestRetrySet = true
	})
	for _, test := range []struct {
		model      string
		wantStatus int
	}{
		{model: "unsupported-model", wantStatus: http.StatusNotFound},
		{model: "supported-model", wantStatus: http.StatusOK},
	} {
		req := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":true,"input":"hello"}`, test.model)),
		)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		server.ServeHTTP(resp, req)
		if resp.Code != test.wantStatus {
			t.Fatalf("model %s status=%d, want %d, body=%s", test.model, resp.Code, test.wantStatus, resp.Body.String())
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want model exclusion to permit another model", calls.Load())
	}
}

func TestServerResponsesWebSocketHandshakeFailsOverThenStaysSticky(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
	var mu sync.Mutex
	var authorizations []string
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		mu.Lock()
		authorizations = append(authorizations, authorization)
		mu.Unlock()
		if authorization == "Bearer access-a" {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade: %v", err)
			return
		}
		defer conn.Close()
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.WriteMessage(messageType, payload)
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, nil)
	proxy := httptest.NewServer(server)
	defer proxy.Close()

	for range 2 {
		header := http.Header{
			"Authorization": {"Bearer " + apiKey},
			"Session-Id":    {"websocket-failover"},
		}
		conn, resp, err := websocket.DefaultDialer.Dial(
			"ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses",
			header,
		)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			t.Fatalf("websocket dial status=%d error=%v", status, err)
		}
		if err = conn.WriteMessage(websocket.TextMessage, []byte("hello")); err != nil {
			t.Fatalf("write websocket: %v", err)
		}
		_, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read websocket: %v", err)
		}
		if string(payload) != "hello" {
			t.Fatalf("websocket payload = %q, want hello", payload)
		}
		_ = conn.Close()
	}

	mu.Lock()
	got := slices.Clone(authorizations)
	mu.Unlock()
	want := []string{"Bearer access-a", "Bearer access-b", "Bearer access-b"}
	if !slices.Equal(got, want) {
		t.Fatalf("websocket auths = %v, want %v", got, want)
	}
}

func TestServerResponsesWebSocketDoesNotRetryAfterUpgrade(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
	var calls atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstreamClosed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade: %v", err)
			close(upstreamClosed)
			return
		}
		_ = conn.Close()
		close(upstreamClosed)
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, nil)
	proxy := httptest.NewServer(server)
	defer proxy.Close()
	conn, resp, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses",
		http.Header{"Authorization": {"Bearer " + apiKey}},
	)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("websocket dial status=%d error=%v", status, err)
	}
	_, _, _ = conn.ReadMessage()
	_ = conn.Close()
	select {
	case <-upstreamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream websocket did not close")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream websocket calls = %d, want 1 after successful upgrade", got)
	}
}

func newFailoverTestServer(t *testing.T, authDir string, upstreamURL string, configure func(*Config)) (*Server, string) {
	t.Helper()
	cfg := &Config{
		AuthDir:        authDir,
		AdminAPIKey:    "admin-key",
		Database:       DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:   upstreamURL + "/backend-api/codex",
		ChatGPTBaseURL: upstreamURL + "/backend-api",
	}
	if configure != nil {
		configure(cfg)
	}
	server, err := NewHandler(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	apiKey := createManagedUser(t, server, "admin-key", "Alice").PlaintextAPIKey
	return server, apiKey
}

type failoverRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f failoverRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type failingResponseBody struct {
	prefix []byte
	err    error
	failed bool
}

func newFailingResponseBody(prefix string, err error) *failingResponseBody {
	return &failingResponseBody{
		prefix: []byte(prefix),
		err:    err,
	}
}

func (b *failingResponseBody) Read(p []byte) (int, error) {
	if len(b.prefix) > 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		return n, nil
	}
	if !b.failed {
		b.failed = true
		return 0, b.err
	}
	return 0, io.EOF
}

func (b *failingResponseBody) Close() error {
	return nil
}
