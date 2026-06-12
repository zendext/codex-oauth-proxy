package codexonly

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const maxUsageCaptureBytes = 2 << 20

type usageCaptureContext struct {
	Authorization proxyAuthorization
	AuthID        string
	Model         string
	StatusCode    int
	RequestID     string
	RetryAfter    string
	Truncated     bool
	Payload       []byte
	Counters      UsageCounters
	HasUsage      bool
}

type usageCaptureReadCloser struct {
	body      io.ReadCloser
	limit     int
	buf       bytes.Buffer
	truncated bool
	finish    func([]byte, bool)
	once      sync.Once
}

func newUsageCaptureReadCloser(body io.ReadCloser, limit int, finish func([]byte, bool)) *usageCaptureReadCloser {
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
	if c.limit <= 0 || len(chunk) == 0 || c.buf.Len() >= c.limit {
		if len(chunk) > 0 {
			c.truncated = true
		}
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
		if c.finish != nil {
			c.finish(c.buf.Bytes(), c.truncated)
		}
	})
}

func (s *Server) shouldRecordUsage(authorization proxyAuthorization) bool {
	return s != nil &&
		s.users != nil &&
		authorization.Credential != nil &&
		usageTrackingEnabled(s.cfg)
}

func (s *Server) recordProxyUsageFromPayload(_ context.Context, capture usageCaptureContext) {
	if !s.shouldRecordUsage(capture.Authorization) {
		return
	}
	counters, hasUsage := extractUsageCounters(capture.Payload)
	capture.Counters = counters
	capture.HasUsage = hasUsage
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
			"usage response request_id=%s user_id=%s api_key_id=%s key_hash=%s masked_key=%s auth_id=%s model=%s status=%d total_tokens=%d has_usage=%t truncated=%t retry_after=%q",
			capture.RequestID,
			credential.User.ID,
			credential.APIKey.ID,
			credential.APIKey.KeyHash,
			credential.APIKey.MaskedKey,
			capture.AuthID,
			normalizeUsageText(capture.Model, "unknown"),
			capture.StatusCode,
			capture.Counters.TotalTokens,
			capture.HasUsage,
			capture.Truncated,
			capture.RetryAfter,
		)
	}
	err := s.users.RecordUsage(context.Background(), UsageRecordParams{
		Timestamp:   time.Now().UTC(),
		User:        credential.User,
		APIKey:      credential.APIKey,
		Model:       capture.Model,
		AuthID:      capture.AuthID,
		RequestID:   capture.RequestID,
		StatusCode:  capture.StatusCode,
		Counters:    capture.Counters,
		Diagnostics: diagnostics,
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
		"request_id":    capture.RequestID,
		"user_id":       credential.User.ID,
		"api_key_id":    credential.APIKey.ID,
		"key_hash":      credential.APIKey.KeyHash,
		"masked_key":    credential.APIKey.MaskedKey,
		"auth_id":       capture.AuthID,
		"model":         normalizeUsageText(capture.Model, "unknown"),
		"status":        capture.StatusCode,
		"retry_after":   strings.TrimSpace(capture.RetryAfter),
		"has_usage":     capture.HasUsage,
		"truncated":     capture.Truncated,
		"usage_summary": capture.Counters,
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

func captureProxyRequestModel(r *http.Request) string {
	if r == nil || r.URL == nil {
		return "unknown"
	}
	if queryModel := strings.TrimSpace(r.URL.Query().Get("model")); queryModel != "" {
		return queryModel
	}
	if r.Body == nil || r.Body == http.NoBody {
		return "unknown"
	}
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	jsonCandidate := strings.Contains(contentType, "json") ||
		(contentType == "" && (strings.Contains(r.URL.Path, "/responses") || strings.Contains(r.URL.Path, "/alpha/search")))
	if !jsonCandidate {
		return "unknown"
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return "unknown"
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err = decoder.Decode(&payload); err != nil {
		return "unknown"
	}
	if model, ok := payload["model"].(string); ok && strings.TrimSpace(model) != "" {
		return strings.TrimSpace(model)
	}
	return "unknown"
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

func (s *Server) proxyCodexWebSocket(w http.ResponseWriter, r *http.Request, route upstreamRoute, authorization proxyAuthorization, auth *Auth) {
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	upstreamURL := websocketURL(route.baseURL, route.targetPath, r.URL.RawQuery)
	header := outboundWebSocketHeader(r, auth, s.cfg, route.responsesWebsocket)
	dialer := websocketDialer(s.httpClient)
	dialer.Subprotocols = websocket.Subprotocols(r)

	s.debugf(
		"proxy upstream websocket dial method=%s path=%s target=%s auth_id=%s",
		r.Method,
		r.URL.Path,
		upstreamURL,
		auth.ID,
	)
	upstreamConn, upstreamResp, err := dialer.DialContext(r.Context(), upstreamURL, header)
	if err != nil {
		statusCode := http.StatusBadGateway
		if upstreamResp != nil {
			statusCode = upstreamResp.StatusCode
			_ = upstreamResp.Body.Close()
		}
		s.debugf("proxy upstream websocket dial failed method=%s path=%s status=%d error=%q", r.Method, r.URL.Path, statusCode, err.Error())
		if s.shouldRecordUsage(authorization) {
			s.recordProxyUsage(usageCaptureContext{
				Authorization: authorization,
				AuthID:        auth.ID,
				Model:         model,
				StatusCode:    statusCode,
				RequestID:     requestIDFromRequest(r),
			})
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
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
				Authorization: authorization,
				AuthID:        auth.ID,
				Model:         model,
				StatusCode:    http.StatusBadGateway,
				RequestID:     requestIDFromRequest(r),
			})
		}
		return
	}
	defer clientConn.Close()

	var counters UsageCounters
	var hasUsage bool
	var countersMu sync.Mutex
	done := make(chan struct{}, 2)
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = clientConn.Close()
			_ = upstreamConn.Close()
		})
	}

	go func() {
		defer func() {
			closeBoth()
			done <- struct{}{}
		}()
		copyWebSocketMessages(clientConn, upstreamConn, nil)
	}()
	go func() {
		defer func() {
			closeBoth()
			done <- struct{}{}
		}()
		copyWebSocketMessages(upstreamConn, clientConn, func(messageType int, payload []byte) {
			if messageType != websocket.TextMessage {
				return
			}
			if extracted, ok := extractUsageCounters(payload); ok {
				countersMu.Lock()
				counters = extracted
				hasUsage = true
				countersMu.Unlock()
			}
		})
	}()
	<-done
	<-done

	countersMu.Lock()
	captureCounters := counters
	captureHasUsage := hasUsage
	countersMu.Unlock()

	if s.shouldRecordUsage(authorization) {
		s.recordProxyUsage(usageCaptureContext{
			Authorization: authorization,
			AuthID:        auth.ID,
			Model:         model,
			StatusCode:    http.StatusSwitchingProtocols,
			RequestID:     requestIDFromRequest(r),
			Counters:      captureCounters,
			HasUsage:      captureHasUsage,
		})
	}
}

func copyWebSocketMessages(src *websocket.Conn, dst *websocket.Conn, inspect func(int, []byte)) {
	for {
		messageType, payload, err := src.ReadMessage()
		if err != nil {
			return
		}
		if inspect != nil {
			inspect(messageType, payload)
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
