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
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxSessionAffinitySignalBytes    = 512
	sessionAffinityReplayMemoryBytes = 64 << 10
	sessionAffinityExpiry            = time.Hour
	sessionAffinityRenewalThreshold  = 30 * time.Minute
	sessionAffinityCleanupInterval   = 5 * time.Minute
	sessionAffinityCleanupBatchSize  = 100
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
	closers []io.Closer
	once    sync.Once
}

type replayErrorReader struct {
	err error
}

type sessionAffinitySignalAccumulator struct {
	candidates map[string]string
	conflicts  map[string]bool
}

type sessionAffinityReplayStore struct {
	memory        bytes.Buffer
	file          *os.File
	path          string
	spillDisabled bool
}

type sessionAffinitySourceReader struct {
	reader      io.Reader
	err         error
	bytesRead   int64
	errorOffset int64
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
	if r == nil {
		return nil
	}
	var closeErr error
	r.once.Do(func() {
		for _, closer := range r.closers {
			if closer == nil {
				continue
			}
			if err := closer.Close(); err != nil && closeErr == nil {
				closeErr = err
			}
		}
	})
	return closeErr
}

func newSessionAffinitySignalAccumulator() *sessionAffinitySignalAccumulator {
	return &sessionAffinitySignalAccumulator{
		candidates: make(map[string]string),
		conflicts:  make(map[string]bool),
	}
}

func (a *sessionAffinitySignalAccumulator) add(kind string, raw string) {
	if a == nil {
		return
	}
	value, ok := normalizeSessionAffinitySignal(raw)
	if !ok || a.conflicts[kind] {
		return
	}
	if current, exists := a.candidates[kind]; exists && current != value {
		delete(a.candidates, kind)
		a.conflicts[kind] = true
		return
	}
	a.candidates[kind] = value
}

func (a *sessionAffinitySignalAccumulator) merge(other *sessionAffinitySignalAccumulator) {
	if a == nil || other == nil {
		return
	}
	for kind := range other.conflicts {
		delete(a.candidates, kind)
		a.conflicts[kind] = true
	}
	for kind, value := range other.candidates {
		a.add(kind, value)
	}
}

func (a *sessionAffinitySignalAccumulator) signals() []sessionAffinitySignal {
	if a == nil {
		return nil
	}
	kinds := []string{
		sessionAffinitySignalSessionID,
		sessionAffinitySignalPromptCacheKey,
		sessionAffinitySignalConversationID,
	}
	signals := make([]sessionAffinitySignal, 0, len(kinds))
	for _, kind := range kinds {
		if value := a.candidates[kind]; value != "" && !a.conflicts[kind] {
			signals = append(signals, sessionAffinitySignal{Kind: kind, Value: value})
		}
	}
	return signals
}

func extractSessionAffinitySignals(r *http.Request) []sessionAffinitySignal {
	if r == nil {
		return nil
	}
	signals := newSessionAffinitySignalAccumulator()
	for _, name := range []string{"Session-Id", "Session_id"} {
		values := r.Header.Values(name)
		if len(values) > 4 {
			values = values[:4]
		}
		for _, value := range values {
			signals.add(sessionAffinitySignalSessionID, value)
		}
	}

	if requestMayContainSessionAffinityJSON(r) {
		bodySignals, valid := extractSessionAffinityJSONSignals(r)
		if valid {
			signals.merge(bodySignals)
		}
	}
	return signals.signals()
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

func extractSessionAffinityJSONSignals(r *http.Request) (*sessionAffinitySignalAccumulator, bool) {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return nil, false
	}
	original := r.Body
	replay := &sessionAffinityReplayStore{}
	source := &sessionAffinitySourceReader{reader: original}
	decoder := json.NewDecoder(io.TeeReader(source, replay))
	decoder.UseNumber()
	signals, valid := parseSessionAffinityJSONObject(decoder)
	if source.err == nil {
		_, _ = io.Copy(replay, source)
	}
	replayedBody, err := replay.body(original, source.err, source.errorOffset)
	if err != nil {
		replayedBody = &replayReadCloser{
			Reader:  bytes.NewReader(replay.memory.Bytes()),
			closers: []io.Closer{original, replay},
		}
	}
	r.Body = replayedBody
	return signals, valid && source.err == nil
}

func parseSessionAffinityJSONObject(decoder *json.Decoder) (*sessionAffinitySignalAccumulator, bool) {
	signals := newSessionAffinitySignalAccumulator()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	for decoder.More() {
		keyToken, errKey := decoder.Token()
		if errKey != nil {
			return nil, false
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false
		}
		switch key {
		case "session_id", "sessionId":
			if !readSessionAffinityString(decoder, sessionAffinitySignalSessionID, signals) {
				return nil, false
			}
		case "prompt_cache_key":
			if !readSessionAffinityString(decoder, sessionAffinitySignalPromptCacheKey, signals) {
				return nil, false
			}
		case "conversation_id":
			if !readSessionAffinityString(decoder, sessionAffinitySignalConversationID, signals) {
				return nil, false
			}
		case "conversation":
			if !readSessionAffinityConversation(decoder, signals) {
				return nil, false
			}
		default:
			if err = skipSessionAffinityJSONValue(decoder); err != nil {
				return nil, false
			}
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil, false
	}
	return signals, true
}

func readSessionAffinityString(
	decoder *json.Decoder,
	kind string,
	signals *sessionAffinitySignalAccumulator,
) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	if value, ok := token.(string); ok {
		signals.add(kind, value)
		return true
	}
	if delimiter, ok := token.(json.Delim); ok {
		return skipOpenedSessionAffinityJSONValue(decoder, delimiter) == nil
	}
	return true
}

func readSessionAffinityConversation(
	decoder *json.Decoder,
	signals *sessionAffinitySignalAccumulator,
) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return true
	}
	if delimiter != json.Delim('{') {
		return skipOpenedSessionAffinityJSONValue(decoder, delimiter) == nil
	}
	for decoder.More() {
		keyToken, errKey := decoder.Token()
		if errKey != nil {
			return false
		}
		key, okKey := keyToken.(string)
		if !okKey {
			return false
		}
		if key == "id" {
			if !readSessionAffinityString(decoder, sessionAffinitySignalConversationID, signals) {
				return false
			}
			continue
		}
		if err = skipSessionAffinityJSONValue(decoder); err != nil {
			return false
		}
	}
	token, err = decoder.Token()
	return err == nil && token == json.Delim('}')
}

func skipSessionAffinityJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	return skipOpenedSessionAffinityJSONValue(decoder, delimiter)
}

func skipOpenedSessionAffinityJSONValue(decoder *json.Decoder, delimiter json.Delim) error {
	if delimiter != json.Delim('{') && delimiter != json.Delim('[') {
		return nil
	}
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		nested, ok := token.(json.Delim)
		if !ok {
			continue
		}
		switch nested {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return nil
}

func (r *sessionAffinitySourceReader) Read(p []byte) (int, error) {
	if r == nil || r.reader == nil {
		return 0, io.EOF
	}
	n, err := r.reader.Read(p)
	r.bytesRead += int64(n)
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
		r.errorOffset = r.bytesRead
	}
	return n, err
}

func (s *sessionAffinityReplayStore) Write(p []byte) (int, error) {
	if s == nil {
		return len(p), nil
	}
	if s.file == nil && (s.spillDisabled || s.memory.Len()+len(p) <= sessionAffinityReplayMemoryBytes) {
		return s.memory.Write(p)
	}
	if s.file == nil {
		if err := s.spillToFile(); err != nil {
			s.spillDisabled = true
			return s.memory.Write(p)
		}
	}
	n, err := s.file.Write(p)
	if err == nil {
		return n, nil
	}
	if errFallback := s.fallbackToMemory(p[n:]); errFallback != nil {
		return n, err
	}
	return len(p), nil
}

func (s *sessionAffinityReplayStore) spillToFile() error {
	file, err := os.CreateTemp("", "codex-oauth-proxy-session-affinity-*")
	if err != nil {
		return err
	}
	if err = file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return err
	}
	if _, err = file.Write(s.memory.Bytes()); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return err
	}
	s.memory.Reset()
	s.file = file
	s.path = file.Name()
	if err = os.Remove(s.path); err == nil {
		s.path = ""
	}
	return nil
}

func (s *sessionAffinityReplayStore) fallbackToMemory(remaining []byte) error {
	if s == nil || s.file == nil {
		return nil
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(&s.memory, s.file); err != nil {
		return err
	}
	if _, err := s.memory.Write(remaining); err != nil {
		return err
	}
	if err := s.closeFile(); err != nil {
		return err
	}
	s.spillDisabled = true
	return nil
}

func (s *sessionAffinityReplayStore) body(
	original io.Closer,
	sourceErr error,
	errorOffset int64,
) (io.ReadCloser, error) {
	var reader io.Reader
	if s.file != nil {
		if _, err := s.file.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		reader = s.file
	} else {
		reader = bytes.NewReader(s.memory.Bytes())
	}
	if sourceErr != nil {
		reader = io.LimitReader(reader, errorOffset)
	}
	readers := []io.Reader{reader}
	if sourceErr != nil {
		readers = append(readers, &replayErrorReader{err: sourceErr})
	}
	return &replayReadCloser{
		Reader:  io.MultiReader(readers...),
		closers: []io.Closer{original, s},
	}, nil
}

func (s *sessionAffinityReplayStore) Close() error {
	if s == nil {
		return nil
	}
	return s.closeFile()
}

func (s *sessionAffinityReplayStore) closeFile() error {
	if s == nil || s.file == nil {
		return nil
	}
	errClose := s.file.Close()
	s.file = nil
	if s.path != "" {
		errRemove := os.Remove(s.path)
		s.path = ""
		if errClose == nil {
			errClose = errRemove
		}
	}
	return errClose
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
