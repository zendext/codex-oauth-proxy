package codexonly

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrAuthNotFound     = errors.New("auth not found")
	ErrUnidentifiedAuth = errors.New("unidentified auth is read-only")
	ErrAuthFilesInvalid = errors.New("auth files are invalid")
)

const maxManagementAccountIDBytes = 512

type authManagementRequest struct {
	AccountID string `json:"account_id"`
}

type clearSessionBindingsRequest struct {
	UserID     string         `json:"user_id"`
	SessionKey optionalString `json:"session_key"`
	AccountID  string         `json:"account_id"`
}

type optionalString struct {
	Value   string
	Present bool
}

func (s *optionalString) UnmarshalJSON(data []byte) error {
	s.Present = true
	return json.Unmarshal(data, &s.Value)
}

type authManagementLastError struct {
	Code   string `json:"code,omitempty"`
	Status int    `json:"status,omitempty"`
}

type authManagementModelExclusion struct {
	Model     string    `json:"model"`
	Reason    string    `json:"reason"`
	Status    int       `json:"status,omitempty"`
	ErrorCode string    `json:"error_code,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

type authManagementStatus struct {
	AccountID             string                         `json:"account_id,omitempty"`
	IdentityState         string                         `json:"identity_state"`
	Manageable            bool                           `json:"manageable"`
	Email                 string                         `json:"email,omitempty"`
	SourceFileCount       int                            `json:"source_file_count"`
	Enabled               bool                           `json:"enabled"`
	RuntimeState          string                         `json:"runtime_state"`
	RuntimeReason         string                         `json:"runtime_reason,omitempty"`
	TokenExpiresAt        *time.Time                     `json:"token_expires_at,omitempty"`
	LastRefreshAt         *time.Time                     `json:"last_refresh_at,omitempty"`
	CooldownUntil         *time.Time                     `json:"cooldown_until,omitempty"`
	CooldownReason        string                         `json:"cooldown_reason,omitempty"`
	ModelCapabilityKnown  bool                           `json:"model_capability_known"`
	KnownSupportedModels  []string                       `json:"known_supported_models"`
	ModelExclusions       []authManagementModelExclusion `json:"model_exclusions"`
	SessionBindingCount   int64                          `json:"session_binding_count"`
	ActiveConnectionCount int64                          `json:"active_connection_count"`
	LastError             *authManagementLastError       `json:"last_error,omitempty"`
}

type activeAuthConnections struct {
	mu     sync.RWMutex
	counts map[string]int64
}

type activeAuthReadCloser struct {
	io.ReadCloser
	done func()
	once sync.Once
}

func (r *activeAuthReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.done)
	return err
}

func newActiveAuthConnections() *activeAuthConnections {
	return &activeAuthConnections{counts: make(map[string]int64)}
}

func (r *activeAuthConnections) Begin(authID string) func() {
	authID = strings.TrimSpace(authID)
	if r == nil || authID == "" {
		return func() {}
	}
	r.mu.Lock()
	r.counts[authID]++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			if r.counts[authID] <= 1 {
				delete(r.counts, authID)
			} else {
				r.counts[authID]--
			}
			r.mu.Unlock()
		})
	}
}

func (r *activeAuthConnections) Count(authID string) int64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.counts[strings.TrimSpace(authID)]
}

func (s *Server) beginAuthConnection(authID string) func() {
	if s == nil || s.activeAuths == nil {
		return func() {}
	}
	return s.activeAuths.Begin(authID)
}

func (s *Server) trackAuthResponseBody(resp *http.Response, authID string) {
	if resp == nil {
		return
	}
	body := resp.Body
	if body == nil {
		body = http.NoBody
	}
	resp.Body = &activeAuthReadCloser{
		ReadCloser: body,
		done:       s.beginAuthConnection(authID),
	}
}

func (s *Server) handleManagementAuthStatus(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.managementAuthStatuses(r.Context())
	if err != nil {
		writeAuthManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auths": statuses})
}

func (s *Server) handleManagementAuthAction(w http.ResponseWriter, r *http.Request, action string) {
	switch action {
	case "refresh", "enable", "disable", "cooldown/clear":
	default:
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var req authManagementRequest
	if !decodeJSONRequest(w, r, &req) {
		return
	}
	accountID, ok := normalizeManagementAccountID(req.AccountID)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid account_id")
		return
	}
	if err := s.users.CheckHealth(r.Context()); err != nil {
		writeStoreError(w, err)
		return
	}
	if action == "refresh" {
		s.handleManagementAuthRefresh(w, r, accountID)
		return
	}
	target, err := s.managementAuthByAccountID(r.Context(), accountID)
	if err != nil {
		writeAuthManagementError(w, err)
		return
	}

	switch action {
	case "enable", "disable":
		disabled := action == "disable"
		if _, err = s.auths.Store.SetAccountDisabled(r.Context(), accountID, disabled); err != nil {
			logAuthManagementOutcome(action, accountID, "failed", 0, "")
			writeAuthManagementError(w, err)
			return
		}
		if _, err = s.reconcileAuths(r.Context()); err != nil {
			writeAuthManagementError(w, err)
			return
		}
		logAuthManagementOutcome(action, accountID, "succeeded", 0, "")
	case "cooldown/clear":
		var cleared bool
		cleared, err = s.health.ClearCooldown(r.Context(), target)
		if err != nil {
			writeAuthManagementError(w, err)
			return
		}
		status, errStatus := s.managementAuthStatusByAccountID(r.Context(), accountID)
		if errStatus != nil {
			writeAuthManagementError(w, errStatus)
			return
		}
		logAuthManagementOutcome("clear_cooldown", accountID, "succeeded", 0, "")
		writeJSON(w, http.StatusOK, map[string]any{"cleared": cleared, "auth": status})
		return
	}

	status, err := s.managementAuthStatusByAccountID(r.Context(), accountID)
	if err != nil {
		writeAuthManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auth": status})
}

func (s *Server) handleManagementAuthRefresh(w http.ResponseWriter, r *http.Request, accountID string) {
	refreshed := &Auth{
		ID:        stableAuthID(accountID, ""),
		AccountID: accountID,
	}
	if err := s.auths.ForceRefresh(r.Context(), refreshed); err != nil {
		target, errTarget := s.managementAuthByAccountID(r.Context(), accountID)
		if errTarget != nil {
			writeAuthManagementError(w, errTarget)
			return
		}
		failure := preparationFailure(err, s.health.currentTime())
		if errMark := s.markAuthFailure(r.Context(), target, "", failure); errMark != nil {
			writeStoreError(w, errMark)
			return
		}
		logAuthManagementOutcome("force_refresh", accountID, "failed", failure.StatusCode, failure.ErrorCode)
		writeError(w, http.StatusBadGateway, "Codex OAuth refresh failed")
		return
	}
	if err := s.health.MarkCredentialHealthy(r.Context(), refreshed); err != nil {
		writeStoreError(w, err)
		return
	}
	if _, err := s.reconcileAuths(r.Context()); err != nil {
		writeAuthManagementError(w, err)
		return
	}
	logAuthManagementOutcome("force_refresh", accountID, "succeeded", 0, "")
	status, err := s.managementAuthStatusByAccountID(r.Context(), accountID)
	if err != nil {
		writeAuthManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"auth": status})
}

func (s *Server) handleManagementSessionBindingClear(w http.ResponseWriter, r *http.Request) {
	var req clearSessionBindingsRequest
	if !decodeJSONRequest(w, r, &req) {
		return
	}
	req.UserID = strings.TrimSpace(req.UserID)
	req.AccountID = strings.TrimSpace(req.AccountID)
	if req.SessionKey.Present && strings.TrimSpace(req.SessionKey.Value) == "" {
		writeError(w, http.StatusBadRequest, "invalid session_key")
		return
	}
	exactScope := req.UserID != "" && req.SessionKey.Present && req.AccountID == ""
	userScope := req.UserID != "" && !req.SessionKey.Present && req.AccountID == ""
	accountScope := req.UserID == "" && !req.SessionKey.Present && req.AccountID != ""
	if !exactScope && !userScope && !accountScope {
		writeError(w, http.StatusBadRequest, "exactly one binding clear scope is required")
		return
	}
	if err := s.users.CheckHealth(r.Context()); err != nil {
		writeStoreError(w, err)
		return
	}

	var (
		deleted int64
		err     error
	)
	switch {
	case exactScope:
		if _, err = s.users.GetUser(r.Context(), req.UserID); err != nil {
			writeStoreError(w, err)
			return
		}
		deleted, err = s.users.DeleteSessionAffinityByTenantSession(
			r.Context(),
			"user:"+req.UserID,
			req.SessionKey.Value,
		)
		if err == nil {
			logSessionBindingClear("exact_session", req.UserID, "", deleted)
		}
	case userScope:
		if _, err = s.users.GetUser(r.Context(), req.UserID); err != nil {
			writeStoreError(w, err)
			return
		}
		deleted, err = s.users.DeleteSessionAffinityByTenantScope(r.Context(), "user:"+req.UserID)
		if err == nil {
			logSessionBindingClear("user", req.UserID, "", deleted)
		}
	case accountScope:
		accountID, valid := normalizeManagementAccountID(req.AccountID)
		if !valid {
			writeError(w, http.StatusBadRequest, "invalid account_id")
			return
		}
		target, errTarget := s.managementAuthByAccountID(r.Context(), accountID)
		if errTarget != nil {
			writeAuthManagementError(w, errTarget)
			return
		}
		deleted, err = s.users.DeleteSessionAffinityByAccountID(r.Context(), target.ID)
		if err == nil {
			logSessionBindingClear("account", "", accountID, deleted)
		}
	}
	if err != nil {
		writeAuthManagementError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted_count": deleted})
}

func (s *Server) managementAuthStatuses(ctx context.Context) ([]authManagementStatus, error) {
	if err := s.users.CheckHealth(ctx); err != nil {
		return nil, err
	}
	result, err := s.reconcileAuths(ctx)
	if err != nil {
		return nil, err
	}
	counts, err := s.users.CountSessionAffinityBindingsByAuthIDs(ctx, stableAuthIDs(result.Auths))
	if err != nil {
		return nil, err
	}
	statuses := make([]authManagementStatus, 0, len(result.Auths))
	clientVersion := configuredClientVersion(s.cfg)
	for _, auth := range result.Auths {
		if auth == nil {
			continue
		}
		healthState, hasHealth, exclusions := s.health.Snapshot(auth.ID)
		runtimeState, runtimeReason := authManagementRuntimeState(auth, healthState, hasHealth, s.health.currentTime())
		knownModels, capabilityKnown := s.models.KnownModelsForAuth(clientVersion, result.Auths, auth.ID)
		if knownModels == nil {
			knownModels = []string{}
		}
		modelExclusions := make([]authManagementModelExclusion, 0, len(exclusions))
		for _, exclusion := range exclusions {
			modelExclusions = append(modelExclusions, authManagementModelExclusion{
				Model:     exclusion.Model,
				Reason:    exclusion.Reason,
				Status:    exclusion.StatusCode,
				ErrorCode: exclusion.ErrorCode,
				UpdatedAt: exclusion.UpdatedAt,
			})
		}
		status := authManagementStatus{
			AccountID:             auth.AccountID,
			IdentityState:         "identified",
			Manageable:            auth.Identified(),
			Email:                 maskEmail(auth.Email),
			SourceFileCount:       auth.SourceCount(),
			Enabled:               !auth.Disabled,
			RuntimeState:          runtimeState,
			RuntimeReason:         runtimeReason,
			TokenExpiresAt:        managementTimePointer(auth.ExpiresAt),
			LastRefreshAt:         managementTimePointer(auth.LastRefreshAt()),
			ModelCapabilityKnown:  capabilityKnown,
			KnownSupportedModels:  knownModels,
			ModelExclusions:       modelExclusions,
			SessionBindingCount:   counts[auth.ID],
			ActiveConnectionCount: s.activeAuths.Count(auth.ID),
		}
		if !auth.Identified() {
			status.AccountID = ""
			status.IdentityState = "unidentified"
		}
		if hasHealth {
			if healthState.RetryAt.After(s.health.currentTime()) {
				status.CooldownUntil = managementTimePointer(healthState.RetryAt)
				status.CooldownReason = healthState.Reason
			}
			status.LastError = &authManagementLastError{
				Code:   healthState.ErrorCode,
				Status: healthState.StatusCode,
			}
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

func (s *Server) managementAuthStatusByAccountID(ctx context.Context, accountID string) (authManagementStatus, error) {
	statuses, err := s.managementAuthStatuses(ctx)
	if err != nil {
		return authManagementStatus{}, err
	}
	for _, status := range statuses {
		if status.AccountID == accountID {
			return status, nil
		}
	}
	return authManagementStatus{}, ErrAuthNotFound
}

func (s *Server) managementAuthByAccountID(ctx context.Context, accountID string) (*Auth, error) {
	result, err := s.reconcileAuths(ctx)
	if err != nil {
		return nil, err
	}
	if auth := authByManageableAccountID(result.Auths, accountID); auth != nil {
		return auth, nil
	}
	for _, auth := range result.Auths {
		if auth != nil && !auth.Identified() && auth.ID == accountID {
			return nil, ErrUnidentifiedAuth
		}
	}
	return nil, ErrAuthNotFound
}

func authManagementRuntimeState(auth *Auth, state AuthHealthState, hasHealth bool, now time.Time) (string, string) {
	if auth == nil {
		return "unavailable", "missing"
	}
	if auth.Disabled {
		return "unavailable", "disabled"
	}
	if !hasHealth {
		return "active", ""
	}
	switch state.Kind {
	case AuthHealthQuota, AuthHealthTransient:
		if state.RetryAt.After(now) {
			return "cooling", state.Reason
		}
		return "active", ""
	case AuthHealthInvalidGrant, AuthHealthUnauthorized, AuthHealthCredentialInvalid:
		return "unavailable", state.Reason
	default:
		return "active", ""
	}
}

func managementTimePointer(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

func normalizeManagementAccountID(accountID string) (string, bool) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || len(accountID) > maxManagementAccountIDBytes || !utf8.ValidString(accountID) {
		return "", false
	}
	for _, char := range accountID {
		if unicode.IsControl(char) {
			return "", false
		}
	}
	return accountID, true
}

func maskEmail(email string) string {
	email = strings.TrimSpace(email)
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" {
		return ""
	}
	runes := []rune(local)
	maskedLocal := "*"
	if len(runes) > 0 {
		maskedLocal = string(runes[0]) + "***"
	}
	return maskedLocal + "@" + domain
}

func writeAuthManagementError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrStorageFailure):
		writeStoreError(w, err)
	case errors.Is(err, ErrInvalidInput):
		writeError(w, http.StatusBadRequest, "invalid request")
	case errors.Is(err, ErrAuthNotFound):
		writeError(w, http.StatusNotFound, "auth not found")
	case errors.Is(err, ErrUnidentifiedAuth):
		writeError(w, http.StatusConflict, "unidentified auth is read-only")
	case errors.Is(err, ErrAuthFilesInvalid):
		writeError(w, http.StatusConflict, "auth files failed local validation")
	default:
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

func logAuthManagementOutcome(action string, accountID string, outcome string, status int, errorCode string) {
	log.Printf(
		"auth management action=%s account_id=%s outcome=%s status=%d error_code=%s",
		action,
		safeLogAccountID(accountID),
		outcome,
		status,
		firstNonEmptyString(errorCode, "none"),
	)
}

func logSessionBindingClear(scope string, userID string, accountID string, deleted int64) {
	fields := []string{"session binding management action=clear", "scope=" + scope}
	if userID != "" {
		fields = append(fields, "user_id="+safeLogIdentifier(userID))
	}
	if accountID != "" {
		fields = append(fields, "account_id="+safeLogAccountID(accountID))
	}
	fields = append(fields, "deleted_count="+formatInt64(deleted))
	log.Print(strings.Join(fields, " "))
}

func safeLogIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 || !utf8.ValidString(value) {
		return "invalid"
	}
	for _, char := range value {
		if unicode.IsControl(char) || unicode.IsSpace(char) {
			return "invalid"
		}
	}
	return value
}

func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
