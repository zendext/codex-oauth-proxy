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
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
)

func TestChatCompletionsPrecommitSSEFailuresUseExecutor(t *testing.T) {
	t.Run("request scoped error is sanitized", func(t *testing.T) {
		var calls atomic.Int32
		server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			writeChatUpstreamSSE(w,
				`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"secret upstream request detail"}}`,
			)
		}, nil)

		resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(true, "gpt-test"), apiKey)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400, body: %s", resp.Code, resp.Body.String())
		}
		if got := resp.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		if strings.Contains(resp.Body.String(), "secret upstream request detail") {
			t.Fatalf("response leaked upstream detail: %s", resp.Body.String())
		}
		if calls.Load() != 1 {
			t.Fatalf("upstream calls = %d, want request-scoped stop after one", calls.Load())
		}
	})

	t.Run("model failure fails over and records exclusion", func(t *testing.T) {
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
				writeChatUpstreamSSE(w,
					`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","code":"model_not_supported","message":"The requested model is not supported."}}}`,
				)
				return
			}
			writeChatUpstreamSSE(w,
				`{"type":"response.output_text.delta","delta":"ok"}`,
				`{"type":"response.completed","response":{"id":"resp_ok","model":"gpt-test"}}`,
			)
		}))
		defer upstream.Close()

		server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, func(cfg *Config) {
			cfg.RequestRetry = 0
			cfg.requestRetrySet = true
			cfg.MaxRetryInterval = 0
			cfg.maxRetryIntervalSet = true
		})
		resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(false, "gpt-test"), apiKey)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
		}
		var payload chatCompletionTestResponse
		decodeResponse(t, resp, &payload)
		if payload.Choices[0].Message.Content != "ok" {
			t.Fatalf("content = %q, want ok", payload.Choices[0].Message.Content)
		}

		mu.Lock()
		gotAuthorizations := slices.Clone(authorizations)
		mu.Unlock()
		wantAuthorizations := []string{"Bearer access-a", "Bearer access-b"}
		if !slices.Equal(gotAuthorizations, wantAuthorizations) {
			t.Fatalf("upstream authorizations = %v, want %v", gotAuthorizations, wantAuthorizations)
		}
		state, ok := server.health.State("account:acct_a", "gpt-test")
		if !ok || state.Kind != AuthHealthModelUnsupported {
			t.Fatalf("auth health = %#v, %t, want model exclusion", state, ok)
		}
	})

	t.Run("quota failure fails over and records cooldown", func(t *testing.T) {
		authDir := t.TempDir()
		writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
		writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.Header.Get("Authorization") == "Bearer access-a" {
				writeChatUpstreamSSE(w,
					`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"capacity exhausted","resets_in_seconds":300}}`,
				)
				return
			}
			writeChatUpstreamSSE(w,
				`{"type":"response.completed","response":{"id":"resp_ok","model":"gpt-test"}}`,
			)
		}))
		defer upstream.Close()

		server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, func(cfg *Config) {
			cfg.RequestRetry = 0
			cfg.requestRetrySet = true
			cfg.MaxRetryInterval = 0
			cfg.maxRetryIntervalSet = true
		})
		resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(false, "gpt-test"), apiKey)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
		}
		if calls.Load() != 2 {
			t.Fatalf("upstream calls = %d, want quota failover", calls.Load())
		}
		state, ok := server.health.State("account:acct_a", "")
		if !ok || state.Kind != AuthHealthQuota || state.RetryAt.IsZero() {
			t.Fatalf("auth health = %#v, %t, want quota cooldown", state, ok)
		}
	})
}

func TestChatCompletionsPrecommitSSEUnauthorizedRefreshes(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "auth.json", `{
		"type": "codex",
		"account_id": "acct_1",
		"access_token": "old-access",
		"refresh_token": "old-refresh",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	var tokenCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tokenCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "new-access",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") == "Bearer old-access" {
			writeChatUpstreamSSE(w,
				`{"type":"error","error":{"type":"authentication_error","code":"unauthorized","message":"expired"}}`,
			)
			return
		}
		writeChatUpstreamSSE(w,
			`{"type":"response.output_text.delta","delta":"ok"}`,
			`{"type":"response.completed","response":{"id":"resp_ok","model":"gpt-test"}}`,
		)
	}))
	defer upstream.Close()

	server, err := NewHandler(context.Background(), &Config{
		AuthDir:              authDir,
		AdminAPIKey:          "admin-key",
		Database:             DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:         upstream.URL + "/backend-api/codex",
		CodexRefreshTokenURL: tokenServer.URL,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	apiKey := createManagedUser(t, server, "admin-key", "Alice").PlaintextAPIKey

	resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(false, "gpt-test"), apiKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want unauthorized then refreshed retry", upstreamCalls.Load())
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("token calls = %d, want one refresh", tokenCalls.Load())
	}
}

func TestChatCompletionsCommittedFailuresEmitSanitizedSSEError(t *testing.T) {
	tests := []struct {
		name   string
		events []string
	}{
		{
			name: "terminal error",
			events: []string{
				`{"type":"response.output_text.delta","delta":"partial"}`,
				`{"type":"error","error":{"type":"upstream_error","code":"backend_failed","message":"secret terminal detail"}}`,
			},
		},
		{
			name: "malformed event",
			events: []string{
				`{"type":"response.created","response":{"id":"resp_1","model":"gpt-test"}}`,
				`{"type":`,
			},
		},
		{
			name: "eof before terminal",
			events: []string{
				`{"type":"response.output_text.delta","delta":"partial"}`,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				writeChatUpstreamSSE(w, test.events...)
			}, nil)

			resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(true, "gpt-test"), apiKey)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want committed 200, body: %s", resp.Code, resp.Body.String())
			}
			events := chatStreamDataLines(resp.Body.String())
			if len(events) < 2 {
				t.Fatalf("stream events = %#v, want role and error", events)
			}
			if slices.Contains(events, "[DONE]") {
				t.Fatalf("stream emitted [DONE] after failure: %s", resp.Body.String())
			}
			if strings.Contains(resp.Body.String(), `"finish_reason":"stop"`) ||
				strings.Contains(resp.Body.String(), `"finish_reason":"tool_calls"`) {
				t.Fatalf("stream emitted finish chunk after failure: %s", resp.Body.String())
			}
			var errorEnvelope map[string]any
			if err := json.Unmarshal([]byte(events[len(events)-1]), &errorEnvelope); err != nil {
				t.Fatalf("decode SSE error %q: %v", events[len(events)-1], err)
			}
			if _, ok := errorEnvelope["error"].(map[string]any); !ok {
				t.Fatalf("last event = %#v, want OpenAI error envelope", errorEnvelope)
			}
			if strings.Contains(resp.Body.String(), "secret terminal detail") {
				t.Fatalf("stream leaked upstream error detail: %s", resp.Body.String())
			}
		})
	}
}

func TestChatCompletionsRejectsMalformedTerminalSequences(t *testing.T) {
	tests := []struct {
		name   string
		events []string
	}{
		{
			name:   "malformed JSON",
			events: []string{`{"type":`},
		},
		{
			name:   "EOF before terminal",
			events: []string{`{"type":"response.output_text.delta","delta":"partial"}`},
		},
		{
			name: "duplicate terminal",
			events: []string{
				`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-test"}}`,
				`{"type":"response.incomplete","response":{"id":"resp_1","model":"gpt-test","incomplete_details":{"reason":"max_output_tokens"}}}`,
			},
		},
		{
			name: "data after terminal",
			events: []string{
				`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-test"}}`,
				`{"type":"response.output_text.delta","delta":"late"}`,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				writeChatUpstreamSSE(w, test.events...)
			}, func(cfg *Config) {
				cfg.RequestRetry = 0
				cfg.requestRetrySet = true
				cfg.MaxRetryInterval = 0
				cfg.maxRetryIntervalSet = true
			})

			resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(false, "gpt-test"), apiKey)
			if resp.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502, body: %s", resp.Code, resp.Body.String())
			}
			if strings.Contains(resp.Body.String(), "late") || strings.Contains(resp.Body.String(), `{"type":`) {
				t.Fatalf("error response leaked malformed upstream event: %s", resp.Body.String())
			}
		})
	}
}

func TestResponsesSSEReaderBoundsOneEvent(t *testing.T) {
	reader := io.MultiReader(
		strings.NewReader(`data: {"type":"future.event","payload":"`),
		io.LimitReader(repeatedByteReader('x'), 52_428_800),
		strings.NewReader("\"}\n\n"),
	)
	called := false
	err := readResponsesSSE(reader, func(sseEvent) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("readResponsesSSE returned nil error for event larger than 50 MB")
	}
	if called {
		t.Fatal("oversized SSE event reached the handler")
	}
}

func TestChatCompletionsIncompleteFinishReasons(t *testing.T) {
	tests := []struct {
		reason string
		want   string
	}{
		{reason: "max_tokens", want: "length"},
		{reason: "max_output_tokens", want: "length"},
		{reason: "content_filter", want: "content_filter"},
		{reason: "future_limit", want: "length"},
	}
	for _, test := range tests {
		for _, stream := range []bool{false, true} {
			name := test.reason + "/nonstream"
			if stream {
				name = test.reason + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
					writeChatUpstreamSSE(w,
						`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"codex\"}"}}`,
						`{"type":"response.incomplete","response":{"id":"resp_incomplete","model":"gpt-test","status":"incomplete","incomplete_details":{"reason":"`+test.reason+`"},"output":[],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`,
					)
				}, nil)

				resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(stream, "gpt-test"), apiKey)
				if resp.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
				}
				if strings.Contains(resp.Body.String(), "native_finish_reason") {
					t.Fatalf("response contains native_finish_reason: %s", resp.Body.String())
				}
				if stream {
					events := chatStreamDataLines(resp.Body.String())
					if events[len(events)-1] != "[DONE]" {
						t.Fatalf("last event = %q, want [DONE]", events[len(events)-1])
					}
					var gotFinish string
					for _, event := range events {
						if event == "[DONE]" {
							continue
						}
						chunk := decodeChatStreamChunk(t, event)
						if len(chunk.Choices) == 1 && chunk.Choices[0].FinishReason != "" {
							gotFinish = chunk.Choices[0].FinishReason
						}
					}
					if gotFinish != test.want {
						t.Fatalf("finish_reason = %q, want %q, body: %s", gotFinish, test.want, resp.Body.String())
					}
					return
				}
				var payload chatCompletionTestResponse
				decodeResponse(t, resp, &payload)
				if payload.Choices[0].FinishReason != test.want {
					t.Fatalf("finish_reason = %q, want %q", payload.Choices[0].FinishReason, test.want)
				}
				if len(payload.Choices[0].Message.ToolCalls) != 1 {
					t.Fatalf("tool_calls = %#v, want incomplete tool call output", payload.Choices[0].Message.ToolCalls)
				}
			})
		}
	}
}

func TestChatCompletionsReconstructsTerminalOutput(t *testing.T) {
	t.Run("nonstream terminal snapshot is authoritative", func(t *testing.T) {
		server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
			writeChatUpstreamSSE(w,
				`{"type":"response.output_text.delta","delta":"wrong"}`,
				`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"right"}]}]}}`,
			)
		}, nil)
		resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(false, "gpt-test"), apiKey)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
		}
		var payload chatCompletionTestResponse
		decodeResponse(t, resp, &payload)
		if payload.Choices[0].Message.Content != "right" {
			t.Fatalf("content = %q, want authoritative terminal output", payload.Choices[0].Message.Content)
		}
	})

	t.Run("output item fills missing terminal message", func(t *testing.T) {
		server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
			writeChatUpstreamSSE(w,
				`{"type":"response.output_text.delta","delta":"hel"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`,
				`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-test","output":[]}}`,
			)
		}, nil)
		resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(false, "gpt-test"), apiKey)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
		}
		var payload chatCompletionTestResponse
		decodeResponse(t, resp, &payload)
		if payload.Choices[0].Message.Content != "hello" {
			t.Fatalf("content = %q, want reconstructed hello", payload.Choices[0].Message.Content)
		}
	})

	t.Run("stream emits only missing text suffix", func(t *testing.T) {
		server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
			writeChatUpstreamSSE(w,
				`{"type":"response.output_text.delta","delta":"hel"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`,
				`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-test","output":[]}}`,
			)
		}, nil)
		resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(true, "gpt-test"), apiKey)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
		}
		if got := streamedChatText(t, resp.Body.String()); got != "hello" {
			t.Fatalf("streamed text = %q, want hello, body: %s", got, resp.Body.String())
		}
	})

	t.Run("stream emits only missing tool argument suffix", func(t *testing.T) {
		server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
			writeChatUpstreamSSE(w,
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"lookup"}}`,
				`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":\"co"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","name":"lookup","arguments":"{\"q\":\"codex\"}"}}`,
				`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-test","output":[{"type":"function_call","name":"lookup","arguments":"{\"q\":\"codex\"}"}]}}`,
			)
		}, nil)
		resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(true, "gpt-test"), apiKey)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
		}
		var arguments strings.Builder
		for _, event := range chatStreamDataLines(resp.Body.String()) {
			if event == "[DONE]" {
				continue
			}
			chunk := decodeChatStreamChunk(t, event)
			if len(chunk.Choices) == 1 && len(chunk.Choices[0].Delta.ToolCalls) == 1 {
				arguments.WriteString(chunk.Choices[0].Delta.ToolCalls[0].Function.Arguments)
			}
		}
		if arguments.String() != `{"q":"codex"}` {
			t.Fatalf("streamed arguments = %q, want complete arguments without duplication", arguments.String())
		}
	})

	t.Run("stream rejects non-prefix terminal conflict", func(t *testing.T) {
		server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
			writeChatUpstreamSSE(w,
				`{"type":"response.output_text.delta","delta":"abc"}`,
				`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ax"}]}]}}`,
			)
		}, nil)
		resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(true, "gpt-test"), apiKey)
		events := chatStreamDataLines(resp.Body.String())
		if slices.Contains(events, "[DONE]") {
			t.Fatalf("conflicting stream emitted [DONE]: %s", resp.Body.String())
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(events[len(events)-1]), &envelope); err != nil {
			t.Fatalf("decode final event: %v", err)
		}
		if _, ok := envelope["error"]; !ok {
			t.Fatalf("final event = %#v, want protocol error", envelope)
		}
	})
}

func TestChatStopFilterPreservesUnicodeAcrossBoundaries(t *testing.T) {
	filter := newChatStopFilter([]string{"🙂END"})
	var output strings.Builder
	for _, delta := range []string{"你好", "🙂E", "NDhidden"} {
		content, _ := filter.push(delta)
		if !utf8.ValidString(content) {
			t.Fatalf("stop filter emitted invalid UTF-8 chunk: %q", content)
		}
		output.WriteString(content)
	}
	if !utf8.ValidString(output.String()) {
		t.Fatalf("stop filter emitted invalid UTF-8 bytes: %q", output.String())
	}
	if output.String() != "你好" {
		t.Fatalf("stop-filtered output = %q, want 你好", output.String())
	}
}

func TestChatCompletionsTerminalSurvivesTrailingTransportError(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			server, apiKey := newChatTerminalTestServer(t, func(http.ResponseWriter, *http.Request) {}, nil)
			completed := `data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}}` + "\n\n"
			server.httpClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body:       io.NopCloser(io.MultiReader(strings.NewReader(completed), chatUnexpectedEOFReader{})),
					Request:    req,
				}, nil
			})

			resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(stream, "gpt-test"), apiKey)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
			}
			if stream {
				if !strings.Contains(resp.Body.String(), "[DONE]") || strings.Contains(resp.Body.String(), `"error"`) {
					t.Fatalf("stream did not preserve terminal success: %s", resp.Body.String())
				}
				return
			}
			var payload chatCompletionTestResponse
			decodeResponse(t, resp, &payload)
			if payload.Choices[0].Message.Content != "ok" {
				t.Fatalf("content = %q, want ok", payload.Choices[0].Message.Content)
			}
		})
	}
}

func TestChatCompletionsStopSuppressesLaterTextAndTools(t *testing.T) {
	server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeChatUpstreamSSE(w,
			`{"type":"response.output_text.delta","delta":"visible ST"}`,
			`{"type":"response.output_text.delta","delta":"OP hidden"}`,
			`{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup"}}`,
			`{"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_1","delta":"{\"q\":\"secret\"}"}`,
			`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-test","usage":{"input_tokens":5,"output_tokens":4,"total_tokens":9}}}`,
		)
	}, nil)

	body := `{
		"model":"gpt-test",
		"messages":[{"role":"user","content":"hello"}],
		"stream":true,
		"stream_options":{"include_usage":true},
		"stop":[" STOP"]
	}`
	resp := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", body, apiKey)
	if got := streamedChatText(t, resp.Body.String()); got != "visible" {
		t.Fatalf("streamed text = %q, want visible", got)
	}
	if strings.Contains(resp.Body.String(), "tool_calls") || strings.Contains(resp.Body.String(), "secret") {
		t.Fatalf("stream emitted post-stop tool output: %s", resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"finish_reason":"stop"`) ||
		!strings.Contains(resp.Body.String(), `"total_tokens":9`) ||
		!strings.Contains(resp.Body.String(), "[DONE]") {
		t.Fatalf("stream did not drain terminal state and usage: %s", resp.Body.String())
	}
}

func TestChatCompletionsWriteFailureCancelsUpstream(t *testing.T) {
	tests := []struct {
		name     string
		failFrom int
		want     int
	}{
		{name: "initial role", failFrom: 1, want: 1},
		{name: "later delta", failFrom: 3, want: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstreamCanceled := make(chan struct{})
			server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher, _ := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"one\"}\n\n")
				if flusher != nil {
					flusher.Flush()
				}
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"two\"}\n\n")
				if flusher != nil {
					flusher.Flush()
				}
				<-r.Context().Done()
				close(upstreamCanceled)
			}, nil)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatTerminalRequestBody(true, "gpt-test")))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			req.Header.Set("Content-Type", "application/json")
			writer := newChatFailingResponseWriter(test.failFrom)
			server.ServeHTTP(writer, req)

			if writer.writeCount() != test.want {
				t.Fatalf("write attempts = %d, want %d", writer.writeCount(), test.want)
			}
			if strings.Contains(writer.bodyString(), "[DONE]") || strings.Contains(writer.bodyString(), `"error"`) {
				t.Fatalf("write failure produced further events: %s", writer.bodyString())
			}
			select {
			case <-upstreamCanceled:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream request was not canceled after downstream write failure")
			}
			state, blocked := server.health.State("account:acct_1", "gpt-test")
			if blocked {
				t.Fatalf("write failure penalized auth health: %#v", state)
			}
		})
	}
}

func TestChatCompletionsWriteFailureDoesNotRetryOrRebind(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
	var calls atomic.Int32
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatTerminalRequestBody(true, "gpt-test")))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session-Id", "write-failure-session")
	writer := newChatFailingResponseWriter(1)
	server.ServeHTTP(writer, req)

	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want no retry after write failure", calls.Load())
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not canceled")
	}
	var boundAuthID string
	if err := server.users.db.QueryRow(`SELECT auth_id FROM session_affinity_bindings LIMIT 1`).Scan(&boundAuthID); err != nil {
		t.Fatalf("read session binding: %v", err)
	}
	if boundAuthID != "account:acct_a" {
		t.Fatalf("session binding auth_id = %q, want original account:acct_a", boundAuthID)
	}
}

func TestChatCompletionsClientCancellationWritesNothingFurther(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)
	var calls atomic.Int32
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()
	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, nil)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatTerminalRequestBody(true, "gpt-test"))).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session-Id", "client-cancel-session")
	writer := newChatFailingResponseWriter(0)
	done := make(chan struct{})
	go func() {
		server.ServeHTTP(writer, req)
		close(done)
	}()
	writer.waitForWrites(t, 2)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("chat request did not stop after client cancellation")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not canceled")
	}
	if strings.Contains(writer.bodyString(), "[DONE]") || strings.Contains(writer.bodyString(), `"error"`) {
		t.Fatalf("client cancellation produced further events: %s", writer.bodyString())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want no retry after cancellation", calls.Load())
	}
	if state, blocked := server.health.State("account:acct_a", "gpt-test"); blocked {
		t.Fatalf("client cancellation penalized auth health: %#v", state)
	}
	var boundAuthID string
	if err := server.users.db.QueryRow(`SELECT auth_id FROM session_affinity_bindings LIMIT 1`).Scan(&boundAuthID); err != nil {
		t.Fatalf("read session binding: %v", err)
	}
	if boundAuthID != "account:acct_a" {
		t.Fatalf("session binding auth_id = %q, want original account:acct_a", boundAuthID)
	}
}

func TestChatCompletionsRecordsDistinctOutcomesWithoutLeakingBodies(t *testing.T) {
	var logs bytes.Buffer
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})

	cancelStarted := make(chan struct{})
	server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte(`"model":"success-model"`)):
			writeChatUpstreamSSE(w,
				`{"type":"response.completed","response":{"id":"resp_success","model":"success-model","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`,
			)
		case bytes.Contains(body, []byte(`"model":"cancel-model"`)):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			close(cancelStarted)
			<-r.Context().Done()
		default:
			writeChatUpstreamSSE(w,
				`{"type":"response.output_text.delta","delta":"secret upstream response body"}`,
			)
		}
	}, func(cfg *Config) {
		cfg.Debug = true
		enabled := true
		cfg.Usage = UsageConfig{Enabled: &enabled, DebugOpenAIResponse: true}
		cfg.RequestRetry = 0
		cfg.requestRetrySet = true
		cfg.MaxRetryInterval = 0
		cfg.maxRetryIntervalSet = true
	})

	success := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(false, "success-model"), apiKey)
	if success.Code != http.StatusOK {
		t.Fatalf("success status = %d, body: %s", success.Code, success.Body.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancelReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatTerminalRequestBody(true, "cancel-model"))).WithContext(ctx)
	cancelReq.Header.Set("Authorization", "Bearer "+apiKey)
	cancelReq.Header.Set("Content-Type", "application/json")
	cancelWriter := newChatFailingResponseWriter(0)
	cancelDone := make(chan struct{})
	go func() {
		server.ServeHTTP(cancelWriter, cancelReq)
		close(cancelDone)
	}()
	<-cancelStarted
	cancelWriter.waitForWrites(t, 2)
	cancel()
	<-cancelDone

	failure := doJSONRequest(t, server, http.MethodPost, "/v1/chat/completions", chatTerminalRequestBody(false, "failure-model"), apiKey)
	if failure.Code != http.StatusBadGateway {
		t.Fatalf("failure status = %d, want 502, body: %s", failure.Code, failure.Body.String())
	}
	if strings.Contains(failure.Body.String(), "secret upstream response body") {
		t.Fatalf("failure response leaked upstream body: %s", failure.Body.String())
	}

	todayResp := doJSONRequest(t, server, http.MethodGet, "/v0/user/usage/today", "", apiKey)
	if todayResp.Code != http.StatusOK {
		t.Fatalf("usage status = %d, body: %s", todayResp.Code, todayResp.Body.String())
	}
	var today UserUsageToday
	decodeResponse(t, todayResp, &today)
	if today.RequestCount != 3 || today.FailedRequestCount != 2 || today.TotalTokens != 3 {
		t.Fatalf("usage = %#v, want 3 requests, 2 failed, 3 tokens", today.UsageCounters)
	}
	logText := logs.String()
	for _, outcome := range []string{"success", "upstream_failure", "client_canceled"} {
		if !strings.Contains(logText, "outcome="+outcome) {
			t.Fatalf("logs missing outcome %q: %s", outcome, logText)
		}
	}
	if strings.Contains(logText, "secret upstream response body") {
		t.Fatalf("logs leaked upstream body: %s", logText)
	}
}

func TestNativeResponsesHTTPAndWebSocketRemainTransparent(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "auth.json", `{
		"type": "codex",
		"account_id": "acct_1",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	httpPayload := "event: error\ndata: {not-json}\n\n"
	websocketPayload := []byte(`{"type":"response.failed","response":{"error":{"message":"unchanged"}}}`)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocketRequested(r) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upstream websocket upgrade: %v", err)
				return
			}
			defer conn.Close()
			if _, _, err = conn.ReadMessage(); err != nil {
				return
			}
			_ = conn.WriteMessage(websocket.TextMessage, websocketPayload)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, httpPayload)
	}))
	defer upstream.Close()

	server, err := NewHandler(context.Background(), &Config{
		AuthDir:        authDir,
		AdminAPIKey:    "admin-key",
		Database:       DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:   upstream.URL + "/backend-api/codex",
		ChatGPTBaseURL: upstream.URL + "/backend-api",
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	apiKey := createManagedUser(t, server, "admin-key", "Alice").PlaintextAPIKey

	httpResp := doJSONRequest(t, server, http.MethodPost, "/v1/responses", `{"model":"gpt-test","stream":true,"input":"hello"}`, apiKey)
	if httpResp.Code != http.StatusOK || httpResp.Body.String() != httpPayload {
		t.Fatalf("native Responses status=%d body=%q, want unchanged %q", httpResp.Code, httpResp.Body.String(), httpPayload)
	}

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
	defer conn.Close()
	if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-test","input":"hello"}`)); err != nil {
		t.Fatalf("write websocket: %v", err)
	}
	_, gotPayload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket: %v", err)
	}
	if !bytes.Equal(gotPayload, websocketPayload) {
		t.Fatalf("websocket payload = %s, want unchanged %s", gotPayload, websocketPayload)
	}
}

func newChatTerminalTestServer(t *testing.T, upstreamHandler http.HandlerFunc, configure func(*Config)) (*Server, string) {
	t.Helper()
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "auth.json", `{
		"type": "codex",
		"account_id": "acct_1",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"expired": "2099-01-01T00:00:00Z"
	}`)
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

func writeChatUpstreamSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		_, _ = io.WriteString(w, "data: "+event+"\n\n")
	}
}

func chatTerminalRequestBody(stream bool, model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}],"stream":` +
		map[bool]string{false: "false", true: "true"}[stream] + `}`
}

func streamedChatText(t *testing.T, body string) string {
	t.Helper()
	var text strings.Builder
	for _, event := range chatStreamDataLines(body) {
		if event == "[DONE]" {
			continue
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(event), &envelope); err != nil {
			t.Fatalf("decode stream event %q: %v", event, err)
		}
		if _, isError := envelope["error"]; isError {
			continue
		}
		chunk := decodeChatStreamChunk(t, event)
		if len(chunk.Choices) == 1 {
			text.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	return text.String()
}

type chatFailingResponseWriter struct {
	mu       sync.Mutex
	header   http.Header
	body     bytes.Buffer
	status   int
	writes   int
	failFrom int
	notify   chan struct{}
}

func newChatFailingResponseWriter(failFrom int) *chatFailingResponseWriter {
	return &chatFailingResponseWriter{
		header:   make(http.Header),
		failFrom: failFrom,
		notify:   make(chan struct{}, 32),
	}
}

func (w *chatFailingResponseWriter) Header() http.Header {
	return w.header
}

func (w *chatFailingResponseWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = status
	}
}

func (w *chatFailingResponseWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.writes++
	select {
	case w.notify <- struct{}{}:
	default:
	}
	if w.failFrom > 0 && w.writes >= w.failFrom {
		return 0, errors.New("injected downstream write failure")
	}
	return w.body.Write(data)
}

func (w *chatFailingResponseWriter) FlushError() error {
	return nil
}

func (w *chatFailingResponseWriter) writeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writes
}

func (w *chatFailingResponseWriter) bodyString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func (w *chatFailingResponseWriter) waitForWrites(t *testing.T, count int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for w.writeCount() < count {
		select {
		case <-w.notify:
		case <-deadline:
			t.Fatalf("write count = %d, want at least %d", w.writeCount(), count)
		}
	}
}

type repeatedByteReader byte

func (r repeatedByteReader) Read(data []byte) (int, error) {
	for index := range data {
		data[index] = byte(r)
	}
	return len(data), nil
}

type chatUnexpectedEOFReader struct{}

func (chatUnexpectedEOFReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}
