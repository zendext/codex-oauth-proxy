package codexonly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxReplayBodyBytes          = 32 << 20
	maxFailureInspectionBytes   = 1 << 20
	defaultTransientCooldown    = time.Minute
	proxyRetryAfterHeader       = "Retry-After"
	proxyErrorCodeModelNotFound = "model_not_found"
	proxyErrorCodeRateLimited   = "rate_limited"
	proxyErrorCodeAuth          = "auth_unavailable"
	proxyErrorCodeUpstream      = "upstream_unavailable"
)

type upstreamFailureKind string

const (
	upstreamFailureNone             upstreamFailureKind = ""
	upstreamFailureRequestScoped    upstreamFailureKind = "request_scoped"
	upstreamFailureUnauthorized     upstreamFailureKind = "unauthorized"
	upstreamFailureQuota            upstreamFailureKind = "quota"
	upstreamFailureTransient        upstreamFailureKind = "transient"
	upstreamFailureModelUnsupported upstreamFailureKind = "model_not_supported"
	upstreamFailureCredential       upstreamFailureKind = "credential_invalid"
)

type upstreamFailure struct {
	Kind       upstreamFailureKind
	StatusCode int
	ErrorCode  string
	Reason     string
	RetryAt    time.Time
	Explicit   bool
}

func waitForProxyRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type upstreamExecution struct {
	Response  *http.Response
	Auth      *Auth
	Selection AuthSelection
}

type upstreamAttemptFunc func(context.Context, *Auth) (*http.Response, error)

type failureAggregate struct {
	byAuth map[string]upstreamFailure
}

func (a *failureAggregate) record(auth *Auth, failure upstreamFailure) {
	if a == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return
	}
	if a.byAuth == nil {
		a.byAuth = make(map[string]upstreamFailure)
	}
	a.byAuth[auth.ID] = failure
}

type proxyFinalError struct {
	StatusCode int
	Code       string
	Message    string
	RetryAt    time.Time
}

func (e *proxyFinalError) Error() string {
	if e == nil || e.Message == "" {
		return "upstream request failed"
	}
	return e.Message
}

func (s *Server) executeUpstream(
	ctx context.Context,
	authorization proxyAuthorization,
	signals []sessionAffinitySignal,
	model string,
	replayable bool,
	attempt upstreamAttemptFunc,
) (upstreamExecution, error) {
	if s == nil || s.auths == nil || s.health == nil || attempt == nil {
		return upstreamExecution{}, fmt.Errorf("upstream execution is not configured")
	}
	initial, err := s.reconcileAuths(ctx)
	if err != nil {
		return upstreamExecution{}, err
	}
	allowed := make(map[string]struct{}, len(initial.Auths))
	for _, auth := range initial.Auths {
		if auth != nil {
			allowed[auth.ID] = struct{}{}
		}
	}
	aggregate := failureAggregate{}
	selection := AuthSelection{}
	requestRetry := s.cfg.RequestRetry
	maxCredentials := s.cfg.MaxRetryCredentials
	for round := 0; ; round++ {
		tried := make(map[string]struct{})
		attempted := 0
		for {
			if maxCredentials > 0 && attempted >= maxCredentials {
				break
			}
			next, err := s.selectProxyAuthCandidate(ctx, authorization, signals, model, tried, allowed, selection)
			if err != nil {
				if errors.Is(err, errNoEligibleAuth) {
					break
				}
				return upstreamExecution{}, err
			}
			selection = next
			auth := selection.Auth
			tried[auth.ID] = struct{}{}
			attempted++

			prepared, err := s.auths.prepareAuth(ctx, auth)
			if err != nil {
				if ctx.Err() != nil {
					return upstreamExecution{}, ctx.Err()
				}
				failure := preparationFailure(err, s.health.currentTime())
				if err = s.markAuthFailure(ctx, auth, model, failure); err != nil {
					return upstreamExecution{}, err
				}
				aggregate.record(auth, failure)
				continue
			}
			auth = prepared
			selection.Auth = auth

			resp, err := attempt(ctx, auth)
			if err != nil {
				if ctx.Err() != nil {
					return upstreamExecution{}, ctx.Err()
				}
				failure := upstreamFailure{
					Kind:       upstreamFailureTransient,
					StatusCode: http.StatusBadGateway,
					Reason:     "network",
					RetryAt:    s.health.currentTime().Add(defaultTransientCooldown),
				}
				if err = s.markAuthFailure(ctx, auth, model, failure); err != nil {
					return upstreamExecution{}, err
				}
				aggregate.record(auth, failure)
				if !replayable {
					return upstreamExecution{Auth: auth, Selection: selection}, s.finalProxyError(ctx, model, aggregate, allowed)
				}
				continue
			}

			failure, err := classifyUpstreamResponse(resp, model, s.health.currentTime())
			if err != nil {
				_ = resp.Body.Close()
				return upstreamExecution{}, err
			}
			if failure.Kind == upstreamFailureUnauthorized {
				if !replayable {
					healthFailure := upstreamFailure{
						Kind:       upstreamFailureCredential,
						StatusCode: http.StatusUnauthorized,
						ErrorCode:  firstNonEmptyString(failure.ErrorCode, "unauthorized"),
						Reason:     "unauthorized",
					}
					if err = s.markAuthFailure(ctx, auth, model, healthFailure); err != nil {
						_ = resp.Body.Close()
						return upstreamExecution{}, err
					}
					return upstreamExecution{Response: resp, Auth: auth, Selection: selection}, nil
				}
				result, handled, errUnauthorized := s.repairUnauthorized(
					ctx,
					authorization,
					signals,
					model,
					replayable,
					attempt,
					selection,
					resp,
					auth,
					&aggregate,
				)
				if errUnauthorized != nil {
					return upstreamExecution{}, errUnauthorized
				}
				if handled {
					return result, nil
				}
				continue
			}

			if failure.Kind == upstreamFailureNone {
				if err = s.health.MarkHealthy(ctx, auth, model); err != nil {
					_ = resp.Body.Close()
					return upstreamExecution{}, err
				}
				return upstreamExecution{Response: resp, Auth: auth, Selection: selection}, nil
			}
			if failure.Kind == upstreamFailureRequestScoped {
				return upstreamExecution{Response: resp, Auth: auth, Selection: selection}, nil
			}
			if err = s.markAuthFailure(ctx, auth, model, failure); err != nil {
				_ = resp.Body.Close()
				return upstreamExecution{}, err
			}
			aggregate.record(auth, failure)
			if !replayable {
				return upstreamExecution{Response: resp, Auth: auth, Selection: selection}, nil
			}
			discardAndCloseResponse(resp)
		}

		if !replayable || round >= requestRetry || s.cfg.MaxRetryInterval <= 0 {
			break
		}
		deadline, ok, err := s.nearestCooldown(ctx, model, aggregate, allowed)
		if err != nil {
			return upstreamExecution{}, err
		}
		if !ok {
			break
		}
		delay := deadline.Sub(s.health.currentTime())
		if delay < 0 {
			delay = 0
		}
		maxWait := time.Duration(s.cfg.MaxRetryInterval) * time.Second
		if delay > maxWait {
			break
		}
		s.debugf(
			"proxy retry round=%d next_round=%d wait_ms=%d model=%s",
			round,
			round+1,
			delay.Milliseconds(),
			safeLogModel(model),
		)
		wait := s.waitRetry
		if wait == nil {
			wait = waitForProxyRetry
		}
		if err = wait(ctx, delay); err != nil {
			return upstreamExecution{}, err
		}
	}
	return upstreamExecution{}, s.finalProxyError(ctx, model, aggregate, allowed)
}

func (s *Server) repairUnauthorized(
	ctx context.Context,
	authorization proxyAuthorization,
	signals []sessionAffinitySignal,
	model string,
	replayable bool,
	attempt upstreamAttemptFunc,
	selection AuthSelection,
	firstResp *http.Response,
	auth *Auth,
	aggregate *failureAggregate,
) (upstreamExecution, bool, error) {
	_ = authorization
	_ = signals
	failedAccessToken := auth.AccessToken
	if replayable {
		discardAndCloseResponse(firstResp)
	}
	refreshed := cloneAuth(auth)
	err := s.auths.RefreshAfterUnauthorized(ctx, refreshed, failedAccessToken)
	if err != nil {
		if ctx.Err() != nil {
			return upstreamExecution{}, false, ctx.Err()
		}
		failure := preparationFailure(err, s.health.currentTime())
		if failure.Kind == upstreamFailureCredential && failure.ErrorCode == "" {
			failure.ErrorCode = "unauthorized"
		}
		if err = s.markAuthFailure(ctx, auth, model, failure); err != nil {
			return upstreamExecution{}, false, err
		}
		aggregate.record(auth, failure)
		if !replayable {
			return upstreamExecution{Response: firstResp, Auth: auth, Selection: selection}, true, nil
		}
		return upstreamExecution{}, false, nil
	}
	if err = s.health.MarkCredentialHealthy(ctx, refreshed); err != nil {
		return upstreamExecution{}, false, err
	}
	selection.Auth = refreshed
	if !replayable {
		return upstreamExecution{Response: firstResp, Auth: refreshed, Selection: selection}, true, nil
	}

	retryResp, err := attempt(ctx, refreshed)
	if err != nil {
		if ctx.Err() != nil {
			return upstreamExecution{}, false, ctx.Err()
		}
		failure := upstreamFailure{
			Kind:       upstreamFailureTransient,
			StatusCode: http.StatusBadGateway,
			Reason:     "network",
			RetryAt:    s.health.currentTime().Add(defaultTransientCooldown),
		}
		if err = s.markAuthFailure(ctx, refreshed, model, failure); err != nil {
			return upstreamExecution{}, false, err
		}
		aggregate.record(refreshed, failure)
		return upstreamExecution{}, false, nil
	}
	failure, err := classifyUpstreamResponse(retryResp, model, s.health.currentTime())
	if err != nil {
		_ = retryResp.Body.Close()
		return upstreamExecution{}, false, err
	}
	if failure.Kind == upstreamFailureNone {
		if err = s.health.MarkHealthy(ctx, refreshed, model); err != nil {
			_ = retryResp.Body.Close()
			return upstreamExecution{}, false, err
		}
		return upstreamExecution{Response: retryResp, Auth: refreshed, Selection: selection}, true, nil
	}
	if failure.Kind == upstreamFailureRequestScoped {
		return upstreamExecution{Response: retryResp, Auth: refreshed, Selection: selection}, true, nil
	}
	if failure.Kind == upstreamFailureUnauthorized {
		failure = upstreamFailure{
			Kind:       upstreamFailureUnauthorized,
			StatusCode: http.StatusUnauthorized,
			ErrorCode:  firstNonEmptyString(failure.ErrorCode, "unauthorized"),
			Reason:     "unauthorized",
		}
	}
	if err = s.markAuthFailure(ctx, refreshed, model, failure); err != nil {
		_ = retryResp.Body.Close()
		return upstreamExecution{}, false, err
	}
	aggregate.record(refreshed, failure)
	discardAndCloseResponse(retryResp)
	return upstreamExecution{}, false, nil
}

func preparationFailure(err error, now time.Time) upstreamFailure {
	failure := upstreamFailure{
		Kind:       upstreamFailureCredential,
		StatusCode: http.StatusServiceUnavailable,
		Reason:     "credential_invalid",
	}
	var refreshErr *oauthRefreshError
	if !errors.As(err, &refreshErr) || refreshErr == nil {
		return failure
	}
	failure.StatusCode = refreshErr.status
	failure.ErrorCode = safeHealthCode(refreshErr.code)
	if failure.ErrorCode == "invalid_grant" {
		failure.Reason = "invalid_grant"
		return failure
	}
	if refreshErr.retryable {
		failure.Kind = upstreamFailureTransient
		failure.Reason = "refresh_failed"
		failure.RetryAt = now.Add(defaultTransientCooldown)
	}
	return failure
}

func (s *Server) markAuthFailure(ctx context.Context, auth *Auth, model string, failure upstreamFailure) error {
	switch failure.Kind {
	case upstreamFailureModelUnsupported:
		return s.health.MarkModelUnsupported(ctx, auth, model, failure)
	case upstreamFailureQuota:
		retryAt := failure.RetryAt
		if retryAt.IsZero() {
			retryAt = s.health.currentTime().Add(defaultTransientCooldown)
		}
		return s.health.MarkUnavailable(ctx, auth, AuthHealthState{
			Kind:          AuthHealthQuota,
			Reason:        "quota",
			RetryAt:       retryAt,
			Authoritative: failure.Explicit,
			StatusCode:    failure.StatusCode,
			ErrorCode:     failure.ErrorCode,
		})
	case upstreamFailureUnauthorized:
		return s.health.MarkUnavailable(ctx, auth, AuthHealthState{
			Kind:       AuthHealthUnauthorized,
			Reason:     "unauthorized",
			StatusCode: http.StatusUnauthorized,
			ErrorCode:  firstNonEmptyString(failure.ErrorCode, "unauthorized"),
		})
	case upstreamFailureCredential:
		kind := AuthHealthCredentialInvalid
		if failure.ErrorCode == "invalid_grant" {
			kind = AuthHealthInvalidGrant
		}
		return s.health.MarkUnavailable(ctx, auth, AuthHealthState{
			Kind:       kind,
			Reason:     failure.Reason,
			StatusCode: failure.StatusCode,
			ErrorCode:  failure.ErrorCode,
		})
	case upstreamFailureTransient:
		retryAt := failure.RetryAt
		if retryAt.IsZero() {
			retryAt = s.health.currentTime().Add(defaultTransientCooldown)
		}
		return s.health.MarkUnavailable(ctx, auth, AuthHealthState{
			Kind:       AuthHealthTransient,
			Reason:     failure.Reason,
			RetryAt:    retryAt,
			StatusCode: failure.StatusCode,
			ErrorCode:  failure.ErrorCode,
		})
	default:
		return nil
	}
}

func (s *Server) nearestCooldown(ctx context.Context, model string, aggregate failureAggregate, allowed map[string]struct{}) (time.Time, bool, error) {
	result, err := s.reconcileAuths(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	active := make([]*Auth, 0, len(result.Active))
	for _, auth := range result.Active {
		if _, ok := allowed[auth.ID]; ok {
			active = append(active, auth)
		}
	}
	nearest, found := s.health.NearestCooldown(active, model)
	now := s.health.currentTime()
	for _, failure := range aggregate.byAuth {
		if failure.RetryAt.IsZero() {
			continue
		}
		retryAt := failure.RetryAt
		if retryAt.Before(now) {
			retryAt = now
		}
		if !found || retryAt.Before(nearest) {
			nearest = retryAt
			found = true
		}
	}
	return nearest, found, nil
}

func (s *Server) finalProxyError(ctx context.Context, model string, aggregate failureAggregate, allowed map[string]struct{}) error {
	result, err := s.reconcileAuths(ctx)
	if err != nil {
		return err
	}
	now := s.health.currentTime()
	type counts struct {
		total      int
		model      int
		disabled   int
		credential int
		quota      int
		transient  int
	}
	var stateCounts counts
	var earliestQuota time.Time
	for _, auth := range result.Auths {
		if auth == nil {
			continue
		}
		if _, ok := allowed[auth.ID]; !ok {
			continue
		}
		stateCounts.total++
		state, blocked := s.health.Blocked(auth, model)
		if !blocked {
			if failure, ok := aggregate.byAuth[auth.ID]; ok {
				state = healthStateFromFailure(failure)
				blocked = true
			}
		}
		if state.Kind == AuthHealthModelUnsupported {
			stateCounts.model++
			continue
		}
		if auth.Disabled {
			stateCounts.disabled++
			continue
		}
		if !blocked {
			continue
		}
		switch state.Kind {
		case AuthHealthInvalidGrant, AuthHealthUnauthorized, AuthHealthCredentialInvalid:
			stateCounts.credential++
		case AuthHealthQuota:
			stateCounts.quota++
			if !state.RetryAt.IsZero() && (earliestQuota.IsZero() || state.RetryAt.Before(earliestQuota)) {
				earliestQuota = state.RetryAt
			}
		case AuthHealthTransient:
			stateCounts.transient++
		}
	}
	if stateCounts.total > 0 && stateCounts.model == stateCounts.total {
		return &proxyFinalError{
			StatusCode: http.StatusNotFound,
			Code:       proxyErrorCodeModelNotFound,
			Message:    "requested model is unavailable for all configured Codex auths",
		}
	}
	if stateCounts.total == 0 || stateCounts.disabled+stateCounts.credential == stateCounts.total {
		return &proxyFinalError{
			StatusCode: http.StatusServiceUnavailable,
			Code:       proxyErrorCodeAuth,
			Message:    "upstream authentication unavailable",
		}
	}
	serviceableFailures := stateCounts.quota + stateCounts.transient
	if serviceableFailures > 0 && stateCounts.quota == serviceableFailures {
		return &proxyFinalError{
			StatusCode: http.StatusTooManyRequests,
			Code:       proxyErrorCodeRateLimited,
			Message:    "all available Codex auths are rate limited",
			RetryAt:    earliestQuota,
		}
	}
	if stateCounts.quota > 0 && stateCounts.transient > 0 && !earliestQuota.IsZero() {
		maxWait := time.Duration(s.cfg.MaxRetryInterval) * time.Second
		if maxWait > 0 && earliestQuota.Sub(now) <= maxWait {
			return &proxyFinalError{
				StatusCode: http.StatusTooManyRequests,
				Code:       proxyErrorCodeRateLimited,
				Message:    "Codex capacity is temporarily rate limited",
				RetryAt:    earliestQuota,
			}
		}
	}
	return &proxyFinalError{
		StatusCode: http.StatusBadGateway,
		Code:       proxyErrorCodeUpstream,
		Message:    "upstream Codex service unavailable",
	}
}

func healthStateFromFailure(failure upstreamFailure) AuthHealthState {
	state := AuthHealthState{
		Reason:     failure.Reason,
		RetryAt:    failure.RetryAt,
		StatusCode: failure.StatusCode,
		ErrorCode:  failure.ErrorCode,
	}
	switch failure.Kind {
	case upstreamFailureModelUnsupported:
		state.Kind = AuthHealthModelUnsupported
	case upstreamFailureQuota:
		state.Kind = AuthHealthQuota
	case upstreamFailureUnauthorized:
		state.Kind = AuthHealthUnauthorized
	case upstreamFailureCredential:
		state.Kind = AuthHealthCredentialInvalid
	case upstreamFailureTransient:
		state.Kind = AuthHealthTransient
	}
	return state
}

type proxyAttemptTransport struct {
	server        *Server
	incoming      *http.Request
	route         upstreamRoute
	authorization proxyAuthorization
	signals       []sessionAffinitySignal
	model         string
	replayable    bool
	base          *http.Request

	mu        sync.RWMutex
	result    upstreamExecution
	attempts  int
	transport http.RoundTripper
}

func (t *proxyAttemptTransport) RoundTrip(base *http.Request) (*http.Response, error) {
	t.base = base
	result, err := t.server.executeUpstream(
		base.Context(),
		t.authorization,
		t.signals,
		t.model,
		t.replayable,
		t.roundTrip,
	)
	t.mu.Lock()
	t.result = result
	t.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return result.Response, nil
}

func (t *proxyAttemptTransport) roundTrip(ctx context.Context, auth *Auth) (*http.Response, error) {
	req := t.base.Clone(ctx)
	req.Header = t.base.Header.Clone()
	if t.attempts == 0 {
		req.Body = t.base.Body
	} else if t.base.Body != nil && t.base.Body != http.NoBody {
		if t.base.GetBody == nil {
			return nil, fmt.Errorf("upstream request body is not replayable")
		}
		body, err := t.base.GetBody()
		if err != nil {
			return nil, err
		}
		req.Body = body
	}
	t.attempts++
	applyCodexProxyHeaders(req, t.incoming, auth, t.server.cfg, t.route.responsesWebsocket)
	transport := t.transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	return transport.RoundTrip(req)
}

func (t *proxyAttemptTransport) Result() upstreamExecution {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.result
}

func requestReplayCandidate(r *http.Request) bool {
	if r == nil || r.URL == nil {
		return false
	}
	path := r.URL.Path
	method := strings.ToUpper(strings.TrimSpace(r.Method))
	if websocketRequested(r) {
		if path != "/v1/responses" && path != "/backend-api/codex/responses" {
			return false
		}
		return requestBodyWithinReplayLimit(r)
	}
	if method == http.MethodGet || method == http.MethodHead {
		return requestBodyWithinReplayLimit(r)
	}
	if method != http.MethodPost {
		return false
	}

	allowed := path == "/v1/chat/completions"
	if suffix, ok := codexEndpointSuffix(path); ok {
		switch suffix {
		case "/responses", "/responses/compact", "/alpha/search", "/memories/trace_summarize":
			allowed = true
		case "/images/generations":
			contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
			allowed = contentType == "" || strings.Contains(contentType, "json")
		}
	}
	return allowed && requestBodyWithinReplayLimit(r)
}

func requestBodyWithinReplayLimit(r *http.Request) bool {
	if r == nil || r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return true
	}
	return r.ContentLength > 0 && r.ContentLength <= maxReplayBodyBytes
}

func prepareReplayBody(r *http.Request, candidate bool) bool {
	if !candidate || r == nil {
		return false
	}
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		r.Body = http.NoBody
		r.ContentLength = 0
		r.GetBody = func() (io.ReadCloser, error) {
			return http.NoBody, nil
		}
		return true
	}
	if r.ContentLength < 0 || r.ContentLength > maxReplayBodyBytes {
		return false
	}

	original := r.Body
	body, err := io.ReadAll(io.LimitReader(original, maxReplayBodyBytes+1))
	switch {
	case err != nil:
		r.Body = &replayReadCloser{
			Reader: io.MultiReader(bytes.NewReader(body), &replayErrorReader{err: err}, original),
			closers: []io.Closer{
				original,
			},
		}
		r.GetBody = nil
		return false
	case len(body) > maxReplayBodyBytes:
		r.Body = &replayReadCloser{
			Reader:  io.MultiReader(bytes.NewReader(body), original),
			closers: []io.Closer{original},
		}
		r.GetBody = nil
		return false
	default:
		_ = original.Close()
		resetRequestBody(r, body)
		return true
	}
}

func classifyUpstreamResponse(resp *http.Response, model string, now time.Time) (upstreamFailure, error) {
	if resp == nil {
		return upstreamFailure{Kind: upstreamFailureTransient, Reason: "network"}, nil
	}
	if resp.StatusCode < http.StatusBadRequest {
		return upstreamFailure{}, nil
	}
	body := inspectResponseBody(resp)
	info := parseUpstreamErrorInfo(body)
	failure := upstreamFailure{
		StatusCode: resp.StatusCode,
		ErrorCode:  safeHealthCode(info.Code),
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		failure.Kind = upstreamFailureUnauthorized
		failure.Reason = "unauthorized"
	case http.StatusTooManyRequests:
		failure.Kind = upstreamFailureQuota
		failure.Reason = "quota"
		failure.RetryAt = parseRetryAfterDeadline(resp.Header.Get(proxyRetryAfterHeader), now)
		if failure.RetryAt.IsZero() {
			failure.RetryAt = explicitQuotaRecoveryTime(info, now)
		}
		failure.Explicit = !failure.RetryAt.IsZero()
	case http.StatusRequestTimeout:
		failure.Kind = upstreamFailureTransient
		failure.Reason = "request_timeout"
	case http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		failure.Kind = upstreamFailureTransient
		failure.Reason = "upstream_error"
	default:
		if isModelUnsupportedFailure(resp.StatusCode, model, info, body) {
			failure.Kind = upstreamFailureModelUnsupported
			failure.Reason = "model_not_supported"
		} else if resp.StatusCode >= http.StatusBadRequest && resp.StatusCode < http.StatusInternalServerError {
			failure.Kind = upstreamFailureRequestScoped
			failure.Reason = "request_error"
		} else {
			failure.Kind = upstreamFailureTransient
			failure.Reason = "upstream_error"
		}
	}
	return failure, nil
}

type upstreamErrorInfo struct {
	Code            string
	Type            string
	Message         string
	ResetsAt        int64
	ResetsInSeconds int64
}

func parseUpstreamErrorInfo(body []byte) upstreamErrorInfo {
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return upstreamErrorInfo{}
	}
	errorPayload, _ := payload["error"].(map[string]any)
	if errorPayload == nil {
		errorPayload = payload
	}
	return upstreamErrorInfo{
		Code:            stringMapValue(errorPayload, "code"),
		Type:            stringMapValue(errorPayload, "type"),
		Message:         firstNonEmptyString(stringMapValue(errorPayload, "message"), stringMapValue(payload, "message")),
		ResetsAt:        int64MapValue(errorPayload, "resets_at"),
		ResetsInSeconds: int64MapValue(errorPayload, "resets_in_seconds"),
	}
}

func inspectResponseBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil || resp.Body == http.NoBody {
		return nil
	}
	original := resp.Body
	body, err := io.ReadAll(io.LimitReader(original, maxFailureInspectionBytes+1))
	if err != nil {
		resp.Body = &replayReadCloser{
			Reader:  io.MultiReader(bytes.NewReader(body), &replayErrorReader{err: err}, original),
			closers: []io.Closer{original},
		}
		return body
	}
	if len(body) > maxFailureInspectionBytes {
		resp.Body = &replayReadCloser{
			Reader:  io.MultiReader(bytes.NewReader(body), original),
			closers: []io.Closer{original},
		}
		return body[:maxFailureInspectionBytes]
	}
	_ = original.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return body
}

func isModelUnsupportedFailure(status int, model string, info upstreamErrorInfo, body []byte) bool {
	if _, ok := normalizeModelIdentifier(model); !ok {
		return false
	}
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity:
	default:
		return false
	}
	for _, value := range []string{info.Code, info.Type} {
		normalized := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
		switch normalized {
		case "model_not_supported", "unsupported_model", "model_not_found", "unknown_model", "model_unavailable":
			return true
		}
	}
	lower := strings.ToLower(strings.Join([]string{info.Message, string(body)}, " "))
	for _, pattern := range []string{
		"requested model is not supported",
		"requested model is unsupported",
		"model is not supported",
		"model not supported",
		"unsupported model",
		"not available for your plan",
		"not available for your account",
	} {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func parseRetryAfterDeadline(value string, now time.Time) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		maxSeconds := uint64(math.MaxInt64 / int64(time.Second))
		if seconds > maxSeconds {
			return now.Add(time.Duration(math.MaxInt64))
		}
		return now.Add(time.Duration(seconds) * time.Second)
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}
	}
	return retryAt
}

func explicitQuotaRecoveryTime(info upstreamErrorInfo, now time.Time) time.Time {
	if info.ResetsAt > 0 {
		retryAt := time.Unix(info.ResetsAt, 0).UTC()
		if retryAt.After(now) {
			return retryAt
		}
	}
	if info.ResetsInSeconds > 0 {
		return now.Add(time.Duration(info.ResetsInSeconds) * time.Second)
	}
	return time.Time{}
}

func stringMapValue(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

func int64MapValue(values map[string]any, key string) int64 {
	if values == nil {
		return 0
	}
	switch value := values[key].(type) {
	case json.Number:
		parsed, _ := value.Int64()
		return parsed
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	default:
		return 0
	}
}
