package codexonly

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxModelIdentifierBytes   = 128
	maxModelExclusionsPerAuth = 64
)

type AuthHealthKind string

const (
	AuthHealthQuota             AuthHealthKind = "quota"
	AuthHealthInvalidGrant      AuthHealthKind = "invalid_grant"
	AuthHealthUnauthorized      AuthHealthKind = "unauthorized"
	AuthHealthCredentialInvalid AuthHealthKind = "credential_invalid"
	AuthHealthTransient         AuthHealthKind = "transient"
	AuthHealthModelUnsupported  AuthHealthKind = "model_not_supported"
)

type AuthHealthState struct {
	Kind                  AuthHealthKind
	Reason                string
	RetryAt               time.Time
	Authoritative         bool
	CredentialFingerprint string
	StatusCode            int
	ErrorCode             string
	UpdatedAt             time.Time
}

type PersistedAuthHealthState struct {
	AuthID                string
	Kind                  AuthHealthKind
	Reason                string
	RetryAt               time.Time
	CredentialFingerprint string
	StatusCode            int
	ErrorCode             string
	UpdatedAt             time.Time
}

type authModelExclusion struct {
	Fingerprint string
	Reason      string
	StatusCode  int
	ErrorCode   string
	UpdatedAt   time.Time
}

type authModelHealthState struct {
	Model      string
	Reason     string
	StatusCode int
	ErrorCode  string
	UpdatedAt  time.Time
}

type authHealthRegistry struct {
	store *UserStore
	now   func() time.Time

	mu              sync.RWMutex
	states          map[string]AuthHealthState
	modelExclusions map[string]map[string]authModelExclusion
	healthyEpochs   map[string]uint64
}

func newAuthHealthRegistry(store *UserStore) *authHealthRegistry {
	return &authHealthRegistry{
		store:           store,
		now:             time.Now,
		states:          make(map[string]AuthHealthState),
		modelExclusions: make(map[string]map[string]authModelExclusion),
		healthyEpochs:   make(map[string]uint64),
	}
}

func (s *UserStore) SaveAuthHealthState(ctx context.Context, state PersistedAuthHealthState) error {
	if err := s.checkReady(); err != nil {
		return err
	}
	state.AuthID = strings.TrimSpace(state.AuthID)
	state.Reason = safeHealthText(state.Reason)
	state.ErrorCode = safeHealthCode(state.ErrorCode)
	state.CredentialFingerprint = strings.TrimSpace(state.CredentialFingerprint)
	if state.AuthID == "" || !persistedAuthHealthKind(state.Kind) {
		return ErrInvalidInput
	}
	if state.Kind == AuthHealthQuota && state.RetryAt.IsZero() {
		return ErrInvalidInput
	}
	if state.UpdatedAt.IsZero() {
		state.UpdatedAt = s.storeNow()
	}
	var retryAt any
	if !state.RetryAt.IsZero() {
		retryAt = formatDBTime(state.RetryAt)
	}
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO auth_health_states (
			auth_id, kind, reason, retry_at, credential_fingerprint,
			status_code, error_code, updated_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(auth_id) DO UPDATE SET
			kind = excluded.kind,
			reason = excluded.reason,
			retry_at = excluded.retry_at,
			credential_fingerprint = excluded.credential_fingerprint,
			status_code = excluded.status_code,
			error_code = excluded.error_code,
			updated_at = excluded.updated_at`,
		state.AuthID,
		state.Kind,
		state.Reason,
		retryAt,
		state.CredentialFingerprint,
		state.StatusCode,
		state.ErrorCode,
		formatDBTime(state.UpdatedAt),
	)
	if err != nil {
		return s.databaseError("save auth health state", err)
	}
	return nil
}

func (s *UserStore) LoadAuthHealthStates(ctx context.Context) ([]PersistedAuthHealthState, error) {
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT auth_id, kind, reason, retry_at, credential_fingerprint,
			status_code, error_code, updated_at
		 FROM auth_health_states
		 ORDER BY auth_id`,
	)
	if err != nil {
		return nil, s.databaseError("load auth health states", err)
	}
	defer rows.Close()

	var states []PersistedAuthHealthState
	for rows.Next() {
		var state PersistedAuthHealthState
		var retryAt sql.NullString
		var updatedAt string
		if err = rows.Scan(
			&state.AuthID,
			&state.Kind,
			&state.Reason,
			&retryAt,
			&state.CredentialFingerprint,
			&state.StatusCode,
			&state.ErrorCode,
			&updatedAt,
		); err != nil {
			return nil, s.databaseError("scan auth health state", err)
		}
		if retryAt.Valid {
			if state.RetryAt, err = parseDBTime(retryAt.String); err != nil {
				return nil, s.databaseError("parse auth health retry time", err)
			}
		}
		if state.UpdatedAt, err = parseDBTime(updatedAt); err != nil {
			return nil, s.databaseError("parse auth health update time", err)
		}
		states = append(states, state)
	}
	if err = rows.Err(); err != nil {
		return nil, s.databaseError("read auth health states", err)
	}
	return states, nil
}

func (s *UserStore) DeleteAuthHealthState(ctx context.Context, authID string) error {
	if err := s.checkReady(); err != nil {
		return err
	}
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return ErrInvalidInput
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM auth_health_states WHERE auth_id = ?`, authID); err != nil {
		return s.databaseError("delete auth health state", err)
	}
	return nil
}

func (s *UserStore) DeleteAuthHealthExceptAuthIDs(ctx context.Context, authIDs []string) error {
	if err := s.checkReady(); err != nil {
		return err
	}
	authIDs = normalizeStrings(authIDs)
	query := `DELETE FROM auth_health_states`
	args := make([]any, len(authIDs))
	if len(authIDs) > 0 {
		query += ` WHERE auth_id NOT IN (` + sqlPlaceholders(len(authIDs)) + `)`
		for index, authID := range authIDs {
			args[index] = authID
		}
	}
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return s.databaseError("delete unavailable auth health states", err)
	}
	return nil
}

func (r *authHealthRegistry) Restore(ctx context.Context, auths []*Auth) error {
	if r == nil || r.store == nil {
		return fmt.Errorf("auth health registry is not configured")
	}
	byID := authsByStableID(auths)
	if err := r.store.DeleteAuthHealthExceptAuthIDs(ctx, stableAuthIDs(auths)); err != nil {
		return err
	}
	rows, err := r.store.LoadAuthHealthStates(ctx)
	if err != nil {
		return err
	}
	now := r.currentTime()
	states := make(map[string]AuthHealthState, len(rows))
	for _, row := range rows {
		auth := byID[row.AuthID]
		remove := auth == nil ||
			(!row.RetryAt.IsZero() && !row.RetryAt.After(now)) ||
			credentialHealthKind(row.Kind) &&
				row.CredentialFingerprint != authCredentialFingerprint(auth)
		if remove {
			if err = r.store.DeleteAuthHealthState(ctx, row.AuthID); err != nil {
				return err
			}
			continue
		}
		states[row.AuthID] = AuthHealthState{
			Kind:                  row.Kind,
			Reason:                row.Reason,
			RetryAt:               row.RetryAt,
			Authoritative:         row.Kind == AuthHealthQuota,
			CredentialFingerprint: row.CredentialFingerprint,
			StatusCode:            row.StatusCode,
			ErrorCode:             row.ErrorCode,
			UpdatedAt:             row.UpdatedAt,
		}
	}
	r.mu.Lock()
	r.states = states
	r.modelExclusions = make(map[string]map[string]authModelExclusion)
	r.healthyEpochs = make(map[string]uint64)
	r.mu.Unlock()
	return nil
}

func (r *authHealthRegistry) MarkUnavailable(ctx context.Context, auth *Auth, state AuthHealthState) error {
	if r == nil || r.store == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return ErrInvalidInput
	}
	state.Reason = safeHealthText(state.Reason)
	state.ErrorCode = safeHealthCode(state.ErrorCode)
	state.UpdatedAt = r.currentTime()
	if credentialHealthKind(state.Kind) {
		state.CredentialFingerprint = authCredentialFingerprint(auth)
	}
	r.mu.Lock()
	previous, hadPrevious := r.states[auth.ID]
	if hadPrevious && preserveExistingHealthState(previous, state, r.currentTime()) {
		r.mu.Unlock()
		return nil
	}
	persist := state.Kind == AuthHealthQuota && state.Authoritative && !state.RetryAt.IsZero() ||
		state.Kind == AuthHealthInvalidGrant ||
		state.Kind == AuthHealthUnauthorized
	if persist {
		if err := r.store.SaveAuthHealthState(ctx, PersistedAuthHealthState{
			AuthID:                auth.ID,
			Kind:                  state.Kind,
			Reason:                state.Reason,
			RetryAt:               state.RetryAt,
			CredentialFingerprint: state.CredentialFingerprint,
			StatusCode:            state.StatusCode,
			ErrorCode:             state.ErrorCode,
			UpdatedAt:             state.UpdatedAt,
		}); err != nil {
			r.mu.Unlock()
			return err
		}
	} else if err := r.store.DeleteAuthHealthState(ctx, auth.ID); err != nil {
		r.mu.Unlock()
		return err
	}
	r.states[auth.ID] = state
	r.mu.Unlock()
	if !hadPrevious || !authHealthStatesEqual(previous, state) {
		log.Printf(
			"auth health transition auth_id=%s account_id=%s state=%s reason=%s retry_at=%s status=%d error_code=%s",
			auth.ID,
			safeLogAccountID(auth.AccountID),
			state.Kind,
			state.Reason,
			formatHealthLogTime(state.RetryAt),
			state.StatusCode,
			state.ErrorCode,
		)
	}
	return nil
}

func (r *authHealthRegistry) State(authID string, model string) (AuthHealthState, bool) {
	if r == nil {
		return AuthHealthState{}, false
	}
	authID = strings.TrimSpace(authID)
	model, modelValid := normalizeModelIdentifier(model)
	r.mu.RLock()
	defer r.mu.RUnlock()
	if modelValid {
		if models := r.modelExclusions[authID]; models != nil {
			if exclusion, ok := models[model]; ok {
				return AuthHealthState{
					Kind:       AuthHealthModelUnsupported,
					Reason:     exclusion.Reason,
					StatusCode: exclusion.StatusCode,
					ErrorCode:  exclusion.ErrorCode,
					UpdatedAt:  exclusion.UpdatedAt,
				}, true
			}
		}
	}
	state, ok := r.states[authID]
	return state, ok
}

func (r *authHealthRegistry) Snapshot(authID string) (AuthHealthState, bool, []authModelHealthState) {
	if r == nil {
		return AuthHealthState{}, false, nil
	}
	authID = strings.TrimSpace(authID)
	r.mu.RLock()
	defer r.mu.RUnlock()
	state, ok := r.states[authID]
	models := r.modelExclusions[authID]
	exclusions := make([]authModelHealthState, 0, len(models))
	for model, exclusion := range models {
		exclusions = append(exclusions, authModelHealthState{
			Model:      model,
			Reason:     exclusion.Reason,
			StatusCode: exclusion.StatusCode,
			ErrorCode:  exclusion.ErrorCode,
			UpdatedAt:  exclusion.UpdatedAt,
		})
	}
	sort.Slice(exclusions, func(i, j int) bool {
		return exclusions[i].Model < exclusions[j].Model
	})
	return state, ok, exclusions
}

func (r *authHealthRegistry) ClearCooldown(ctx context.Context, auth *Auth) (bool, error) {
	if r == nil || r.store == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return false, ErrInvalidInput
	}
	r.mu.Lock()
	state, ok := r.states[auth.ID]
	if !ok || state.RetryAt.IsZero() ||
		state.Kind != AuthHealthQuota && state.Kind != AuthHealthTransient {
		r.mu.Unlock()
		return false, nil
	}
	if persistedAuthHealthKind(state.Kind) {
		if err := r.store.DeleteAuthHealthState(ctx, auth.ID); err != nil {
			r.mu.Unlock()
			return false, err
		}
	}
	delete(r.states, auth.ID)
	r.healthyEpochs[auth.ID]++
	r.mu.Unlock()
	logAuthHealthy(auth, "cooldown_cleared")
	return true, nil
}

func (r *authHealthRegistry) MarkHealthy(ctx context.Context, auth *Auth, model string) error {
	if r == nil || r.store == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return ErrInvalidInput
	}
	r.mu.Lock()
	cleared, err := r.markHealthyLocked(ctx, auth, model)
	if err == nil {
		r.healthyEpochs[auth.ID]++
	}
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if cleared {
		logAuthHealthy(auth, "request_succeeded")
	}
	return nil
}

func (r *authHealthRegistry) MarkCredentialHealthy(ctx context.Context, auth *Auth) error {
	if r == nil || r.store == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return ErrInvalidInput
	}
	r.mu.Lock()
	state, ok := r.states[auth.ID]
	if !ok || !credentialHealthKind(state.Kind) {
		r.mu.Unlock()
		return nil
	}
	if persistedAuthHealthKind(state.Kind) {
		if err := r.store.DeleteAuthHealthState(ctx, auth.ID); err != nil {
			r.mu.Unlock()
			return err
		}
	}
	delete(r.states, auth.ID)
	r.healthyEpochs[auth.ID]++
	r.mu.Unlock()
	logAuthHealthy(auth, "credential_repaired")
	return nil
}

func (r *authHealthRegistry) MarkModelUnsupported(ctx context.Context, auth *Auth, model string, failure upstreamFailure) error {
	if r == nil || auth == nil || strings.TrimSpace(auth.ID) == "" {
		return ErrInvalidInput
	}
	var modelValid bool
	model, modelValid = normalizeModelIdentifier(model)
	if !modelValid {
		return ErrInvalidInput
	}
	exclusion := authModelExclusion{
		Fingerprint: authCredentialFingerprint(auth),
		Reason:      "model_not_supported",
		StatusCode:  failure.StatusCode,
		ErrorCode:   failure.ErrorCode,
		UpdatedAt:   r.currentTime(),
	}
	r.mu.Lock()
	cleared, err := r.markHealthyLocked(ctx, auth, "")
	if err != nil {
		r.mu.Unlock()
		return err
	}
	if r.modelExclusions[auth.ID] == nil {
		r.modelExclusions[auth.ID] = make(map[string]authModelExclusion)
	}
	_, existed := r.modelExclusions[auth.ID][model]
	if !existed && len(r.modelExclusions[auth.ID]) >= maxModelExclusionsPerAuth {
		delete(r.modelExclusions[auth.ID], oldestModelExclusion(r.modelExclusions[auth.ID]))
	}
	r.modelExclusions[auth.ID][model] = exclusion
	r.mu.Unlock()
	if cleared {
		logAuthHealthy(auth, "request_succeeded")
	}
	if !existed {
		log.Printf(
			"auth health transition auth_id=%s account_id=%s state=model_not_supported model=%s status=%d error_code=%s",
			auth.ID,
			safeLogAccountID(auth.AccountID),
			safeLogModel(model),
			failure.StatusCode,
			failure.ErrorCode,
		)
	}
	return nil
}

func (r *authHealthRegistry) Blocked(auth *Auth, model string) (AuthHealthState, bool) {
	if r == nil || auth == nil {
		return AuthHealthState{}, false
	}
	state, ok := r.State(auth.ID, model)
	if !ok {
		return AuthHealthState{}, false
	}
	if !state.RetryAt.IsZero() && !state.RetryAt.After(r.currentTime()) {
		return AuthHealthState{}, false
	}
	return state, true
}

func (r *authHealthRegistry) PruneExpired(ctx context.Context) error {
	if r == nil || r.store == nil {
		return ErrInvalidInput
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pruneExpiredLocked(ctx)
}

func (r *authHealthRegistry) pruneExpiredLocked(ctx context.Context) error {
	now := r.currentTime()
	for authID, state := range r.states {
		if state.RetryAt.IsZero() || state.RetryAt.After(now) {
			continue
		}
		if persistedAuthHealthKind(state.Kind) {
			if err := r.store.DeleteAuthHealthState(ctx, authID); err != nil {
				return err
			}
		}
		delete(r.states, authID)
	}
	return nil
}

func (r *authHealthRegistry) Reconcile(ctx context.Context, result AuthReconcileResult) error {
	if r == nil || r.store == nil {
		return ErrInvalidInput
	}
	byID := authsByStableID(result.Auths)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, change := range result.Changes {
		switch change.Kind {
		case AuthRemoved:
			state, hadState := r.states[change.ID]
			if hadState && persistedAuthHealthKind(state.Kind) {
				if err := r.store.DeleteAuthHealthState(ctx, change.ID); err != nil {
					return err
				}
			}
			delete(r.states, change.ID)
			delete(r.modelExclusions, change.ID)
			delete(r.healthyEpochs, change.ID)
		case AuthUpdated:
			auth := byID[change.ID]
			if auth == nil {
				continue
			}
			fingerprint := authCredentialFingerprint(auth)
			state, hadState := r.states[change.ID]
			exclusions := r.modelExclusions[change.ID]
			if hadState && credentialHealthKind(state.Kind) && state.CredentialFingerprint != fingerprint {
				if persistedAuthHealthKind(state.Kind) {
					if err := r.store.DeleteAuthHealthState(ctx, change.ID); err != nil {
						return err
					}
				}
				delete(r.states, change.ID)
			}
			if len(exclusions) > 0 {
				for model, exclusion := range r.modelExclusions[change.ID] {
					if exclusion.Fingerprint != fingerprint {
						delete(r.modelExclusions[change.ID], model)
					}
				}
				if len(r.modelExclusions[change.ID]) == 0 {
					delete(r.modelExclusions, change.ID)
				}
			}
			if change.CredentialsChanged {
				r.healthyEpochs[change.ID]++
			}
		}
	}
	return r.pruneExpiredLocked(ctx)
}

func (r *authHealthRegistry) HealthyEpoch(authID string) uint64 {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.healthyEpochs[strings.TrimSpace(authID)]
}

func (r *authHealthRegistry) NearestCooldown(auths []*Auth, model string) (time.Time, bool) {
	if r == nil {
		return time.Time{}, false
	}
	now := r.currentTime()
	var nearest time.Time
	for _, auth := range auths {
		state, blocked := r.Blocked(auth, model)
		if !blocked || state.RetryAt.IsZero() || !state.RetryAt.After(now) {
			continue
		}
		if nearest.IsZero() || state.RetryAt.Before(nearest) {
			nearest = state.RetryAt
		}
	}
	return nearest, !nearest.IsZero()
}

func (r *authHealthRegistry) currentTime() time.Time {
	if r != nil && r.now != nil {
		return r.now().UTC()
	}
	return time.Now().UTC()
}

func authCredentialFingerprint(auth *Auth) string {
	if auth == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		auth.AccessToken,
		auth.RefreshToken,
		auth.IDToken,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (r *authHealthRegistry) markHealthyLocked(ctx context.Context, auth *Auth, model string) (bool, error) {
	state, hadState := r.states[auth.ID]
	preserveQuota := hadState &&
		state.Kind == AuthHealthQuota &&
		state.Authoritative &&
		state.RetryAt.After(r.currentTime())
	if hadState && !preserveQuota && persistedAuthHealthKind(state.Kind) {
		if err := r.store.DeleteAuthHealthState(ctx, auth.ID); err != nil {
			return false, err
		}
	}
	if !preserveQuota {
		delete(r.states, auth.ID)
	}
	model, modelValid := normalizeModelIdentifier(model)
	if modelValid {
		if exclusions := r.modelExclusions[auth.ID]; exclusions != nil {
			delete(exclusions, model)
			if len(exclusions) == 0 {
				delete(r.modelExclusions, auth.ID)
			}
		}
	}
	return hadState && !preserveQuota, nil
}

func preserveExistingHealthState(existing AuthHealthState, incoming AuthHealthState, now time.Time) bool {
	if !existing.RetryAt.IsZero() && !existing.RetryAt.After(now) {
		return false
	}
	weaker := incoming.Kind == AuthHealthTransient ||
		incoming.Kind == AuthHealthCredentialInvalid ||
		incoming.Kind == AuthHealthQuota && !incoming.Authoritative
	if !weaker {
		return false
	}
	return credentialHealthKind(existing.Kind) ||
		existing.Kind == AuthHealthQuota && existing.Authoritative
}

func normalizeModelIdentifier(model string) (string, bool) {
	model = strings.Trim(model, " ")
	if model == "" || len(model) > maxModelIdentifierBytes {
		return "", false
	}
	for _, char := range model {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '-',
			char == '_',
			char == '.',
			char == '/',
			char == ':',
			char == '@':
		default:
			return "", false
		}
	}
	return model, true
}

func oldestModelExclusion(exclusions map[string]authModelExclusion) string {
	oldestModel := ""
	var oldest authModelExclusion
	for model, exclusion := range exclusions {
		if oldestModel == "" ||
			exclusion.UpdatedAt.Before(oldest.UpdatedAt) ||
			exclusion.UpdatedAt.Equal(oldest.UpdatedAt) && model < oldestModel {
			oldestModel = model
			oldest = exclusion
		}
	}
	return oldestModel
}

func safeLogModel(model string) string {
	if normalized, ok := normalizeModelIdentifier(model); ok {
		return normalized
	}
	if strings.TrimSpace(model) == "" {
		return "unknown"
	}
	return "invalid"
}

func logAuthHealthy(auth *Auth, reason string) {
	log.Printf(
		"auth health transition auth_id=%s account_id=%s state=active reason=%s",
		auth.ID,
		safeLogAccountID(auth.AccountID),
		reason,
	)
}

func persistedAuthHealthKind(kind AuthHealthKind) bool {
	return slices.Contains(
		[]AuthHealthKind{AuthHealthQuota, AuthHealthInvalidGrant, AuthHealthUnauthorized},
		kind,
	)
}

func credentialHealthKind(kind AuthHealthKind) bool {
	return kind == AuthHealthInvalidGrant ||
		kind == AuthHealthUnauthorized ||
		kind == AuthHealthCredentialInvalid
}

func safeHealthCode(code string) string {
	code = strings.TrimSpace(code)
	if len(code) > 64 {
		return ""
	}
	for _, char := range code {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '_',
			char == '-',
			char == '.':
		default:
			return ""
		}
	}
	return code
}

func safeHealthText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 {
		return value[:128]
	}
	return value
}

func authHealthStatesEqual(first AuthHealthState, second AuthHealthState) bool {
	return first.Kind == second.Kind &&
		first.Reason == second.Reason &&
		first.RetryAt.Equal(second.RetryAt) &&
		first.Authoritative == second.Authoritative &&
		first.CredentialFingerprint == second.CredentialFingerprint &&
		first.StatusCode == second.StatusCode &&
		first.ErrorCode == second.ErrorCode
}

func safeLogAccountID(accountID string) string {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return "unknown"
	}
	if len(accountID) > maxManagementAccountIDBytes || !utf8.ValidString(accountID) {
		return "invalid"
	}
	for _, char := range accountID {
		if unicode.IsControl(char) || unicode.IsSpace(char) {
			return "invalid"
		}
	}
	return accountID
}

func formatHealthLogTime(value time.Time) string {
	if value.IsZero() {
		return "none"
	}
	return value.UTC().Format(time.RFC3339)
}
