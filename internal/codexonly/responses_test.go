package codexonly

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestResponsesCreateAionUINonStreamNormalizesAndAggregates(t *testing.T) {
	var upstreamRequest map[string]any
	server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&upstreamRequest); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		writeChatUpstreamSSE(w,
			`{"type":"response.completed","response":{"id":"resp_aion","object":"response","model":"gpt-test","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}}`,
		)
	}, nil)

	resp := doJSONRequest(t, server, http.MethodPost, "/v1/responses", `{
		"model":"gpt-test",
		"input":"Reply with exactly OK.",
		"max_output_tokens":16
	}`, apiKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if _, ok := upstreamRequest["max_output_tokens"]; ok {
		t.Fatalf("upstream request retained max_output_tokens: %#v", upstreamRequest)
	}
	if upstreamRequest["stream"] != true || upstreamRequest["store"] != false {
		t.Fatalf("upstream stream/store = %#v/%#v, want true/false", upstreamRequest["stream"], upstreamRequest["store"])
	}

	var response map[string]any
	decodeResponse(t, resp, &response)
	if response["id"] != "resp_aion" {
		t.Fatalf("response id = %#v, want resp_aion", response["id"])
	}
	if _, ok := response["output"].([]any); !ok {
		t.Fatalf("response output = %#v, want preserved array", response["output"])
	}
	usage, ok := response["usage"].(map[string]any)
	if !ok || usage["total_tokens"] != float64(6) {
		t.Fatalf("response usage = %#v, want total_tokens 6", response["usage"])
	}
}

func TestResponsesCreateStreamNormalizesRequestAndPreservesSSE(t *testing.T) {
	var upstreamRequest map[string]any
	var upstreamHeader http.Header
	upstreamSSE := ": keep this comment\r\nevent: response.output_text.delta\r\nid: 7\r\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\r\n\r\n"
	server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHeader = r.Header.Clone()
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&upstreamRequest); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, upstreamSSE)
	}, nil)

	req := httptest.NewRequest(http.MethodPost, responsesCreatePath, strings.NewReader(`{
		"model":"gpt-test",
		"stream":true,
		"store":false,
		"background":false,
		"max_output_tokens":16,
		"input":[{"role":"user","content":"hello"}],
		"tools":[{"type":"function","name":"lookup"}],
		"tool_choice":"auto",
		"client_metadata":{"origin":"codex_cli_rs"},
		"stream_options":{"include_obfuscation":true},
		"unknown_public_field":{"keep":true}
	}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session-Id", "explicit-session")
	req.Header.Set("OpenAI-Beta", "custom-beta=v1")
	req.Header.Set("X-Client-Capability", "responses-v1")
	req.Header.Set("Connection", "X-Hop-Only")
	req.Header.Set("X-Hop-Only", "must-not-forward")
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Body.String() != upstreamSSE {
		t.Fatalf("stream status=%d body=%q, want unchanged %q", resp.Code, resp.Body.String(), upstreamSSE)
	}
	if _, ok := upstreamRequest["max_output_tokens"]; ok {
		t.Fatalf("upstream request retained max_output_tokens: %#v", upstreamRequest)
	}
	for _, field := range []string{"input", "tools", "tool_choice", "client_metadata", "stream_options", "unknown_public_field"} {
		if _, ok := upstreamRequest[field]; !ok {
			t.Errorf("upstream request dropped %s: %#v", field, upstreamRequest)
		}
	}
	if upstreamRequest["stream"] != true || upstreamRequest["store"] != false {
		t.Fatalf("upstream stream/store = %#v/%#v, want true/false", upstreamRequest["stream"], upstreamRequest["store"])
	}
	if upstreamRequest["background"] != false {
		t.Fatalf("upstream background = %#v, want accepted false", upstreamRequest["background"])
	}
	for header, want := range map[string]string{
		"Session-Id":          "explicit-session",
		"OpenAI-Beta":         "custom-beta=v1",
		"X-Client-Capability": "responses-v1",
	} {
		if got := upstreamHeader.Get(header); got != want {
			t.Errorf("upstream %s = %q, want %q", header, got, want)
		}
	}
	if got := upstreamHeader.Get("X-Hop-Only"); got != "" {
		t.Errorf("upstream hop-by-hop header = %q, want empty", got)
	}
	if got := upstreamHeader.Get("Authorization"); got != "Bearer access-1" {
		t.Errorf("upstream Authorization = %q, want OAuth access token", got)
	}
}

func TestPrivateResponsesRemainByteTransparent(t *testing.T) {
	requestBody := " { \"model\" : \"gpt-test\", \"max_output_tokens\" : 16 }\n"
	responseBody := "{\"detail\":\"private unchanged\"}\n"
	var upstreamBody []byte
	server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}, nil)

	resp := doJSONRequest(t, server, http.MethodPost, "/backend-api/codex/responses", requestBody, apiKey)
	if !bytes.Equal(upstreamBody, []byte(requestBody)) {
		t.Fatalf("private upstream body = %q, want %q", upstreamBody, requestBody)
	}
	if resp.Code != http.StatusOK || resp.Body.String() != responseBody {
		t.Fatalf("private response status=%d body=%q, want unchanged", resp.Code, resp.Body.String())
	}
}

func TestResponsesCreateRejectsInvalidStatelessParametersWithoutUpstream(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		param    string
		wantCode string
	}{
		{name: "zero max", body: `{"max_output_tokens":0}`, param: "max_output_tokens", wantCode: "invalid_value"},
		{name: "negative max", body: `{"max_output_tokens":-1}`, param: "max_output_tokens", wantCode: "invalid_value"},
		{name: "fractional max", body: `{"max_output_tokens":1.5}`, param: "max_output_tokens", wantCode: "invalid_value"},
		{name: "string max", body: `{"max_output_tokens":"16"}`, param: "max_output_tokens", wantCode: "invalid_value"},
		{name: "boolean max", body: `{"max_output_tokens":true}`, param: "max_output_tokens", wantCode: "invalid_value"},
		{name: "null max", body: `{"max_output_tokens":null}`, param: "max_output_tokens", wantCode: "invalid_value"},
		{name: "overflow max", body: `{"max_output_tokens":9223372036854775808}`, param: "max_output_tokens", wantCode: "invalid_value"},
		{name: "background true", body: `{"background":true}`, param: "background", wantCode: "unsupported_value"},
		{name: "background string", body: `{"background":"true"}`, param: "background", wantCode: "invalid_value"},
		{name: "background number", body: `{"background":1}`, param: "background", wantCode: "invalid_value"},
		{name: "background null", body: `{"background":null}`, param: "background", wantCode: "invalid_value"},
		{name: "background object", body: `{"background":{}}`, param: "background", wantCode: "invalid_value"},
		{name: "store true", body: `{"store":true}`, param: "store", wantCode: "unsupported_value"},
		{name: "store string", body: `{"stream":true,"store":"true"}`, param: "store", wantCode: "invalid_value"},
		{name: "store number", body: `{"store":1}`, param: "store", wantCode: "invalid_value"},
		{name: "store null", body: `{"store":null}`, param: "store", wantCode: "invalid_value"},
		{name: "store object", body: `{"store":{}}`, param: "store", wantCode: "invalid_value"},
	}

	var upstreamCalls atomic.Int32
	server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}, nil)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resp := doJSONRequest(t, server, http.MethodPost, responsesCreatePath, test.body, apiKey)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body: %s", resp.Code, resp.Body.String())
			}
			var envelope struct {
				Error struct {
					Type  string `json:"type"`
					Code  string `json:"code"`
					Param string `json:"param"`
				} `json:"error"`
			}
			decodeResponse(t, resp, &envelope)
			if envelope.Error.Type != "invalid_request_error" || envelope.Error.Param != test.param || envelope.Error.Code != test.wantCode {
				t.Fatalf("error = %#v, want deterministic param %q", envelope.Error, test.param)
			}
		})
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls.Load())
	}
}

func TestResponsesCreateNormalizesUpstreamDetailError(t *testing.T) {
	server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"detail":"Unsupported parameter: max_output_tokens"}`)
	}, nil)

	resp := doJSONRequest(t, server, http.MethodPost, responsesCreatePath, `{"model":"gpt-test","stream":true}`, apiKey)
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", resp.Code, resp.Body.String())
	}
	var payload map[string]any
	decodeResponse(t, resp, &payload)
	if _, ok := payload["detail"]; ok {
		t.Fatalf("response retained private detail envelope: %#v", payload)
	}
	errorPayload, ok := payload["error"].(map[string]any)
	if !ok || errorPayload["message"] != "Unsupported parameter: max_output_tokens" {
		t.Fatalf("response error = %#v", payload["error"])
	}
}

func TestResponsesCreateNonStreamTerminalSemantics(t *testing.T) {
	tests := []struct {
		name       string
		events     []string
		wantStatus int
		wantID     string
	}{
		{name: "premature EOF", events: []string{`{"type":"response.created","response":{"id":"resp_1"}}`}, wantStatus: http.StatusBadGateway},
		{name: "malformed event", events: []string{`not-json`}, wantStatus: http.StatusBadGateway},
		{name: "duplicate terminal", events: []string{
			`{"type":"response.completed","response":{"id":"resp_1"}}`,
			`{"type":"response.completed","response":{"id":"resp_2"}}`,
		}, wantStatus: http.StatusBadGateway},
		{name: "post-terminal data", events: []string{
			`{"type":"response.completed","response":{"id":"resp_1"}}`,
			`{"type":"response.output_text.delta","delta":"late"}`,
		}, wantStatus: http.StatusBadGateway},
		{name: "failed", events: []string{`{"type":"response.failed","response":{"error":{"type":"invalid_request_error","message":"secret upstream detail"}}}`}, wantStatus: http.StatusBadRequest},
		{name: "error", events: []string{`{"type":"error","error":{"type":"invalid_request_error","message":"secret upstream detail"}}`}, wantStatus: http.StatusBadRequest},
		{name: "incomplete missing response", events: []string{`{"type":"response.incomplete"}`}, wantStatus: http.StatusBadGateway},
		{name: "incomplete null response", events: []string{`{"type":"response.incomplete","response":null}`}, wantStatus: http.StatusBadGateway},
		{name: "valid incomplete", events: []string{`{"type":"response.incomplete","response":{"id":"resp_incomplete","status":"incomplete","output":[],"usage":{"total_tokens":3}}}`}, wantStatus: http.StatusOK, wantID: "resp_incomplete"},
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

			resp := doJSONRequest(t, server, http.MethodPost, responsesCreatePath, `{"model":"gpt-test"}`, apiKey)
			if resp.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d, body: %s", resp.Code, test.wantStatus, resp.Body.String())
			}
			if strings.Contains(resp.Body.String(), "secret upstream detail") {
				t.Fatalf("response leaked upstream event detail: %s", resp.Body.String())
			}
			if test.wantID != "" {
				var response map[string]any
				decodeResponse(t, resp, &response)
				if response["id"] != test.wantID {
					t.Fatalf("response id = %#v, want %q", response["id"], test.wantID)
				}
			}
		})
	}
}

func TestResponsesCreateCancellationStopsUpstream(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	server, apiKey := newChatTerminalTestServer(t, func(w http.ResponseWriter, r *http.Request) {
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
	req := httptest.NewRequest(http.MethodPost, responsesCreatePath, strings.NewReader(`{"model":"gpt-test"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.ServeHTTP(recorder, req)
		close(done)
	}()
	<-upstreamStarted
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after cancellation")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request was not canceled")
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("canceled response body = %q, want empty", recorder.Body.String())
	}
}

func TestResponsesCreateNonStreamFailoverPreservesSessionAffinity(t *testing.T) {
	authDir := t.TempDir()
	writeSessionAffinityAuth(t, authDir, "a.json", "acct_a", "access-a", false)
	writeSessionAffinityAuth(t, authDir, "b.json", "acct_b", "access-b", false)

	var mu sync.Mutex
	var authorizations []string
	var upstreamRequests []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&payload); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		authorization := r.Header.Get("Authorization")
		mu.Lock()
		authorizations = append(authorizations, authorization)
		upstreamRequests = append(upstreamRequests, payload)
		mu.Unlock()
		if authorization == "Bearer access-a" {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		writeChatUpstreamSSE(w,
			`{"type":"response.completed","response":{"id":"resp_affinity","object":"response","output":[],"usage":{"total_tokens":1}}}`,
		)
	}))
	defer upstream.Close()

	server, apiKey := newFailoverTestServer(t, authDir, upstream.URL, func(cfg *Config) {
		cfg.RequestRetry = 0
		cfg.requestRetrySet = true
		cfg.MaxRetryInterval = 0
		cfg.maxRetryIntervalSet = true
	})
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, responsesCreatePath, strings.NewReader(`{"model":"gpt-test","input":"hello"}`))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Session-Id", "responses-non-stream-affinity")
		resp := httptest.NewRecorder()
		server.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
		}
		var response map[string]any
		decodeResponse(t, resp, &response)
		if response["id"] != "resp_affinity" {
			t.Fatalf("response id = %#v, want resp_affinity", response["id"])
		}
	}

	mu.Lock()
	gotAuthorizations := append([]string(nil), authorizations...)
	gotRequests := append([]map[string]any(nil), upstreamRequests...)
	mu.Unlock()
	wantAuthorizations := []string{"Bearer access-a", "Bearer access-b", "Bearer access-b"}
	if len(gotAuthorizations) != len(wantAuthorizations) {
		t.Fatalf("upstream authorizations = %v, want %v", gotAuthorizations, wantAuthorizations)
	}
	for i := range wantAuthorizations {
		if gotAuthorizations[i] != wantAuthorizations[i] {
			t.Fatalf("upstream authorizations = %v, want %v", gotAuthorizations, wantAuthorizations)
		}
	}
	for i, request := range gotRequests {
		if request["stream"] != true || request["store"] != false {
			t.Fatalf("upstream request #%d stream/store = %#v/%#v, want true/false", i+1, request["stream"], request["store"])
		}
	}
}
