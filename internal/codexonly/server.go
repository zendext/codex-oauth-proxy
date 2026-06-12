package codexonly

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const websocketBetaHeader = "responses_websockets=2026-02-06"

//go:embed codex_client_models.json
var codexClientModelsJSON []byte

type codexClientModelsPayload struct {
	Models []map[string]any `json:"models"`
}

var (
	codexClientModelsOnce sync.Once
	codexClientModelsList []map[string]any
	codexClientModelsErr  error
)

type AuthManager struct {
	Store     *FileAuthStore
	Refresher AuthRefresher
	Now       func() time.Time

	next atomic.Uint64
}

func (m *AuthManager) Select(ctx context.Context) (*Auth, error) {
	if m == nil || m.Store == nil {
		return nil, fmt.Errorf("auth manager is not configured")
	}
	auths, err := m.Store.Load(ctx)
	if err != nil {
		return nil, err
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("no active codex auth files found")
	}
	idx := int(m.next.Add(1)-1) % len(auths)
	auth := auths[idx]
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	if auth.Expired(now()) {
		if m.Refresher == nil {
			return nil, fmt.Errorf("codex auth %s is expired and refresher is not configured", auth.ID)
		}
		if err = m.Refresher.Refresh(ctx, auth); err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(auth.AccessToken) == "" {
		return nil, fmt.Errorf("codex auth %s has no access token", auth.ID)
	}
	return auth, nil
}

type Server struct {
	cfg            *Config
	auths          *AuthManager
	users          *UserStore
	httpClient     *http.Client
	baseURL        *url.URL
	chatGPTBaseURL *url.URL
}

type upstreamRoute struct {
	baseURL            *url.URL
	targetPath         string
	responsesWebsocket bool
	allowUpstreamAuth  bool
}

func NewHandler(ctx context.Context, cfg *Config) (http.Handler, error) {
	if cfg == nil {
		cfg = &Config{}
	}
	ApplyDefaults(cfg)
	authDir, err := ResolveAuthDir(cfg.AuthDir)
	if err != nil {
		return nil, err
	}
	cfg.AuthDir = authDir

	upstream, err := url.Parse(strings.TrimRight(cfg.CodexBaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse codex base URL: %w", err)
	}
	if upstream.Scheme == "" || upstream.Host == "" {
		return nil, fmt.Errorf("codex base URL must include scheme and host")
	}
	chatGPTUpstream, err := url.Parse(strings.TrimRight(cfg.ChatGPTBaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse ChatGPT base URL: %w", err)
	}
	if chatGPTUpstream.Scheme == "" || chatGPTUpstream.Host == "" {
		return nil, fmt.Errorf("ChatGPT base URL must include scheme and host")
	}
	client, err := NewHTTPClient(cfg.ProxyURL, 0)
	if err != nil {
		return nil, err
	}
	refreshClient, err := NewHTTPClient(cfg.ProxyURL, 30*time.Second)
	if err != nil {
		return nil, err
	}
	refresher := &Refresher{
		Client:   refreshClient,
		TokenURL: cfg.CodexRefreshTokenURL,
	}
	manager := &AuthManager{
		Store:     NewFileAuthStore(cfg.AuthDir),
		Refresher: refresher,
	}
	if _, err = manager.Store.Load(ctx); err != nil {
		return nil, err
	}
	databasePath, err := ResolveDatabasePath(cfg.Database.Path, cfg.AuthDir)
	if err != nil {
		return nil, err
	}
	cfg.Database.Path = databasePath
	userStore, err := OpenUserStore(ctx, cfg.Database.Path)
	if err != nil {
		return nil, err
	}
	server := &Server{
		cfg:            cfg,
		auths:          manager,
		users:          userStore,
		httpClient:     client,
		baseURL:        upstream,
		chatGPTBaseURL: chatGPTUpstream,
	}
	server.debugf(
		"debug enabled listen=%s auth_dir=%s database_path=%s codex_base_url=%s chatgpt_base_url=%s",
		ListenAddr(cfg),
		cfg.AuthDir,
		cfg.Database.Path,
		safeURLString(upstream),
		safeURLString(chatGPTUpstream),
	)
	return server, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.debugEnabled() {
		start := time.Now()
		recorder := &debugResponseWriter{ResponseWriter: w}
		route, routeOK := s.proxyRoute(r.URL.Path)
		s.debugf(
			"request received method=%s path=%s route=%s remote=%s user_agent=%q token_sources=%s",
			r.Method,
			r.URL.Path,
			s.debugRouteName(r, routeOK),
			r.RemoteAddr,
			r.UserAgent(),
			tokenSourceSummary(r),
		)
		defer func() {
			s.debugf(
				"request completed method=%s path=%s status=%d duration_ms=%d",
				r.Method,
				r.URL.Path,
				recorder.statusCode(),
				time.Since(start).Milliseconds(),
			)
		}()
		s.serveHTTP(recorder, r, route, routeOK)
		return
	}
	route, routeOK := s.proxyRoute(r.URL.Path)
	s.serveHTTP(w, r, route, routeOK)
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request, route upstreamRoute, routeOK bool) {
	switch {
	case r.URL.Path == "/healthz":
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	case r.URL.Path == "/":
		writeJSON(w, http.StatusOK, map[string]any{"message": "codex-oauth-proxy"})
	case strings.HasPrefix(r.URL.Path, "/v0/management/"):
		if !s.managementAPIEnabled() {
			s.debugf("management auth failed method=%s path=%s reason=disabled", r.Method, r.URL.Path)
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if !s.authorizedAdmin(r) {
			s.debugf(
				"management auth failed method=%s path=%s reason=invalid_admin_key token_sources=%s",
				r.Method,
				r.URL.Path,
				tokenSourceSummary(r),
			)
			writeError(w, http.StatusUnauthorized, "invalid admin API key")
			return
		}
		s.debugf("management auth ok method=%s path=%s", r.Method, r.URL.Path)
		s.handleManagement(w, r)
	case strings.HasPrefix(r.URL.Path, "/v0/user/"):
		credential, err := s.authenticateUserAPIKey(r)
		if err != nil {
			s.debugf(
				"user auth failed method=%s path=%s reason=%s token_sources=%s",
				r.Method,
				r.URL.Path,
				authFailureReason(err),
				tokenSourceSummary(r),
			)
			writeAuthError(w, err)
			return
		}
		s.debugf(
			"user auth ok method=%s path=%s user_id=%s api_key_id=%s",
			r.Method,
			r.URL.Path,
			credential.User.ID,
			credential.APIKey.ID,
		)
		s.handleUserAPI(w, r, credential)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		if err := s.authorizeProxy(r, false); err != nil {
			writeAuthError(w, err)
			return
		}
		s.handleModels(w, r)
	case routeOK:
		if err := s.authorizeProxy(r, route.allowUpstreamAuth); err != nil {
			writeAuthError(w, err)
			return
		}
		s.proxyCodex(w, r, route)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) managementAPIEnabled() bool {
	return s != nil && s.cfg != nil && strings.TrimSpace(s.cfg.AdminAPIKey) != ""
}

func (s *Server) authorizedAdmin(r *http.Request) bool {
	if !s.managementAPIEnabled() {
		return false
	}
	adminKey := strings.TrimSpace(s.cfg.AdminAPIKey)
	for _, token := range candidateProxyTokens(r) {
		if subtle.ConstantTimeCompare([]byte(token), []byte(adminKey)) == 1 {
			return true
		}
	}
	return false
}

func (s *Server) authenticateUserAPIKey(r *http.Request) (AuthenticatedAPIKey, error) {
	return s.authenticateUserAPIKeyFromTokens(r.Context(), candidateProxyTokens(r))
}

func (s *Server) authenticateUserAPIKeyFromTokens(ctx context.Context, tokens []string) (AuthenticatedAPIKey, error) {
	if s == nil || s.users == nil {
		return AuthenticatedAPIKey{}, ErrInvalidAPIKey
	}
	var disabledErr error
	for _, token := range tokens {
		credential, err := s.users.AuthenticateAPIKey(ctx, token)
		if err == nil {
			return credential, nil
		}
		if errors.Is(err, ErrDisabledCredential) {
			disabledErr = err
		}
	}
	if disabledErr != nil {
		return AuthenticatedAPIKey{}, disabledErr
	}
	return AuthenticatedAPIKey{}, ErrInvalidAPIKey
}

func (s *Server) authorizeProxy(r *http.Request, allowUpstreamAuth bool) error {
	tokens := candidateProxyTokens(r)
	if credential, err := s.authenticateUserAPIKeyFromTokens(r.Context(), tokens); err == nil {
		s.debugf(
			"proxy auth ok method=%s path=%s auth=user_api_key user_id=%s api_key_id=%s token_sources=%s",
			r.Method,
			r.URL.Path,
			credential.User.ID,
			credential.APIKey.ID,
			tokenSourceSummary(r),
		)
		return nil
	} else if errors.Is(err, ErrDisabledCredential) {
		s.debugf(
			"proxy auth failed method=%s path=%s reason=disabled_credential token_sources=%s",
			r.Method,
			r.URL.Path,
			tokenSourceSummary(r),
		)
		return err
	}
	if allowUpstreamAuth && s.matchesCurrentCodexAccessToken(r.Context(), tokens) {
		s.debugf(
			"proxy auth ok method=%s path=%s auth=upstream_access_token token_sources=%s",
			r.Method,
			r.URL.Path,
			tokenSourceSummary(r),
		)
		return nil
	}
	s.debugf(
		"proxy auth failed method=%s path=%s reason=invalid_api_key allow_upstream_auth=%t token_sources=%s",
		r.Method,
		r.URL.Path,
		allowUpstreamAuth,
		tokenSourceSummary(r),
	)
	return ErrInvalidAPIKey
}

func (s *Server) matchesCurrentCodexAccessToken(ctx context.Context, tokens []string) bool {
	if s == nil || s.auths == nil || s.auths.Store == nil || len(tokens) == 0 {
		return false
	}
	auths, err := s.auths.Store.Load(ctx)
	if err != nil {
		return false
	}
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		accessToken := strings.TrimSpace(auth.AccessToken)
		if accessToken == "" {
			continue
		}
		for _, token := range tokens {
			if subtle.ConstantTimeCompare([]byte(token), []byte(accessToken)) == 1 {
				return true
			}
		}
	}
	return false
}

func candidateProxyTokens(r *http.Request) []string {
	if r == nil {
		return nil
	}
	var tokens []string
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		auth = strings.TrimSpace(auth[7:])
	}
	if auth != "" {
		tokens = append(tokens, auth)
	}
	if apiKey := strings.TrimSpace(r.Header.Get("X-API-Key")); apiKey != "" {
		tokens = append(tokens, apiKey)
	}
	return tokens
}

type createUserRequest struct {
	Name    string `json:"name"`
	Enabled *bool  `json:"enabled"`
}

type updateUserRequest struct {
	Name    *string `json:"name"`
	Enabled *bool   `json:"enabled"`
}

func (s *Server) handleManagement(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v0/management")
	switch {
	case path == "/users" && r.Method == http.MethodPost:
		var req createUserRequest
		if !decodeJSONRequest(w, r, &req) {
			return
		}
		created, err := s.users.CreateUser(r.Context(), CreateUserParams{Name: req.Name, Enabled: req.Enabled})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, created)
	case path == "/users" && r.Method == http.MethodGet:
		enabled, ok := enabledFilter(w, r)
		if !ok {
			return
		}
		users, err := s.users.ListUsers(r.Context(), enabled)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": users})
	case strings.HasPrefix(path, "/users/"):
		s.handleManagementUser(w, r, strings.TrimPrefix(path, "/users/"))
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleManagementUser(w http.ResponseWriter, r *http.Request, suffix string) {
	if strings.HasSuffix(suffix, "/api-key/reset") {
		userID := strings.TrimSuffix(suffix, "/api-key/reset")
		if userID == "" || strings.Contains(userID, "/") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		if r.Method != http.MethodPost {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		created, err := s.users.ResetUserAPIKey(r.Context(), userID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, created)
		return
	}
	if suffix == "" || strings.Contains(suffix, "/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		user, err := s.users.GetUser(r.Context(), suffix)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, user)
	case http.MethodPatch:
		var req updateUserRequest
		if !decodeJSONRequest(w, r, &req) {
			return
		}
		user, err := s.users.UpdateUser(r.Context(), suffix, UpdateUserParams{Name: req.Name, Enabled: req.Enabled})
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, user)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func (s *Server) handleUserAPI(w http.ResponseWriter, r *http.Request, credential AuthenticatedAPIKey) {
	switch {
	case r.URL.Path == "/v0/user/api-key" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, UserWithAPIKey{User: credential.User, APIKey: &credential.APIKey})
	case r.URL.Path == "/v0/user/api-key/reset" && r.Method == http.MethodPost:
		created, err := s.users.ResetUserAPIKey(r.Context(), credential.User.ID)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, created)
	default:
		writeError(w, http.StatusNotFound, "not found")
	}
}

func enabledFilter(w http.ResponseWriter, r *http.Request) (*bool, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("enabled"))
	if raw == "" {
		return nil, true
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid enabled filter")
		return nil, false
	}
	return &value, true
}

func decodeJSONRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	if r.Body == nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request body")
		return false
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request body")
		return false
	}
	return true
}

func writeAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrDisabledCredential) {
		writeError(w, http.StatusForbidden, "disabled credential")
		return
	}
	writeError(w, http.StatusUnauthorized, "invalid API key")
}

func authFailureReason(err error) string {
	if errors.Is(err, ErrDisabledCredential) {
		return "disabled_credential"
	}
	return "invalid_api_key"
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid request")
	case errors.Is(err, ErrDuplicateUserName):
		writeError(w, http.StatusConflict, "duplicate user name")
	case errors.Is(err, ErrUserNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, ErrDisabledCredential):
		writeError(w, http.StatusForbidden, "disabled credential")
	case errors.Is(err, ErrInvalidAPIKey):
		writeError(w, http.StatusUnauthorized, "invalid API key")
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, ok := r.URL.Query()["client_version"]; ok {
		writeJSON(w, http.StatusOK, map[string]any{"models": codexClientModels()})
		return
	}
	data := make([]map[string]any, 0, len(codexModelIDs()))
	for _, id := range codexModelIDs() {
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  0,
			"owned_by": "openai",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) proxyCodex(w http.ResponseWriter, r *http.Request, route upstreamRoute) {
	auth, err := s.auths.Select(r.Context())
	if err != nil {
		s.debugf("proxy upstream auth unavailable method=%s path=%s error=%q", r.Method, r.URL.Path, err.Error())
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	s.debugf(
		"proxy upstream request method=%s path=%s target_scheme=%s target_host=%s target_path=%s websocket=%t allow_upstream_auth=%t auth_id=%s account_id_present=%t",
		r.Method,
		r.URL.Path,
		route.baseURL.Scheme,
		route.baseURL.Host,
		route.targetPath,
		route.responsesWebsocket && websocketRequested(r),
		route.allowUpstreamAuth,
		auth.ID,
		strings.TrimSpace(auth.AccountID) != "",
	)
	proxy := &httputil.ReverseProxy{
		Director: func(out *http.Request) {
			out.URL.Scheme = route.baseURL.Scheme
			out.URL.Host = route.baseURL.Host
			out.URL.Path = route.targetPath
			out.URL.RawQuery = r.URL.RawQuery
			out.Host = route.baseURL.Host
			applyCodexProxyHeaders(out, r, auth, s.cfg, route.responsesWebsocket)
		},
		Transport: s.httpClient.Transport,
		ModifyResponse: func(resp *http.Response) error {
			s.debugf(
				"proxy upstream response method=%s path=%s status=%d target_host=%s target_path=%s",
				r.Method,
				r.URL.Path,
				resp.StatusCode,
				route.baseURL.Host,
				route.targetPath,
			)
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
			s.debugf(
				"proxy upstream error method=%s path=%s target_host=%s target_path=%s error=%q",
				r.Method,
				r.URL.Path,
				route.baseURL.Host,
				route.targetPath,
				proxyErr.Error(),
			)
			writeError(rw, http.StatusBadGateway, proxyErr.Error())
		},
	}
	proxy.ServeHTTP(w, r)
}

func (s *Server) debugEnabled() bool {
	return s != nil && s.cfg != nil && s.cfg.Debug
}

func (s *Server) debugf(format string, args ...any) {
	if !s.debugEnabled() {
		return
	}
	log.Printf("debug "+format, args...)
}

func (s *Server) debugRouteName(r *http.Request, routeOK bool) string {
	switch {
	case r == nil:
		return "unknown"
	case r.URL.Path == "/healthz":
		return "health"
	case r.URL.Path == "/":
		return "root"
	case strings.HasPrefix(r.URL.Path, "/v0/management/"):
		return "management"
	case strings.HasPrefix(r.URL.Path, "/v0/user/"):
		return "user"
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		return "models"
	case routeOK:
		return "proxy"
	default:
		return "not_found"
	}
}

func tokenSourceSummary(r *http.Request) string {
	if r == nil {
		return "none"
	}
	var parts []string
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if authHeader != "" {
		kind := "raw"
		token := authHeader
		if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
			kind = "bearer"
			token = strings.TrimSpace(authHeader[7:])
		}
		parts = append(parts, fmt.Sprintf("authorization:%s(len=%d,sha256=%s)", kind, len(token), tokenFingerprint(token)))
	}
	if apiKey := strings.TrimSpace(r.Header.Get("X-API-Key")); apiKey != "" {
		parts = append(parts, fmt.Sprintf("x-api-key(len=%d,sha256=%s)", len(apiKey), tokenFingerprint(apiKey)))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

func tokenFingerprint(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

func safeURLString(raw *url.URL) string {
	if raw == nil {
		return ""
	}
	copied := *raw
	copied.User = nil
	copied.RawQuery = ""
	copied.Fragment = ""
	return copied.String()
}

type debugResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *debugResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *debugResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *debugResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *debugResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *debugResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return hijacker.Hijack()
}

func (w *debugResponseWriter) Push(target string, opts *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}

func (w *debugResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(src)
	}
	return io.Copy(w.ResponseWriter, src)
}

func (s *Server) proxyRoute(path string) (upstreamRoute, bool) {
	if suffix, ok := codexEndpointSuffix(path); ok {
		return upstreamRoute{
			baseURL:            s.baseURL,
			targetPath:         targetPath(s.baseURL, "/backend-api/codex", suffix),
			responsesWebsocket: suffix == "/responses",
		}, true
	}
	if suffix, ok := chatGPTFileEndpointSuffix(path); ok {
		return upstreamRoute{
			baseURL:           s.chatGPTBaseURL,
			targetPath:        targetPath(s.chatGPTBaseURL, "/backend-api", suffix),
			allowUpstreamAuth: true,
		}, true
	}
	if suffix, ok := chatGPTWhamEndpointSuffix(path); ok {
		return upstreamRoute{
			baseURL:           s.chatGPTBaseURL,
			targetPath:        targetPath(s.chatGPTBaseURL, "/backend-api", suffix),
			allowUpstreamAuth: true,
		}, true
	}
	if suffix, ok := chatGPTHostedMCPEndpointSuffix(path); ok {
		return upstreamRoute{
			baseURL:           s.chatGPTBaseURL,
			targetPath:        targetPath(s.chatGPTBaseURL, "/backend-api", suffix),
			allowUpstreamAuth: true,
		}, true
	}
	return upstreamRoute{}, false
}

func targetPath(baseURL *url.URL, defaultBasePath string, suffix string) string {
	basePath := ""
	if baseURL != nil {
		basePath = strings.TrimRight(baseURL.EscapedPath(), "/")
	}
	if basePath == "" {
		basePath = defaultBasePath
	}
	if !strings.HasPrefix(suffix, "/") {
		suffix = "/" + suffix
	}
	return basePath + suffix
}

func codexEndpointSuffix(path string) (string, bool) {
	suffix, ok := stripPathPrefix(path, "/backend-api/codex")
	if !ok {
		suffix, ok = stripPathPrefix(path, "/v1")
	}
	if !ok || !isCodexEndpointSuffix(suffix) {
		return "", false
	}
	return suffix, true
}

func isCodexEndpointSuffix(suffix string) bool {
	switch suffix {
	case "/responses",
		"/responses/compact",
		"/alpha/search",
		"/images/generations",
		"/images/edits",
		"/memories/trace_summarize",
		"/realtime/calls",
		"/realtime":
		return true
	default:
		return false
	}
}

func chatGPTFileEndpointSuffix(path string) (string, bool) {
	suffix, ok := stripPathPrefix(path, "/backend-api")
	if !ok {
		suffix, ok = stripPathPrefix(path, "")
	}
	if !ok || !isChatGPTFileEndpointSuffix(suffix) {
		return "", false
	}
	return suffix, true
}

func isChatGPTFileEndpointSuffix(suffix string) bool {
	if suffix == "/files" {
		return true
	}
	const prefix = "/files/"
	const uploadSuffix = "/uploaded"
	if !strings.HasPrefix(suffix, prefix) || !strings.HasSuffix(suffix, uploadSuffix) {
		return false
	}
	fileID := strings.TrimSuffix(strings.TrimPrefix(suffix, prefix), uploadSuffix)
	return fileID != "" && !strings.Contains(fileID, "/")
}

func chatGPTWhamEndpointSuffix(path string) (string, bool) {
	suffix, ok := stripPathPrefix(path, "/backend-api")
	if !ok || !isChatGPTWhamEndpointSuffix(suffix) {
		return "", false
	}
	return suffix, true
}

func isChatGPTWhamEndpointSuffix(suffix string) bool {
	switch suffix {
	case "/wham/usage",
		"/wham/profiles/me",
		"/wham/accounts/check",
		"/wham/accounts/send_add_credits_nudge_email":
		return true
	default:
		return false
	}
}

func chatGPTHostedMCPEndpointSuffix(path string) (string, bool) {
	suffix, ok := stripPathPrefix(path, "/backend-api")
	if !ok || !isChatGPTHostedMCPEndpointSuffix(suffix) {
		return "", false
	}
	return suffix, true
}

func isChatGPTHostedMCPEndpointSuffix(suffix string) bool {
	switch suffix {
	case "/wham/apps",
		"/ps/mcp":
		return true
	default:
		return false
	}
}

func stripPathPrefix(path string, prefix string) (string, bool) {
	if prefix == "" {
		if strings.HasPrefix(path, "/") {
			return path, true
		}
		return "", false
	}
	if path == prefix {
		return "/", true
	}
	if strings.HasPrefix(path, prefix+"/") {
		return strings.TrimPrefix(path, prefix), true
	}
	return "", false
}

func applyCodexProxyHeaders(out *http.Request, in *http.Request, auth *Auth, cfg *Config, responsesWebsocket bool) {
	out.Header.Del("Authorization")
	out.Header.Set("Authorization", "Bearer "+auth.AccessToken)
	if out.Header.Get("Content-Type") == "" && in.Method != http.MethodGet {
		out.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(auth.AccountID) != "" {
		out.Header.Set("Chatgpt-Account-Id", auth.AccountID)
	}
	if strings.TrimSpace(out.Header.Get("Originator")) == "" {
		out.Header.Set("Originator", "codex-tui")
	}
	userAgent := ""
	if cfg != nil {
		userAgent = cfg.CodexUserAgent
	}
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		userAgent = strings.TrimSpace(in.Header.Get("User-Agent"))
	}
	if userAgent == "" || strings.HasPrefix(userAgent, "Go-http-client/") {
		userAgent = DefaultCodexUA
	}
	out.Header.Set("User-Agent", userAgent)
	if strings.Contains(userAgent, "Mac OS") && strings.TrimSpace(out.Header.Get("Session_id")) == "" {
		out.Header.Set("Session_id", newSessionID())
	}
	if cfg != nil && strings.TrimSpace(cfg.CodexBetaFeatures) != "" && strings.TrimSpace(out.Header.Get("X-Codex-Beta-Features")) == "" {
		out.Header.Set("X-Codex-Beta-Features", cfg.CodexBetaFeatures)
	}
	if responsesWebsocket && websocketRequested(in) {
		beta := strings.TrimSpace(out.Header.Get("OpenAI-Beta"))
		if beta == "" || !strings.Contains(beta, "responses_websockets=") {
			out.Header.Set("OpenAI-Beta", websocketBetaHeader)
		}
	}
}

func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func websocketRequested(r *http.Request) bool {
	if r == nil {
		return false
	}
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    http.StatusText(status),
		},
	})
}

func codexModelIDs() []string {
	return []string{
		"gpt-5.5",
		"gpt-5.4",
		"gpt-5.4-mini",
		"gpt-5.3-codex",
		"gpt-5.2",
		"codex-auto-review",
	}
}

func codexClientModels() []map[string]any {
	codexClientModelsOnce.Do(func() {
		var payload codexClientModelsPayload
		codexClientModelsErr = json.Unmarshal(codexClientModelsJSON, &payload)
		if codexClientModelsErr != nil {
			return
		}
		codexClientModelsList = payload.Models
	})
	if codexClientModelsErr != nil {
		return fallbackCodexClientModels()
	}
	out := make([]map[string]any, 0, len(codexClientModelsList))
	for _, model := range codexClientModelsList {
		out = append(out, cloneCodexClientModelMap(model))
	}
	return out
}

func fallbackCodexClientModels() []map[string]any {
	out := make([]map[string]any, 0, len(codexModelIDs()))
	for _, id := range codexModelIDs() {
		out = append(out, map[string]any{
			"slug":                         id,
			"display_name":                 id,
			"supported_in_api":             true,
			"prefer_websockets":            true,
			"supports_parallel_tool_calls": true,
		})
	}
	return out
}

func cloneCodexClientModelMap(model map[string]any) map[string]any {
	if model == nil {
		return nil
	}
	cloned := make(map[string]any, len(model))
	for key, value := range model {
		cloned[key] = cloneCodexClientModelValue(value)
	}
	return cloned
}

func cloneCodexClientModelValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneCodexClientModelMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for i, entry := range typed {
			cloned[i] = cloneCodexClientModelValue(entry)
		}
		return cloned
	default:
		return value
	}
}

func ListenAddr(cfg *Config) string {
	host := ""
	port := DefaultPort
	if cfg != nil {
		host = cfg.Host
		if cfg.Port != 0 {
			port = cfg.Port
		}
	}
	return net.JoinHostPort(host, fmt.Sprintf("%d", port))
}
