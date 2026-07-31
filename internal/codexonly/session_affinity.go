package codexonly

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxSessionAffinitySignalBytes   = 512
	maxSessionAffinityBodyBytes     = 64 << 10
	sessionAffinityExpiry           = time.Hour
	sessionAffinityRenewalThreshold = 30 * time.Minute
	sessionAffinityCleanupInterval  = 5 * time.Minute
	sessionAffinityCleanupBatchSize = 100
)

const (
	sessionAffinitySignalSessionID      = "session_id"
	sessionAffinitySignalPromptCacheKey = "prompt_cache_key"
	sessionAffinitySignalConversationID = "conversation_id"
)

type sessionAffinitySignal struct {
	Kind  string
	Value string
}

type SessionAffinityDigest string

type SessionAffinityBinding struct {
	Digests       []SessionAffinityDigest
	BindingDigest SessionAffinityDigest
	AuthID        string
}

type sessionAffinityRow struct {
	Digest        SessionAffinityDigest
	BindingDigest SessionAffinityDigest
	AuthID        string
	Created       time.Time
	Updated       time.Time
	Expires       time.Time
}

type replayReadCloser struct {
	io.Reader
	closer io.Closer
}

type replayErrorReader struct {
	err error
}

func (r *replayErrorReader) Read([]byte) (int, error) {
	if r == nil || r.err == nil {
		return 0, io.EOF
	}
	err := r.err
	r.err = nil
	return 0, err
}

func (r *replayReadCloser) Close() error {
	if r == nil || r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

func extractSessionAffinitySignals(r *http.Request) []sessionAffinitySignal {
	if r == nil {
		return nil
	}
	candidates := make(map[string]string)
	conflicts := make(map[string]bool)
	add := func(kind string, raw string) {
		value, ok := normalizeSessionAffinitySignal(raw)
		if !ok || conflicts[kind] {
			return
		}
		if current, exists := candidates[kind]; exists && current != value {
			delete(candidates, kind)
			conflicts[kind] = true
			return
		}
		candidates[kind] = value
	}

	for _, name := range []string{"Session-Id", "Session_id"} {
		values := r.Header.Values(name)
		if len(values) > 4 {
			values = values[:4]
		}
		for _, value := range values {
			add(sessionAffinitySignalSessionID, value)
		}
	}

	if requestMayContainSessionAffinityJSON(r) {
		body := readBoundedSessionAffinityBody(r)
		if len(body) > 0 {
			var payload map[string]any
			decoder := json.NewDecoder(bytes.NewReader(body))
			decoder.UseNumber()
			if err := decoder.Decode(&payload); err == nil {
				addStringSessionAffinitySignal(payload, "session_id", sessionAffinitySignalSessionID, add)
				addStringSessionAffinitySignal(payload, "sessionId", sessionAffinitySignalSessionID, add)
				addStringSessionAffinitySignal(payload, "prompt_cache_key", sessionAffinitySignalPromptCacheKey, add)
				addStringSessionAffinitySignal(payload, "conversation_id", sessionAffinitySignalConversationID, add)
				if conversation, ok := payload["conversation"].(map[string]any); ok {
					addStringSessionAffinitySignal(conversation, "id", sessionAffinitySignalConversationID, add)
				}
			}
		}
	}

	kinds := []string{
		sessionAffinitySignalSessionID,
		sessionAffinitySignalPromptCacheKey,
		sessionAffinitySignalConversationID,
	}
	signals := make([]sessionAffinitySignal, 0, len(kinds))
	for _, kind := range kinds {
		if value := candidates[kind]; value != "" && !conflicts[kind] {
			signals = append(signals, sessionAffinitySignal{Kind: kind, Value: value})
		}
	}
	return signals
}

func requestMayContainSessionAffinityJSON(r *http.Request) bool {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return false
	}
	contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if strings.Contains(contentType, "json") {
		return true
	}
	return contentType == "" && r.Method != http.MethodGet && r.Method != http.MethodHead
}

func readBoundedSessionAffinityBody(r *http.Request) []byte {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	original := r.Body
	prefix, err := io.ReadAll(io.LimitReader(original, maxSessionAffinityBodyBytes+1))
	readers := []io.Reader{bytes.NewReader(prefix)}
	if err != nil {
		readers = append(readers, &replayErrorReader{err: err})
	}
	readers = append(readers, original)
	r.Body = &replayReadCloser{
		Reader: io.MultiReader(readers...),
		closer: original,
	}
	if err != nil || len(prefix) > maxSessionAffinityBodyBytes {
		return nil
	}
	return prefix
}

func addStringSessionAffinitySignal(payload map[string]any, key string, kind string, add func(string, string)) {
	value, ok := payload[key].(string)
	if !ok {
		return
	}
	add(kind, value)
}

func normalizeSessionAffinitySignal(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > maxSessionAffinitySignalBytes || !utf8.ValidString(value) {
		return "", false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return value, true
}

func digestSessionAffinitySignals(tenantScope string, signals []sessionAffinitySignal) []SessionAffinityDigest {
	digests := make([]SessionAffinityDigest, 0, len(signals))
	for _, signal := range signals {
		digest := digestSessionAffinitySignal(tenantScope, signal)
		if digest == "" || slices.Contains(digests, digest) {
			continue
		}
		digests = append(digests, digest)
	}
	return digests
}

func digestSessionAffinitySignal(tenantScope string, signal sessionAffinitySignal) SessionAffinityDigest {
	tenantScope = strings.TrimSpace(tenantScope)
	value, ok := normalizeSessionAffinitySignal(signal.Value)
	if tenantScope == "" || !ok {
		return ""
	}
	switch signal.Kind {
	case sessionAffinitySignalSessionID, sessionAffinitySignalPromptCacheKey, sessionAffinitySignalConversationID:
	default:
		return ""
	}
	sum := sha256.Sum256([]byte(tenantScope + "\x00" + signal.Kind + "\x00" + value))
	return SessionAffinityDigest(hex.EncodeToString(sum[:]))
}

func (s *UserStore) LookupSessionAffinity(ctx context.Context, digests []SessionAffinityDigest) (SessionAffinityBinding, bool, error) {
	if err := s.checkReady(); err != nil {
		return SessionAffinityBinding{}, false, err
	}
	digests = normalizeSessionAffinityDigests(digests)
	if len(digests) == 0 {
		return SessionAffinityBinding{}, false, nil
	}
	now := s.storeNow()
	rows, err := loadSessionAffinityRows(ctx, s.db, digests, nil)
	if err != nil {
		return SessionAffinityBinding{}, false, s.databaseError("lookup session affinity", err)
	}
	winner, ok := chooseSessionAffinityWinner(rows, now)
	if !ok {
		return SessionAffinityBinding{}, false, nil
	}
	if sessionAffinityNeedsWrite(rows, digests, winner, now) {
		binding, found, errRefresh := s.refreshSessionAffinity(ctx, digests)
		if errRefresh != nil {
			return SessionAffinityBinding{}, false, errRefresh
		}
		return binding, found, nil
	}
	return SessionAffinityBinding{
		Digests:       sessionAffinityRowDigests(rows, digests),
		BindingDigest: winner.BindingDigest,
		AuthID:        winner.AuthID,
	}, true, nil
}

func (s *UserStore) BindSessionAffinity(ctx context.Context, digests []SessionAffinityDigest, candidateAuthID string) (SessionAffinityBinding, error) {
	if err := s.checkReady(); err != nil {
		return SessionAffinityBinding{}, err
	}
	digests = normalizeSessionAffinityDigests(digests)
	candidateAuthID = strings.TrimSpace(candidateAuthID)
	if len(digests) == 0 || candidateAuthID == "" {
		return SessionAffinityBinding{}, ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionAffinityBinding{}, s.databaseError("begin bind session affinity", err)
	}
	defer rollbackUnlessCommitted(tx)

	now := s.storeNow()
	rows, err := loadSessionAffinityRows(ctx, tx, digests, nil)
	if err != nil {
		return SessionAffinityBinding{}, s.databaseError("read session affinity before bind", err)
	}
	allDigests := sessionAffinityRowDigests(rows, digests)
	winner, ok := chooseSessionAffinityWinner(rows, now)
	if !ok {
		winner = sessionAffinityRow{
			BindingDigest: canonicalSessionAffinityDigest(allDigests),
			AuthID:        candidateAuthID,
			Created:       now,
			Updated:       now,
			Expires:       now.Add(sessionAffinityExpiry),
		}
	}
	if err = writeSessionAffinityAliases(ctx, tx, allDigests, winner, now); err != nil {
		return SessionAffinityBinding{}, s.databaseError("write session affinity binding", err)
	}
	if err = tx.Commit(); err != nil {
		return SessionAffinityBinding{}, s.databaseError("commit session affinity binding", err)
	}
	return SessionAffinityBinding{
		Digests:       allDigests,
		BindingDigest: winner.BindingDigest,
		AuthID:        winner.AuthID,
	}, nil
}

func (s *UserStore) RebindSessionAffinity(ctx context.Context, binding SessionAffinityBinding, candidateAuthID string) (SessionAffinityBinding, error) {
	if err := s.checkReady(); err != nil {
		return SessionAffinityBinding{}, err
	}
	digests := normalizeSessionAffinityDigests(binding.Digests)
	expectedAuthID := strings.TrimSpace(binding.AuthID)
	candidateAuthID = strings.TrimSpace(candidateAuthID)
	if len(digests) == 0 || expectedAuthID == "" || candidateAuthID == "" {
		return SessionAffinityBinding{}, ErrInvalidInput
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionAffinityBinding{}, s.databaseError("begin rebind session affinity", err)
	}
	defer rollbackUnlessCommitted(tx)

	now := s.storeNow()
	bindingDigests := []SessionAffinityDigest{binding.BindingDigest}
	rows, err := loadSessionAffinityRows(ctx, tx, digests, bindingDigests)
	if err != nil {
		return SessionAffinityBinding{}, s.databaseError("read session affinity before rebind", err)
	}
	allDigests := sessionAffinityRowDigests(rows, digests)
	winner, ok := chooseSessionAffinityWinner(rows, now)
	switch {
	case !ok:
		winner = sessionAffinityRow{
			BindingDigest: canonicalSessionAffinityDigest(allDigests),
			AuthID:        candidateAuthID,
			Created:       now,
			Updated:       now,
			Expires:       now.Add(sessionAffinityExpiry),
		}
	case winner.AuthID == expectedAuthID:
		winner.AuthID = candidateAuthID
		winner.Updated = now
		winner.Expires = now.Add(sessionAffinityExpiry)
	}
	if err = writeSessionAffinityAliases(ctx, tx, allDigests, winner, now); err != nil {
		return SessionAffinityBinding{}, s.databaseError("write rebound session affinity", err)
	}
	if err = tx.Commit(); err != nil {
		return SessionAffinityBinding{}, s.databaseError("commit rebound session affinity", err)
	}
	return SessionAffinityBinding{
		Digests:       allDigests,
		BindingDigest: winner.BindingDigest,
		AuthID:        winner.AuthID,
	}, nil
}

func (s *UserStore) ClearSessionAffinity(ctx context.Context) (int64, error) {
	if err := s.checkReady(); err != nil {
		return 0, err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM session_affinity_bindings`)
	if err != nil {
		return 0, s.databaseError("clear session affinity", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, s.databaseError("read cleared session affinity count", err)
	}
	return deleted, nil
}

func (s *UserStore) DeleteSessionAffinityByAuthIDs(ctx context.Context, authIDs []string) (int64, error) {
	if err := s.checkReady(); err != nil {
		return 0, err
	}
	authIDs = normalizeStrings(authIDs)
	if len(authIDs) == 0 {
		return 0, nil
	}
	args := make([]any, len(authIDs))
	for i, authID := range authIDs {
		args[i] = authID
	}
	result, err := s.db.ExecContext(
		ctx,
		`DELETE FROM session_affinity_bindings WHERE auth_id IN (`+sqlPlaceholders(len(authIDs))+`)`,
		args...,
	)
	if err != nil {
		return 0, s.databaseError("delete session affinity for removed auths", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, s.databaseError("read removed auth session affinity count", err)
	}
	return deleted, nil
}

func (s *UserStore) DeleteSessionAffinityExceptAuthIDs(ctx context.Context, authIDs []string) (int64, error) {
	if err := s.checkReady(); err != nil {
		return 0, err
	}
	authIDs = normalizeStrings(authIDs)
	query := `DELETE FROM session_affinity_bindings`
	args := make([]any, len(authIDs))
	if len(authIDs) > 0 {
		query += ` WHERE auth_id NOT IN (` + sqlPlaceholders(len(authIDs)) + `)`
		for i, authID := range authIDs {
			args[i] = authID
		}
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, s.databaseError("delete session affinity for unavailable auths", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, s.databaseError("read unavailable auth session affinity count", err)
	}
	return deleted, nil
}

func (s *UserStore) CleanupExpiredSessionAffinity(ctx context.Context, limit int) (int64, error) {
	if err := s.checkReady(); err != nil {
		return 0, err
	}
	if limit <= 0 {
		limit = sessionAffinityCleanupBatchSize
	}
	result, err := s.db.ExecContext(
		ctx,
		`DELETE FROM session_affinity_bindings
		 WHERE session_digest IN (
			SELECT session_digest
			FROM session_affinity_bindings
			WHERE expires_at <= ?
			ORDER BY expires_at ASC
			LIMIT ?
		 )`,
		formatDBTime(s.storeNow()),
		limit,
	)
	if err != nil {
		return 0, s.databaseError("cleanup expired session affinity", err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return 0, s.databaseError("read expired session affinity cleanup count", err)
	}
	return deleted, nil
}

func (s *UserStore) maybeCleanupExpiredSessionAffinity(ctx context.Context) error {
	now := s.storeNow()
	s.affinityCleanupMu.Lock()
	defer s.affinityCleanupMu.Unlock()
	if !s.nextAffinityCleanupAt.IsZero() && now.Before(s.nextAffinityCleanupAt) {
		return nil
	}
	deleted, err := s.CleanupExpiredSessionAffinity(ctx, sessionAffinityCleanupBatchSize)
	if err != nil {
		return err
	}
	if deleted < sessionAffinityCleanupBatchSize {
		s.nextAffinityCleanupAt = now.Add(sessionAffinityCleanupInterval)
	} else {
		s.nextAffinityCleanupAt = now
	}
	return nil
}

func (s *UserStore) refreshSessionAffinity(ctx context.Context, digests []SessionAffinityDigest) (SessionAffinityBinding, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionAffinityBinding{}, false, s.databaseError("begin refresh session affinity", err)
	}
	defer rollbackUnlessCommitted(tx)

	now := s.storeNow()
	rows, err := loadSessionAffinityRows(ctx, tx, digests, nil)
	if err != nil {
		return SessionAffinityBinding{}, false, s.databaseError("read session affinity for refresh", err)
	}
	allDigests := sessionAffinityRowDigests(rows, digests)
	winner, ok := chooseSessionAffinityWinner(rows, now)
	if !ok {
		return SessionAffinityBinding{}, false, nil
	}
	if !winner.Updated.Add(sessionAffinityRenewalThreshold).After(now) {
		winner.Updated = now
		winner.Expires = now.Add(sessionAffinityExpiry)
	}
	if err = writeSessionAffinityAliases(ctx, tx, allDigests, winner, now); err != nil {
		return SessionAffinityBinding{}, false, s.databaseError("refresh session affinity aliases", err)
	}
	if err = tx.Commit(); err != nil {
		return SessionAffinityBinding{}, false, s.databaseError("commit refreshed session affinity", err)
	}
	return SessionAffinityBinding{
		Digests:       allDigests,
		BindingDigest: winner.BindingDigest,
		AuthID:        winner.AuthID,
	}, true, nil
}

type sessionAffinityRowsQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadSessionAffinityRows(
	ctx context.Context,
	queryer sessionAffinityRowsQueryer,
	digests []SessionAffinityDigest,
	bindingDigests []SessionAffinityDigest,
) ([]sessionAffinityRow, error) {
	rows, err := readSessionAffinityRows(ctx, queryer, "session_digest", digests)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		bindingDigests = append(bindingDigests, row.BindingDigest)
	}
	bindingDigests = normalizeSessionAffinityDigests(bindingDigests)
	if len(bindingDigests) == 0 {
		return rows, nil
	}
	groupRows, err := readSessionAffinityRows(ctx, queryer, "binding_digest", bindingDigests)
	if err != nil {
		return nil, err
	}
	byDigest := make(map[SessionAffinityDigest]sessionAffinityRow, len(rows)+len(groupRows))
	for _, row := range rows {
		byDigest[row.Digest] = row
	}
	for _, row := range groupRows {
		byDigest[row.Digest] = row
	}
	rows = rows[:0]
	for _, row := range byDigest {
		rows = append(rows, row)
	}
	return rows, nil
}

func readSessionAffinityRows(
	ctx context.Context,
	queryer sessionAffinityRowsQueryer,
	column string,
	digests []SessionAffinityDigest,
) ([]sessionAffinityRow, error) {
	digests = normalizeSessionAffinityDigests(digests)
	if len(digests) == 0 {
		return nil, nil
	}
	if column != "session_digest" && column != "binding_digest" {
		return nil, ErrInvalidInput
	}
	args := make([]any, len(digests))
	for i, digest := range digests {
		args[i] = digest
	}
	rows, err := queryer.QueryContext(
		ctx,
		`SELECT session_digest, binding_digest, auth_id, created_at, updated_at, expires_at
		 FROM session_affinity_bindings
		 WHERE `+column+` IN (`+sqlPlaceholders(len(digests))+`)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]sessionAffinityRow, 0, len(digests))
	for rows.Next() {
		var row sessionAffinityRow
		var createdRaw string
		var updatedRaw string
		var expiresRaw string
		if err = rows.Scan(&row.Digest, &row.BindingDigest, &row.AuthID, &createdRaw, &updatedRaw, &expiresRaw); err != nil {
			return nil, err
		}
		if row.Created, err = parseDBTime(createdRaw); err != nil {
			return nil, err
		}
		if row.Updated, err = parseDBTime(updatedRaw); err != nil {
			return nil, err
		}
		if row.Expires, err = parseDBTime(expiresRaw); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	return result, nil
}

func chooseSessionAffinityWinner(rows []sessionAffinityRow, now time.Time) (sessionAffinityRow, bool) {
	var winner sessionAffinityRow
	found := false
	for _, row := range rows {
		if !row.Expires.After(now) {
			continue
		}
		if !found || row.Created.Before(winner.Created) ||
			(row.Created.Equal(winner.Created) && row.Digest < winner.Digest) {
			winner = row
			found = true
		}
	}
	return winner, found
}

func sessionAffinityNeedsWrite(rows []sessionAffinityRow, digests []SessionAffinityDigest, winner sessionAffinityRow, now time.Time) bool {
	if !winner.Updated.Add(sessionAffinityRenewalThreshold).After(now) {
		return true
	}
	active := make(map[SessionAffinityDigest]sessionAffinityRow, len(rows))
	for _, row := range rows {
		if row.Expires.After(now) {
			active[row.Digest] = row
		}
	}
	for _, digest := range digests {
		row, ok := active[digest]
		if !ok || row.AuthID != winner.AuthID || row.BindingDigest != winner.BindingDigest {
			return true
		}
	}
	return false
}

func writeSessionAffinityAliases(ctx context.Context, tx *sql.Tx, digests []SessionAffinityDigest, winner sessionAffinityRow, now time.Time) error {
	if winner.BindingDigest == "" {
		winner.BindingDigest = canonicalSessionAffinityDigest(digests)
	}
	if winner.Created.IsZero() {
		winner.Created = now
	}
	if winner.Updated.IsZero() {
		winner.Updated = now
	}
	if winner.Expires.IsZero() || !winner.Expires.After(now) {
		winner.Expires = winner.Updated.Add(sessionAffinityExpiry)
	}
	for _, digest := range digests {
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO session_affinity_bindings
				(session_digest, binding_digest, auth_id, created_at, updated_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(session_digest) DO UPDATE SET
				binding_digest = excluded.binding_digest,
				auth_id = excluded.auth_id,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at,
				expires_at = excluded.expires_at`,
			digest,
			winner.BindingDigest,
			winner.AuthID,
			formatDBTime(winner.Created),
			formatDBTime(winner.Updated),
			formatDBTime(winner.Expires),
		); err != nil {
			return err
		}
	}
	return nil
}

func normalizeSessionAffinityDigests(digests []SessionAffinityDigest) []SessionAffinityDigest {
	result := make([]SessionAffinityDigest, 0, len(digests))
	for _, digest := range digests {
		digest = SessionAffinityDigest(strings.TrimSpace(string(digest)))
		if len(digest) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(string(digest)); err != nil || slices.Contains(result, digest) {
			continue
		}
		result = append(result, digest)
	}
	return result
}

func canonicalSessionAffinityDigest(digests []SessionAffinityDigest) SessionAffinityDigest {
	digests = normalizeSessionAffinityDigests(digests)
	if len(digests) == 0 {
		return ""
	}
	slices.Sort(digests)
	return digests[0]
}

func sessionAffinityRowDigests(rows []sessionAffinityRow, requested []SessionAffinityDigest) []SessionAffinityDigest {
	digests := slices.Clone(requested)
	for _, row := range rows {
		digests = append(digests, row.Digest)
	}
	return normalizeSessionAffinityDigests(digests)
}

func normalizeStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || slices.Contains(result, value) {
			continue
		}
		result = append(result, value)
	}
	return result
}

func sqlPlaceholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func (s *UserStore) storeNow() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func sessionAffinityTenantScope(authorization proxyAuthorization) string {
	if authorization.Credential != nil {
		return "user:" + authorization.Credential.User.ID
	}
	if authorization.CompatibilityAuthID != "" {
		return "auth:" + authorization.CompatibilityAuthID
	}
	return ""
}

func sessionAffinityDigestsForRequest(authorization proxyAuthorization, signals []sessionAffinitySignal) []SessionAffinityDigest {
	return digestSessionAffinitySignals(sessionAffinityTenantScope(authorization), signals)
}

func (b SessionAffinityBinding) valid() bool {
	return strings.TrimSpace(b.AuthID) != "" &&
		canonicalSessionAffinityDigest([]SessionAffinityDigest{b.BindingDigest}) != "" &&
		len(normalizeSessionAffinityDigests(b.Digests)) > 0
}
