package codexonly

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

const validZedEditPredictionBody = `{
	"model":"gpt-5.6-luna",
	"prompt":"<|fim_prefix|>package main\n\nfunc main() {<|fim_suffix|>\n}\n<|fim_middle|>",
	"max_tokens":256,
	"temperature":0.2,
	"stop":["<|endoftext|>"]
}`

func TestZedEditPredictionsConvertsAndAggregates(t *testing.T) {
	var upstreamRequest map[string]any
	var upstreamCalls atomic.Int32
	server, apiKey := newZedEditPredictionTestServer(t, []zedTestAuth{
		{accountID: "acct_1", accessToken: "access-1"},
	}, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Errorf("upstream path = %q, want /backend-api/codex/responses", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer access-1" {
			t.Errorf("upstream Authorization = %q, want Bearer access-1", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Chatgpt-Account-Id") != "acct_1" {
			t.Errorf("upstream Chatgpt-Account-Id = %q, want acct_1", r.Header.Get("Chatgpt-Account-Id"))
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &upstreamRequest); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		writeChatUpstreamSSE(w,
			`{"type":"response.output_text.delta","delta":"你好"}`,
			`{"type":"response.output_text.delta","delta":"🙂E"}`,
			`{"type":"response.output_text.delta","delta":"NDhidden"}`,
			`{"type":"response.completed","response":{"id":"resp_zed","model":"gpt-5.4","usage":{"input_tokens":5,"output_tokens":4,"total_tokens":9}}}`,
		)
	}, nil)

	body := strings.Replace(validZedEditPredictionBody, `"gpt-5.6-luna"`, `"gpt-5.4"`, 1)
	body = strings.Replace(body, `["<|endoftext|>"]`, `"🙂END"`, 1)
	resp := doJSONRequest(t, server, http.MethodPost, "/v1/zed/edit-predictions", body, apiKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	var payload zedEditPredictionTestResponse
	decodeResponse(t, resp, &payload)
	if !strings.HasPrefix(payload.ID, "cmpl_") {
		t.Fatalf("id = %q, want cmpl_ prefix", payload.ID)
	}
	if payload.Object != "text_completion" || payload.Model != "gpt-5.4" {
		t.Fatalf("response identity = %q/%q, want text_completion/gpt-5.4", payload.Object, payload.Model)
	}
	if payload.Created <= 0 {
		t.Fatalf("created = %d, want positive Unix timestamp", payload.Created)
	}
	if len(payload.Choices) != 1 || payload.Choices[0].Index != 0 {
		t.Fatalf("choices = %#v, want one index 0 choice", payload.Choices)
	}
	if payload.Choices[0].Text != "你好" || !utf8.ValidString(payload.Choices[0].Text) {
		t.Fatalf("text = %q, want Unicode-safe stop-filtered 你好", payload.Choices[0].Text)
	}
	if payload.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", payload.Choices[0].FinishReason)
	}
	if payload.Usage.PromptTokens != 5 || payload.Usage.CompletionTokens != 4 || payload.Usage.TotalTokens != 9 {
		t.Fatalf("usage = %#v, want 5/4/9", payload.Usage)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
	}

	if upstreamRequest["model"] != "gpt-5.4" ||
		upstreamRequest["stream"] != true ||
		upstreamRequest["store"] != false {
		t.Fatalf("upstream request basics = %#v", upstreamRequest)
	}
	reasoning, ok := upstreamRequest["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "low" {
		t.Fatalf("reasoning = %#v, want low effort", upstreamRequest["reasoning"])
	}
	for _, field := range []string{"max_tokens", "max_output_tokens", "temperature", "stop", "tools", "tool_choice"} {
		if _, exists := upstreamRequest[field]; exists {
			t.Fatalf("upstream request includes unsupported field %q: %#v", field, upstreamRequest)
		}
	}
	instructions, _ := upstreamRequest["instructions"].(string)
	if !strings.Contains(instructions, "only the missing text") {
		t.Fatalf("instructions = %q, want narrow missing-text instruction", instructions)
	}
	input, _ := json.Marshal(upstreamRequest["input"])
	if !strings.Contains(string(input), `package main`) || !strings.Contains(string(input), `func main() {`) {
		t.Fatalf("input = %s, want extracted prefix and suffix", input)
	}

	waitForUsageTotal(t, server, apiKey, 9)
}

func TestZedEditPredictionsNormalizesModelIdentifier(t *testing.T) {
	var upstreamModel string
	server, apiKey := newZedEditPredictionTestServer(t, []zedTestAuth{
		{accountID: "acct_1", accessToken: "access-1"},
	}, func(w http.ResponseWriter, r *http.Request) {
		var upstreamRequest map[string]any
		if err := json.NewDecoder(r.Body).Decode(&upstreamRequest); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		upstreamModel, _ = upstreamRequest["model"].(string)
		writeChatUpstreamSSE(w,
			`{"type":"response.completed","response":{"id":"resp_normalized","model":"upstream-model"}}`,
		)
	}, nil)

	body := strings.Replace(validZedEditPredictionBody, `"gpt-5.6-luna"`, `"  gpt-5.4  "`, 1)
	resp := doJSONRequest(t, server, http.MethodPost, "/v1/zed/edit-predictions", body, apiKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	var payload zedEditPredictionTestResponse
	decodeResponse(t, resp, &payload)
	if upstreamModel != "gpt-5.4" {
		t.Fatalf("upstream model = %q, want normalized gpt-5.4", upstreamModel)
	}
	if payload.Model != "gpt-5.4" {
		t.Fatalf("response model = %q, want normalized gpt-5.4", payload.Model)
	}
}

func TestZedEditPredictionsRejectsInvalidRequestsBeforeUpstream(t *testing.T) {
	var upstreamCalls atomic.Int32
	server, apiKey := newZedEditPredictionTestServer(t, []zedTestAuth{
		{accountID: "acct_1", accessToken: "access-1"},
	}, func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		writeChatUpstreamSSE(w,
			`{"type":"response.completed","response":{"id":"resp_unexpected","model":"gpt-5.6-luna"}}`,
		)
	}, nil)

	tests := []struct {
		name string
		body string
	}{
		{name: "invalid JSON", body: `{`},
		{name: "trailing JSON", body: validZedEditPredictionBody + `{}`},
		{name: "missing model", body: `{"prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>"}`},
		{name: "invalid model type", body: `{"model":1,"prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>"}`},
		{name: "empty model", body: `{"model":"   ","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>"}`},
		{name: "control in model", body: `{"model":"gpt-5.4\nsecret","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>"}`},
		{name: "illegal character in model", body: `{"model":"gpt-5.4?","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>"}`},
		{name: "oversized model", body: `{"model":"` + strings.Repeat("m", maxModelIdentifierBytes+1) + `","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>"}`},
		{name: "missing prompt", body: `{"model":"gpt-5.6-luna"}`},
		{name: "missing prefix", body: `{"model":"gpt-5.6-luna","prompt":"a<|fim_suffix|>b<|fim_middle|>"}`},
		{name: "missing suffix", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>ab<|fim_middle|>"}`},
		{name: "missing middle", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b"}`},
		{name: "duplicate prefix", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_prefix|><|fim_suffix|>b<|fim_middle|>"}`},
		{name: "duplicate suffix", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_suffix|><|fim_middle|>"}`},
		{name: "duplicate middle", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|><|fim_middle|>"}`},
		{name: "out of order", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_suffix|>b<|fim_prefix|>a<|fim_middle|>"}`},
		{name: "leading data", body: `{"model":"gpt-5.6-luna","prompt":"x<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>"}`},
		{name: "trailing data", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>x"}`},
		{name: "zero max tokens", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>","max_tokens":0}`},
		{name: "negative max tokens", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>","max_tokens":-1}`},
		{name: "fractional max tokens", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>","max_tokens":1.5}`},
		{name: "string max tokens", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>","max_tokens":"1"}`},
		{name: "null max tokens", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>","max_tokens":null}`},
		{name: "excessive max tokens", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>","max_tokens":4097}`},
		{name: "invalid temperature", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>","temperature":"low"}`},
		{name: "null temperature", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>","temperature":null}`},
		{name: "invalid stop", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>","stop":1}`},
		{name: "invalid stop list", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>","stop":["ok",1]}`},
		{name: "empty stop", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>","stop":""}`},
		{name: "null stop", body: `{"model":"gpt-5.6-luna","prompt":"<|fim_prefix|>a<|fim_suffix|>b<|fim_middle|>","stop":null}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp := doJSONRequest(t, server, http.MethodPost, "/v1/zed/edit-predictions", test.body, apiKey)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body: %s", resp.Code, resp.Body.String())
			}
			assertSafeErrorResponse(t, resp)
		})
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/zed/edit-predictions", strings.NewReader(validZedEditPredictionBody))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("missing Content-Type status = %d, want 400, body: %s", resp.Code, resp.Body.String())
	}

	if upstreamCalls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0 for invalid requests", upstreamCalls.Load())
	}
}

func TestZedEditPredictionsDefersUnknownModelSupportToUpstream(t *testing.T) {
	var upstreamCalls atomic.Int32
	server, apiKey := newZedEditPredictionTestServer(t, []zedTestAuth{
		{accountID: "acct_1", accessToken: "access-1"},
	}, func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		writeChatUpstreamSSE(w,
			`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"model_not_supported","message":"unsupported"}}}`,
		)
	}, func(cfg *Config) {
		cfg.RequestRetry = 0
		cfg.requestRetrySet = true
		cfg.MaxRetryInterval = 0
		cfg.maxRetryIntervalSet = true
	})

	body := strings.Replace(validZedEditPredictionBody, `"gpt-5.6-luna"`, `"future-codex-model"`, 1)
	resp := doJSONRequest(t, server, http.MethodPost, "/v1/zed/edit-predictions", body, apiKey)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want upstream model 404, body: %s", resp.Code, resp.Body.String())
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
	}
	assertSafeErrorResponse(t, resp)
}

func TestZedEditPredictionsRouteScope(t *testing.T) {
	server, apiKey := newZedEditPredictionTestServer(t, []zedTestAuth{
		{accountID: "acct_1", accessToken: "access-1"},
	}, func(http.ResponseWriter, *http.Request) {
		t.Fatal("unexpected upstream request")
	}, nil)

	methodResp := doJSONRequest(t, server, http.MethodGet, "/v1/zed/edit-predictions", "", apiKey)
	if methodResp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405, body: %s", methodResp.Code, methodResp.Body.String())
	}
	if methodResp.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", methodResp.Header().Get("Allow"))
	}

	genericResp := doJSONRequest(t, server, http.MethodPost, "/v1/completions", validZedEditPredictionBody, apiKey)
	if genericResp.Code != http.StatusNotFound {
		t.Fatalf("/v1/completions status = %d, want 404, body: %s", genericResp.Code, genericResp.Body.String())
	}
}

func TestZedEditPredictionsRequiresManagedAuthentication(t *testing.T) {
	var upstreamCalls atomic.Int32
	server, _ := newZedEditPredictionTestServer(t, []zedTestAuth{
		{accountID: "acct_1", accessToken: "access-1"},
	}, func(http.ResponseWriter, *http.Request) {
		upstreamCalls.Add(1)
	}, nil)

	resp := doJSONRequest(t, server, http.MethodPost, "/v1/zed/edit-predictions", validZedEditPredictionBody, "")
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body: %s", resp.Code, resp.Body.String())
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls.Load())
	}
}

func TestZedEditPredictionsAppliesUnicodeOutputBudgetAfterDrain(t *testing.T) {
	server, apiKey := newZedEditPredictionTestServer(t, []zedTestAuth{
		{accountID: "acct_1", accessToken: "access-1"},
	}, func(w http.ResponseWriter, _ *http.Request) {
		writeChatUpstreamSSE(w,
			`{"type":"response.output_text.delta","delta":"你🙂ab"}`,
			`{"type":"response.completed","response":{"id":"resp_budget","model":"gpt-5.6-luna","usage":{"input_tokens":7,"output_tokens":6,"total_tokens":13}}}`,
		)
	}, nil)

	body := strings.Replace(validZedEditPredictionBody, `"max_tokens":256`, `"max_tokens":3`, 1)
	resp := doJSONRequest(t, server, http.MethodPost, "/v1/zed/edit-predictions", body, apiKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	var payload zedEditPredictionTestResponse
	decodeResponse(t, resp, &payload)
	if payload.Choices[0].Text != "你🙂a" || !utf8.ValidString(payload.Choices[0].Text) {
		t.Fatalf("text = %q, want first 3 Unicode code points", payload.Choices[0].Text)
	}
	if payload.Choices[0].FinishReason != "length" {
		t.Fatalf("finish_reason = %q, want length", payload.Choices[0].FinishReason)
	}
	if payload.Usage.TotalTokens != 13 {
		t.Fatalf("usage = %#v, want terminal usage after local budget", payload.Usage)
	}
	waitForUsageTotal(t, server, apiKey, 13)
}

func TestZedEditPredictionsTerminalSemantics(t *testing.T) {
	tests := []struct {
		name          string
		events        []string
		wantStatus    int
		wantText      string
		wantFinish    string
		forbiddenText string
	}{
		{
			name: "completed",
			events: []string{
				`{"type":"response.completed","response":{"id":"resp_done","model":"gpt-5.6-luna","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}}`,
			},
			wantStatus: http.StatusOK,
			wantText:   "done",
			wantFinish: "stop",
		},
		{
			name: "incomplete max output",
			events: []string{
				`{"type":"response.output_text.delta","delta":"partial"}`,
				`{"type":"response.incomplete","response":{"id":"resp_partial","model":"gpt-5.6-luna","incomplete_details":{"reason":"max_output_tokens"}}}`,
			},
			wantStatus: http.StatusOK,
			wantText:   "partial",
			wantFinish: "length",
		},
		{
			name: "incomplete content filter",
			events: []string{
				`{"type":"response.incomplete","response":{"id":"resp_filtered","model":"gpt-5.6-luna","incomplete_details":{"reason":"content_filter"}}}`,
			},
			wantStatus: http.StatusOK,
			wantFinish: "content_filter",
		},
		{
			name: "malformed event",
			events: []string{
				`{"type":`,
			},
			wantStatus: http.StatusBadGateway,
		},
		{
			name: "EOF before terminal",
			events: []string{
				`{"type":"response.output_text.delta","delta":"secret partial"}`,
			},
			wantStatus:    http.StatusBadGateway,
			forbiddenText: "secret partial",
		},
		{
			name: "duplicate terminal",
			events: []string{
				`{"type":"response.completed","response":{"id":"resp_done","model":"gpt-5.6-luna"}}`,
				`{"type":"response.incomplete","response":{"id":"resp_late","model":"gpt-5.6-luna","incomplete_details":{"reason":"max_output_tokens"}}}`,
			},
			wantStatus: http.StatusBadGateway,
		},
		{
			name: "data after terminal",
			events: []string{
				`{"type":"response.completed","response":{"id":"resp_done","model":"gpt-5.6-luna"}}`,
				`{"type":"response.output_text.delta","delta":"late"}`,
			},
			wantStatus:    http.StatusBadGateway,
			forbiddenText: "late",
		},
		{
			name: "unexpected tool output",
			events: []string{
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","name":"lookup","arguments":"{}"}}`,
				`{"type":"response.completed","response":{"id":"resp_tool","model":"gpt-5.6-luna"}}`,
			},
			wantStatus: http.StatusBadGateway,
		},
		{
			name: "failed event",
			events: []string{
				`{"type":"response.failed","response":{"error":{"type":"upstream_error","code":"backend_failed","message":"secret upstream failure"}}}`,
			},
			wantStatus:    http.StatusBadGateway,
			forbiddenText: "secret upstream failure",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, apiKey := newZedEditPredictionTestServer(t, []zedTestAuth{
				{accountID: "acct_1", accessToken: "access-1"},
			}, func(w http.ResponseWriter, _ *http.Request) {
				writeChatUpstreamSSE(w, test.events...)
			}, func(cfg *Config) {
				cfg.RequestRetry = 0
				cfg.requestRetrySet = true
				cfg.MaxRetryInterval = 0
				cfg.maxRetryIntervalSet = true
			})

			resp := doJSONRequest(t, server, http.MethodPost, "/v1/zed/edit-predictions", validZedEditPredictionBody, apiKey)
			if resp.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d, body: %s", resp.Code, test.wantStatus, resp.Body.String())
			}
			if test.wantStatus == http.StatusOK {
				var payload zedEditPredictionTestResponse
				decodeResponse(t, resp, &payload)
				if payload.Choices[0].Text != test.wantText || payload.Choices[0].FinishReason != test.wantFinish {
					t.Fatalf("choice = %#v, want text %q finish %q", payload.Choices[0], test.wantText, test.wantFinish)
				}
			} else {
				assertSafeErrorResponse(t, resp)
			}
			if test.forbiddenText != "" && strings.Contains(resp.Body.String(), test.forbiddenText) {
				t.Fatalf("response leaked upstream text %q: %s", test.forbiddenText, resp.Body.String())
			}
		})
	}
}

func TestZedEditPredictionsReuseFailoverAndSessionAffinity(t *testing.T) {
	var mu sync.Mutex
	var authorizations []string
	server, apiKey := newZedEditPredictionTestServer(t, []zedTestAuth{
		{accountID: "acct_a", accessToken: "access-a"},
		{accountID: "acct_b", accessToken: "access-b"},
	}, func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		mu.Lock()
		authorizations = append(authorizations, authorization)
		mu.Unlock()
		if authorization == "Bearer access-a" {
			writeChatUpstreamSSE(w,
				`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"model_not_supported","message":"unsupported"}}}`,
			)
			return
		}
		writeChatUpstreamSSE(w,
			`{"type":"response.completed","response":{"id":"resp_ok","model":"gpt-5.6-luna","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		)
	}, func(cfg *Config) {
		cfg.RequestRetry = 0
		cfg.requestRetrySet = true
		cfg.MaxRetryInterval = 0
		cfg.maxRetryIntervalSet = true
	})

	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/zed/edit-predictions", strings.NewReader(validZedEditPredictionBody))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Session-Id", "zed-edit-session")
		resp := httptest.NewRecorder()
		server.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
		}
	}

	mu.Lock()
	gotAuthorizations := slices.Clone(authorizations)
	mu.Unlock()
	wantAuthorizations := []string{"Bearer access-a", "Bearer access-b", "Bearer access-b"}
	if !slices.Equal(gotAuthorizations, wantAuthorizations) {
		t.Fatalf("upstream authorizations = %v, want %v", gotAuthorizations, wantAuthorizations)
	}
	state, blocked := server.health.State("account:acct_a", "gpt-5.6-luna")
	if !blocked || state.Kind != AuthHealthModelUnsupported {
		t.Fatalf("auth health = %#v, %t, want model exclusion", state, blocked)
	}
	var boundAuthID string
	if err := server.users.db.QueryRow(`SELECT auth_id FROM session_affinity_bindings LIMIT 1`).Scan(&boundAuthID); err != nil {
		t.Fatalf("read session binding: %v", err)
	}
	if boundAuthID != "account:acct_b" {
		t.Fatalf("session binding auth_id = %q, want account:acct_b", boundAuthID)
	}
	waitForUsageTotal(t, server, apiKey, 4)
}

func TestZedEditPredictionsClientCancellationStopsWithoutRetry(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	server, apiKey := newZedEditPredictionTestServer(t, []zedTestAuth{
		{accountID: "acct_1", accessToken: "access-1"},
	}, func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/zed/edit-predictions", strings.NewReader(validZedEditPredictionBody)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.ServeHTTP(resp, req)
		close(done)
	}()
	<-upstreamStarted
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not stop after cancellation")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not canceled")
	}
	if resp.Body.Len() != 0 {
		t.Fatalf("canceled response body = %q, want empty", resp.Body.String())
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
	}
	if state, blocked := server.health.State("account:acct_1", "gpt-5.6-luna"); blocked {
		t.Fatalf("client cancellation penalized auth health: %#v", state)
	}

	todayResp := doJSONRequest(t, server, http.MethodGet, "/v0/user/usage/today", "", apiKey)
	var today UserUsageToday
	decodeResponse(t, todayResp, &today)
	if today.RequestCount != 1 || today.FailedRequestCount != 1 {
		t.Fatalf("usage = %#v, want one failed canceled request", today.UsageCounters)
	}
}

type zedTestAuth struct {
	accountID   string
	accessToken string
}

func newZedEditPredictionTestServer(
	t *testing.T,
	auths []zedTestAuth,
	upstreamHandler http.HandlerFunc,
	configure func(*Config),
) (*Server, string) {
	t.Helper()
	authDir := t.TempDir()
	for _, auth := range auths {
		writeAuthFile(t, authDir, auth.accountID+".json", `{
			"type": "codex",
			"account_id": "`+auth.accountID+`",
			"access_token": "`+auth.accessToken+`",
			"refresh_token": "refresh-`+auth.accountID+`",
			"expired": "2099-01-01T00:00:00Z"
		}`)
	}
	upstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(upstream.Close)
	cfg := &Config{
		AuthDir:        authDir,
		AdminAPIKey:    "admin-key",
		Database:       DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:   upstream.URL + "/backend-api/codex",
		ChatGPTBaseURL: upstream.URL + "/backend-api",
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

func assertSafeErrorResponse(t *testing.T, resp *httptest.ResponseRecorder) {
	t.Helper()
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	decodeResponse(t, resp, &payload)
	if payload.Error.Message == "" || payload.Error.Type == "" {
		t.Fatalf("error response = %#v, want safe message and type", payload)
	}
}

type zedEditPredictionTestResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		Text         string `json:"text"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage chatCompletionTestUsage `json:"usage"`
}
