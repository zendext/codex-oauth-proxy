package codexonly

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxClientVersionBytes         = 64
	maxModelCatalogVersions       = 16
	modelCatalogTTL               = 3 * time.Hour
	modelCatalogBackgroundRetries = 3
	modelCatalogRetryBaseDelay    = time.Second
	modelCatalogFetchTimeout      = 5 * time.Second
	maxModelCatalogBodyBytes      = 4 << 20
)

var errInvalidClientVersion = errors.New("invalid Codex client version")

type modelCatalogFetchResult struct {
	Models  []map[string]any
	RetryAt time.Time
}

type modelCatalogFetchError struct {
	RetryAt time.Time
	Err     error
}

func (e *modelCatalogFetchError) Error() string {
	if e == nil || e.Err == nil {
		return "model catalog fetch failed"
	}
	return e.Err.Error()
}

func (e *modelCatalogFetchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type modelCatalogView struct {
	Models           []map[string]any
	SupportingAuths  map[string][]string
	CapabilityKnown  bool
	EmbeddedFallback bool
}

func (v modelCatalogView) SupportingAuthIDs(model string) []string {
	model, ok := normalizeModelIdentifier(model)
	if !ok || v.SupportingAuths == nil {
		return nil
	}
	return slices.Clone(v.SupportingAuths[model])
}

type modelCatalogFetchFunc func(context.Context, *Auth, string) (modelCatalogFetchResult, error)

type runtimeModelCatalog struct {
	ctx               context.Context
	cancel            context.CancelFunc
	fetch             modelCatalogFetchFunc
	healthyGeneration func(string) uint64
	now               func() time.Time
	waitRetry         func(context.Context, time.Duration) error
	retryJitter       func(time.Duration) time.Duration

	mu       sync.Mutex
	versions map[string]*modelCatalogVersion
	lru      *list.List
}

type modelCatalogVersion struct {
	version string
	ctx     context.Context
	cancel  context.CancelFunc
	element *list.Element
	auths   map[string]*modelCatalogAuth
	evicted bool
}

type modelCatalogAuth struct {
	snapshot            []map[string]any
	fetchedAt           time.Time
	foreground          *modelCatalogCall
	background          bool
	cycleStartedAt      time.Time
	exhausted           bool
	exhaustedGeneration uint64
}

type modelCatalogCall struct {
	done chan struct{}
}

type modelCatalogFetchStart struct {
	entry *modelCatalogVersion
	auth  *Auth
	call  *modelCatalogCall
}

func newRuntimeModelCatalog(
	ctx context.Context,
	fetch modelCatalogFetchFunc,
	healthyGeneration func(string) uint64,
) *runtimeModelCatalog {
	if ctx == nil {
		ctx = context.Background()
	}
	catalogCtx, cancel := context.WithCancel(ctx)
	return &runtimeModelCatalog{
		ctx:               catalogCtx,
		cancel:            cancel,
		fetch:             fetch,
		healthyGeneration: healthyGeneration,
		now:               time.Now,
		waitRetry:         waitForProxyRetry,
		retryJitter: func(max time.Duration) time.Duration {
			if max <= 0 {
				return 0
			}
			return time.Duration(rand.Int64N(int64(max) + 1))
		},
		versions: make(map[string]*modelCatalogVersion),
		lru:      list.New(),
	}
}

func (c *runtimeModelCatalog) Close() {
	if c != nil && c.cancel != nil {
		c.cancel()
	}
}

func (c *runtimeModelCatalog) Catalog(
	ctx context.Context,
	clientVersion string,
	auths []*Auth,
) (modelCatalogView, error) {
	if c == nil || c.fetch == nil {
		return embeddedModelCatalogView(), nil
	}
	clientVersion, ok := normalizeClientVersion(clientVersion)
	if !ok {
		return modelCatalogView{}, errInvalidClientVersion
	}
	if ctx == nil {
		ctx = context.Background()
	}
	auths = distinctSortedAuths(auths)
	now := c.currentTime()

	c.mu.Lock()
	entry := c.versionLocked(clientVersion)
	var calls []*modelCatalogCall
	var starts []modelCatalogFetchStart
	for _, auth := range auths {
		state := entry.auths[auth.ID]
		if state == nil {
			state = &modelCatalogAuth{}
			entry.auths[auth.ID] = state
		}
		if state.fetchedAt.Add(modelCatalogTTL).After(now) {
			continue
		}
		if state.foreground != nil {
			calls = append(calls, state.foreground)
			continue
		}
		if state.background {
			continue
		}
		if state.exhausted {
			healthyGeneration := c.authHealthyGeneration(auth.ID)
			nextPeriod := state.cycleStartedAt.Add(modelCatalogTTL)
			if healthyGeneration <= state.exhaustedGeneration && nextPeriod.After(now) {
				continue
			}
			state.exhausted = false
			state.cycleStartedAt = now
		}
		if state.cycleStartedAt.IsZero() {
			state.cycleStartedAt = now
		}
		call := &modelCatalogCall{done: make(chan struct{})}
		state.foreground = call
		calls = append(calls, call)
		starts = append(starts, modelCatalogFetchStart{
			entry: entry,
			auth:  cloneAuth(auth),
			call:  call,
		})
	}
	c.mu.Unlock()

	for _, start := range starts {
		go c.runForegroundFetch(start)
	}
	for _, call := range calls {
		select {
		case <-ctx.Done():
			return modelCatalogView{}, ctx.Err()
		case <-call.done:
		}
	}
	return c.aggregate(entry, auths), nil
}

func (c *runtimeModelCatalog) CachedCatalog(
	clientVersion string,
	auths []*Auth,
) modelCatalogView {
	if c == nil {
		return embeddedModelCatalogView()
	}
	clientVersion, ok := normalizeClientVersion(clientVersion)
	if !ok {
		return embeddedModelCatalogView()
	}
	auths = distinctSortedAuths(auths)
	c.mu.Lock()
	entry := c.versions[clientVersion]
	c.mu.Unlock()
	if entry == nil {
		return embeddedModelCatalogView()
	}
	return c.aggregate(entry, auths)
}

func (c *runtimeModelCatalog) Reconcile(result AuthReconcileResult) {
	if c == nil || len(result.Changes) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, change := range result.Changes {
		if change.Kind != AuthRemoved {
			continue
		}
		for _, entry := range c.versions {
			delete(entry.auths, change.ID)
		}
	}
}

func (c *runtimeModelCatalog) runForegroundFetch(start modelCatalogFetchStart) {
	result, err := c.fetch(start.entry.ctx, start.auth, start.entry.version)

	c.mu.Lock()
	if !c.entryCurrentLocked(start.entry) {
		c.mu.Unlock()
		close(start.call.done)
		return
	}
	state := start.entry.auths[start.auth.ID]
	if state == nil || state.foreground != start.call {
		c.mu.Unlock()
		close(start.call.done)
		return
	}
	state.foreground = nil
	if err == nil {
		c.applyFetchSuccessLocked(state, result)
		c.mu.Unlock()
		close(start.call.done)
		return
	}
	if !state.background {
		state.background = true
		go c.runBackgroundFetch(start.entry, start.auth, result, err)
	}
	c.mu.Unlock()
	close(start.call.done)
}

func (c *runtimeModelCatalog) runBackgroundFetch(
	entry *modelCatalogVersion,
	auth *Auth,
	previous modelCatalogFetchResult,
	previousErr error,
) {
	result := previous
	err := previousErr
	for attempt := 1; attempt <= modelCatalogBackgroundRetries; attempt++ {
		delay := c.backgroundRetryDelay(attempt, result, err)
		if errWait := c.waitForRetry(entry.ctx, delay); errWait != nil {
			c.finishBackground(entry, auth.ID, false)
			return
		}
		result, err = c.fetch(entry.ctx, cloneAuth(auth), entry.version)
		if err == nil {
			c.mu.Lock()
			if c.entryCurrentLocked(entry) {
				if state := entry.auths[auth.ID]; state != nil {
					c.applyFetchSuccessLocked(state, result)
				}
			}
			c.mu.Unlock()
			return
		}
	}
	c.finishBackground(entry, auth.ID, true)
}

func (c *runtimeModelCatalog) finishBackground(entry *modelCatalogVersion, authID string, exhausted bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.entryCurrentLocked(entry) {
		return
	}
	state := entry.auths[authID]
	if state == nil {
		return
	}
	state.background = false
	state.exhausted = exhausted
	if exhausted {
		state.exhaustedGeneration = c.authHealthyGeneration(authID)
	}
}

func (c *runtimeModelCatalog) applyFetchSuccessLocked(
	state *modelCatalogAuth,
	result modelCatalogFetchResult,
) {
	state.snapshot = cloneCodexClientModels(result.Models)
	state.fetchedAt = c.currentTime()
	state.background = false
	state.exhausted = false
	state.exhaustedGeneration = 0
	state.cycleStartedAt = time.Time{}
}

func (c *runtimeModelCatalog) aggregate(
	entry *modelCatalogVersion,
	auths []*Auth,
) modelCatalogView {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.entryCurrentLocked(entry) {
		return embeddedModelCatalogView()
	}
	c.lru.MoveToFront(entry.element)

	type canonicalModel struct {
		authID string
		model  map[string]any
		json   []byte
	}
	canonical := make(map[string]canonicalModel)
	supporting := make(map[string]map[string]struct{})
	usableSnapshots := 0
	for _, auth := range auths {
		state := entry.auths[auth.ID]
		if state == nil || state.snapshot == nil {
			continue
		}
		usableSnapshots++
		for _, model := range state.snapshot {
			slug, _ := model["slug"].(string)
			slug, ok := normalizeModelIdentifier(slug)
			if !ok {
				continue
			}
			if supporting[slug] == nil {
				supporting[slug] = make(map[string]struct{})
			}
			supporting[slug][auth.ID] = struct{}{}
			encoded, _ := json.Marshal(model)
			current, exists := canonical[slug]
			if !exists ||
				auth.ID < current.authID ||
				auth.ID == current.authID && bytes.Compare(encoded, current.json) < 0 {
				canonical[slug] = canonicalModel{
					authID: auth.ID,
					model:  model,
					json:   encoded,
				}
			}
		}
	}
	if usableSnapshots == 0 {
		return embeddedModelCatalogView()
	}

	models := make([]map[string]any, 0, len(canonical))
	for _, candidate := range canonical {
		models = append(models, cloneCodexClientModelMap(candidate.model))
	}
	sort.Slice(models, func(i, j int) bool {
		firstPriority := codexModelPriority(models[i])
		secondPriority := codexModelPriority(models[j])
		if firstPriority != secondPriority {
			return firstPriority < secondPriority
		}
		firstSlug, _ := models[i]["slug"].(string)
		secondSlug, _ := models[j]["slug"].(string)
		return firstSlug < secondSlug
	})

	supportSets := make(map[string][]string, len(supporting))
	for slug, authSet := range supporting {
		authIDs := make([]string, 0, len(authSet))
		for authID := range authSet {
			authIDs = append(authIDs, authID)
		}
		sort.Strings(authIDs)
		supportSets[slug] = authIDs
	}
	return modelCatalogView{
		Models:          models,
		SupportingAuths: supportSets,
		CapabilityKnown: true,
	}
}

func (c *runtimeModelCatalog) versionLocked(clientVersion string) *modelCatalogVersion {
	if entry := c.versions[clientVersion]; entry != nil {
		c.lru.MoveToFront(entry.element)
		return entry
	}
	entryCtx, cancel := context.WithCancel(c.ctx)
	entry := &modelCatalogVersion{
		version: clientVersion,
		ctx:     entryCtx,
		cancel:  cancel,
		auths:   make(map[string]*modelCatalogAuth),
	}
	entry.element = c.lru.PushFront(entry)
	c.versions[clientVersion] = entry
	for len(c.versions) > maxModelCatalogVersions {
		oldestElement := c.lru.Back()
		if oldestElement == nil {
			break
		}
		oldest := oldestElement.Value.(*modelCatalogVersion)
		delete(c.versions, oldest.version)
		c.lru.Remove(oldestElement)
		oldest.evicted = true
		oldest.cancel()
	}
	return entry
}

func (c *runtimeModelCatalog) entryCurrentLocked(entry *modelCatalogVersion) bool {
	return entry != nil && !entry.evicted && c.versions[entry.version] == entry
}

func (c *runtimeModelCatalog) backgroundRetryDelay(
	attempt int,
	result modelCatalogFetchResult,
	err error,
) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exponent := attempt - 1
	if exponent > 20 {
		exponent = 20
	}
	backoff := modelCatalogRetryBaseDelay * time.Duration(1<<exponent)
	jitterMax := backoff / 4
	if c.retryJitter != nil {
		backoff += c.retryJitter(jitterMax)
	}
	retryAt := result.RetryAt
	var fetchErr *modelCatalogFetchError
	if errors.As(err, &fetchErr) && fetchErr != nil && fetchErr.RetryAt.After(retryAt) {
		retryAt = fetchErr.RetryAt
	}
	if retryAt.After(c.currentTime()) {
		retryDelay := retryAt.Sub(c.currentTime())
		if retryDelay > backoff {
			return retryDelay
		}
	}
	return backoff
}

func (c *runtimeModelCatalog) waitForRetry(ctx context.Context, delay time.Duration) error {
	if c.waitRetry == nil {
		return waitForProxyRetry(ctx, delay)
	}
	return c.waitRetry(ctx, delay)
}

func (c *runtimeModelCatalog) currentTime() time.Time {
	if c != nil && c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

func (c *runtimeModelCatalog) authHealthyGeneration(authID string) uint64 {
	if c == nil || c.healthyGeneration == nil {
		return 0
	}
	return c.healthyGeneration(authID)
}

func normalizeClientVersion(version string) (string, bool) {
	version = strings.TrimSpace(version)
	if version == "" || len(version) > maxClientVersionBytes {
		return "", false
	}
	for _, char := range version {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '.',
			char == '-',
			char == '_',
			char == '+':
		default:
			return "", false
		}
	}
	return version, true
}

func distinctSortedAuths(auths []*Auth) []*Auth {
	byID := make(map[string]*Auth, len(auths))
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		byID[auth.ID] = auth
	}
	ids := make([]string, 0, len(byID))
	for authID := range byID {
		ids = append(ids, authID)
	}
	sort.Strings(ids)
	distinct := make([]*Auth, 0, len(ids))
	for _, authID := range ids {
		distinct = append(distinct, byID[authID])
	}
	return distinct
}

func codexModelPriority(model map[string]any) int64 {
	value := model["priority"]
	switch priority := value.(type) {
	case json.Number:
		if parsed, err := priority.Int64(); err == nil {
			return parsed
		}
	case float64:
		if priority >= math.MinInt64 && priority <= math.MaxInt64 {
			return int64(priority)
		}
	case int:
		return int64(priority)
	case int64:
		return priority
	}
	return math.MaxInt64
}

func cloneCodexClientModels(models []map[string]any) []map[string]any {
	if models == nil {
		return nil
	}
	cloned := make([]map[string]any, len(models))
	for index, model := range models {
		cloned[index] = cloneCodexClientModelMap(model)
	}
	return cloned
}

func filterCodexClientModels(models []map[string]any, allowFastMode bool) []map[string]any {
	filtered := cloneCodexClientModels(models)
	if allowFastMode {
		return filtered
	}
	for _, model := range filtered {
		delete(model, "service_tiers")
		delete(model, "additional_speed_tiers")
	}
	return filtered
}

func embeddedModelCatalogView() modelCatalogView {
	return modelCatalogView{
		Models:           codexClientModels(true),
		EmbeddedFallback: true,
	}
}

func configuredClientVersion(cfg *Config) string {
	userAgent := DefaultCodexUA
	if cfg != nil && strings.TrimSpace(cfg.CodexUserAgent) != "" {
		userAgent = cfg.CodexUserAgent
	}
	if version, ok := clientVersionFromUserAgent(userAgent, false); ok {
		return version
	}
	version, _ := clientVersionFromUserAgent(DefaultCodexUA, false)
	return version
}

func requestClientVersion(r *http.Request, cfg *Config) string {
	if cfg != nil && strings.TrimSpace(cfg.CodexUserAgent) != "" {
		return configuredClientVersion(cfg)
	}
	if r != nil {
		if version, ok := clientVersionFromUserAgent(r.UserAgent(), true); ok {
			return version
		}
	}
	return configuredClientVersion(cfg)
}

func clientVersionFromUserAgent(userAgent string, requireCodexProduct bool) (string, bool) {
	for _, field := range strings.Fields(strings.TrimSpace(userAgent)) {
		field = strings.Trim(field, "()")
		product, version, found := strings.Cut(field, "/")
		if !found || product == "" || version == "" {
			continue
		}
		if requireCodexProduct && !strings.Contains(strings.ToLower(product), "codex") {
			continue
		}
		version = strings.TrimRight(version, ");,")
		if normalized, ok := normalizeClientVersion(version); ok {
			return normalized, true
		}
	}
	return "", false
}

func modelsClientVersion(r *http.Request, cfg *Config) (string, error) {
	if r == nil {
		return configuredClientVersion(cfg), nil
	}
	values, present := r.URL.Query()["client_version"]
	if !present || len(values) == 0 || strings.TrimSpace(values[0]) == "" {
		return configuredClientVersion(cfg), nil
	}
	version, ok := normalizeClientVersion(values[0])
	if !ok {
		return "", errInvalidClientVersion
	}
	return version, nil
}

func (s *Server) fetchModelCatalog(
	ctx context.Context,
	auth *Auth,
	clientVersion string,
) (modelCatalogFetchResult, error) {
	if s == nil || s.auths == nil || s.health == nil || auth == nil {
		return modelCatalogFetchResult{}, errors.New("model catalog fetch is not configured")
	}
	prepared, err := s.auths.prepareAuth(ctx, cloneAuth(auth))
	if err != nil {
		failure := preparationFailure(err, s.health.currentTime())
		if errMark := s.markAuthFailure(ctx, auth, "", failure); errMark != nil {
			return modelCatalogFetchResult{}, errMark
		}
		return modelCatalogFetchResult{RetryAt: failure.RetryAt}, &modelCatalogFetchError{
			RetryAt: failure.RetryAt,
			Err:     errors.New("prepare model catalog auth"),
		}
	}

	result, failure, err := s.fetchModelCatalogAttempt(ctx, prepared, clientVersion)
	if err == nil {
		if err = s.health.MarkHealthy(ctx, prepared, ""); err != nil {
			return modelCatalogFetchResult{}, err
		}
		return result, nil
	}
	if failure.Kind != upstreamFailureUnauthorized {
		if failure.Kind == upstreamFailureQuota {
			if errMark := s.markAuthFailure(ctx, prepared, "", failure); errMark != nil {
				return modelCatalogFetchResult{}, errMark
			}
		}
		return result, err
	}

	failedAccessToken := prepared.AccessToken
	refreshed := cloneAuth(prepared)
	if errRefresh := s.auths.RefreshAfterUnauthorized(ctx, refreshed, failedAccessToken); errRefresh != nil {
		refreshFailure := preparationFailure(errRefresh, s.health.currentTime())
		if refreshFailure.Kind == upstreamFailureCredential && refreshFailure.ErrorCode == "" {
			refreshFailure.ErrorCode = "unauthorized"
		}
		if errMark := s.markAuthFailure(ctx, prepared, "", refreshFailure); errMark != nil {
			return modelCatalogFetchResult{}, errMark
		}
		return modelCatalogFetchResult{RetryAt: refreshFailure.RetryAt}, &modelCatalogFetchError{
			RetryAt: refreshFailure.RetryAt,
			Err:     errors.New("refresh model catalog auth"),
		}
	}
	if err = s.health.MarkCredentialHealthy(ctx, refreshed); err != nil {
		return modelCatalogFetchResult{}, err
	}

	result, failure, err = s.fetchModelCatalogAttempt(ctx, refreshed, clientVersion)
	if err == nil {
		if err = s.health.MarkHealthy(ctx, refreshed, ""); err != nil {
			return modelCatalogFetchResult{}, err
		}
		return result, nil
	}
	switch failure.Kind {
	case upstreamFailureUnauthorized:
		failure = upstreamFailure{
			Kind:       upstreamFailureUnauthorized,
			StatusCode: http.StatusUnauthorized,
			ErrorCode:  firstNonEmptyString(failure.ErrorCode, "unauthorized"),
			Reason:     "unauthorized",
		}
		if errMark := s.markAuthFailure(ctx, refreshed, "", failure); errMark != nil {
			return modelCatalogFetchResult{}, errMark
		}
	case upstreamFailureQuota:
		if errMark := s.markAuthFailure(ctx, refreshed, "", failure); errMark != nil {
			return modelCatalogFetchResult{}, errMark
		}
	}
	return result, err
}

func (s *Server) fetchModelCatalogAttempt(
	ctx context.Context,
	auth *Auth,
	clientVersion string,
) (modelCatalogFetchResult, upstreamFailure, error) {
	if s == nil || s.baseURL == nil || s.httpClient == nil {
		return modelCatalogFetchResult{}, upstreamFailure{}, errors.New("model catalog upstream is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, modelCatalogFetchTimeout)
	defer cancel()

	endpoint := *s.baseURL
	endpoint.Path = targetPath(s.baseURL, "/backend-api/codex", "/models")
	endpoint.RawPath = ""
	query := endpoint.Query()
	query.Set("client_version", clientVersion)
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return modelCatalogFetchResult{}, upstreamFailure{}, fmt.Errorf("create model catalog request: %w", err)
	}
	incoming := &http.Request{Method: http.MethodGet, Header: make(http.Header)}
	incoming.Header.Set("User-Agent", "codex_cli_rs/"+clientVersion)
	applyCodexProxyHeaders(req, incoming, auth, s.cfg, false)
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return modelCatalogFetchResult{}, upstreamFailure{
			Kind:   upstreamFailureTransient,
			Reason: "network",
		}, &modelCatalogFetchError{Err: fmt.Errorf("request model catalog: %w", err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		now := s.health.currentTime()
		failure, errClassify := classifyUpstreamResponse(resp, "", now)
		if errClassify != nil {
			return modelCatalogFetchResult{}, upstreamFailure{}, errClassify
		}
		retryAt := failure.RetryAt
		if retryAt.IsZero() && failure.Kind == upstreamFailureTransient {
			retryAt = parseRetryAfterDeadline(resp.Header.Get(proxyRetryAfterHeader), now)
		}
		return modelCatalogFetchResult{RetryAt: retryAt}, failure, &modelCatalogFetchError{
			RetryAt: retryAt,
			Err:     fmt.Errorf("model catalog upstream status %d", resp.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelCatalogBodyBytes+1))
	if err != nil {
		return modelCatalogFetchResult{}, upstreamFailure{}, &modelCatalogFetchError{
			Err: fmt.Errorf("read model catalog: %w", err),
		}
	}
	if len(body) > maxModelCatalogBodyBytes {
		return modelCatalogFetchResult{}, upstreamFailure{}, &modelCatalogFetchError{
			Err: errors.New("model catalog response is too large"),
		}
	}
	var payload codexClientModelsPayload
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err = decoder.Decode(&payload); err != nil || payload.Models == nil {
		return modelCatalogFetchResult{}, upstreamFailure{}, &modelCatalogFetchError{
			Err: errors.New("decode model catalog response"),
		}
	}
	if err = ensureJSONEOF(decoder); err != nil {
		return modelCatalogFetchResult{}, upstreamFailure{}, &modelCatalogFetchError{
			Err: errors.New("decode model catalog response"),
		}
	}
	return modelCatalogFetchResult{Models: payload.Models}, upstreamFailure{}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}
