package codexonly

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	maxUsageCaptureBytes     = 2 << 20
	maxUsageCaptureLineBytes = 1 << 20
)

type usageCaptureContext struct {
	Authorization   proxyAuthorization
	AuthID          string
	Model           string
	ReasoningEffort string
	ServiceTier     string
	StatusCode      int
	RequestID       string
	RetryAfter      string
	Truncated       bool
	Payload         []byte
	Counters        UsageCounters
	HasUsage        bool
	DeltaOnly       bool
}

type usageCaptureReadCloser struct {
	body      io.ReadCloser
	limit     int
	buf       bytes.Buffer
	truncated bool
	lineBuf   bytes.Buffer
	lineSkip  bool
	counters  UsageCounters
	hasUsage  bool
	finish    func([]byte, bool, UsageCounters, bool)
	once      sync.Once
}

func newUsageCaptureReadCloser(body io.ReadCloser, limit int, finish func([]byte, bool, UsageCounters, bool)) *usageCaptureReadCloser {
	return &usageCaptureReadCloser{
		body:   body,
		limit:  limit,
		finish: finish,
	}
}

func (c *usageCaptureReadCloser) Read(p []byte) (int, error) {
	n, err := c.body.Read(p)
	if n > 0 {
		c.capture(p[:n])
	}
	if err == io.EOF {
		c.finishOnce()
	}
	return n, err
}

func (c *usageCaptureReadCloser) Close() error {
	err := c.body.Close()
	c.finishOnce()
	return err
}

func (c *usageCaptureReadCloser) capture(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	c.captureUsageLines(chunk)
	if c.limit <= 0 || c.buf.Len() >= c.limit {
		c.truncated = true
		return
	}
	remaining := c.limit - c.buf.Len()
	if len(chunk) > remaining {
		_, _ = c.buf.Write(chunk[:remaining])
		c.truncated = true
		return
	}
	_, _ = c.buf.Write(chunk)
}

func (c *usageCaptureReadCloser) finishOnce() {
	c.once.Do(func() {
		c.flushUsageLine()
		if c.finish != nil {
			c.finish(c.buf.Bytes(), c.truncated, c.counters, c.hasUsage)
		}
	})
}

func (c *usageCaptureReadCloser) captureUsageLines(chunk []byte) {
	for len(chunk) > 0 {
		newline := bytes.IndexByte(chunk, '\n')
		if newline < 0 {
			c.appendUsageLine(chunk)
			return
		}
		c.appendUsageLine(chunk[:newline])
		c.flushUsageLine()
		chunk = chunk[newline+1:]
	}
}

func (c *usageCaptureReadCloser) appendUsageLine(part []byte) {
	if c.lineSkip {
		return
	}
	if c.lineBuf.Len()+len(part) > maxUsageCaptureLineBytes {
		c.lineBuf.Reset()
		c.lineSkip = true
		return
	}
	_, _ = c.lineBuf.Write(part)
}

func (c *usageCaptureReadCloser) flushUsageLine() {
	if c.lineSkip {
		c.lineBuf.Reset()
		c.lineSkip = false
		return
	}
	line := bytes.TrimSpace(c.lineBuf.Bytes())
	c.lineBuf.Reset()
	if len(line) == 0 {
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	} else if !bytes.HasPrefix(line, []byte("{")) && !bytes.HasPrefix(line, []byte("[")) {
		return
	}
	if bytes.Equal(line, []byte("[DONE]")) {
		return
	}
	if !bytes.Contains(line, []byte(`"usage"`)) {
		return
	}
	if counters, ok := extractUsageCounters(line); ok {
		c.counters = counters
		c.hasUsage = true
	}
}

func (s *Server) shouldRecordUsage(authorization proxyAuthorization) bool {
	return s != nil &&
		s.ctx != nil &&
		s.ctx.Err() == nil &&
		s.users != nil &&
		authorization.Credential != nil &&
		usageTrackingEnabled(s.cfg)
}

func (s *Server) recordProxyUsageFromPayload(_ context.Context, capture usageCaptureContext) {
	if !s.shouldRecordUsage(capture.Authorization) {
		return
	}
	if !capture.HasUsage {
		counters, hasUsage := extractUsageCounters(capture.Payload)
		capture.Counters = counters
		capture.HasUsage = hasUsage
	}
	s.recordProxyUsage(capture)
}

func (s *Server) recordProxyUsage(capture usageCaptureContext) {
	if !s.shouldRecordUsage(capture.Authorization) {
		return
	}
	credential := capture.Authorization.Credential
	if capture.RequestID == "" {
		capture.RequestID = newUsageRequestID()
	}
	diagnostics := s.usageDiagnostics(capture)
	if s.debugUsageResponseEnabled() {
		s.debugf(
			"usage response request_id=%s user_id=%s api_key_id=%s key_hash=%s masked_key=%s auth_id=%s model=%s reasoning_effort=%s service_tier=%s status=%d total_tokens=%d has_usage=%t truncated=%t retry_after=%q",
			capture.RequestID,
			credential.User.ID,
			credential.APIKey.ID,
			credential.APIKey.KeyHash,
			credential.APIKey.MaskedKey,
			capture.AuthID,
			safeLogModel(capture.Model),
			normalizeUsageText(capture.ReasoningEffort, "unknown"),
			normalizeServiceTier(capture.ServiceTier),
			capture.StatusCode,
			capture.Counters.TotalTokens,
			capture.HasUsage,
			capture.Truncated,
			capture.RetryAfter,
		)
	}
	err := s.users.RecordUsage(s.ctx, UsageRecordParams{
		Timestamp:       time.Now().UTC(),
		User:            credential.User,
		APIKey:          credential.APIKey,
		Model:           capture.Model,
		ReasoningEffort: capture.ReasoningEffort,
		ServiceTier:     capture.ServiceTier,
		AuthID:          capture.AuthID,
		RequestID:       capture.RequestID,
		StatusCode:      capture.StatusCode,
		Counters:        capture.Counters,
		Diagnostics:     diagnostics,
		DeltaOnly:       capture.DeltaOnly,
	}, s.cfg.Usage)
	if err != nil {
		s.debugf("usage record failed request_id=%s user_id=%s api_key_id=%s error=%q", capture.RequestID, credential.User.ID, credential.APIKey.ID, err.Error())
	}
}

func (s *Server) usageDiagnostics(capture usageCaptureContext) string {
	if !s.shouldRecordUsage(capture.Authorization) {
		return ""
	}
	credential := capture.Authorization.Credential
	payload := map[string]any{
		"request_id":       capture.RequestID,
		"user_id":          credential.User.ID,
		"api_key_id":       credential.APIKey.ID,
		"key_hash":         credential.APIKey.KeyHash,
		"masked_key":       credential.APIKey.MaskedKey,
		"auth_id":          capture.AuthID,
		"model":            normalizeUsageText(capture.Model, "unknown"),
		"reasoning_effort": normalizeUsageText(capture.ReasoningEffort, "unknown"),
		"service_tier":     normalizeServiceTier(capture.ServiceTier),
		"status":           capture.StatusCode,
		"retry_after":      strings.TrimSpace(capture.RetryAfter),
		"has_usage":        capture.HasUsage,
		"truncated":        capture.Truncated,
		"usage_summary":    capture.Counters,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(raw)
}

func (s *Server) debugUsageResponseEnabled() bool {
	return s != nil && s.debugEnabled() && s.cfg != nil && s.cfg.Usage.DebugOpenAIResponse
}

type proxyRequestUsageMetadata struct {
	Model           string
	ReasoningEffort string
	ServiceTier     string
	SkipUsage       bool
}

func captureProxyRequestUsageMetadata(r *http.Request) proxyRequestUsageMetadata {
	metadata := proxyRequestUsageMetadata{}
	if r == nil || r.URL == nil {
		return metadata
	}
	if queryModel := strings.TrimSpace(r.URL.Query().Get("model")); queryModel != "" {
		metadata.Model = queryModel
	}
	if queryEffort := strings.TrimSpace(r.URL.Query().Get("reasoning_effort")); queryEffort != "" {
		metadata.ReasoningEffort = queryEffort
	}
	if queryServiceTier := requestServiceTierFromQuery(r.URL.Query()); queryServiceTier != "" {
		metadata.ServiceTier = normalizeServiceTier(queryServiceTier)
	}
	if r.Body == nil || r.Body == http.NoBody {
		return metadata
	}
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	jsonCandidate := strings.Contains(contentType, "json") ||
		(contentType == "" && (strings.Contains(r.URL.Path, "/responses") || strings.Contains(r.URL.Path, "/alpha/search")))
	if !jsonCandidate {
		return metadata
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		resetRequestBody(r, nil)
		return metadata
	}
	resetRequestBody(r, body)
	if bodyMetadata, ok := usageMetadataFromJSON(body); ok {
		metadata = mergeUsageMetadata(metadata, bodyMetadata)
	}
	return metadata
}

func usageMetadataFromJSON(payload []byte) (proxyRequestUsageMetadata, bool) {
	var value map[string]any
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return proxyRequestUsageMetadata{}, false
	}
	return usageMetadataFromMap(value)
}

func usageMetadataFromMap(payload map[string]any) (proxyRequestUsageMetadata, bool) {
	var metadata proxyRequestUsageMetadata
	if model, ok := payload["model"].(string); ok && strings.TrimSpace(model) != "" {
		metadata.Model = strings.TrimSpace(model)
	}
	if reasoning, ok := mapValue(payload["reasoning"]); ok {
		if effort, okEffort := reasoning["effort"].(string); okEffort && strings.TrimSpace(effort) != "" {
			metadata.ReasoningEffort = strings.TrimSpace(effort)
		}
	}
	if effort, ok := payload["reasoning_effort"].(string); ok && strings.TrimSpace(effort) != "" {
		metadata.ReasoningEffort = strings.TrimSpace(effort)
	}
	if effort, ok := payload["model_reasoning_effort"].(string); ok && strings.TrimSpace(effort) != "" {
		metadata.ReasoningEffort = strings.TrimSpace(effort)
	}
	if serviceTier, ok := payload["service_tier"].(string); ok && strings.TrimSpace(serviceTier) != "" {
		metadata.ServiceTier = normalizeServiceTier(serviceTier)
	}
	if generate, ok := payload["generate"].(bool); ok && !generate {
		metadata.SkipUsage = true
	}
	return metadata, metadata.Model != "" || metadata.ReasoningEffort != "" || metadata.ServiceTier != "" || metadata.SkipUsage
}

func mergeUsageMetadata(current proxyRequestUsageMetadata, next proxyRequestUsageMetadata) proxyRequestUsageMetadata {
	if strings.TrimSpace(next.Model) != "" {
		current.Model = strings.TrimSpace(next.Model)
	}
	if strings.TrimSpace(next.ReasoningEffort) != "" {
		current.ReasoningEffort = strings.TrimSpace(next.ReasoningEffort)
	}
	if strings.TrimSpace(next.ServiceTier) != "" {
		current.ServiceTier = normalizeServiceTier(next.ServiceTier)
	}
	current.SkipUsage = next.SkipUsage
	return current
}

func requestServiceTierFromQuery(query url.Values) string {
	for _, key := range []string{"service_tier", "service-tier"} {
		for _, value := range query[key] {
			if strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

type webSocketUsageEvent struct {
	ResponseID string
	Counters   UsageCounters
}

func webSocketUsageEventFromJSON(payload []byte) (webSocketUsageEvent, bool) {
	counters, ok := extractUsageCounters(payload)
	if !ok {
		return webSocketUsageEvent{}, false
	}
	var responseID string
	if value, okValue := decodeUsageJSON(payload); okValue {
		responseID = usageResponseIDFromValue(value)
	}
	return webSocketUsageEvent{
		ResponseID: responseID,
		Counters:   counters,
	}, true
}

func usageResponseIDFromValue(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		if response, ok := mapValue(typed["response"]); ok {
			if _, hasUsage := response["usage"]; hasUsage {
				if id := stringValue(response["id"]); id != "" {
					return id
				}
			}
			if id := usageResponseIDFromValue(response); id != "" {
				return id
			}
		}
		if _, hasUsage := typed["usage"]; hasUsage {
			if id := stringValue(typed["id"]); id != "" {
				return id
			}
			if id := stringValue(typed["response_id"]); id != "" {
				return id
			}
		}
		if id := stringValue(typed["response_id"]); id != "" {
			return id
		}
		for _, child := range typed {
			if id := usageResponseIDFromValue(child); id != "" {
				return id
			}
		}
	case []any:
		for _, item := range typed {
			if id := usageResponseIDFromValue(item); id != "" {
				return id
			}
		}
	}
	return ""
}

func stringValue(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}

func usageRequestID(r *http.Request, resp *http.Response) string {
	if resp != nil {
		for _, header := range []string{"OpenAI-Request-ID", "X-Request-ID", "Request-ID", "CF-Ray"} {
			if value := strings.TrimSpace(resp.Header.Get(header)); value != "" {
				return value
			}
		}
	}
	return requestIDFromRequest(r)
}

func requestIDFromRequest(r *http.Request) string {
	if r != nil {
		for _, header := range []string{"X-Request-ID", "Request-ID"} {
			if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
				return value
			}
		}
	}
	return newUsageRequestID()
}

func newUsageRequestID() string {
	id, err := randomID("req")
	if err != nil {
		return fmt.Sprintf("req_%d", time.Now().UnixNano())
	}
	return id
}

func (s *Server) proxyCodexWebSocket(
	w http.ResponseWriter,
	r *http.Request,
	route upstreamRoute,
	authorization proxyAuthorization,
	signals []sessionAffinitySignal,
	replayable bool,
) {
	if !s.fastModeAllowed() && queryHasFastServiceTier(r.URL.Query()) {
		writeError(w, http.StatusBadRequest, "fast mode is disabled")
		return
	}
	metadata := proxyRequestUsageMetadata{
		Model:           strings.TrimSpace(r.URL.Query().Get("model")),
		ReasoningEffort: strings.TrimSpace(r.URL.Query().Get("reasoning_effort")),
		ServiceTier:     normalizeServiceTier(requestServiceTierFromQuery(r.URL.Query())),
	}
	var metadataMu sync.Mutex
	currentMetadata := func() proxyRequestUsageMetadata {
		metadataMu.Lock()
		defer metadataMu.Unlock()
		return metadata
	}
	updateMetadata := func(next proxyRequestUsageMetadata) {
		metadataMu.Lock()
		metadata = mergeUsageMetadata(metadata, next)
		metadataMu.Unlock()
	}
	upstreamURL := websocketURL(route.baseURL, route.targetPath, r.URL.RawQuery)
	dialer := websocketDialer(s.httpClient)
	dialer.Subprotocols = websocket.Subprotocols(r)
	var upstreamConn *websocket.Conn
	result, err := s.executeUpstream(
		r.Context(),
		authorization,
		signals,
		metadata.Model,
		replayable,
		func(ctx context.Context, auth *Auth) (*http.Response, error) {
			header := outboundWebSocketHeader(r, auth, s.cfg, route.responsesWebsocket)
			s.debugf(
				"proxy upstream websocket dial method=%s path=%s target=%s auth_id=%s",
				r.Method,
				r.URL.Path,
				upstreamURL,
				auth.ID,
			)
			conn, resp, errDial := dialer.DialContext(ctx, upstreamURL, header)
			if errDial != nil {
				statusCode := http.StatusBadGateway
				if resp != nil {
					statusCode = resp.StatusCode
				}
				s.debugf(
					"proxy upstream websocket dial failed method=%s path=%s status=%d auth_id=%s error=%q",
					r.Method,
					r.URL.Path,
					statusCode,
					auth.ID,
					errDial.Error(),
				)
				if errors.Is(errDial, websocket.ErrBadHandshake) && resp != nil {
					return resp, nil
				}
				return nil, errDial
			}
			upstreamConn = conn
			return resp, nil
		},
	)
	if err != nil {
		if upstreamConn != nil {
			_ = upstreamConn.Close()
		}
		if r.Context().Err() != nil {
			return
		}
		statusCode := http.StatusBadGateway
		var finalErr *proxyFinalError
		if errors.As(err, &finalErr) && finalErr != nil {
			statusCode = finalErr.StatusCode
		}
		authID := ""
		if result.Auth != nil {
			authID = result.Auth.ID
		}
		if s.shouldRecordUsage(authorization) {
			s.recordProxyUsage(usageCaptureContext{
				Authorization:   authorization,
				AuthID:          authID,
				Model:           metadata.Model,
				ReasoningEffort: metadata.ReasoningEffort,
				ServiceTier:     metadata.ServiceTier,
				StatusCode:      statusCode,
				RequestID:       requestIDFromRequest(r),
			})
		}
		if errors.Is(err, ErrStorageFailure) {
			writeStoreError(w, err)
			return
		}
		if finalErr != nil {
			writeProxyError(w, finalErr)
			return
		}
		writeProxyError(w, &proxyFinalError{
			StatusCode: http.StatusBadGateway,
			Code:       proxyErrorCodeUpstream,
			Message:    "upstream Codex service unavailable",
		})
		return
	}
	auth := result.Auth
	upstreamResp := result.Response
	if upstreamConn == nil {
		if upstreamResp == nil {
			writeProxyError(w, &proxyFinalError{
				StatusCode: http.StatusBadGateway,
				Code:       proxyErrorCodeUpstream,
				Message:    "upstream Codex service unavailable",
			})
			return
		}
		defer upstreamResp.Body.Close()
		if s.shouldRecordUsage(authorization) {
			s.recordProxyUsage(usageCaptureContext{
				Authorization:   authorization,
				AuthID:          auth.ID,
				Model:           metadata.Model,
				ReasoningEffort: metadata.ReasoningEffort,
				ServiceTier:     metadata.ServiceTier,
				StatusCode:      upstreamResp.StatusCode,
				RequestID:       usageRequestID(r, upstreamResp),
				RetryAfter:      upstreamResp.Header.Get("Retry-After"),
			})
		}
		writeWebSocketHandshakeResponse(w, upstreamResp)
		return
	}
	if upstreamResp != nil && upstreamResp.Body != nil {
		_ = upstreamResp.Body.Close()
	}
	defer upstreamConn.Close()

	subprotocols := websocket.Subprotocols(r)
	if accepted := upstreamConn.Subprotocol(); accepted != "" {
		subprotocols = []string{accepted}
	}
	upgrader := websocket.Upgrader{
		CheckOrigin:  func(_ *http.Request) bool { return true },
		Subprotocols: subprotocols,
	}
	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.debugf("proxy client websocket upgrade failed method=%s path=%s error=%q", r.Method, r.URL.Path, err.Error())
		if s.shouldRecordUsage(authorization) {
			s.recordProxyUsage(usageCaptureContext{
				Authorization:   authorization,
				AuthID:          auth.ID,
				Model:           metadata.Model,
				ReasoningEffort: metadata.ReasoningEffort,
				ServiceTier:     metadata.ServiceTier,
				StatusCode:      http.StatusBadGateway,
				RequestID:       requestIDFromRequest(r),
			})
		}
		return
	}
	defer clientConn.Close()

	var recordedUsage bool
	recordedResponses := map[string]UsageCounters{}
	var recordedUsageMu sync.Mutex
	recordWebSocketUsage := func(event webSocketUsageEvent) {
		recordMetadata := currentMetadata()
		if recordMetadata.SkipUsage {
			recordedUsageMu.Lock()
			recordedUsage = true
			recordedUsageMu.Unlock()
			return
		}
		counters := event.Counters
		deltaOnly := false
		recordedUsageMu.Lock()
		recordedUsage = true
		if event.ResponseID != "" {
			if previous, ok := recordedResponses[event.ResponseID]; ok {
				counters = counters.subtract(previous)
				deltaOnly = true
			}
			recordedResponses[event.ResponseID] = event.Counters
		}
		if counters.isZero() {
			recordedUsageMu.Unlock()
			return
		}
		recordedUsageMu.Unlock()
		if s.shouldRecordUsage(authorization) {
			s.recordProxyUsage(usageCaptureContext{
				Authorization:   authorization,
				AuthID:          auth.ID,
				Model:           recordMetadata.Model,
				ReasoningEffort: recordMetadata.ReasoningEffort,
				ServiceTier:     recordMetadata.ServiceTier,
				StatusCode:      http.StatusSwitchingProtocols,
				RequestID:       requestIDFromRequest(r),
				Counters:        counters,
				HasUsage:        true,
				DeltaOnly:       deltaOnly,
			})
		}
	}
	done := make(chan struct{}, 2)
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = clientConn.Close()
			_ = upstreamConn.Close()
		})
	}
	stopShutdown := context.AfterFunc(r.Context(), closeBoth)
	defer stopShutdown()

	go func() {
		defer func() {
			closeBoth()
			done <- struct{}{}
		}()
		copyWebSocketMessages(clientConn, upstreamConn, func(messageType int, payload []byte) bool {
			if messageType != websocket.TextMessage {
				return true
			}
			if !s.fastModeAllowed() && payloadHasFastServiceTier(payload) {
				_ = clientConn.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "fast mode is disabled"),
					time.Now().Add(time.Second),
				)
				return false
			}
			if requestMetadata, ok := usageMetadataFromJSON(payload); ok {
				updateMetadata(requestMetadata)
			}
			return true
		})
	}()
	go func() {
		defer func() {
			closeBoth()
			done <- struct{}{}
		}()
		copyWebSocketMessages(upstreamConn, clientConn, func(messageType int, payload []byte) bool {
			if messageType != websocket.TextMessage {
				return true
			}
			if usageEvent, ok := webSocketUsageEventFromJSON(payload); ok {
				recordWebSocketUsage(usageEvent)
			}
			return true
		})
	}()
	<-done
	<-done

	recordedUsageMu.Lock()
	captureHasUsage := recordedUsage
	recordedUsageMu.Unlock()

	if s.shouldRecordUsage(authorization) && !captureHasUsage {
		recordMetadata := currentMetadata()
		s.recordProxyUsage(usageCaptureContext{
			Authorization:   authorization,
			AuthID:          auth.ID,
			Model:           recordMetadata.Model,
			ReasoningEffort: recordMetadata.ReasoningEffort,
			ServiceTier:     recordMetadata.ServiceTier,
			StatusCode:      http.StatusSwitchingProtocols,
			RequestID:       requestIDFromRequest(r),
		})
	}
}

func writeWebSocketHandshakeResponse(w http.ResponseWriter, resp *http.Response) {
	if resp == nil {
		writeProxyError(w, nil)
		return
	}
	excluded := map[string]struct{}{
		"Connection":          {},
		"Content-Length":      {},
		"Keep-Alive":          {},
		"Proxy-Authenticate":  {},
		"Proxy-Authorization": {},
		"Te":                  {},
		"Trailer":             {},
		"Transfer-Encoding":   {},
		"Upgrade":             {},
	}
	for _, value := range resp.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = http.CanonicalHeaderKey(strings.TrimSpace(token)); token != "" {
				excluded[token] = struct{}{}
			}
		}
	}
	for key, values := range resp.Header {
		if _, skip := excluded[http.CanonicalHeaderKey(key)]; skip {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if resp.Body != nil {
		_, _ = io.Copy(w, resp.Body)
	}
}

func copyWebSocketMessages(src *websocket.Conn, dst *websocket.Conn, inspect func(int, []byte) bool) {
	for {
		messageType, payload, err := src.ReadMessage()
		if err != nil {
			return
		}
		if inspect != nil && !inspect(messageType, payload) {
			return
		}
		if err = dst.WriteMessage(messageType, payload); err != nil {
			return
		}
	}
}

func websocketURL(baseURL *url.URL, targetPath string, rawQuery string) string {
	out := &url.URL{}
	if baseURL != nil {
		*out = *baseURL
	}
	switch out.Scheme {
	case "https":
		out.Scheme = "wss"
	default:
		out.Scheme = "ws"
	}
	out.Path = targetPath
	out.RawQuery = rawQuery
	out.Fragment = ""
	out.User = nil
	return out.String()
}

func outboundWebSocketHeader(in *http.Request, auth *Auth, cfg *Config, responsesWebsocket bool) http.Header {
	header := http.Header{}
	if in != nil {
		header = in.Header.Clone()
	}
	for _, key := range []string{
		"Connection",
		"Upgrade",
		"Sec-WebSocket-Key",
		"Sec-WebSocket-Version",
		"Sec-WebSocket-Extensions",
		"Sec-WebSocket-Protocol",
	} {
		header.Del(key)
	}
	out := &http.Request{
		Method: http.MethodGet,
		Header: header,
	}
	applyCodexProxyHeaders(out, in, auth, cfg, responsesWebsocket)
	return out.Header
}

func websocketDialer(client *http.Client) websocket.Dialer {
	dialer := websocket.Dialer{
		Proxy: http.ProxyFromEnvironment,
	}
	if client == nil || client.Transport == nil {
		return dialer
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		return dialer
	}
	dialer.Proxy = transport.Proxy
	dialer.TLSClientConfig = cloneWebSocketTLSConfig(transport.TLSClientConfig)
	dialer.NetDialContext = transport.DialContext
	return dialer
}

func cloneWebSocketTLSConfig(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		return &tls.Config{NextProtos: []string{"http/1.1"}}
	}
	cloned := cfg.Clone()
	cloned.NextProtos = []string{"http/1.1"}
	return cloned
}
