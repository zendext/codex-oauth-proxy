package codexonly

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestChatCompletionsNonStreamReturnsAggregatedMessage(t *testing.T) {
	handler, userKey, saw := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/responses" {
			t.Errorf("upstream path = %q, want /backend-api/codex/responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"ok"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_ok","model":"gpt-5.3-codex","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}

`))
	})

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[
			{"role":"system","content":"Be concise."},
			{"role":"user","content":"say ok"}
		],
		"max_completion_tokens":8,
		"stream":false
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}

	var payload chatCompletionTestResponse
	decodeResponse(t, resp, &payload)
	if payload.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", payload.Object)
	}
	if payload.ID != "resp_ok" {
		t.Fatalf("id = %q, want resp_ok", payload.ID)
	}
	if len(payload.Choices) != 1 || payload.Choices[0].Message.Role != "assistant" || payload.Choices[0].Message.Content != "ok" {
		t.Fatalf("message = %#v, want assistant ok", payload.Choices)
	}
	if payload.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", payload.Choices[0].FinishReason)
	}
	if payload.Usage.PromptTokens != 3 || payload.Usage.CompletionTokens != 1 || payload.Usage.TotalTokens != 4 {
		t.Fatalf("usage = %#v, want 3/1/4", payload.Usage)
	}

	upstreamReq := saw.request(t)
	if upstreamReq["stream"] != true {
		t.Fatalf("upstream stream = %#v, want true", upstreamReq["stream"])
	}
	if upstreamReq["instructions"] != "Be concise." {
		t.Fatalf("instructions = %#v, want system text", upstreamReq["instructions"])
	}
	if _, ok := upstreamReq["max_completion_tokens"]; ok {
		t.Fatalf("upstream request should not include max_completion_tokens: %#v", upstreamReq)
	}
	if _, ok := upstreamReq["max_output_tokens"]; ok {
		t.Fatalf("upstream request should not include max_output_tokens: %#v", upstreamReq)
	}
	input, ok := upstreamReq["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %#v, want one message", upstreamReq["input"])
	}
	message, ok := input[0].(map[string]any)
	if !ok || message["role"] != "user" || message["content"] != "say ok" {
		t.Fatalf("input message = %#v, want user say ok", input[0])
	}
}

func TestChatCompletionsSuppliesDefaultInstructions(t *testing.T) {
	handler, userKey, saw := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"ok"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_default_instructions","model":"gpt-5.3-codex","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}

`))
	})

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[{"role":"user","content":"say ok"}],
		"stream":false
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}

	upstreamReq := saw.request(t)
	if upstreamReq["instructions"] != "You are a helpful assistant." {
		t.Fatalf("instructions = %#v, want default instructions", upstreamReq["instructions"])
	}
	if upstreamReq["store"] != false {
		t.Fatalf("store = %#v, want false", upstreamReq["store"])
	}
}

func TestChatCompletionsStreamReturnsChunksAndDone(t *testing.T) {
	handler, userKey, _ := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":" ok"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":" "}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_stream","model":"gpt-5.3-codex","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}

`))
	})

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[{"role":"user","content":"say ok"}],
		"stream":true
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	events := chatStreamDataLines(resp.Body.String())
	if len(events) < 5 {
		t.Fatalf("stream events = %#v, want role, two deltas, finish, done", events)
	}
	if events[len(events)-1] != "[DONE]" {
		t.Fatalf("last event = %q, want [DONE]", events[len(events)-1])
	}
	roleChunk := decodeChatStreamChunk(t, events[0])
	if roleChunk.Object != "chat.completion.chunk" || len(roleChunk.Choices) != 1 || roleChunk.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("first chunk = %#v, want assistant role", roleChunk)
	}
	var gotText strings.Builder
	for _, event := range events[:len(events)-1] {
		chunk := decodeChatStreamChunk(t, event)
		if len(chunk.Choices) == 1 {
			gotText.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if gotText.String() != " ok " {
		t.Fatalf("streamed text = %q, want spaces preserved", gotText.String())
	}
	finishChunk := decodeChatStreamChunk(t, events[len(events)-2])
	if len(finishChunk.Choices) != 1 || finishChunk.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish chunk = %#v, want stop", finishChunk)
	}
}

func TestChatCompletionsStreamIncludesUsageChunk(t *testing.T) {
	handler, userKey, _ := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"ok"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_usage","model":"gpt-5.3-codex","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}

`))
	})

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[{"role":"user","content":"say ok"}],
		"stream":true,
		"stream_options":{"include_usage":true}
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}

	events := chatStreamDataLines(resp.Body.String())
	if events[len(events)-1] != "[DONE]" {
		t.Fatalf("last event = %q, want [DONE]", events[len(events)-1])
	}
	usageChunk := decodeChatStreamChunk(t, events[len(events)-2])
	if len(usageChunk.Choices) != 0 {
		t.Fatalf("usage chunk choices = %#v, want empty", usageChunk.Choices)
	}
	if usageChunk.Usage.PromptTokens != 5 || usageChunk.Usage.CompletionTokens != 2 || usageChunk.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %#v, want 5/2/7", usageChunk.Usage)
	}
}

func TestChatCompletionsMapsJSONSchemaResponseFormat(t *testing.T) {
	handler, userKey, saw := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"{\"status\":\"ok\"}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_json","model":"gpt-5.3-codex","usage":{"input_tokens":8,"output_tokens":4,"total_tokens":12}}}

`))
	})

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[{"role":"user","content":"json"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"result","schema":{"type":"object","properties":{"status":{"type":"string"}},"required":["status"],"additionalProperties":false},"strict":true}},
		"reasoning_effort":"low",
		"verbosity":"low"
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	var payload chatCompletionTestResponse
	decodeResponse(t, resp, &payload)
	var content map[string]string
	if err := json.Unmarshal([]byte(payload.Choices[0].Message.Content), &content); err != nil {
		t.Fatalf("message content is not JSON: %q: %v", payload.Choices[0].Message.Content, err)
	}
	if content["status"] != "ok" {
		t.Fatalf("JSON content = %#v, want status ok", content)
	}

	upstreamReq := saw.request(t)
	text, ok := upstreamReq["text"].(map[string]any)
	if !ok {
		t.Fatalf("upstream text = %#v, want object", upstreamReq["text"])
	}
	format, ok := text["format"].(map[string]any)
	if !ok || format["type"] != "json_schema" || format["name"] != "result" {
		t.Fatalf("text.format = %#v, want json_schema result", text["format"])
	}
	reasoning, ok := upstreamReq["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "low" {
		t.Fatalf("reasoning = %#v, want low effort", upstreamReq["reasoning"])
	}
	if text["verbosity"] != "low" {
		t.Fatalf("text.verbosity = %#v, want low", text["verbosity"])
	}
}

func TestChatCompletionsNormalizesReasoningEffort(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "minimal", in: "minimal", want: "low"},
		{name: "max", in: "max", want: "xhigh"},
		{name: "none", in: "none", want: "none"},
		{name: "low", in: "low", want: "low"},
		{name: "medium", in: "medium", want: "medium"},
		{name: "high", in: "high", want: "high"},
		{name: "xhigh", in: "xhigh", want: "xhigh"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, userKey, saw := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"ok"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_reasoning","model":"gpt-5.3-codex","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}

`))
			})

			resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
				"model":"gpt-5.3-codex",
				"messages":[{"role":"user","content":"say ok"}],
				"reasoning_effort":"`+tt.in+`"
			}`, userKey)
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
			}

			reasoning, ok := saw.request(t)["reasoning"].(map[string]any)
			if !ok || reasoning["effort"] != tt.want {
				t.Fatalf("reasoning = %#v, want effort %q", saw.request(t)["reasoning"], tt.want)
			}
		})
	}
}

func TestChatCompletionsIgnoresUnsupportedSamplingParameters(t *testing.T) {
	handler, userKey, saw := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"ok"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_sampling","model":"gpt-5.3-codex","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}

`))
	})

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[{"role":"user","content":"say ok"}],
		"temperature":0,
		"top_p":1
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}

	upstreamReq := saw.request(t)
	if _, ok := upstreamReq["temperature"]; ok {
		t.Fatalf("upstream request should not include temperature: %#v", upstreamReq)
	}
	if _, ok := upstreamReq["top_p"]; ok {
		t.Fatalf("upstream request should not include top_p: %#v", upstreamReq)
	}
}

func TestChatCompletionsToolChoiceReturnsToolCalls(t *testing.T) {
	handler, userKey, saw := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.output_item.added
data: {"type":"response.output_item.added","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"q\":\"codex\"}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_tool","model":"gpt-5.3-codex","usage":{"input_tokens":9,"output_tokens":3,"total_tokens":12}}}

`))
	})

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[{"role":"user","content":"lookup codex"}],
		"tools":[{"type":"function","function":{"name":"lookup","description":"Search","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}}
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	var payload chatCompletionTestResponse
	decodeResponse(t, resp, &payload)
	if payload.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", payload.Choices[0].FinishReason)
	}
	calls := payload.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Type != "function" || calls[0].Function.Name != "lookup" || calls[0].Function.Arguments != `{"q":"codex"}` {
		t.Fatalf("tool_calls = %#v, want lookup call", calls)
	}

	upstreamReq := saw.request(t)
	tools, ok := upstreamReq["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("upstream tools = %#v, want one function tool", upstreamReq["tools"])
	}
	tool, ok := tools[0].(map[string]any)
	if !ok || tool["type"] != "function" || tool["name"] != "lookup" {
		t.Fatalf("upstream tool = %#v, want function lookup", tools[0])
	}
	choice, ok := upstreamReq["tool_choice"].(map[string]any)
	if !ok || choice["type"] != "function" || choice["name"] != "lookup" {
		t.Fatalf("tool_choice = %#v, want forced lookup", upstreamReq["tool_choice"])
	}
}

func TestChatCompletionsStreamConvertsCompletedToolCallItem(t *testing.T) {
	handler, userKey, _ := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.output_item.done
data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"codex\"}"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_tool_stream","model":"gpt-5.3-codex","usage":{"input_tokens":9,"output_tokens":3,"total_tokens":12}}}

`))
	})

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[{"role":"user","content":"lookup codex"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"stream":true
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}

	events := chatStreamDataLines(resp.Body.String())
	var gotToolCall bool
	for _, event := range events {
		if event == "[DONE]" {
			continue
		}
		chunk := decodeChatStreamChunk(t, event)
		if len(chunk.Choices) != 1 || len(chunk.Choices[0].Delta.ToolCalls) != 1 {
			continue
		}
		call := chunk.Choices[0].Delta.ToolCalls[0]
		if call.ID == "call_1" && call.Type == "function" && call.Function.Name == "lookup" && call.Function.Arguments == `{"q":"codex"}` {
			gotToolCall = true
		}
	}
	if !gotToolCall {
		t.Fatalf("stream did not include completed tool call arguments: %s", resp.Body.String())
	}
}

func TestChatCompletionsReplaysToolCallHistory(t *testing.T) {
	handler, userKey, saw := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"final"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_final","model":"gpt-5.3-codex","usage":{"input_tokens":13,"output_tokens":2,"total_tokens":15}}}

`))
	})

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[
			{"role":"user","content":"lookup codex"},
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"codex\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"{\"result\":\"ok\"}"}
		]
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	var payload chatCompletionTestResponse
	decodeResponse(t, resp, &payload)
	if payload.Choices[0].Message.Content != "final" {
		t.Fatalf("message content = %q, want final", payload.Choices[0].Message.Content)
	}

	input, ok := saw.request(t)["input"].([]any)
	if !ok || len(input) != 3 {
		t.Fatalf("upstream input = %#v, want user, function_call, function_call_output", saw.request(t)["input"])
	}
	call, ok := input[1].(map[string]any)
	if !ok || call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "lookup" || call["arguments"] != `{"q":"codex"}` {
		t.Fatalf("function_call replay = %#v, want call_1 lookup", input[1])
	}
	output, ok := input[2].(map[string]any)
	if !ok || output["type"] != "function_call_output" || output["call_id"] != "call_1" || output["output"] != `{"result":"ok"}` {
		t.Fatalf("function_call_output replay = %#v, want call_1 output", input[2])
	}
}

func TestChatCompletionsMapsHonchoCompatibilityParameters(t *testing.T) {
	handler, userKey, saw := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"ok"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_params","model":"gpt-5.3-codex","usage":{"input_tokens":6,"output_tokens":1,"total_tokens":7}}}

`))
	})

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[{"role":"user","content":"say ok"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],
		"tool_choice":"any",
		"stop":["Observation:"],
		"reasoning_effort":"high",
		"reasoning":{"max_tokens":2048,"summary":"auto"}
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}

	upstreamReq := saw.request(t)
	if upstreamReq["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %#v, want required", upstreamReq["tool_choice"])
	}
	if _, ok := upstreamReq["stop"]; ok {
		t.Fatalf("upstream request should not include stop: %#v", upstreamReq)
	}
	reasoning, ok := upstreamReq["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning = %#v, want object", upstreamReq["reasoning"])
	}
	if reasoning["effort"] != "high" || reasoning["summary"] != "auto" {
		t.Fatalf("reasoning = %#v, want effort high and summary auto", reasoning)
	}
	maxTokens, ok := reasoning["max_tokens"].(float64)
	if !ok || maxTokens != 2048 {
		t.Fatalf("reasoning.max_tokens = %#v, want 2048", reasoning["max_tokens"])
	}
}

func TestChatCompletionsAppliesStopSequencesWithoutForwardingStop(t *testing.T) {
	handler, userKey, _ := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"visible STOP hidden"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_stop","model":"gpt-5.3-codex","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}

`))
	})

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[{"role":"user","content":"say visible"}],
		"stop":[" STOP"],
		"stream":false
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}

	var payload chatCompletionTestResponse
	decodeResponse(t, resp, &payload)
	if payload.Choices[0].Message.Content != "visible" {
		t.Fatalf("message content = %q, want visible", payload.Choices[0].Message.Content)
	}
}

func TestChatCompletionsStreamAppliesStopSequencesAcrossChunks(t *testing.T) {
	handler, userKey, _ := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"vis"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"ible ST"}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"OP hidden"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_stream_stop","model":"gpt-5.3-codex","usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}}

`))
	})

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[{"role":"user","content":"say visible"}],
		"stop":[" STOP"],
		"stream":true,
		"stream_options":{"include_usage":true}
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}

	events := chatStreamDataLines(resp.Body.String())
	var gotText strings.Builder
	var gotUsage bool
	for _, event := range events {
		if event == "[DONE]" {
			continue
		}
		chunk := decodeChatStreamChunk(t, event)
		if len(chunk.Choices) == 1 {
			gotText.WriteString(chunk.Choices[0].Delta.Content)
		}
		if len(chunk.Choices) == 0 && chunk.Usage.TotalTokens == 7 {
			gotUsage = true
		}
	}
	if gotText.String() != "visible" {
		t.Fatalf("streamed text = %q, want visible", gotText.String())
	}
	if !gotUsage {
		t.Fatalf("stream did not include usage chunk: %s", resp.Body.String())
	}
}

func TestChatCompletionsIncludesUsageDetails(t *testing.T) {
	handler, userKey, _ := newChatCompletionTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"ok"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_usage_details","model":"gpt-5.3-codex","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":3}}}}

`))
	})

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/chat/completions", `{
		"model":"gpt-5.3-codex",
		"messages":[{"role":"user","content":"say ok"}],
		"stream":false
	}`, userKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}

	var payload chatCompletionTestResponse
	decodeResponse(t, resp, &payload)
	if payload.Usage.PromptTokensDetails.CachedTokens != 4 {
		t.Fatalf("prompt_tokens_details.cached_tokens = %d, want 4", payload.Usage.PromptTokensDetails.CachedTokens)
	}
	if payload.Usage.CompletionTokensDetails.ReasoningTokens != 3 {
		t.Fatalf("completion_tokens_details.reasoning_tokens = %d, want 3", payload.Usage.CompletionTokensDetails.ReasoningTokens)
	}
}

type chatCompletionUpstreamRequest struct {
	mu   sync.Mutex
	body map[string]any
}

func (s *chatCompletionUpstreamRequest) set(t *testing.T, body []byte) {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode upstream request %q: %v", string(body), err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = decoded
}

func (s *chatCompletionUpstreamRequest) request(t *testing.T) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.body == nil {
		t.Fatal("upstream request was not captured")
	}
	return s.body
}

func newChatCompletionTestHandler(t *testing.T, upstreamHandler http.HandlerFunc) (http.Handler, string, *chatCompletionUpstreamRequest) {
	t.Helper()
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"account_id": "acct_1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	saw := &chatCompletionUpstreamRequest{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer access-1" {
			t.Errorf("upstream Authorization = %q, want Bearer access-1", got)
		}
		if got := r.Header.Get("Chatgpt-Account-Id"); got != "acct_1" {
			t.Errorf("upstream Chatgpt-Account-Id = %q, want acct_1", got)
		}
		body, _ := io.ReadAll(r.Body)
		saw.set(t, body)
		upstreamHandler(w, r)
	}))
	t.Cleanup(upstream.Close)

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:      authDir,
		AdminAPIKey:  "admin-key",
		Database:     DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL: upstream.URL + "/backend-api/codex",
		RequestRetry: 1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	userKey := createManagedUser(t, handler, "admin-key", "Alice").PlaintextAPIKey
	return handler, userKey, saw
}

func chatStreamDataLines(body string) []string {
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return lines
}

func decodeChatStreamChunk(t *testing.T, data string) chatCompletionTestChunk {
	t.Helper()
	if data == "[DONE]" {
		t.Fatal("cannot decode [DONE] as chat chunk")
	}
	var chunk chatCompletionTestChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		t.Fatalf("decode stream chunk %q: %v", data, err)
	}
	return chunk
}

type chatCompletionTestResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string                       `json:"role"`
			Content   string                       `json:"content"`
			ToolCalls []chatCompletionTestToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage chatCompletionTestUsage `json:"usage"`
}

type chatCompletionTestChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Role      string                       `json:"role"`
			Content   string                       `json:"content"`
			ToolCalls []chatCompletionTestToolCall `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage chatCompletionTestUsage `json:"usage"`
}

type chatCompletionTestToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatCompletionTestUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}
