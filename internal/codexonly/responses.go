package codexonly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const responsesCreatePath = "/v1/responses"

const responsesProtocolFailureHeader = "X-Codex-Proxy-Responses-Protocol-Failure"

type responsesCreateRequest struct {
	payload  []byte
	metadata proxyRequestUsageMetadata
	stream   bool
}

type responsesTerminalCollector struct {
	response json.RawMessage
	terminal bool
}

func (s *Server) handleResponsesCreate(
	w http.ResponseWriter,
	r *http.Request,
	authorization proxyAuthorization,
	signals []sessionAffinitySignal,
	replayCandidate bool,
) {
	request, validationErr := decodeResponsesCreateRequest(r)
	if validationErr != nil {
		writeResponsesError(w, http.StatusBadRequest, validationErr.message, "invalid_request_error", validationErr.code, validationErr.param)
		return
	}

	replayable := replayCandidate && len(request.payload) <= maxReplayBodyBytes
	s.debugf(
		"proxy upstream request method=%s path=%s target_scheme=%s target_host=%s target_path=%s websocket=false allow_upstream_auth=false replayable=%t",
		r.Method,
		r.URL.Path,
		s.baseURL.Scheme,
		s.baseURL.Host,
		targetPath(s.baseURL, "/backend-api/codex", "/responses"),
		replayable,
	)
	upstreamCtx, cancelUpstream := context.WithCancel(r.Context())
	defer cancelUpstream()
	result, err := s.executeUpstream(
		upstreamCtx,
		authorization,
		signals,
		request.metadata.Model,
		requestClientVersion(r, s.cfg),
		replayable,
		func(ctx context.Context, auth *Auth) (*http.Response, error) {
			upstreamReq, errRequest := s.newResponsesCreateUpstreamRequest(r.WithContext(ctx), request.payload, auth)
			if errRequest != nil {
				return nil, errRequest
			}
			upstreamResp, errRoundTrip := s.httpClient.Transport.RoundTrip(upstreamReq)
			if errRoundTrip != nil {
				return nil, errRoundTrip
			}
			return prepareResponsesCreateUpstreamResponse(ctx, upstreamResp, request.stream)
		},
	)
	auth := result.Auth
	upstreamResp := result.Response
	if err != nil {
		s.debugf("Responses upstream request failed method=%s path=%s error=%q", r.Method, r.URL.Path, err.Error())
		if r.Context().Err() != nil || errors.Is(err, context.Canceled) {
			s.recordResponsesCreateUsage(r, authorization, auth, request.metadata, nil, statusClientClosedRequest, "", chatOutcomeClientCanceled)
			return
		}
		if errors.Is(err, ErrStorageFailure) {
			writeStoreError(w, err)
			return
		}
		var finalErr *proxyFinalError
		if errors.As(err, &finalErr) && finalErr != nil {
			s.recordResponsesCreateUsage(r, authorization, auth, request.metadata, nil, finalErr.StatusCode, "", chatOutcomeUpstreamFailure)
			writeProxyError(w, finalErr)
			return
		}
		s.recordResponsesCreateUsage(r, authorization, auth, request.metadata, nil, http.StatusBadGateway, "", chatOutcomeUpstreamFailure)
		writeProxyError(w, nil)
		return
	}
	if upstreamResp == nil {
		s.recordResponsesCreateUsage(r, authorization, auth, request.metadata, nil, http.StatusBadGateway, "", chatOutcomeUpstreamFailure)
		writeProxyError(w, nil)
		return
	}

	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	endActiveConnection := s.beginAuthConnection(authID)
	defer endActiveConnection()
	defer upstreamResp.Body.Close()
	s.debugf(
		"proxy upstream response method=%s path=%s status=%d target_host=%s target_path=%s auth_id=%s",
		r.Method,
		r.URL.Path,
		upstreamResp.StatusCode,
		s.baseURL.Host,
		targetPath(s.baseURL, "/backend-api/codex", "/responses"),
		authID,
	)

	if upstreamResp.StatusCode < http.StatusOK || upstreamResp.StatusCode >= http.StatusMultipleChoices {
		body := inspectResponseBody(upstreamResp)
		statusCode := upstreamResp.StatusCode
		s.recordResponsesCreateUsage(r, authorization, auth, request.metadata, body, statusCode, usageRequestID(r, upstreamResp), chatOutcomeUpstreamFailure)
		if upstreamResp.Header.Get(responsesProtocolFailureHeader) != "" {
			failure, errClassify := classifyUpstreamResponse(upstreamResp, request.metadata.Model, time.Now())
			if errClassify != nil {
				failure = upstreamFailure{Kind: upstreamFailureTransient, StatusCode: http.StatusBadGateway, Reason: "upstream_error"}
			}
			writeProxyError(w, chatProxyErrorFromFailure(failure))
			return
		}
		writeResponsesUpstreamError(w, statusCode, body)
		return
	}

	if s.shouldRecordUsage(authorization) {
		upstreamResp.Body = newUsageCaptureReadCloser(upstreamResp.Body, maxUsageCaptureBytes, func(payload []byte, truncated bool, counters UsageCounters, hasUsage bool) {
			s.recordProxyUsageFromPayload(r.Context(), usageCaptureContext{
				Authorization:   authorization,
				AuthID:          authID,
				Model:           request.metadata.Model,
				ReasoningEffort: request.metadata.ReasoningEffort,
				ServiceTier:     request.metadata.ServiceTier,
				StatusCode:      upstreamResp.StatusCode,
				RequestID:       usageRequestID(r, upstreamResp),
				Truncated:       truncated,
				Payload:         payload,
				Counters:        counters,
				HasUsage:        hasUsage,
				Outcome:         chatOutcomeSuccess,
			})
		})
	}
	copyResponsesHeaders(w.Header(), upstreamResp.Header)
	w.WriteHeader(upstreamResp.StatusCode)
	if errCopy := copyResponsesBody(w, upstreamResp.Body, request.stream); errCopy != nil {
		cancelUpstream()
	}
}

type responsesValidationError struct {
	message string
	code    string
	param   string
}

func decodeResponsesCreateRequest(r *http.Request) (responsesCreateRequest, *responsesValidationError) {
	if r == nil || r.Body == nil {
		return responsesCreateRequest{}, invalidResponsesJSONError()
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return responsesCreateRequest{}, invalidResponsesJSONError()
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var raw map[string]any
	if err = decoder.Decode(&raw); err != nil || raw == nil {
		return responsesCreateRequest{}, invalidResponsesJSONError()
	}
	var extra any
	if err = decoder.Decode(&extra); err != io.EOF {
		return responsesCreateRequest{}, invalidResponsesJSONError()
	}

	normalizePayload := false
	if value, ok := raw["max_output_tokens"]; ok {
		number, numberOK := value.(json.Number)
		if !numberOK {
			return responsesCreateRequest{}, invalidResponsesParamError("max_output_tokens", "must be a positive integer")
		}
		limit, errLimit := strconv.ParseInt(number.String(), 10, 64)
		if errLimit != nil || limit <= 0 {
			return responsesCreateRequest{}, invalidResponsesParamError("max_output_tokens", "must be a positive integer")
		}
		delete(raw, "max_output_tokens")
		normalizePayload = true
	}
	for _, param := range []string{"background", "store"} {
		value, exists := raw[param]
		if !exists {
			continue
		}
		enabled, ok := value.(bool)
		if !ok {
			return responsesCreateRequest{}, invalidResponsesParamError(param, "must be a boolean")
		}
		if enabled {
			return responsesCreateRequest{}, &responsesValidationError{
				message: fmt.Sprintf("Unsupported value for '%s': this proxy only supports stateless Responses requests.", param),
				code:    "unsupported_value",
				param:   param,
			}
		}
	}

	stream := false
	if value, ok := raw["stream"]; ok {
		stream, ok = value.(bool)
		if !ok {
			return responsesCreateRequest{}, invalidResponsesParamError("stream", "must be a boolean")
		}
	}
	if !stream {
		raw["stream"] = true
		raw["store"] = false
		normalizePayload = true
	}
	metadata, _ := usageMetadataFromMap(raw)

	normalized := body
	if normalizePayload {
		normalized, err = json.Marshal(raw)
		if err != nil {
			return responsesCreateRequest{}, invalidResponsesJSONError()
		}
	}
	return responsesCreateRequest{payload: normalized, metadata: metadata, stream: stream}, nil
}

func invalidResponsesJSONError() *responsesValidationError {
	return &responsesValidationError{message: "Invalid JSON request body.", code: "invalid_json"}
}

func invalidResponsesParamError(param string, reason string) *responsesValidationError {
	return &responsesValidationError{
		message: fmt.Sprintf("Invalid value for '%s': %s.", param, reason),
		code:    "invalid_value",
		param:   param,
	}
}

func (s *Server) newResponsesCreateUpstreamRequest(incoming *http.Request, payload []byte, auth *Auth) (*http.Request, error) {
	upstreamReq, err := http.NewRequestWithContext(incoming.Context(), http.MethodPost, s.codexResponsesURL(), bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	upstreamReq.Header = incoming.Header.Clone()
	removeHopByHopHeaders(upstreamReq.Header)
	upstreamReq.URL.RawQuery = incoming.URL.RawQuery
	upstreamReq.Header.Set("Accept", "text/event-stream")
	applyCodexProxyHeaders(upstreamReq, incoming, auth, s.cfg, false)
	return upstreamReq, nil
}

func prepareResponsesCreateUpstreamResponse(ctx context.Context, resp *http.Response, streaming bool) (*http.Response, error) {
	if streaming || resp == nil || resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return resp, nil
	}
	if resp.Body == nil {
		resp = replaceChatCompletionResponse(resp, newChatProtocolFailure("incomplete_stream"))
		resp.Header.Set(responsesProtocolFailureHeader, "1")
		return resp, nil
	}
	originalBody := resp.Body
	collector := &responsesTerminalCollector{}
	readErr := readResponsesSSE(originalBody, collector.apply)
	if readErr != nil && collector.terminal && !isChatStreamFailure(readErr) {
		readErr = nil
	}
	if readErr == nil && !collector.terminal {
		readErr = newChatProtocolFailure("incomplete_stream")
	}
	if readErr != nil {
		if ctx.Err() != nil {
			_ = originalBody.Close()
			return nil, ctx.Err()
		}
		resp = replaceChatCompletionResponse(resp, chatStreamFailureFromError(readErr))
		resp.Header.Set(responsesProtocolFailureHeader, "1")
		return resp, nil
	}
	_ = originalBody.Close()
	resp.Header = resp.Header.Clone()
	resp.Header.Set("Content-Type", "application/json")
	resp.Body = io.NopCloser(bytes.NewReader(collector.response))
	resp.ContentLength = int64(len(collector.response))
	return resp, nil
}

func (c *responsesTerminalCollector) apply(event sseEvent) error {
	payload, eventType, err := decodeChatResponsesEvent(event)
	if err != nil {
		return err
	}
	if c.terminal {
		return newChatProtocolFailure("data_after_terminal")
	}
	switch eventType {
	case "response.completed", "response.incomplete":
		var raw map[string]json.RawMessage
		if err = json.Unmarshal([]byte(event.Data), &raw); err != nil {
			return newChatProtocolFailure("malformed_event")
		}
		response := bytes.TrimSpace(raw["response"])
		if len(response) == 0 || response[0] != '{' {
			return newChatProtocolFailure("malformed_event")
		}
		var object map[string]any
		if err = json.Unmarshal(response, &object); err != nil || object == nil {
			return newChatProtocolFailure("malformed_event")
		}
		c.response = append(c.response[:0], response...)
		c.terminal = true
	case "response.failed", "error":
		return chatFailureFromEvent(payload, eventType)
	}
	return nil
}

func copyResponsesHeaders(dst http.Header, src http.Header) {
	header := src.Clone()
	removeHopByHopHeaders(header)
	header.Del(responsesProtocolFailureHeader)
	for key, values := range header {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func removeHopByHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = http.CanonicalHeaderKey(strings.TrimSpace(token)); token != "" {
				header.Del(token)
			}
		}
	}
	for _, key := range []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Proxy-Connection",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	} {
		header.Del(key)
	}
}

func copyResponsesBody(w http.ResponseWriter, body io.Reader, streaming bool) error {
	if !streaming {
		_, err := io.Copy(w, body)
		return err
	}
	buffer := make([]byte, 32<<10)
	for {
		n, err := body.Read(buffer)
		if n > 0 {
			if _, errWrite := w.Write(buffer[:n]); errWrite != nil {
				return errWrite
			}
			if errFlush := flushChatCompletion(w); errFlush != nil {
				return errFlush
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func writeResponsesUpstreamError(w http.ResponseWriter, status int, body []byte) {
	info := parseResponsesUpstreamError(body)
	message := info.Message
	if message == "" {
		message = "Upstream Codex request failed."
	}
	errorType := info.Type
	if errorType == "" {
		errorType = "invalid_request_error"
		if status >= http.StatusInternalServerError {
			errorType = "upstream_error"
		}
	}
	writeResponsesError(w, status, message, errorType, info.Code, info.Param)
}

type responsesUpstreamError struct {
	Message string
	Type    string
	Code    string
	Param   string
}

func parseResponsesUpstreamError(body []byte) responsesUpstreamError {
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&payload); err != nil {
		return responsesUpstreamError{}
	}
	source, _ := mapValue(payload["error"])
	if source == nil {
		source = payload
	}
	return responsesUpstreamError{
		Message: firstNonEmptyString(rawStringFromMap(source, "message"), rawStringFromMap(payload, "detail"), rawStringFromMap(payload, "message")),
		Type:    rawStringFromMap(source, "type"),
		Code:    rawStringFromMap(source, "code"),
		Param:   rawStringFromMap(source, "param"),
	}
}

func writeResponsesError(w http.ResponseWriter, status int, message string, errorType string, code string, param string) {
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusBadGateway
	}
	payload := map[string]any{
		"message": message,
		"type":    errorType,
	}
	if strings.TrimSpace(code) != "" {
		payload["code"] = code
	}
	if strings.TrimSpace(param) != "" {
		payload["param"] = param
	}
	writeJSON(w, status, map[string]any{"error": payload})
}

func (s *Server) recordResponsesCreateUsage(r *http.Request, authorization proxyAuthorization, auth *Auth, metadata proxyRequestUsageMetadata, payload []byte, statusCode int, requestID string, outcome string) {
	authID := ""
	if auth != nil {
		authID = auth.ID
	}
	s.recordProxyUsageFromPayload(r.Context(), usageCaptureContext{
		Authorization:   authorization,
		AuthID:          authID,
		Model:           metadata.Model,
		ReasoningEffort: metadata.ReasoningEffort,
		ServiceTier:     metadata.ServiceTier,
		StatusCode:      statusCode,
		RequestID:       requestID,
		Payload:         payload,
		Outcome:         outcome,
	})
}
