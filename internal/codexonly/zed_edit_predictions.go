package codexonly

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

const (
	zedEditPredictionsPath        = "/v1/zed/edit-predictions"
	defaultZedEditPredictionLimit = 256
	maxZedEditPredictionLimit     = 4096
	qwenFIMPrefixMarker           = "<|fim_prefix|>"
	qwenFIMSuffixMarker           = "<|fim_suffix|>"
	qwenFIMMiddleMarker           = "<|fim_middle|>"
	zedEditPredictionInstructions = "Complete the code between the provided prefix and suffix. Return only the missing text, with no Markdown fences, explanation, or repetition of the prefix or suffix."
)

type zedEditPredictionConversion struct {
	responses     map[string]any
	metadata      proxyRequestUsageMetadata
	stopSequences []string
	maxTokens     int
}

func (s *Server) handleZedEditPredictions(
	w http.ResponseWriter,
	r *http.Request,
	authorization proxyAuthorization,
	signals []sessionAffinitySignal,
	replayCandidate bool,
) {
	conversion, err := decodeZedEditPredictionRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	payload, err := json.Marshal(conversion.responses)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid edit prediction request")
		return
	}

	replayable := replayCandidate && len(payload) <= maxReplayBodyBytes
	upstreamCtx, cancelUpstream := context.WithCancel(r.Context())
	defer cancelUpstream()
	result, err := s.executeUpstream(
		upstreamCtx,
		authorization,
		signals,
		conversion.metadata.Model,
		requestClientVersion(r, s.cfg),
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
			return prepareChatCompletionUpstreamResponse(ctx, upstreamResp, false)
		},
	)
	auth := result.Auth
	upstreamResp := result.Response
	if err != nil {
		s.debugf("zed edit prediction upstream request failed method=%s path=%s error=%q", r.Method, r.URL.Path, err.Error())
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			s.recordZedEditPredictionUsage(r, authorization, auth, conversion.metadata, UsageCounters{}, false, statusClientClosedRequest, "", chatOutcomeClientCanceled)
			return
		}
		if errors.Is(err, ErrStorageFailure) {
			writeStoreError(w, err)
			return
		}
		var finalErr *proxyFinalError
		if errors.As(err, &finalErr) && finalErr != nil {
			s.recordZedEditPredictionUsage(r, authorization, auth, conversion.metadata, UsageCounters{}, false, finalErr.StatusCode, "", chatOutcomeUpstreamFailure)
			writeProxyError(w, finalErr)
			return
		}
		s.recordZedEditPredictionUsage(r, authorization, auth, conversion.metadata, UsageCounters{}, false, http.StatusBadGateway, "", chatOutcomeUpstreamFailure)
		writeProxyError(w, nil)
		return
	}

	endActiveConnection := s.beginAuthConnection(auth.ID)
	defer endActiveConnection()
	defer upstreamResp.Body.Close()

	if upstreamResp.StatusCode < http.StatusOK || upstreamResp.StatusCode >= http.StatusMultipleChoices {
		failure, errClassify := classifyUpstreamResponse(upstreamResp, conversion.metadata.Model, time.Now())
		if errClassify != nil {
			failure = upstreamFailure{
				Kind:       upstreamFailureTransient,
				StatusCode: http.StatusBadGateway,
				Reason:     "upstream_error",
			}
		}
		finalErr := chatProxyErrorFromFailure(failure)
		s.recordZedEditPredictionUsage(r, authorization, auth, conversion.metadata, UsageCounters{}, false, finalErr.StatusCode, usageRequestID(r, upstreamResp), chatOutcomeUpstreamFailure)
		writeProxyError(w, finalErr)
		return
	}

	state := newChatCompletionState(conversion.metadata.Model)
	readErr := readResponsesSSE(upstreamResp.Body, func(event sseEvent) error {
		update, errApply := state.applyResponsesEvent(event, false)
		if update.UnknownReason != "" {
			s.debugf("zed edit prediction unknown incomplete reason=%s", safeChatLogValue(update.UnknownReason))
		}
		return errApply
	})
	if readErr != nil && state.terminal && !isChatStreamFailure(readErr) {
		readErr = nil
	}
	if readErr == nil && !state.terminal {
		readErr = newChatProtocolFailure("incomplete_stream")
	}
	if readErr == nil && len(state.toolCalls) > 0 {
		readErr = newChatProtocolFailure("unexpected_tool_output")
	}
	if readErr != nil {
		finalErr, statusCode := chatProxyErrorFromStreamError(readErr, conversion.metadata.Model)
		s.recordZedEditPredictionUsage(r, authorization, auth, conversion.metadata, state.usage, state.hasUsage, statusCode, usageRequestID(r, upstreamResp), chatOutcomeUpstreamFailure)
		writeProxyError(w, finalErr)
		return
	}

	text := state.content.String()
	finishReason := state.finalFinishReason()
	if filtered, stopped := truncateAtStopSequence(text, conversion.stopSequences); stopped {
		text = filtered
		finishReason = "stop"
	}
	if limited, truncated := truncateZedEditPrediction(text, conversion.maxTokens); truncated {
		text = limited
		finishReason = "length"
	}

	created := time.Now().Unix()
	responseID, errID := randomID("cmpl")
	if errID != nil {
		responseID = fmt.Sprintf("cmpl_%d", time.Now().UnixNano())
	}
	s.recordZedEditPredictionUsage(r, authorization, auth, conversion.metadata, state.usage, state.hasUsage, upstreamResp.StatusCode, usageRequestID(r, upstreamResp), chatOutcomeSuccess)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      responseID,
		"object":  "text_completion",
		"created": created,
		"model":   conversion.metadata.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"text":          text,
			"finish_reason": finishReason,
		}},
		"usage": state.chatUsage(),
	})
}

func decodeZedEditPredictionRequest(r *http.Request) (zedEditPredictionConversion, error) {
	if r == nil || r.Body == nil {
		return zedEditPredictionConversion{}, fmt.Errorf("invalid JSON request body")
	}
	contentType, _, err := mime.ParseMediaType(strings.TrimSpace(r.Header.Get("Content-Type")))
	if err != nil || !strings.EqualFold(contentType, "application/json") {
		return zedEditPredictionConversion{}, fmt.Errorf("content type must be application/json")
	}

	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	var raw map[string]any
	if err = decoder.Decode(&raw); err != nil {
		return zedEditPredictionConversion{}, fmt.Errorf("invalid JSON request body")
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return zedEditPredictionConversion{}, fmt.Errorf("invalid JSON request body")
	}

	modelValue, ok := raw["model"].(string)
	if !ok {
		return zedEditPredictionConversion{}, fmt.Errorf("missing or invalid required field: model")
	}
	model, ok := normalizeModelIdentifier(modelValue)
	if !ok {
		return zedEditPredictionConversion{}, fmt.Errorf("missing or invalid required field: model")
	}
	prompt, ok := raw["prompt"].(string)
	if !ok {
		return zedEditPredictionConversion{}, fmt.Errorf("missing or invalid required field: prompt")
	}
	prefix, suffix, err := parseQwenFIMPrompt(prompt)
	if err != nil {
		return zedEditPredictionConversion{}, err
	}

	maxTokensValue, hasMaxTokens := raw["max_tokens"]
	maxTokens, err := zedEditPredictionMaxTokens(maxTokensValue, hasMaxTokens)
	if err != nil {
		return zedEditPredictionConversion{}, err
	}
	if temperature, exists := raw["temperature"]; exists {
		if _, ok = temperature.(json.Number); !ok {
			return zedEditPredictionConversion{}, fmt.Errorf("invalid field: temperature")
		}
	}
	stopValue, hasStop := raw["stop"]
	stopSequences, err := zedEditPredictionStopSequences(stopValue, hasStop)
	if err != nil {
		return zedEditPredictionConversion{}, err
	}

	return zedEditPredictionConversion{
		responses: map[string]any{
			"model":        model,
			"stream":       true,
			"store":        false,
			"instructions": zedEditPredictionInstructions,
			"reasoning": map[string]any{
				"effort": "low",
			},
			"input": []any{map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "Prefix:\n" + prefix},
					map[string]any{"type": "input_text", "text": "Suffix:\n" + suffix},
				},
			}},
		},
		metadata: proxyRequestUsageMetadata{
			Model:           model,
			ReasoningEffort: "low",
		},
		stopSequences: stopSequences,
		maxTokens:     maxTokens,
	}, nil
}

func parseQwenFIMPrompt(prompt string) (string, string, error) {
	if strings.Count(prompt, qwenFIMPrefixMarker) != 1 ||
		strings.Count(prompt, qwenFIMSuffixMarker) != 1 ||
		strings.Count(prompt, qwenFIMMiddleMarker) != 1 ||
		!strings.HasPrefix(prompt, qwenFIMPrefixMarker) ||
		!strings.HasSuffix(prompt, qwenFIMMiddleMarker) {
		return "", "", fmt.Errorf("prompt must contain exactly one ordered Qwen FIM sequence")
	}

	suffixIndex := strings.Index(prompt, qwenFIMSuffixMarker)
	middleIndex := strings.Index(prompt, qwenFIMMiddleMarker)
	if suffixIndex < len(qwenFIMPrefixMarker) ||
		middleIndex < suffixIndex+len(qwenFIMSuffixMarker) {
		return "", "", fmt.Errorf("prompt must contain exactly one ordered Qwen FIM sequence")
	}
	prefix := prompt[len(qwenFIMPrefixMarker):suffixIndex]
	suffix := prompt[suffixIndex+len(qwenFIMSuffixMarker) : middleIndex]
	return prefix, suffix, nil
}

func zedEditPredictionMaxTokens(value any, exists bool) (int, error) {
	if !exists {
		return defaultZedEditPredictionLimit, nil
	}
	number, ok := value.(json.Number)
	if !ok {
		return 0, fmt.Errorf("invalid field: max_tokens")
	}
	parsed, err := number.Int64()
	if err != nil || parsed <= 0 || parsed > maxZedEditPredictionLimit {
		return 0, fmt.Errorf("max_tokens must be an integer between 1 and %d", maxZedEditPredictionLimit)
	}
	return int(parsed), nil
}

func zedEditPredictionStopSequences(value any, exists bool) ([]string, error) {
	if !exists {
		return nil, nil
	}
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return nil, fmt.Errorf("invalid field: stop")
		}
		return []string{typed}, nil
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			text, ok := entry.(string)
			if !ok || text == "" {
				return nil, fmt.Errorf("invalid field: stop")
			}
			out = append(out, text)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("invalid field: stop")
	}
}

func truncateZedEditPrediction(text string, maxRunes int) (string, bool) {
	if maxRunes <= 0 {
		return "", text != ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text, false
	}
	return string(runes[:maxRunes]), true
}

func (s *Server) recordZedEditPredictionUsage(
	r *http.Request,
	authorization proxyAuthorization,
	auth *Auth,
	metadata proxyRequestUsageMetadata,
	counters UsageCounters,
	hasUsage bool,
	statusCode int,
	requestID string,
	outcome string,
) {
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
