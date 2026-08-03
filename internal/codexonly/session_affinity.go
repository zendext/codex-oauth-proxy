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
	sessionAffinityReplayReadBytes   = 32 << 10
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
	Digests           []SessionAffinityDigest
	BindingDigest     SessionAffinityDigest
	TenantScopeDigest SessionAffinityDigest
	AuthID            string
}

type sessionAffinityRow struct {
	Digest            SessionAffinityDigest
	BindingDigest     SessionAffinityDigest
	TenantScopeDigest SessionAffinityDigest
	AuthID            string
	Created           time.Time
	Updated           time.Time
	Expires           time.Time
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
	memory     bytes.Buffer
	file       *os.File
	fileBytes  int64
	path       string
	tail       []byte
	spillErr   error
	createTemp func() (*os.File, error)
	writeFile  func(*os.File, []byte) (int, error)
}

type sessionAffinitySourceReader struct {
	reader      io.Reader
	err         error
	bytesRead   int64
	errorOffset int64
	unicode     sessionAffinityJSONUnicodeValidator
}

type sessionAffinityReplayReader struct {
	source *sessionAffinitySourceReader
	replay *sessionAffinityReplayStore
}

type sessionAffinityJSONUnicodeValidator struct {
	utf8Pending     [utf8.UTFMax]byte
	utf8PendingLen  int
	inString        bool
	escaped         bool
	unicodeDigits   int
	unicodeValue    uint16
	pendingHigh     bool
	invalidEncoding bool
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
	return extractSessionAffinitySignalsWithReplayStore(r, nil)
}

func extractSessionAffinitySignalsWithReplayStore(
	r *http.Request,
	replay *sessionAffinityReplayStore,
) []sessionAffinitySignal {
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
		bodySignals, valid := extractSessionAffinityJSONSignals(r, replay)
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

func extractSessionAffinityJSONSignals(
	r *http.Request,
	replay *sessionAffinityReplayStore,
) (*sessionAffinitySignalAccumulator, bool) {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return nil, false
	}
	original := r.Body
	if replay == nil {
		replay = &sessionAffinityReplayStore{}
	}
	source := &sessionAffinitySourceReader{reader: original}
	inspectionReader := &sessionAffinityReplayReader{
		source: source,
		replay: replay,
	}
	decoder := json.NewDecoder(inspectionReader)
	decoder.UseNumber()
	signals, valid := parseSessionAffinityJSONObject(decoder)
	if source.err == nil && replay.spillErr == nil {
		_, _ = io.Copy(io.Discard, inspectionReader)
	}
	r.Body = replay.body(original, source.err, source.errorOffset)
	return signals, valid && source.err == nil && replay.spillErr == nil && source.unicode.valid()
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
	r.unicode.write(p[:n])
	if err != nil && err != io.EOF && r.err == nil {
		r.err = err
		r.errorOffset = r.bytesRead
	}
	return n, err
}

func (v *sessionAffinityJSONUnicodeValidator) write(p []byte) {
	if v == nil || len(p) == 0 {
		return
	}
	v.validateUTF8(p)
	for _, b := range p {
		v.validateJSONStringByte(b)
	}
}

func (v *sessionAffinityJSONUnicodeValidator) validateUTF8(p []byte) {
	if v == nil {
		return
	}
	if v.utf8PendingLen > 0 {
		for len(p) > 0 && !utf8.FullRune(v.utf8Pending[:v.utf8PendingLen]) {
			v.utf8Pending[v.utf8PendingLen] = p[0]
			v.utf8PendingLen++
			p = p[1:]
		}
		if !utf8.FullRune(v.utf8Pending[:v.utf8PendingLen]) {
			return
		}
		if r, size := utf8.DecodeRune(v.utf8Pending[:v.utf8PendingLen]); r == utf8.RuneError && size == 1 {
			v.invalidEncoding = true
		}
		v.utf8PendingLen = 0
	}
	for len(p) > 0 {
		if p[0] < utf8.RuneSelf {
			p = p[1:]
			continue
		}
		if !utf8.FullRune(p) {
			v.utf8PendingLen = copy(v.utf8Pending[:], p)
			return
		}
		r, size := utf8.DecodeRune(p)
		if r == utf8.RuneError && size == 1 {
			v.invalidEncoding = true
		}
		p = p[size:]
	}
}

func (v *sessionAffinityJSONUnicodeValidator) validateJSONStringByte(b byte) {
	if v == nil {
		return
	}
	if !v.inString {
		if b == '"' {
			v.inString = true
		}
		return
	}
	if v.unicodeDigits > 0 {
		digit, ok := sessionAffinityHexDigit(b)
		if !ok {
			v.invalidEncoding = true
			v.unicodeDigits = 0
			v.unicodeValue = 0
			return
		}
		v.unicodeValue = v.unicodeValue<<4 | uint16(digit)
		v.unicodeDigits--
		if v.unicodeDigits == 0 {
			v.finishUnicodeEscape()
		}
		return
	}
	if v.escaped {
		v.escaped = false
		if b == 'u' {
			v.unicodeDigits = 4
			v.unicodeValue = 0
			return
		}
		if v.pendingHigh {
			v.invalidEncoding = true
			v.pendingHigh = false
		}
		return
	}
	if v.pendingHigh {
		if b == '\\' {
			v.escaped = true
			return
		}
		v.invalidEncoding = true
		v.pendingHigh = false
	}
	switch b {
	case '\\':
		v.escaped = true
	case '"':
		v.inString = false
	}
}

func (v *sessionAffinityJSONUnicodeValidator) finishUnicodeEscape() {
	switch {
	case v.unicodeValue >= 0xd800 && v.unicodeValue <= 0xdbff:
		if v.pendingHigh {
			v.invalidEncoding = true
		}
		v.pendingHigh = true
	case v.unicodeValue >= 0xdc00 && v.unicodeValue <= 0xdfff:
		if !v.pendingHigh {
			v.invalidEncoding = true
		}
		v.pendingHigh = false
	default:
		if v.pendingHigh {
			v.invalidEncoding = true
			v.pendingHigh = false
		}
	}
	v.unicodeValue = 0
}

func (v *sessionAffinityJSONUnicodeValidator) valid() bool {
	return v != nil &&
		!v.invalidEncoding &&
		v.utf8PendingLen == 0 &&
		v.unicodeDigits == 0 &&
		!v.pendingHigh
}

func sessionAffinityHexDigit(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	default:
		return 0, false
	}
}

func (r *sessionAffinityReplayReader) Read(p []byte) (int, error) {
	if r == nil || r.source == nil || r.replay == nil {
		return 0, io.EOF
	}
	if r.replay.spillErr != nil {
		return 0, r.replay.spillErr
	}
	if len(p) > sessionAffinityReplayReadBytes {
		p = p[:sessionAffinityReplayReadBytes]
	}
	if err := r.replay.prepareRead(len(p)); err != nil {
		r.replay.spillErr = err
		return 0, err
	}
	n, errSource := r.source.Read(p)
	if n == 0 {
		return 0, errSource
	}
	if errReplay := r.replay.append(p[:n]); errReplay != nil {
		return n, errReplay
	}
	return n, errSource
}

func (s *sessionAffinityReplayStore) prepareRead(size int) error {
	if s == nil || s.file != nil || s.memory.Len()+size <= sessionAffinityReplayMemoryBytes {
		return nil
	}
	return s.spillToFile()
}

func (s *sessionAffinityReplayStore) append(p []byte) error {
	if s == nil || len(p) == 0 {
		return nil
	}
	if s.file == nil {
		_, err := s.memory.Write(p)
		return err
	}
	n, err := s.writeReplayFile(p)
	if n > 0 {
		s.fileBytes += int64(n)
	}
	if n < len(p) {
		s.tail = append(s.tail, p[n:]...)
		if err == nil {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		s.spillErr = err
	}
	return err
}

func (s *sessionAffinityReplayStore) spillToFile() error {
	file, err := s.createReplayTemp()
	if err != nil {
		return err
	}
	if err = file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return err
	}
	n, err := s.writeReplayFileTo(file, s.memory.Bytes())
	if err == nil && n != s.memory.Len() {
		err = io.ErrShortWrite
	}
	if err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return err
	}
	s.fileBytes = int64(n)
	s.memory.Reset()
	s.file = file
	s.path = file.Name()
	if err = os.Remove(s.path); err == nil {
		s.path = ""
	}
	return nil
}

func (s *sessionAffinityReplayStore) createReplayTemp() (*os.File, error) {
	if s != nil && s.createTemp != nil {
		return s.createTemp()
	}
	return os.CreateTemp("", "codex-oauth-proxy-session-affinity-*")
}

func (s *sessionAffinityReplayStore) writeReplayFile(p []byte) (int, error) {
	if s == nil || s.file == nil {
		return 0, io.ErrClosedPipe
	}
	return s.writeReplayFileTo(s.file, p)
}

func (s *sessionAffinityReplayStore) writeReplayFileTo(file *os.File, p []byte) (int, error) {
	if s != nil && s.writeFile != nil {
		return s.writeFile(file, p)
	}
	return file.Write(p)
}

func (s *sessionAffinityReplayStore) replayReader() io.Reader {
	readers := make([]io.Reader, 0, 3)
	if s != nil && s.file != nil && s.fileBytes > 0 {
		readers = append(readers, io.NewSectionReader(s.file, 0, s.fileBytes))
	} else if s != nil && s.memory.Len() > 0 {
		readers = append(readers, bytes.NewReader(s.memory.Bytes()))
	}
	if s != nil && len(s.tail) > 0 {
		readers = append(readers, bytes.NewReader(s.tail))
	}
	return io.MultiReader(readers...)
}

func (s *sessionAffinityReplayStore) body(
	original io.ReadCloser,
	sourceErr error,
	errorOffset int64,
) io.ReadCloser {
	reader := s.replayReader()
	if sourceErr != nil {
		reader = io.LimitReader(reader, errorOffset)
	}
	readers := []io.Reader{reader}
	switch {
	case sourceErr != nil:
		readers = append(readers, &replayErrorReader{err: sourceErr})
	case s != nil && s.spillErr != nil:
		readers = append(readers, original)
	}
	return &replayReadCloser{
		Reader:  io.MultiReader(readers...),
		closers: []io.Closer{original, s},
	}
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
	s.fileBytes = 0
	s.tail = nil
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

func sessionAffinityTenantScopeDigest(tenantScope string) SessionAffinityDigest {
	tenantScope = strings.TrimSpace(tenantScope)
	if tenantScope == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(tenantScope))
	return SessionAffinityDigest(hex.EncodeToString(sum[:]))
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
	return s.lookupSessionAffinity(ctx, "", digests)
}

func (s *UserStore) LookupScopedSessionAffinity(
	ctx context.Context,
	tenantScope string,
	digests []SessionAffinityDigest,
) (SessionAffinityBinding, bool, error) {
	return s.lookupSessionAffinity(ctx, sessionAffinityTenantScopeDigest(tenantScope), digests)
}

func (s *UserStore) lookupSessionAffinity(
	ctx context.Context,
	tenantScopeDigest SessionAffinityDigest,
	digests []SessionAffinityDigest,
) (SessionAffinityBinding, bool, error) {
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
	if winner.TenantScopeDigest == "" && tenantScopeDigest != "" {
		winner.TenantScopeDigest = tenantScopeDigest
	}
	if sessionAffinityNeedsWrite(rows, digests, winner, tenantScopeDigest, now) {
		binding, found, errRefresh := s.refreshSessionAffinity(ctx, tenantScopeDigest, digests)
		if errRefresh != nil {
			return SessionAffinityBinding{}, false, errRefresh
		}
		return binding, found, nil
	}
	return SessionAffinityBinding{
		Digests:           sessionAffinityRowDigests(rows, digests),
		BindingDigest:     winner.BindingDigest,
		TenantScopeDigest: winner.TenantScopeDigest,
		AuthID:            winner.AuthID,
	}, true, nil
}

func (s *UserStore) BindSessionAffinity(ctx context.Context, digests []SessionAffinityDigest, candidateAuthID string) (SessionAffinityBinding, error) {
	return s.bindSessionAffinity(ctx, "", digests, candidateAuthID)
}

func (s *UserStore) BindScopedSessionAffinity(
	ctx context.Context,
	tenantScope string,
	digests []SessionAffinityDigest,
	candidateAuthID string,
) (SessionAffinityBinding, error) {
	return s.bindSessionAffinity(ctx, sessionAffinityTenantScopeDigest(tenantScope), digests, candidateAuthID)
}

func (s *UserStore) bindSessionAffinity(
	ctx context.Context,
	tenantScopeDigest SessionAffinityDigest,
	digests []SessionAffinityDigest,
	candidateAuthID string,
) (SessionAffinityBinding, error) {
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
			BindingDigest:     canonicalSessionAffinityDigest(allDigests),
			TenantScopeDigest: tenantScopeDigest,
			AuthID:            candidateAuthID,
			Created:           now,
			Updated:           now,
			Expires:           now.Add(sessionAffinityExpiry),
		}
	}
	if winner.TenantScopeDigest == "" && tenantScopeDigest != "" {
		winner.TenantScopeDigest = tenantScopeDigest
	}
	if err = writeSessionAffinityAliases(ctx, tx, allDigests, winner, now); err != nil {
		return SessionAffinityBinding{}, s.databaseError("write session affinity binding", err)
	}
	if err = tx.Commit(); err != nil {
		return SessionAffinityBinding{}, s.databaseError("commit session affinity binding", err)
	}
	return SessionAffinityBinding{
		Digests:           allDigests,
		BindingDigest:     winner.BindingDigest,
		TenantScopeDigest: winner.TenantScopeDigest,
		AuthID:            winner.AuthID,
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
			BindingDigest:     canonicalSessionAffinityDigest(allDigests),
			TenantScopeDigest: binding.TenantScopeDigest,
			AuthID:            candidateAuthID,
			Created:           now,
			Updated:           now,
			Expires:           now.Add(sessionAffinityExpiry),
		}
	case winner.AuthID == expectedAuthID:
		winner.AuthID = candidateAuthID
		winner.Updated = now
		winner.Expires = now.Add(sessionAffinityExpiry)
	}
	if winner.TenantScopeDigest == "" && binding.TenantScopeDigest != "" {
		winner.TenantScopeDigest = binding.TenantScopeDigest
	}
	if err = writeSessionAffinityAliases(ctx, tx, allDigests, winner, now); err != nil {
		return SessionAffinityBinding{}, s.databaseError("write rebound session affinity", err)
	}
	if err = tx.Commit(); err != nil {
		return SessionAffinityBinding{}, s.databaseError("commit rebound session affinity", err)
	}
	return SessionAffinityBinding{
		Digests:           allDigests,
		BindingDigest:     winner.BindingDigest,
		TenantScopeDigest: winner.TenantScopeDigest,
		AuthID:            winner.AuthID,
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

func (s *UserStore) CountSessionAffinityBindingsByAuthIDs(
	ctx context.Context,
	authIDs []string,
) (map[string]int64, error) {
	if err := s.checkReady(); err != nil {
		return nil, err
	}
	authIDs = normalizeStrings(authIDs)
	counts := make(map[string]int64, len(authIDs))
	if len(authIDs) == 0 {
		return counts, nil
	}
	args := make([]any, 0, len(authIDs)+1)
	for _, authID := range authIDs {
		args = append(args, authID)
	}
	args = append(args, formatDBTime(s.storeNow()))
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT auth_id, COUNT(DISTINCT binding_digest)
		 FROM session_affinity_bindings
		 WHERE auth_id IN (`+sqlPlaceholders(len(authIDs))+`) AND expires_at > ?
		 GROUP BY auth_id`,
		args...,
	)
	if err != nil {
		return nil, s.databaseError("count auth session affinity bindings", err)
	}
	defer rows.Close()
	for rows.Next() {
		var authID string
		var count int64
		if err = rows.Scan(&authID, &count); err != nil {
			return nil, s.databaseError("scan auth session affinity count", err)
		}
		counts[authID] = count
	}
	if err = rows.Err(); err != nil {
		return nil, s.databaseError("read auth session affinity counts", err)
	}
	return counts, nil
}

func (s *UserStore) DeleteSessionAffinityByTenantScope(ctx context.Context, tenantScope string) (int64, error) {
	tenantScopeDigest := sessionAffinityTenantScopeDigest(tenantScope)
	if tenantScopeDigest == "" {
		return 0, ErrInvalidInput
	}
	return s.deleteSessionAffinityBindings(
		ctx,
		"tenant_scope_digest = ?",
		[]any{tenantScopeDigest},
	)
}

func (s *UserStore) DeleteSessionAffinityByTenantSession(
	ctx context.Context,
	tenantScope string,
	rawSessionKey string,
) (int64, error) {
	tenantScopeDigest := sessionAffinityTenantScopeDigest(tenantScope)
	value, ok := normalizeSessionAffinitySignal(rawSessionKey)
	if tenantScopeDigest == "" || !ok {
		return 0, ErrInvalidInput
	}
	digests := digestSessionAffinitySignals(tenantScope, []sessionAffinitySignal{
		{Kind: sessionAffinitySignalSessionID, Value: value},
		{Kind: sessionAffinitySignalPromptCacheKey, Value: value},
		{Kind: sessionAffinitySignalConversationID, Value: value},
	})
	args := make([]any, 0, len(digests)+1)
	args = append(args, tenantScopeDigest)
	for _, digest := range digests {
		args = append(args, digest)
	}
	where := `tenant_scope_digest = ? AND binding_digest IN (
		SELECT binding_digest
		FROM session_affinity_bindings
		WHERE tenant_scope_digest = ? AND session_digest IN (` + sqlPlaceholders(len(digests)) + `)
	)`
	deleteArgs := make([]any, 0, len(args)+1)
	deleteArgs = append(deleteArgs, tenantScopeDigest)
	deleteArgs = append(deleteArgs, args...)
	return s.deleteSessionAffinityBindingsWithArgs(
		ctx,
		`tenant_scope_digest = ? AND session_digest IN (`+sqlPlaceholders(len(digests))+`)`,
		args,
		where,
		deleteArgs,
	)
}

func (s *UserStore) DeleteSessionAffinityByAccountID(ctx context.Context, authID string) (int64, error) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return 0, ErrInvalidInput
	}
	return s.deleteSessionAffinityBindings(ctx, "auth_id = ?", []any{authID})
}

func (s *UserStore) deleteSessionAffinityBindings(
	ctx context.Context,
	where string,
	args []any,
) (int64, error) {
	return s.deleteSessionAffinityBindingsWithArgs(ctx, where, args, where, args)
}

func (s *UserStore) deleteSessionAffinityBindingsWithArgs(
	ctx context.Context,
	countWhere string,
	countArgs []any,
	deleteWhere string,
	deleteArgs []any,
) (int64, error) {
	if err := s.checkReady(); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, s.databaseError("begin clear session affinity", err)
	}
	defer rollbackUnlessCommitted(tx)
	var count int64
	if err = tx.QueryRowContext(
		ctx,
		`SELECT COUNT(DISTINCT binding_digest) FROM session_affinity_bindings WHERE `+countWhere,
		countArgs...,
	).Scan(&count); err != nil {
		return 0, s.databaseError("count cleared session affinity", err)
	}
	if _, err = tx.ExecContext(
		ctx,
		`DELETE FROM session_affinity_bindings WHERE `+deleteWhere,
		deleteArgs...,
	); err != nil {
		return 0, s.databaseError("clear session affinity", err)
	}
	if err = tx.Commit(); err != nil {
		return 0, s.databaseError("commit cleared session affinity", err)
	}
	return count, nil
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

func (s *UserStore) refreshSessionAffinity(
	ctx context.Context,
	tenantScopeDigest SessionAffinityDigest,
	digests []SessionAffinityDigest,
) (SessionAffinityBinding, bool, error) {
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
	if winner.TenantScopeDigest == "" && tenantScopeDigest != "" {
		winner.TenantScopeDigest = tenantScopeDigest
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
		Digests:           allDigests,
		BindingDigest:     winner.BindingDigest,
		TenantScopeDigest: winner.TenantScopeDigest,
		AuthID:            winner.AuthID,
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
		`SELECT session_digest, binding_digest, tenant_scope_digest, auth_id, created_at, updated_at, expires_at
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
		if err = rows.Scan(
			&row.Digest,
			&row.BindingDigest,
			&row.TenantScopeDigest,
			&row.AuthID,
			&createdRaw,
			&updatedRaw,
			&expiresRaw,
		); err != nil {
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

func sessionAffinityNeedsWrite(
	rows []sessionAffinityRow,
	digests []SessionAffinityDigest,
	winner sessionAffinityRow,
	tenantScopeDigest SessionAffinityDigest,
	now time.Time,
) bool {
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
		if !ok || row.AuthID != winner.AuthID || row.BindingDigest != winner.BindingDigest ||
			tenantScopeDigest != "" && row.TenantScopeDigest != tenantScopeDigest {
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
					(session_digest, binding_digest, tenant_scope_digest, auth_id, created_at, updated_at, expires_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT(session_digest) DO UPDATE SET
					binding_digest = excluded.binding_digest,
					tenant_scope_digest = excluded.tenant_scope_digest,
					auth_id = excluded.auth_id,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at,
				expires_at = excluded.expires_at`,
			digest,
			winner.BindingDigest,
			winner.TenantScopeDigest,
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
