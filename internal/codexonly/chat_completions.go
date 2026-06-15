package codexonly

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const defaultChatCompletionInstructions = "You are a helpful assistant."

type chatRequestConversion struct {
	Responses     map[string]any
	Metadata      proxyRequestUsageMetadata
	Stream        bool
	IncludeUsage  bool
	StopSequences []string
}

type chatCompletionState struct {
	id               string
	model            string
	created          int64
	content          strings.Builder
	toolCalls        []chatToolCallState
	toolIndexByItem  map[string]int
	toolIndexByCall  map[string]int
	currentToolIndex int
	usage            UsageCounters
	hasUsage         bool
	finishReason     string
}

type chatToolCallState struct {
	ID        string
	ItemID    string
	Name      string
	Arguments strings.Builder
}

type chatStreamUpdate struct {
	ContentDelta  string
	ToolDeltas    []map[string]any
	Completed     bool
	FinishReason  string
	ResponseID    string
	ResponseModel string
}

type sseEvent struct {
	Event string
	Data  string
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request, authorization proxyAuthorization) {
	conversion, err := decodeChatCompletionRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	auth, err := s.auths.Select(r.Context())
	if err != nil {
		s.debugf("chat completions upstream auth unavailable method=%s path=%s error=%q", r.Method, r.URL.Path, err.Error())
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	payload, err := json.Marshal(conversion.Responses)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chat completion request")
		return
	}
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.codexResponsesURL(), bytes.NewReader(payload))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	upstreamReq.Header.Set("Accept", "text/event-stream")
	applyCodexProxyHeaders(upstreamReq, r, auth, s.cfg, false)

	upstreamResp, err := s.httpClient.Do(upstreamReq)
	if err != nil {
		s.recordChatCompletionUsage(r, authorization, auth, conversion.Metadata, UsageCounters{}, false, http.StatusBadGateway, "")
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer upstreamResp.Body.Close()

	if upstreamResp.StatusCode < 200 || upstreamResp.StatusCode >= 300 {
		s.recordChatCompletionUsage(r, authorization, auth, conversion.Metadata, UsageCounters{}, false, upstreamResp.StatusCode, usageRequestID(r, upstreamResp))
		writeUpstreamChatError(w, upstreamResp)
		return
	}

	state := newChatCompletionState(conversion.Metadata.Model)
	if conversion.Stream {
		s.streamChatCompletion(w, r, upstreamResp, authorization, auth, conversion.Metadata, state, conversion.IncludeUsage, conversion.StopSequences)
		return
	}
	s.aggregateChatCompletion(w, r, upstreamResp, authorization, auth, conversion.Metadata, state, conversion.StopSequences)
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
		if strings.TrimSpace(event.Data) == "[DONE]" {
			return nil
		}
		update, err := state.applyResponsesEvent(event)
		if err != nil {
			return err
		}
		if update.ResponseID != "" {
			state.id = update.ResponseID
		}
		if update.ResponseModel != "" {
			state.model = update.ResponseModel
		}
		return nil
	})
	if readErr != nil {
		s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, http.StatusBadGateway, usageRequestID(r, upstreamResp))
		writeError(w, http.StatusBadGateway, readErr.Error())
		return
	}
	if content, stopped := truncateAtStopSequence(state.content.String(), stopSequences); stopped {
		state.content.Reset()
		state.content.WriteString(content)
		state.finishReason = "stop"
	}
	s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, upstreamResp.StatusCode, usageRequestID(r, upstreamResp))
	writeJSON(w, http.StatusOK, state.chatCompletionResponse())
}

func (s *Server) streamChatCompletion(w http.ResponseWriter, r *http.Request, upstreamResp *http.Response, authorization proxyAuthorization, auth *Auth, metadata proxyRequestUsageMetadata, state *chatCompletionState, includeUsage bool, stopSequences []string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flushChatCompletion(w)

	_ = writeChatSSE(w, state.chatCompletionChunk(map[string]any{"role": "assistant"}, nil, nil, false))
	flushChatCompletion(w)

	var streamErr error
	stopFilter := newChatStopFilter(stopSequences)
	readErr := readResponsesSSE(upstreamResp.Body, func(event sseEvent) error {
		if strings.TrimSpace(event.Data) == "[DONE]" {
			return nil
		}
		update, err := state.applyResponsesEvent(event)
		if err != nil {
			return err
		}
		if update.ResponseID != "" {
			state.id = update.ResponseID
		}
		if update.ResponseModel != "" {
			state.model = update.ResponseModel
		}
		if update.ContentDelta != "" {
			contentDelta, stopped := stopFilter.push(update.ContentDelta)
			if stopped {
				state.finishReason = "stop"
			}
			if contentDelta != "" {
				if errWrite := writeChatSSE(w, state.chatCompletionChunk(map[string]any{"content": contentDelta}, nil, nil, false)); errWrite != nil {
					streamErr = errWrite
					return errWrite
				}
				flushChatCompletion(w)
			}
		}
		for _, delta := range update.ToolDeltas {
			if errWrite := writeChatSSE(w, state.chatCompletionChunk(map[string]any{"tool_calls": []any{delta}}, nil, nil, false)); errWrite != nil {
				streamErr = errWrite
				return errWrite
			}
			flushChatCompletion(w)
		}
		if update.Completed {
			if contentDelta := stopFilter.flush(); contentDelta != "" {
				if errWrite := writeChatSSE(w, state.chatCompletionChunk(map[string]any{"content": contentDelta}, nil, nil, false)); errWrite != nil {
					streamErr = errWrite
					return errWrite
				}
				flushChatCompletion(w)
			}
			finishReason := update.FinishReason
			if finishReason == "" {
				finishReason = state.finalFinishReason()
			}
			if errWrite := writeChatSSE(w, state.chatCompletionChunk(map[string]any{}, &finishReason, nil, false)); errWrite != nil {
				streamErr = errWrite
				return errWrite
			}
			flushChatCompletion(w)
			if includeUsage {
				usage := state.chatUsage()
				if errWrite := writeChatSSE(w, state.chatCompletionChunk(nil, nil, &usage, true)); errWrite != nil {
					streamErr = errWrite
					return errWrite
				}
				flushChatCompletion(w)
			}
		}
		return nil
	})
	statusCode := upstreamResp.StatusCode
	if readErr != nil && streamErr == nil {
		statusCode = http.StatusBadGateway
	}
	s.recordChatCompletionUsage(r, authorization, auth, metadata, state.usage, state.hasUsage, statusCode, usageRequestID(r, upstreamResp))
	if streamErr == nil {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flushChatCompletion(w)
	}
}

func newChatCompletionState(model string) *chatCompletionState {
	created := time.Now().Unix()
	return &chatCompletionState{
		id:               fmt.Sprintf("chatcmpl-%d", created),
		model:            model,
		created:          created,
		toolIndexByItem:  map[string]int{},
		toolIndexByCall:  map[string]int{},
		currentToolIndex: -1,
	}
}

func (s *chatCompletionState) applyResponsesEvent(event sseEvent) (chatStreamUpdate, error) {
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(event.Data))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return chatStreamUpdate{}, err
	}
	eventType := stringFromMap(payload, "type")
	if eventType == "" {
		eventType = event.Event
	}
	switch eventType {
	case "response.created":
		if response, ok := mapValue(payload["response"]); ok {
			s.applyResponseMetadata(response)
			return chatStreamUpdate{ResponseID: s.id, ResponseModel: s.model}, nil
		}
	case "response.output_text.delta":
		delta := rawStringFromMap(payload, "delta")
		s.content.WriteString(delta)
		return chatStreamUpdate{ContentDelta: delta}, nil
	case "response.output_text.done":
		if s.content.Len() == 0 {
			text := firstNonEmptyString(rawStringFromMap(payload, "text"), rawStringFromMap(payload, "output_text"))
			s.content.WriteString(text)
			return chatStreamUpdate{ContentDelta: text}, nil
		}
	case "response.output_item.added", "response.output_item.done":
		if item, ok := mapValue(payload["item"]); ok && stringFromMap(item, "type") == "function_call" {
			existingIndex := s.lookupToolCallIndex(item)
			existingArguments := ""
			if existingIndex >= 0 {
				existingArguments = s.toolCalls[existingIndex].Arguments.String()
			}
			index, created := s.upsertToolCall(item)
			if eventType == "response.output_item.added" || created {
				return chatStreamUpdate{ToolDeltas: []map[string]any{s.initialToolCallDelta(index)}}, nil
			}
			if eventType == "response.output_item.done" && existingArguments == "" {
				if arguments := rawStringFromMap(item, "arguments"); arguments != "" {
					return chatStreamUpdate{ToolDeltas: []map[string]any{s.argumentsToolCallDelta(index, arguments)}}, nil
				}
			}
		}
	case "response.function_call_arguments.delta":
		index := s.toolIndexForPayload(payload)
		if index >= 0 {
			delta := rawStringFromMap(payload, "delta")
			s.toolCalls[index].Arguments.WriteString(delta)
			return chatStreamUpdate{ToolDeltas: []map[string]any{s.argumentsToolCallDelta(index, delta)}}, nil
		}
	case "response.function_call_arguments.done":
		index := s.toolIndexForPayload(payload)
		if index >= 0 {
			arguments := rawStringFromMap(payload, "arguments")
			if arguments != "" {
				s.toolCalls[index].Arguments.Reset()
				s.toolCalls[index].Arguments.WriteString(arguments)
			}
		}
	case "response.completed":
		if response, ok := mapValue(payload["response"]); ok {
			s.applyCompletedResponse(response)
		}
		return chatStreamUpdate{
			Completed:     true,
			FinishReason:  s.finalFinishReason(),
			ResponseID:    s.id,
			ResponseModel: s.model,
		}, nil
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

func (s *chatCompletionState) applyCompletedResponse(response map[string]any) {
	s.applyResponseMetadata(response)
	if counters, ok := usageCountersFromValue(response["usage"]); ok {
		s.usage = counters
		s.hasUsage = true
	}
	if output, ok := response["output"].([]any); ok {
		s.applyResponseOutput(output)
	}
	if reason := responseFinishReason(response, len(s.toolCalls) > 0); reason != "" {
		s.finishReason = reason
	}
}

func (s *chatCompletionState) applyResponseOutput(output []any) {
	for _, itemValue := range output {
		item, ok := itemValue.(map[string]any)
		if !ok {
			continue
		}
		switch stringFromMap(item, "type") {
		case "message":
			if s.content.Len() == 0 {
				s.content.WriteString(outputTextFromMessage(item))
			}
		case "function_call":
			s.upsertToolCall(item)
		}
	}
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

func responseFinishReason(response map[string]any, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	if status := stringFromMap(response, "status"); status == "incomplete" {
		if details, ok := mapValue(response["incomplete_details"]); ok && stringFromMap(details, "reason") == "max_output_tokens" {
			return "length"
		}
	}
	return "stop"
}

func (s *chatCompletionState) upsertToolCall(item map[string]any) (int, bool) {
	itemID := stringFromMap(item, "id")
	callID := firstNonEmptyString(stringFromMap(item, "call_id"), itemID)
	index := s.lookupToolCallIndex(item)
	created := false
	if index < 0 {
		index = len(s.toolCalls)
		created = true
		s.toolCalls = append(s.toolCalls, chatToolCallState{
			ID:     firstNonEmptyString(callID, fmt.Sprintf("call_%d", index)),
			ItemID: itemID,
			Name:   stringFromMap(item, "name"),
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
	if arguments := rawStringFromMap(item, "arguments"); arguments != "" && s.toolCalls[index].Arguments.Len() == 0 {
		s.toolCalls[index].Arguments.WriteString(arguments)
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
	return -1
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
		index := int(outputIndex)
		if index >= 0 && index < len(s.toolCalls) {
			return index
		}
	}
	if s.currentToolIndex >= 0 && s.currentToolIndex < len(s.toolCalls) {
		return s.currentToolIndex
	}
	return -1
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
	maxStopLen    int
	stopped       bool
}

func newChatStopFilter(stopSequences []string) *chatStopFilter {
	filter := &chatStopFilter{stopSequences: stopSequences}
	for _, stop := range stopSequences {
		if len(stop) > filter.maxStopLen {
			filter.maxStopLen = len(stop)
		}
	}
	return filter
}

func (f *chatStopFilter) push(delta string) (string, bool) {
	if f.maxStopLen == 0 {
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
	keep := f.maxStopLen - 1
	if keep <= 0 {
		return combined, false
	}
	if len(combined) <= keep {
		f.buffered = combined
		return "", false
	}
	emitLen := len(combined) - keep
	f.buffered = combined[emitLen:]
	return combined[:emitLen], false
}

func (f *chatStopFilter) flush() string {
	if f.maxStopLen == 0 || f.stopped || f.buffered == "" {
		return ""
	}
	content := f.buffered
	f.buffered = ""
	return content
}

func (s *chatCompletionState) finalFinishReason() string {
	if s.finishReason != "" {
		return s.finishReason
	}
	if len(s.toolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func readResponsesSSE(reader io.Reader, handle func(sseEvent) error) error {
	buffered := bufio.NewReader(reader)
	var event sseEvent
	for {
		line, err := buffered.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				if strings.TrimSpace(event.Data) != "" {
					if errHandle := handle(event); errHandle != nil {
						return errHandle
					}
				}
				event = sseEvent{}
			} else if strings.HasPrefix(line, "event:") {
				event.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if event.Data == "" {
					event.Data = data
				} else {
					event.Data += "\n" + data
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				if strings.TrimSpace(event.Data) != "" {
					if errHandle := handle(event); errHandle != nil {
						return errHandle
					}
				}
				return nil
			}
			return err
		}
	}
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

func flushChatCompletion(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeUpstreamChatError(w http.ResponseWriter, resp *http.Response) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	writeError(w, resp.StatusCode, message)
}

func (s *Server) recordChatCompletionUsage(r *http.Request, authorization proxyAuthorization, auth *Auth, metadata proxyRequestUsageMetadata, counters UsageCounters, hasUsage bool, statusCode int, requestID string) {
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
