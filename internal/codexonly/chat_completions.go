package codexonly

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultChatCompletionInstructions = "You are a helpful assistant."

const (
	maxChatSSEEventBytes       = 52_428_800
	statusClientClosedRequest  = 499
	chatOutcomeSuccess         = "success"
	chatOutcomeUpstreamFailure = "upstream_failure"
	chatOutcomeClientCanceled  = "client_canceled"
)

type chatRequestConversion struct {
	Responses     map[string]any
	Metadata      proxyRequestUsageMetadata
	Stream        bool
	IncludeUsage  bool
	StopSequences []string
}

type chatCompletionState struct {
	id                string
	model             string
	created           int64
	content           strings.Builder
	toolCalls         []chatToolCallState
	toolIndexByItem   map[string]int
	toolIndexByCall   map[string]int
	toolIndexByOutput map[int]int
	currentToolIndex  int
	outputItems       map[int]map[string]any
	nextOutputIndex   int
	usage             UsageCounters
	hasUsage          bool
	finishReason      string
	terminal          bool
	stopped           bool
}

type chatToolCallState struct {
	ID          string
	ItemID      string
	Name        string
	OutputIndex int
	Arguments   strings.Builder
}

type chatStreamUpdate struct {
	ContentDelta  string
	ToolDeltas    []map[string]any
	Terminal      bool
	FinishReason  string
	ResponseID    string
	ResponseModel string
	UnknownReason string
}

type sseEvent struct {
	Event string
	Data  string
}

type chatStreamFailure struct {
	statusCode int
	body       []byte
}

type preparedChatCompletionStreamBody struct {
	*replayReadCloser
	firstEvent sseEvent
}

func (e *chatStreamFailure) Error() string {
	return "upstream Responses stream failed"
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request, authorization proxyAuthorization, signals []sessionAffinitySignal, replayCandidate bool) {
	conversion, err := decodeChatCompletionRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	payload, err := json.Marshal(conversion.Responses)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chat completion request")
		return
	}
	replayable := replayCandidate && len(payload) <= maxReplayBodyBytes
	upstreamCtx, cancelUpstream := context.WithCancel(r.Context())
	defer cancelUpstream()
	result, err := s.executeUpstream(
		upstreamCtx,
		authorization,
		signals,
		conversion.Metadata.Model,
		replayable,
		func(ctx context.Context, auth *Auth) (*http.Response, error) {
			upstreamReq, errRequest := s.newChatCompletionUpstreamRequest(r.WithContext(ctx), payload, auth)
			if errRequest != nil {
				return nil, errRequest
			}
			upstreamResp, errRoundTrip := s.httpClient.Transport.RoundTrip(upstreamReq)
			if errRoundTrip != nil {
				return nil, errRoundTrip
			}
			return prepareChatCompletionUpstreamResponse(ctx, upstreamResp, conversion.Stream)
		},
	)
	auth := result.Auth
	upstreamResp := result.Response
	if err != nil {
		s.debugf("chat completions upstream request failed method=%s path=%s error=%q", r.Method, r.URL.Path, err.Error())
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			s.recordChatCompletionUsage(r, authorization, auth, conversion.Metadata, UsageCounters{}, false, statusClientClosedRequest, "", chatOutcomeClientCanceled)
			return
		}
		if errors.Is(err, ErrStorageFailure) {
			writeStoreError(w, err)
			return
		}
		var finalErr *proxyFinalError
		if errors.As(err, &finalErr) && finalErr != nil {
			s.recordChatCompletionUsage(r, authorization, auth, conversion.Metadata, UsageCounters{}, false, finalErr.StatusCode, "", chatOutcomeUpstreamFailure)
			writeProxyError(w, finalErr)
			return
		}
		statusCode := http.StatusBadGateway
		s.recordChatCompletionUsage(r, authorization, auth, conversion.Metadata, UsageCounters{}, false, statusCode, "", chatOutcomeUpstreamFailure)
		writeProxyError(w, &proxyFinalError{
			StatusCode: statusCode,
			Code:       proxyErrorCodeUpstream,
			Message:    "upstream Codex service unavailable",
		})
		return
	}
	defer upstreamResp.Body.Close()

	if upstreamResp.StatusCode < 200 || upstreamResp.StatusCode >= 300 {
		failure, errClassify := classifyUpstreamResponse(upstreamResp, conversion.Metadata.Model, time.Now())
		if errClassify != nil {
			failure = upstreamFailure{
				Kind:       upstreamFailureTransient,
				StatusCode: http.StatusBadGateway,
				Reason:     "upstream_error",
			}
		}
		finalErr := chatProxyErrorFromFailure(failure)
		s.recordChatCompletionUsage(r, authorization, auth, conversion.Metadata, UsageCounters{}, false, finalErr.StatusCode, usageRequestID(r, upstreamResp), chatOutcomeUpstreamFailure)
		writeProxyError(w, finalErr)
		return
	}

	state := newChatCompletionState(conversion.Metadata.Model)
	if conversion.Stream {
		s.streamChatCompletion(w, r, upstreamResp, authorization, auth, conversion.Metadata, state, conversion.IncludeUsage, conversion.StopSequences, cancelUpstream)
		return
	}
	s.aggregateChatCompletion(w, r, upstreamResp, authorization, auth, conversion.Metadata, state, conversion.StopSequences)
}

func (s *Server) newChatCompletionUpstreamRequest(incoming *http.Request, payload []byte, auth *Auth) (*http.Request, error) {
	upstreamReq, err := http.NewRequestWithContext(incoming.Context(), http.MethodPost, s.codexResponsesURL(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	upstreamReq.Header.Set("Accept", "text/event-stream")
	applyCodexProxyHeaders(upstreamReq, incoming, auth, s.cfg, false)
	return upstreamReq, nil
}

func decodeChatCompletionRequest(r *http.Request) (chatRequestConversion, error) {
	if r.Body == nil {
		return chatRequestConversion{}, fmt.Errorf("invalid JSON request body")
	}
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	var raw map[string]any
	if err := decoder.Decode(&raw); err != nil {
		return chatRequestConversion{}, fmt.Errorf("invalid JSON request body")
	}
	return chatRequestToResponses(raw)
}

func chatRequestToResponses(raw map[string]any) (chatRequestConversion, error) {
	model := stringFromMap(raw, "model")
	if model == "" {
		return chatRequestConversion{}, fmt.Errorf("missing required field: model")
	}
	messages, ok := raw["messages"].([]any)
	if !ok {
		return chatRequestConversion{}, fmt.Errorf("missing required field: messages")
	}
	input, instructions := chatMessagesToResponsesInput(messages)
	out := map[string]any{
		"model":  model,
		"stream": true,
		"store":  false,
		"input":  input,
	}
	if instructions != "" {
		out["instructions"] = instructions
	} else {
		out["instructions"] = defaultChatCompletionInstructions
	}
	if tools := chatToolsToResponses(raw["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if toolChoice, ok := chatToolChoiceToResponses(raw["tool_choice"]); ok {
		out["tool_choice"] = toolChoice
	}
	if text, ok := chatTextOptionsToResponses(raw); ok {
		out["text"] = text
	}
	if reasoning, ok := chatReasoningToResponses(raw); ok {
		out["reasoning"] = reasoning
	}
	if value, ok := raw["parallel_tool_calls"]; ok {
		out["parallel_tool_calls"] = value
	}
	serviceTier := stringFromMap(raw, "service_tier")
	if serviceTier != "" {
		out["service_tier"] = serviceTier
	}
	return chatRequestConversion{
		Responses: out,
		Metadata: proxyRequestUsageMetadata{
			Model:           model,
			ReasoningEffort: chatReasoningEffort(raw),
			ServiceTier:     normalizeServiceTier(serviceTier),
		},
		Stream:        chatBoolFromMap(raw, "stream"),
		IncludeUsage:  chatStreamOptionsIncludeUsage(raw["stream_options"]),
		StopSequences: chatStopSequences(raw["stop"]),
	}, nil
}

func chatMessagesToResponsesInput(messages []any) ([]any, string) {
	var input []any
	var instructions []string
	for _, messageValue := range messages {
		message, ok := messageValue.(map[string]any)
		if !ok {
			continue
		}
		role := strings.TrimSpace(stringFromMap(message, "role"))
		switch role {
		case "system", "developer":
			if text := chatContentToText(message["content"]); text != "" {
				instructions = append(instructions, text)
			}
		case "user":
			input = append(input, map[string]any{
				"type":    "message",
				"role":    "user",
				"content": chatMessageContentToResponses(message["content"], "user"),
			})
		case "assistant":
			if content := chatMessageContentToResponses(message["content"], "assistant"); !chatContentEmpty(content) {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": content,
				})
			}
			for _, call := range chatToolCallsToResponses(message["tool_calls"]) {
				input = append(input, call)
			}
		case "tool":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": stringFromMap(message, "tool_call_id"),
				"output":  chatContentToText(message["content"]),
			})
		}
	}
	return input, strings.Join(instructions, "\n\n")
}

func chatToolCallsToResponses(value any) []any {
	calls, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(calls))
	for _, callValue := range calls {
		call, ok := callValue.(map[string]any)
		if !ok {
			continue
		}
		function, ok := mapValue(call["function"])
		if !ok {
			continue
		}
		item := map[string]any{
			"type":      "function_call",
			"call_id":   stringFromMap(call, "id"),
			"name":      stringFromMap(function, "name"),
			"arguments": rawStringFromMap(function, "arguments"),
		}
		out = append(out, item)
	}
	return out
}

func chatToolsToResponses(value any) []any {
	tools, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(tools))
	for _, toolValue := range tools {
		tool, ok := toolValue.(map[string]any)
		if !ok || stringFromMap(tool, "type") != "function" {
			continue
		}
		function, ok := mapValue(tool["function"])
		if !ok {
			continue
		}
		converted := map[string]any{
			"type": "function",
			"name": stringFromMap(function, "name"),
		}
		if description := stringFromMap(function, "description"); description != "" {
			converted["description"] = description
		}
		if parameters, okParameters := function["parameters"]; okParameters {
			converted["parameters"] = parameters
		}
		if strict, okStrict := function["strict"]; okStrict {
			converted["strict"] = strict
		}
		out = append(out, converted)
	}
	return out
}

func chatToolChoiceToResponses(value any) (any, bool) {
	switch typed := value.(type) {
	case nil:
		return nil, false
	case string:
		typed = strings.TrimSpace(typed)
		if typed == "" {
			return nil, false
		}
		if typed == "any" {
			return "required", true
		}
		return typed, true
	case map[string]any:
		if stringFromMap(typed, "type") == "function" {
			if function, ok := mapValue(typed["function"]); ok {
				name := stringFromMap(function, "name")
				if name != "" {
					return map[string]any{"type": "function", "name": name}, true
				}
			}
		}
		return typed, true
	default:
		return typed, true
	}
}

func chatReasoningToResponses(raw map[string]any) (map[string]any, bool) {
	reasoning := map[string]any{}
	if value, ok := mapValue(raw["reasoning"]); ok {
		for key, entry := range value {
			reasoning[key] = entry
		}
	}
	if extraBody, ok := mapValue(raw["extra_body"]); ok {
		if value, okReasoning := mapValue(extraBody["reasoning"]); okReasoning {
			for key, entry := range value {
				reasoning[key] = entry
			}
		}
	}
	if effort := chatReasoningEffort(raw); effort != "" {
		reasoning["effort"] = effort
	}
	return reasoning, len(reasoning) > 0
}

func chatReasoningEffort(raw map[string]any) string {
	return normalizeChatReasoningEffort(stringFromMap(raw, "reasoning_effort"))
}

func normalizeChatReasoningEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal":
		return "low"
	case "max":
		return "xhigh"
	case "none", "low", "medium", "high", "xhigh":
		return strings.ToLower(strings.TrimSpace(effort))
	default:
		return strings.TrimSpace(effort)
	}
}

func chatTextOptionsToResponses(raw map[string]any) (map[string]any, bool) {
	text := map[string]any{}
	if format, ok := chatResponseFormatToResponses(raw["response_format"]); ok {
		text["format"] = format
	}
	if verbosity := stringFromMap(raw, "verbosity"); verbosity != "" {
		text["verbosity"] = verbosity
	}
	return text, len(text) > 0
}

func chatResponseFormatToResponses(value any) (map[string]any, bool) {
	format, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	switch stringFromMap(format, "type") {
	case "json_object":
		return map[string]any{"type": "json_object"}, true
	case "json_schema":
		jsonSchema, okSchema := mapValue(format["json_schema"])
		if !okSchema {
			return nil, false
		}
		converted := map[string]any{"type": "json_schema"}
		for key, entry := range jsonSchema {
			converted[key] = entry
		}
		return converted, true
	default:
		return format, true
	}
}

func chatStopSequences(value any) []string {
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return nil
		}
		return []string{typed}
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			text, ok := entry.(string)
			if ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func chatMessageContentToResponses(value any, role string) any {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		out := make([]any, 0, len(typed))
		for _, partValue := range typed {
			part, ok := partValue.(map[string]any)
			if !ok {
				continue
			}
			switch stringFromMap(part, "type") {
			case "text":
				partType := "input_text"
				if role == "assistant" {
					partType = "output_text"
				}
				out = append(out, map[string]any{
					"type": partType,
					"text": rawStringFromMap(part, "text"),
				})
			case "image_url":
				if imageURL, okURL := mapValue(part["image_url"]); okURL {
					out = append(out, map[string]any{
						"type":      "input_image",
						"image_url": imageURL["url"],
					})
				}
			default:
				out = append(out, part)
			}
		}
		return out
	default:
		return chatContentToText(value)
	}
}

func chatContentToText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []any:
		var parts []string
		for _, partValue := range typed {
			part, ok := partValue.(map[string]any)
			if !ok {
				continue
			}
			if text := rawStringFromMap(part, "text"); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return ""
		}
		return string(raw)
	}
}

func chatContentEmpty(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	default:
		return false
	}
}

func (s *Server) aggregateChatCompletion(w http.ResponseWriter, r *http.Request, upstreamResp *http.Response, authorization proxyAuthorization, auth *Auth, metadata proxyRequestUsageMetadata, state *chatCompletionState, stopSequences []string) {
	readErr := readResponsesSSE(upstreamResp.Body, func(event sseEvent) error {
		update, err := state.applyResponsesEvent(event, false)
		if err != nil {
			return err
		}
		if update.ResponseID != "" {
			state.id = update.ResponseID
		}
		if update.ResponseModel != "" {
			state.model = update.ResponseModel
		}
		if update.UnknownReason != "" {
			s.debugf("chat completions unknown incomplete reason=%s", safeChatLogValue(update.UnknownReason))
		}
		return nil
	})
	if readErr != nil && state.terminal && !isChatStreamFailure(readErr) {
		readErr = nil
	}
	if readErr == nil && !state.terminal {
		readErr = newChatProtocolFailure("incomplete_stream")
	}
	if readErr != nil {
		finalErr, statusCode := chatProxyErrorFromStreamError(readErr, metadata.Model)
		s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, statusCode, usageRequestID(r, upstreamResp), chatOutcomeUpstreamFailure)
		writeProxyError(w, finalErr)
		return
	}
	if content, stopped := truncateAtStopSequence(state.content.String(), stopSequences); stopped {
		state.content.Reset()
		state.content.WriteString(content)
		state.stopped = true
	}
	s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, upstreamResp.StatusCode, usageRequestID(r, upstreamResp), chatOutcomeSuccess)
	writeJSON(w, http.StatusOK, state.chatCompletionResponse())
}

func (s *Server) streamChatCompletion(
	w http.ResponseWriter,
	r *http.Request,
	upstreamResp *http.Response,
	authorization proxyAuthorization,
	auth *Auth,
	metadata proxyRequestUsageMetadata,
	state *chatCompletionState,
	includeUsage bool,
	stopSequences []string,
	cancelUpstream context.CancelFunc,
) {
	var firstUpdate *chatStreamUpdate
	if preparedBody, ok := upstreamResp.Body.(*preparedChatCompletionStreamBody); ok {
		update, err := state.applyResponsesEvent(preparedBody.firstEvent, true)
		if err != nil {
			finalErr, statusCode := chatProxyErrorFromStreamError(err, metadata.Model)
			s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, statusCode, usageRequestID(r, upstreamResp), chatOutcomeUpstreamFailure)
			writeProxyError(w, finalErr)
			cancelUpstream()
			return
		}
		firstUpdate = &update
	}

	var writeErr error
	stopFilter := newChatStopFilter(stopSequences)
	emitUpdate := func(update chatStreamUpdate) error {
		if update.ResponseID != "" {
			state.id = update.ResponseID
		}
		if update.ResponseModel != "" {
			state.model = update.ResponseModel
		}
		if update.ContentDelta != "" {
			contentDelta, stopped := stopFilter.push(update.ContentDelta)
			if stopped {
				state.stopped = true
			}
			if contentDelta != "" {
				if errWrite := writeAndFlushChatSSE(w, state.chatCompletionChunk(map[string]any{"content": contentDelta}, nil, nil, false)); errWrite != nil {
					writeErr = errWrite
					cancelUpstream()
					return errWrite
				}
			}
		}
		if !state.stopped {
			for _, delta := range update.ToolDeltas {
				if errWrite := writeAndFlushChatSSE(w, state.chatCompletionChunk(map[string]any{"tool_calls": []any{delta}}, nil, nil, false)); errWrite != nil {
					writeErr = errWrite
					cancelUpstream()
					return errWrite
				}
			}
		}
		if update.UnknownReason != "" {
			s.debugf("chat completions unknown incomplete reason=%s", safeChatLogValue(update.UnknownReason))
		}
		return nil
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	if errWrite := writeAndFlushChatSSE(w, state.chatCompletionChunk(map[string]any{"role": "assistant"}, nil, nil, false)); errWrite != nil {
		cancelUpstream()
		s.recordChatCompletionUsage(r, authorization, auth, metadata, UsageCounters{}, false, statusClientClosedRequest, usageRequestID(r, upstreamResp), chatOutcomeClientCanceled)
		return
	}

	if firstUpdate != nil {
		if errEmit := emitUpdate(*firstUpdate); errEmit != nil {
			s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, statusClientClosedRequest, usageRequestID(r, upstreamResp), chatOutcomeClientCanceled)
			return
		}
	}

	readErr := readResponsesSSE(upstreamResp.Body, func(event sseEvent) error {
		update, err := state.applyResponsesEvent(event, true)
		if err != nil {
			return err
		}
		return emitUpdate(update)
	})
	if readErr != nil && state.terminal && !isChatStreamFailure(readErr) {
		readErr = nil
	}
	if writeErr != nil || r.Context().Err() != nil {
		cancelUpstream()
		s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, statusClientClosedRequest, usageRequestID(r, upstreamResp), chatOutcomeClientCanceled)
		return
	}
	if readErr == nil && !state.terminal {
		readErr = newChatProtocolFailure("incomplete_stream")
	}
	if readErr != nil {
		finalErr, statusCode := chatProxyErrorFromStreamError(readErr, metadata.Model)
		s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, statusCode, usageRequestID(r, upstreamResp), chatOutcomeUpstreamFailure)
		_ = writeAndFlushChatSSE(w, chatErrorEnvelope(finalErr))
		cancelUpstream()
		return
	}
	if contentDelta := stopFilter.flush(); contentDelta != "" {
		if errWrite := writeAndFlushChatSSE(w, state.chatCompletionChunk(map[string]any{"content": contentDelta}, nil, nil, false)); errWrite != nil {
			cancelUpstream()
			s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, statusClientClosedRequest, usageRequestID(r, upstreamResp), chatOutcomeClientCanceled)
			return
		}
	}
	finishReason := state.finalFinishReason()
	if errWrite := writeAndFlushChatSSE(w, state.chatCompletionChunk(map[string]any{}, &finishReason, nil, false)); errWrite != nil {
		cancelUpstream()
		s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, statusClientClosedRequest, usageRequestID(r, upstreamResp), chatOutcomeClientCanceled)
		return
	}
	if includeUsage {
		usage := state.chatUsage()
		if errWrite := writeAndFlushChatSSE(w, state.chatCompletionChunk(nil, nil, &usage, true)); errWrite != nil {
			cancelUpstream()
			s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, statusClientClosedRequest, usageRequestID(r, upstreamResp), chatOutcomeClientCanceled)
			return
		}
	}
	if errWrite := writeAndFlushChatDone(w); errWrite != nil {
		cancelUpstream()
		s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, statusClientClosedRequest, usageRequestID(r, upstreamResp), chatOutcomeClientCanceled)
		return
	}
	s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, upstreamResp.StatusCode, usageRequestID(r, upstreamResp), chatOutcomeSuccess)
}

func newChatCompletionState(model string) *chatCompletionState {
	created := time.Now().Unix()
	return &chatCompletionState{
		id:                fmt.Sprintf("chatcmpl-%d", created),
		model:             model,
		created:           created,
		toolIndexByItem:   map[string]int{},
		toolIndexByCall:   map[string]int{},
		toolIndexByOutput: map[int]int{},
		currentToolIndex:  -1,
		outputItems:       map[int]map[string]any{},
	}
}

func (s *chatCompletionState) applyResponsesEvent(event sseEvent, streaming bool) (chatStreamUpdate, error) {
	payload, eventType, err := decodeChatResponsesEvent(event)
	if err != nil {
		return chatStreamUpdate{}, err
	}
	if s.terminal {
		return chatStreamUpdate{}, newChatProtocolFailure("data_after_terminal")
	}
	switch eventType {
	case "response.created":
		response, ok := mapValue(payload["response"])
		if !ok {
			return chatStreamUpdate{}, newChatProtocolFailure("malformed_event")
		}
		s.applyResponseMetadata(response)
		return chatStreamUpdate{ResponseID: s.id, ResponseModel: s.model}, nil
	case "response.output_text.delta":
		delta, ok := payload["delta"].(string)
		if !ok {
			return chatStreamUpdate{}, newChatProtocolFailure("malformed_event")
		}
		s.content.WriteString(delta)
		return chatStreamUpdate{ContentDelta: delta}, nil
	case "response.output_text.done":
		text := firstNonEmptyString(rawStringFromMap(payload, "text"), rawStringFromMap(payload, "output_text"))
		delta, errReconcile := s.reconcileTextSnapshot(text, streaming)
		if errReconcile != nil {
			return chatStreamUpdate{}, errReconcile
		}
		return chatStreamUpdate{ContentDelta: delta}, nil
	case "response.output_item.added":
		item, ok := mapValue(payload["item"])
		if !ok {
			return chatStreamUpdate{}, newChatProtocolFailure("malformed_event")
		}
		if stringFromMap(item, "type") != "function_call" {
			return chatStreamUpdate{}, nil
		}
		outputIndex := s.outputIndexForEvent(payload, item)
		index, created := s.ensureToolCall(item, outputIndex)
		if created {
			return chatStreamUpdate{ToolDeltas: []map[string]any{s.initialToolCallDelta(index)}}, nil
		}
	case "response.output_item.done":
		item, ok := mapValue(payload["item"])
		if !ok {
			return chatStreamUpdate{}, newChatProtocolFailure("malformed_event")
		}
		outputIndex := s.outputIndexForEvent(payload, item)
		s.outputItems[outputIndex] = item
		if streaming {
			return s.reconcileStreamingOutputItem(item, outputIndex)
		}
	case "response.function_call_arguments.delta":
		index := s.toolIndexForPayload(payload)
		delta, ok := payload["delta"].(string)
		if index < 0 || !ok {
			return chatStreamUpdate{}, newChatProtocolFailure("malformed_event")
		}
		s.toolCalls[index].Arguments.WriteString(delta)
		return chatStreamUpdate{ToolDeltas: []map[string]any{s.argumentsToolCallDelta(index, delta)}}, nil
	case "response.function_call_arguments.done":
		index := s.toolIndexForPayload(payload)
		arguments, ok := payload["arguments"].(string)
		if index < 0 || !ok {
			return chatStreamUpdate{}, newChatProtocolFailure("malformed_event")
		}
		delta, errReconcile := s.reconcileToolArguments(index, arguments, streaming)
		if errReconcile != nil {
			return chatStreamUpdate{}, errReconcile
		}
		if delta != "" {
			return chatStreamUpdate{ToolDeltas: []map[string]any{s.argumentsToolCallDelta(index, delta)}}, nil
		}
	case "response.completed", "response.incomplete":
		response, ok := mapValue(payload["response"])
		if !ok {
			return chatStreamUpdate{}, newChatProtocolFailure("malformed_event")
		}
		update, errTerminal := s.applyTerminalResponse(response, eventType, streaming)
		if errTerminal != nil {
			return chatStreamUpdate{}, errTerminal
		}
		s.terminal = true
		update.Terminal = true
		update.ResponseID = s.id
		update.ResponseModel = s.model
		return update, nil
	case "response.failed":
		if response, ok := mapValue(payload["response"]); ok {
			s.applyResponseMetadata(response)
			s.applyResponseUsage(response)
		}
		return chatStreamUpdate{}, chatFailureFromEvent(payload, eventType)
	case "error":
		return chatStreamUpdate{}, chatFailureFromEvent(payload, eventType)
	}
	return chatStreamUpdate{}, nil
}

func (s *chatCompletionState) applyResponseMetadata(response map[string]any) {
	if id := stringFromMap(response, "id"); id != "" {
		s.id = id
	}
	if model := stringFromMap(response, "model"); model != "" {
		s.model = model
	}
}

func (s *chatCompletionState) applyResponseUsage(response map[string]any) {
	if counters, ok := usageCountersFromValue(response["usage"]); ok {
		s.usage = counters
		s.hasUsage = true
	}
}

func (s *chatCompletionState) applyTerminalResponse(response map[string]any, eventType string, streaming bool) (chatStreamUpdate, error) {
	s.applyResponseMetadata(response)
	s.applyResponseUsage(response)
	update, err := s.reconcileTerminalOutput(response, streaming)
	if err != nil {
		return chatStreamUpdate{}, err
	}
	switch eventType {
	case "response.incomplete":
		reason, unknown := incompleteFinishReason(response)
		s.finishReason = reason
		update.UnknownReason = unknown
	default:
		if len(s.toolCalls) > 0 {
			s.finishReason = "tool_calls"
		} else {
			s.finishReason = "stop"
		}
	}
	update.FinishReason = s.finalFinishReason()
	return update, nil
}

func (s *chatCompletionState) reconcileTerminalOutput(response map[string]any, streaming bool) (chatStreamUpdate, error) {
	items, err := s.resolvedResponseOutput(response)
	if err != nil {
		return chatStreamUpdate{}, err
	}
	var messageText strings.Builder
	var toolItems []indexedChatOutputItem
	hasMessage := false
	for _, indexed := range items {
		switch stringFromMap(indexed.item, "type") {
		case "message":
			hasMessage = true
			messageText.WriteString(outputTextFromMessage(indexed.item))
		case "function_call":
			toolItems = append(toolItems, indexed)
		}
	}
	if !hasMessage && len(toolItems) == 0 {
		return chatStreamUpdate{}, nil
	}

	if !streaming {
		s.content.Reset()
		s.resetToolCalls()
		if hasMessage {
			s.content.WriteString(messageText.String())
		}
		for _, indexed := range toolItems {
			index, _ := s.ensureToolCall(indexed.item, indexed.index)
			s.toolCalls[index].Arguments.Reset()
			s.toolCalls[index].Arguments.WriteString(rawStringFromMap(indexed.item, "arguments"))
		}
		return chatStreamUpdate{}, nil
	}

	update := chatStreamUpdate{}
	if hasMessage {
		delta, errText := s.reconcileTextSnapshot(messageText.String(), true)
		if errText != nil {
			return chatStreamUpdate{}, errText
		}
		update.ContentDelta = delta
	} else if s.content.Len() > 0 {
		return chatStreamUpdate{}, newChatProtocolFailure("output_conflict")
	}

	finalTools := make(map[int]struct{}, len(toolItems))
	for _, indexed := range toolItems {
		deltas, errTool := s.reconcileToolSnapshot(indexed.item, indexed.index, true)
		if errTool != nil {
			return chatStreamUpdate{}, errTool
		}
		update.ToolDeltas = append(update.ToolDeltas, deltas...)
		index := s.lookupToolCallIndex(indexed.item)
		if index < 0 {
			index = s.toolIndexByOutput[indexed.index]
		}
		if index >= 0 {
			finalTools[index] = struct{}{}
		}
	}
	if len(finalTools) != len(s.toolCalls) {
		return chatStreamUpdate{}, newChatProtocolFailure("output_conflict")
	}
	return update, nil
}

type indexedChatOutputItem struct {
	index int
	item  map[string]any
}

func (s *chatCompletionState) resolvedResponseOutput(response map[string]any) ([]indexedChatOutputItem, error) {
	terminal := map[int]map[string]any{}
	if rawOutput, exists := response["output"]; exists {
		output, ok := rawOutput.([]any)
		if !ok {
			return nil, newChatProtocolFailure("malformed_event")
		}
		for index, itemValue := range output {
			item, okItem := itemValue.(map[string]any)
			if !okItem {
				return nil, newChatProtocolFailure("malformed_event")
			}
			terminal[index] = item
		}
	}
	maxIndex := -1
	for index := range terminal {
		if index > maxIndex {
			maxIndex = index
		}
	}
	for index := range s.outputItems {
		if index > maxIndex {
			maxIndex = index
		}
	}
	items := make([]indexedChatOutputItem, 0, maxIndex+1)
	for index := 0; index <= maxIndex; index++ {
		item := terminal[index]
		if item == nil {
			item = s.outputItems[index]
		}
		if item != nil {
			items = append(items, indexedChatOutputItem{index: index, item: item})
		}
	}
	return items, nil
}

func outputTextFromMessage(message map[string]any) string {
	content, ok := message["content"].([]any)
	if !ok {
		return ""
	}
	var out strings.Builder
	for _, partValue := range content {
		part, ok := partValue.(map[string]any)
		if !ok {
			continue
		}
		if text := rawStringFromMap(part, "text"); text != "" {
			out.WriteString(text)
		}
	}
	return out.String()
}

func incompleteFinishReason(response map[string]any) (string, string) {
	reason := ""
	if details, ok := mapValue(response["incomplete_details"]); ok {
		reason = stringFromMap(details, "reason")
	}
	switch reason {
	case "content_filter":
		return "content_filter", ""
	case "max_tokens", "max_output_tokens":
		return "length", ""
	default:
		return "length", reason
	}
}

func (s *chatCompletionState) ensureToolCall(item map[string]any, outputIndex int) (int, bool) {
	itemID := stringFromMap(item, "id")
	callID := firstNonEmptyString(stringFromMap(item, "call_id"), itemID)
	index := s.lookupToolCallIndex(item)
	if index < 0 && outputIndex >= 0 {
		if found, ok := s.toolIndexByOutput[outputIndex]; ok {
			index = found
		}
	}
	created := false
	if index < 0 {
		index = len(s.toolCalls)
		created = true
		s.toolCalls = append(s.toolCalls, chatToolCallState{
			ID:          firstNonEmptyString(callID, fmt.Sprintf("call_%d", index)),
			ItemID:      itemID,
			Name:        stringFromMap(item, "name"),
			OutputIndex: outputIndex,
		})
	}
	if itemID != "" {
		s.toolCalls[index].ItemID = itemID
		s.toolIndexByItem[itemID] = index
	}
	if callID != "" {
		s.toolCalls[index].ID = callID
		s.toolIndexByCall[callID] = index
	}
	if name := stringFromMap(item, "name"); name != "" {
		s.toolCalls[index].Name = name
	}
	if outputIndex >= 0 {
		s.toolCalls[index].OutputIndex = outputIndex
		s.toolIndexByOutput[outputIndex] = index
	}
	s.currentToolIndex = index
	return index, created
}

func (s *chatCompletionState) lookupToolCallIndex(item map[string]any) int {
	itemID := stringFromMap(item, "id")
	callID := firstNonEmptyString(stringFromMap(item, "call_id"), itemID)
	if itemID != "" {
		if found, ok := s.toolIndexByItem[itemID]; ok {
			return found
		}
	}
	if callID != "" {
		if found, ok := s.toolIndexByCall[callID]; ok {
			return found
		}
	}
	if outputIndex, ok := int64Value(item["output_index"]); ok {
		if found, exists := s.toolIndexByOutput[int(outputIndex)]; exists {
			return found
		}
	}
	return -1
}

func (s *chatCompletionState) outputIndexForEvent(payload map[string]any, item map[string]any) int {
	if outputIndex, ok := int64Value(payload["output_index"]); ok && outputIndex >= 0 {
		index := int(outputIndex)
		if index >= s.nextOutputIndex {
			s.nextOutputIndex = index + 1
		}
		return index
	}
	if toolIndex := s.lookupToolCallIndex(item); toolIndex >= 0 {
		outputIndex := s.toolCalls[toolIndex].OutputIndex
		if outputIndex >= 0 {
			return outputIndex
		}
	}
	index := s.nextOutputIndex
	s.nextOutputIndex++
	return index
}

func (s *chatCompletionState) toolIndexForPayload(payload map[string]any) int {
	for _, key := range []string{"item_id", "output_item_id", "id"} {
		if id := stringFromMap(payload, key); id != "" {
			if index, ok := s.toolIndexByItem[id]; ok {
				return index
			}
		}
	}
	if callID := stringFromMap(payload, "call_id"); callID != "" {
		if index, ok := s.toolIndexByCall[callID]; ok {
			return index
		}
	}
	if outputIndex, ok := int64Value(payload["output_index"]); ok {
		if index, exists := s.toolIndexByOutput[int(outputIndex)]; exists {
			return index
		}
	}
	if s.currentToolIndex >= 0 && s.currentToolIndex < len(s.toolCalls) {
		return s.currentToolIndex
	}
	return -1
}

func (s *chatCompletionState) reconcileStreamingOutputItem(item map[string]any, outputIndex int) (chatStreamUpdate, error) {
	switch stringFromMap(item, "type") {
	case "message":
		delta, err := s.reconcileTextSnapshot(outputTextFromMessage(item), true)
		return chatStreamUpdate{ContentDelta: delta}, err
	case "function_call":
		deltas, err := s.reconcileToolSnapshot(item, outputIndex, true)
		return chatStreamUpdate{ToolDeltas: deltas}, err
	default:
		return chatStreamUpdate{}, nil
	}
}

func (s *chatCompletionState) reconcileTextSnapshot(snapshot string, streaming bool) (string, error) {
	current := s.content.String()
	if !strings.HasPrefix(snapshot, current) {
		if streaming {
			return "", newChatProtocolFailure("output_conflict")
		}
		s.content.Reset()
		s.content.WriteString(snapshot)
		return "", nil
	}
	suffix := snapshot[len(current):]
	s.content.WriteString(suffix)
	return suffix, nil
}

func (s *chatCompletionState) reconcileToolSnapshot(item map[string]any, outputIndex int, streaming bool) ([]map[string]any, error) {
	index, created := s.ensureToolCall(item, outputIndex)
	fullArguments := rawStringFromMap(item, "arguments")
	if created {
		s.toolCalls[index].Arguments.WriteString(fullArguments)
		return []map[string]any{s.initialToolCallDelta(index)}, nil
	}
	delta, err := s.reconcileToolArguments(index, fullArguments, streaming)
	if err != nil || delta == "" {
		return nil, err
	}
	return []map[string]any{s.argumentsToolCallDelta(index, delta)}, nil
}

func (s *chatCompletionState) reconcileToolArguments(index int, fullArguments string, streaming bool) (string, error) {
	current := s.toolCalls[index].Arguments.String()
	if !strings.HasPrefix(fullArguments, current) {
		if streaming {
			return "", newChatProtocolFailure("output_conflict")
		}
		s.toolCalls[index].Arguments.Reset()
		s.toolCalls[index].Arguments.WriteString(fullArguments)
		return "", nil
	}
	suffix := fullArguments[len(current):]
	s.toolCalls[index].Arguments.WriteString(suffix)
	return suffix, nil
}

func (s *chatCompletionState) resetToolCalls() {
	s.toolCalls = nil
	s.toolIndexByItem = map[string]int{}
	s.toolIndexByCall = map[string]int{}
	s.toolIndexByOutput = map[int]int{}
	s.currentToolIndex = -1
}

func (s *chatCompletionState) initialToolCallDelta(index int) map[string]any {
	call := s.toolCalls[index]
	return map[string]any{
		"index": index,
		"id":    call.ID,
		"type":  "function",
		"function": map[string]any{
			"name":      call.Name,
			"arguments": call.Arguments.String(),
		},
	}
}

func (s *chatCompletionState) argumentsToolCallDelta(index int, delta string) map[string]any {
	return map[string]any{
		"index": index,
		"function": map[string]any{
			"arguments": delta,
		},
	}
}

func (s *chatCompletionState) chatCompletionResponse() map[string]any {
	message := map[string]any{
		"role":    "assistant",
		"content": s.content.String(),
	}
	if len(s.toolCalls) > 0 {
		message["tool_calls"] = s.chatToolCalls()
	}
	return map[string]any{
		"id":      s.id,
		"object":  "chat.completion",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": s.finalFinishReason(),
		}},
		"usage": s.chatUsage(),
	}
}

func (s *chatCompletionState) chatCompletionChunk(delta map[string]any, finishReason *string, usage *map[string]any, emptyChoices bool) map[string]any {
	choices := []any{}
	if !emptyChoices {
		choices = append(choices, map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		})
	}
	chunk := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": choices,
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	return chunk
}

func (s *chatCompletionState) chatToolCalls() []any {
	out := make([]any, 0, len(s.toolCalls))
	for _, call := range s.toolCalls {
		out = append(out, map[string]any{
			"id":   call.ID,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments.String(),
			},
		})
	}
	return out
}

func (s *chatCompletionState) chatUsage() map[string]any {
	usage := map[string]any{
		"prompt_tokens":     s.usage.InputTokens,
		"completion_tokens": s.usage.OutputTokens,
		"total_tokens":      s.usage.normalized().TotalTokens,
	}
	if s.usage.CachedInputTokens > 0 {
		usage["prompt_tokens_details"] = map[string]any{
			"cached_tokens": s.usage.CachedInputTokens,
		}
	}
	if s.usage.ReasoningTokens > 0 {
		usage["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": s.usage.ReasoningTokens,
		}
	}
	return usage
}

func truncateAtStopSequence(content string, stopSequences []string) (string, bool) {
	index := -1
	for _, stop := range stopSequences {
		if stop == "" {
			continue
		}
		found := strings.Index(content, stop)
		if found >= 0 && (index < 0 || found < index) {
			index = found
		}
	}
	if index < 0 {
		return content, false
	}
	return content[:index], true
}

type chatStopFilter struct {
	stopSequences []string
	buffered      string
	maxStopRunes  int
	stopped       bool
}

func newChatStopFilter(stopSequences []string) *chatStopFilter {
	filter := &chatStopFilter{stopSequences: stopSequences}
	for _, stop := range stopSequences {
		if count := len([]rune(stop)); count > filter.maxStopRunes {
			filter.maxStopRunes = count
		}
	}
	return filter
}

func (f *chatStopFilter) push(delta string) (string, bool) {
	if f.maxStopRunes == 0 {
		return delta, false
	}
	if f.stopped || delta == "" {
		return "", f.stopped
	}
	combined := f.buffered + delta
	if content, stopped := truncateAtStopSequence(combined, f.stopSequences); stopped {
		f.buffered = ""
		f.stopped = true
		return content, true
	}
	keep := f.maxStopRunes - 1
	if keep <= 0 {
		return combined, false
	}
	runes := []rune(combined)
	if len(runes) <= keep {
		f.buffered = combined
		return "", false
	}
	emitLen := len(runes) - keep
	f.buffered = string(runes[emitLen:])
	return string(runes[:emitLen]), false
}

func (f *chatStopFilter) flush() string {
	if f.maxStopRunes == 0 || f.stopped || f.buffered == "" {
		return ""
	}
	content := f.buffered
	f.buffered = ""
	return content
}

func (s *chatCompletionState) finalFinishReason() string {
	if s.stopped {
		return "stop"
	}
	if s.finishReason != "" {
		return s.finishReason
	}
	if len(s.toolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func readResponsesSSE(reader io.Reader, handle func(sseEvent) error) error {
	sseReader := newResponsesSSEReader(reader)
	for {
		event, _, err := sseReader.next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if errHandle := handle(event); errHandle != nil {
			return errHandle
		}
	}
}

type responsesSSEReader struct {
	buffered *bufio.Reader
}

func newResponsesSSEReader(reader io.Reader) *responsesSSEReader {
	return &responsesSSEReader{buffered: bufio.NewReader(reader)}
}

func (r *responsesSSEReader) next() (sseEvent, []byte, error) {
	var event sseEvent
	var raw bytes.Buffer
	eventBytes := 0
	for {
		line, err := readBoundedSSELine(r.buffered, maxChatSSEEventBytes-eventBytes)
		eventBytes += len(line)
		if eventBytes > maxChatSSEEventBytes {
			return sseEvent{}, nil, newChatProtocolFailure("event_too_large")
		}
		_, _ = raw.Write(line)
		trimmed := bytes.TrimSuffix(line, []byte("\n"))
		trimmed = bytes.TrimSuffix(trimmed, []byte("\r"))
		if len(trimmed) == 0 {
			if event.Data != "" {
				return event, raw.Bytes(), nil
			}
			raw.Reset()
			eventBytes = 0
			event = sseEvent{}
		} else if trimmed[0] != ':' {
			field, value, hasValue := bytes.Cut(trimmed, []byte(":"))
			if hasValue && len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			switch string(field) {
			case "event":
				event.Event = strings.TrimSpace(string(value))
			case "data":
				if event.Data != "" {
					event.Data += "\n"
				}
				event.Data += string(value)
			}
		}
		if err != nil {
			if err == io.EOF {
				if event.Data != "" {
					return event, raw.Bytes(), nil
				}
				return sseEvent{}, nil, io.EOF
			}
			return sseEvent{}, nil, err
		}
	}
}

func readBoundedSSELine(reader *bufio.Reader, limit int) ([]byte, error) {
	if limit < 0 {
		return nil, newChatProtocolFailure("event_too_large")
	}
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			return nil, newChatProtocolFailure("event_too_large")
		}
		line = append(line, fragment...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

func prepareChatCompletionUpstreamResponse(ctx context.Context, resp *http.Response, streaming bool) (*http.Response, error) {
	if resp == nil || resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return resp, nil
	}
	if resp.Body == nil {
		return replaceChatCompletionResponse(resp, newChatProtocolFailure("incomplete_stream")), nil
	}
	originalBody := resp.Body
	reader := newResponsesSSEReader(originalBody)
	if streaming {
		event, _, err := reader.next()
		if err != nil {
			if ctx.Err() != nil {
				_ = originalBody.Close()
				return nil, ctx.Err()
			}
			return replaceChatCompletionResponse(resp, chatStreamFailureFromError(err)), nil
		}
		state := newChatCompletionState("")
		if _, err = state.applyResponsesEvent(event, true); err != nil {
			return replaceChatCompletionResponse(resp, chatStreamFailureFromError(err)), nil
		}
		resp.Body = &preparedChatCompletionStreamBody{
			replayReadCloser: &replayReadCloser{
				Reader:  reader.buffered,
				closers: []io.Closer{originalBody},
			},
			firstEvent: event,
		}
		return resp, nil
	}

	var replay bytes.Buffer
	state := newChatCompletionState("")
	for {
		event, raw, err := reader.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if state.terminal && !isChatStreamFailure(err) {
				break
			}
			if ctx.Err() != nil {
				_ = originalBody.Close()
				return nil, ctx.Err()
			}
			return replaceChatCompletionResponse(resp, chatStreamFailureFromError(err)), nil
		}
		_, _ = replay.Write(raw)
		if _, err = state.applyResponsesEvent(event, false); err != nil {
			return replaceChatCompletionResponse(resp, chatStreamFailureFromError(err)), nil
		}
	}
	if !state.terminal {
		return replaceChatCompletionResponse(resp, newChatProtocolFailure("incomplete_stream")), nil
	}
	_ = originalBody.Close()
	resp.Body = io.NopCloser(bytes.NewReader(replay.Bytes()))
	resp.ContentLength = int64(replay.Len())
	return resp, nil
}

func replaceChatCompletionResponse(resp *http.Response, failure *chatStreamFailure) *http.Response {
	if failure == nil {
		failure = newChatProtocolFailure("upstream_error")
	}
	if resp == nil {
		resp = &http.Response{Header: make(http.Header)}
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	if resp.Header == nil {
		resp.Header = make(http.Header)
	} else {
		resp.Header = resp.Header.Clone()
	}
	resp.StatusCode = failure.statusCode
	resp.Status = fmt.Sprintf("%d %s", failure.statusCode, http.StatusText(failure.statusCode))
	resp.Header.Set("Content-Type", "application/json")
	resp.Body = io.NopCloser(bytes.NewReader(failure.body))
	resp.ContentLength = int64(len(failure.body))
	return resp
}

func decodeChatResponsesEvent(event sseEvent) (map[string]any, string, error) {
	decoder := json.NewDecoder(strings.NewReader(event.Data))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, "", newChatProtocolFailure("malformed_event")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, "", newChatProtocolFailure("malformed_event")
	}
	eventType := stringFromMap(payload, "type")
	if eventType == "" {
		eventType = strings.TrimSpace(event.Event)
	}
	if eventType == "" {
		return nil, "", newChatProtocolFailure("malformed_event")
	}
	return payload, eventType, nil
}

func chatFailureFromEvent(payload map[string]any, eventType string) *chatStreamFailure {
	var source map[string]any
	switch eventType {
	case "response.failed":
		if response, ok := mapValue(payload["response"]); ok {
			source, _ = mapValue(response["error"])
		}
		if source == nil {
			source, _ = mapValue(payload["error"])
		}
	case "error":
		source, _ = mapValue(payload["error"])
	}

	selected := map[string]any{}
	if source != nil {
		for _, key := range []string{
			"message",
			"type",
			"code",
			"param",
			"status",
			"status_code",
			"resets_at",
			"resets_in_seconds",
		} {
			if value, ok := source[key]; ok {
				selected[key] = value
			}
		}
	} else if message, ok := payload["error"].(string); ok && strings.TrimSpace(message) != "" {
		selected["message"] = message
	}
	if eventType == "error" {
		for _, key := range []string{"message", "code", "param", "status", "status_code", "resets_at", "resets_in_seconds"} {
			if _, exists := selected[key]; exists {
				continue
			}
			if value, ok := payload[key]; ok {
				selected[key] = value
			}
		}
		if _, exists := selected["type"]; !exists {
			if errorType := rawStringFromMap(payload, "error_type"); errorType != "" {
				selected["type"] = errorType
			}
		}
	}
	if strings.TrimSpace(rawStringFromMap(selected, "message")) == "" {
		selected["message"] = "upstream Responses stream failed"
	}
	body, err := json.Marshal(map[string]any{"error": selected})
	if err != nil {
		return newChatProtocolFailure("upstream_error")
	}
	return &chatStreamFailure{
		statusCode: chatFailureStatus(selected),
		body:       body,
	}
}

func chatFailureStatus(errorPayload map[string]any) int {
	for _, key := range []string{"status_code", "status"} {
		if status, ok := int64Value(errorPayload[key]); ok && status >= 400 && status <= 599 {
			return int(status)
		}
	}
	errorType := strings.ToLower(rawStringFromMap(errorPayload, "type"))
	errorCode := strings.ToLower(rawStringFromMap(errorPayload, "code"))
	message := strings.ToLower(rawStringFromMap(errorPayload, "message"))
	switch {
	case errorType == "invalid_request_error",
		errorType == "bad_request_error",
		errorCode == "context_length_exceeded",
		errorCode == "context_too_large",
		strings.Contains(message, "context window"),
		strings.Contains(message, "context length"):
		return http.StatusBadRequest
	case errorType == "authentication_error",
		errorCode == "invalid_api_key",
		errorCode == "unauthorized":
		return http.StatusUnauthorized
	case errorType == "permission_error",
		errorCode == "forbidden",
		errorCode == "permission_denied":
		return http.StatusForbidden
	case errorType == "not_found_error",
		errorCode == "not_found",
		errorCode == "model_not_found":
		return http.StatusNotFound
	case errorType == "rate_limit_error",
		errorType == "usage_limit_reached",
		errorCode == "rate_limit_exceeded":
		return http.StatusTooManyRequests
	case errorCode == "model_not_supported",
		errorCode == "unsupported_model",
		errorCode == "unknown_model",
		errorCode == "model_unavailable":
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

func newChatProtocolFailure(code string) *chatStreamFailure {
	code = safeChatLogValue(code)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": "upstream Responses stream failed",
			"type":    "upstream_error",
			"code":    code,
		},
	})
	return &chatStreamFailure{
		statusCode: http.StatusBadGateway,
		body:       body,
	}
}

func chatStreamFailureFromError(err error) *chatStreamFailure {
	var failure *chatStreamFailure
	if errors.As(err, &failure) && failure != nil {
		return failure
	}
	return newChatProtocolFailure("upstream_error")
}

func isChatStreamFailure(err error) bool {
	var failure *chatStreamFailure
	return errors.As(err, &failure) && failure != nil
}

func chatProxyErrorFromStreamError(err error, model string) (*proxyFinalError, int) {
	streamFailure := chatStreamFailureFromError(err)
	resp := &http.Response{
		StatusCode: streamFailure.statusCode,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(streamFailure.body)),
	}
	failure, errClassify := classifyUpstreamResponse(resp, model, time.Now())
	if errClassify != nil {
		failure = upstreamFailure{
			Kind:       upstreamFailureTransient,
			StatusCode: http.StatusBadGateway,
			Reason:     "upstream_error",
		}
	}
	return chatProxyErrorFromFailure(failure), streamFailure.statusCode
}

func chatProxyErrorFromFailure(failure upstreamFailure) *proxyFinalError {
	switch failure.Kind {
	case upstreamFailureRequestScoped:
		code := safeHealthCode(failure.ErrorCode)
		if code == "" {
			code = "upstream_request_error"
		}
		statusCode := failure.StatusCode
		if statusCode < http.StatusBadRequest || statusCode >= http.StatusInternalServerError {
			statusCode = http.StatusBadRequest
		}
		return &proxyFinalError{
			StatusCode: statusCode,
			Code:       code,
			Message:    "upstream Codex request was rejected",
		}
	case upstreamFailureUnauthorized, upstreamFailureCredential:
		return &proxyFinalError{
			StatusCode: http.StatusServiceUnavailable,
			Code:       proxyErrorCodeAuth,
			Message:    "upstream authentication unavailable",
		}
	case upstreamFailureQuota:
		return &proxyFinalError{
			StatusCode: http.StatusTooManyRequests,
			Code:       proxyErrorCodeRateLimited,
			Message:    "Codex capacity is temporarily rate limited",
			RetryAt:    failure.RetryAt,
		}
	case upstreamFailureModelUnsupported:
		return &proxyFinalError{
			StatusCode: http.StatusNotFound,
			Code:       proxyErrorCodeModelNotFound,
			Message:    "requested model is unavailable for the selected Codex auth",
		}
	default:
		return &proxyFinalError{
			StatusCode: http.StatusBadGateway,
			Code:       proxyErrorCodeUpstream,
			Message:    "upstream Codex service unavailable",
		}
	}
}

func chatErrorEnvelope(err *proxyFinalError) map[string]any {
	if err == nil {
		err = &proxyFinalError{
			Code:    proxyErrorCodeUpstream,
			Message: "upstream Codex service unavailable",
		}
	}
	return map[string]any{
		"error": map[string]any{
			"message": err.Message,
			"type":    "proxy_error",
			"code":    err.Code,
		},
	}
}

func safeChatLogValue(value string) string {
	if safe := safeHealthCode(value); safe != "" {
		return safe
	}
	return "unknown"
}

func (s *Server) codexResponsesURL() string {
	target := *s.baseURL
	target.Path = targetPath(s.baseURL, "/backend-api/codex", "/responses")
	target.RawQuery = ""
	target.Fragment = ""
	return target.String()
}

func writeChatSSE(w io.Writer, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
	return err
}

func writeAndFlushChatSSE(w http.ResponseWriter, payload map[string]any) error {
	if err := writeChatSSE(w, payload); err != nil {
		return err
	}
	return flushChatCompletion(w)
}

func writeAndFlushChatDone(w http.ResponseWriter) error {
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	return flushChatCompletion(w)
}

func flushChatCompletion(w http.ResponseWriter) error {
	err := http.NewResponseController(w).Flush()
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func (s *Server) recordChatCompletionUsage(r *http.Request, authorization proxyAuthorization, auth *Auth, metadata proxyRequestUsageMetadata, counters UsageCounters, hasUsage bool, statusCode int, requestID string, outcome string) {
	if !s.shouldRecordUsage(authorization) {
		return
	}
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	s.recordProxyUsage(usageCaptureContext{
		Authorization:   authorization,
		AuthID:          authID,
		Model:           metadata.Model,
		ReasoningEffort: metadata.ReasoningEffort,
		ServiceTier:     metadata.ServiceTier,
		StatusCode:      statusCode,
		RequestID:       requestID,
		Counters:        counters,
		HasUsage:        hasUsage,
		Outcome:         outcome,
	})
}

func stringFromMap(values map[string]any, key string) string {
	value, ok := values[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func rawStringFromMap(values map[string]any, key string) string {
	value, ok := values[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}

func chatBoolFromMap(values map[string]any, key string) bool {
	value, ok := values[key]
	if !ok {
		return false
	}
	typed, ok := value.(bool)
	return ok && typed
}

func chatStreamOptionsIncludeUsage(value any) bool {
	options, ok := value.(map[string]any)
	if !ok {
		return false
	}
	includeUsage, ok := options["include_usage"].(bool)
	return ok && includeUsage
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
