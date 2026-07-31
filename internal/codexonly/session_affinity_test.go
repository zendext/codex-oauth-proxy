package codexonly

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestExtractSessionAffinitySignalsRestoresRequestBody(t *testing.T) {
	body := `{"sessionId":" session-1 ","prompt_cache_key":"cache-1","conversation":{"id":"conversation-1"},"input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session-Id", "session-1")

	signals := extractSessionAffinitySignals(req)

	if got, want := signals, []sessionAffinitySignal{
		{Kind: sessionAffinitySignalSessionID, Value: "session-1"},
		{Kind: sessionAffinitySignalPromptCacheKey, Value: "cache-1"},
		{Kind: sessionAffinitySignalConversationID, Value: "conversation-1"},
	}; !slices.Equal(got, want) {
		t.Fatalf("signals = %#v, want %#v", got, want)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(restored) != body {
		t.Fatalf("restored body = %q, want %q", string(restored), body)
	}
}

func TestExtractSessionAffinitySignalsAcceptsUnderscoreHeaderAlias(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/backend-api/wham/usage", nil)
	req.Header.Set("Session_id", "session-underscore")

	signals := extractSessionAffinitySignals(req)

	if got, want := signals, []sessionAffinitySignal{
		{Kind: sessionAffinitySignalSessionID, Value: "session-underscore"},
	}; !slices.Equal(got, want) {
		t.Fatalf("signals = %#v, want %#v", got, want)
	}
}

func TestExtractSessionAffinitySignalsRejectsInvalidValuesWithoutChangingRequest(t *testing.T) {
	oversized := strings.Repeat("x", maxSessionAffinitySignalBytes+1)
	body := `{"session_id":"bad\u0000value","prompt_cache_key":` + quotedJSON(oversized) + `,"input":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Session_id", oversized)

	if signals := extractSessionAffinitySignals(req); len(signals) != 0 {
		t.Fatalf("signals = %#v, want none", signals)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read restored body: %v", err)
	}
	if string(restored) != body {
		t.Fatalf("restored body = %q, want %q", string(restored), body)
	}
}

func TestExtractSessionAffinitySignalsFromLargeJSONBodyAndRestoresIt(t *testing.T) {
	body := largeSessionAffinityJSON("large-session")
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	if signals := extractSessionAffinitySignals(req); !slices.Equal(signals, []sessionAffinitySignal{
		{Kind: sessionAffinitySignalSessionID, Value: "large-session"},
	}) {
		t.Fatalf("signals = %#v, want large-session", signals)
	}
	replayed, ok := req.Body.(*replayReadCloser)
	if !ok {
		t.Fatalf("replayed body type = %T, want *replayReadCloser", req.Body)
	}
	replayStore, ok := replayed.closers[1].(*sessionAffinityReplayStore)
	if !ok || replayStore.file == nil {
		t.Fatalf("large body replay store = %#v, want temporary-file spill", replayStore)
	}
	if replayStore.memory.Len() > sessionAffinityReplayMemoryBytes {
		t.Fatalf("replay memory = %d, want at most %d", replayStore.memory.Len(), sessionAffinityReplayMemoryBytes)
	}
	restored, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read restored large body: %v", err)
	}
	if string(restored) != body {
		t.Fatalf("restored large body length = %d, want %d", len(restored), len(body))
	}
	if err = req.Body.Close(); err != nil {
		t.Fatalf("close restored large body: %v", err)
	}
	if replayStore.file != nil || replayStore.path != "" {
		t.Fatalf("replay store remained open after close: file=%v path=%q", replayStore.file, replayStore.path)
	}
}

func TestExtractSessionAffinitySignalsPreservesBodyReadError(t *testing.T) {
	errBody := errors.New("injected body read error")
	body := &singleErrorReadCloser{
		prefix: []byte(`{"session_id":`),
		err:    errBody,
		suffix: []byte(`"session-1"}`),
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Body = body
	req.Header.Set("Content-Type", "application/json")

	if signals := extractSessionAffinitySignals(req); len(signals) != 0 {
		t.Fatalf("signals = %#v, want none after body read error", signals)
	}
	restoredPrefix, err := io.ReadAll(req.Body)
	if !errors.Is(err, errBody) {
		t.Fatalf("restored body error = %v, want %v", err, errBody)
	}
	if string(restoredPrefix) != string(body.prefix) {
		t.Fatalf("restored prefix = %q, want %q", string(restoredPrefix), string(body.prefix))
	}
}

func TestSessionAffinityDigestIsTenantScopedAndDoesNotContainRawValues(t *testing.T) {
	signal := sessionAffinitySignal{Kind: sessionAffinitySignalSessionID, Value: "raw-session-secret"}
	userDigest := digestSessionAffinitySignal("user:usr_1", signal)
	rotatedKeyDigest := digestSessionAffinitySignal("user:usr_1", signal)
	otherUserDigest := digestSessionAffinitySignal("user:usr_2", signal)
	compatDigest := digestSessionAffinitySignal("auth:account:acct_1", signal)

	if userDigest != rotatedKeyDigest {
		t.Fatalf("same user digest changed across API-key rotation: %q != %q", userDigest, rotatedKeyDigest)
	}
	if userDigest == otherUserDigest || userDigest == compatDigest || otherUserDigest == compatDigest {
		t.Fatalf("tenant-scoped digests collided: user=%q other=%q compatibility=%q", userDigest, otherUserDigest, compatDigest)
	}
	for _, digest := range []SessionAffinityDigest{userDigest, otherUserDigest, compatDigest} {
		if strings.Contains(string(digest), signal.Value) {
			t.Fatalf("digest %q contains raw session value", digest)
		}
	}
}

func TestUserStoreSessionAffinityAtomicFirstBindingAndAliases(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()
	signals := []sessionAffinitySignal{
		{Kind: sessionAffinitySignalPromptCacheKey, Value: "cache-secret"},
		{Kind: sessionAffinitySignalConversationID, Value: "conversation-secret"},
	}
	digests := digestSessionAffinitySignals("user:usr_1", signals)

	const requests = 32
	results := make(chan SessionAffinityBinding, requests)
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			candidate := "account:acct_a"
			if index%2 == 1 {
				candidate = "account:acct_b"
			}
			binding, err := store.BindSessionAffinity(ctx, digests, candidate)
			results <- binding
			errs <- err
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("BindSessionAffinity returned error: %v", err)
		}
	}
	var winner string
	for binding := range results {
		if winner == "" {
			winner = binding.AuthID
		}
		if binding.AuthID != winner {
			t.Fatalf("binding auth = %q, want atomic winner %q", binding.AuthID, winner)
		}
	}
	if winner == "" {
		t.Fatal("atomic binding winner is empty")
	}

	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM session_affinity_bindings`).Scan(&count); err != nil {
		t.Fatalf("count session affinity rows: %v", err)
	}
	if count != len(digests) {
		t.Fatalf("session affinity row count = %d, want %d aliases", count, len(digests))
	}
	for _, raw := range []string{"cache-secret", "conversation-secret", "usr_1"} {
		var matches int
		if err := store.db.QueryRow(
			`SELECT COUNT(*) FROM session_affinity_bindings
			 WHERE session_digest LIKE '%' || ? || '%' OR auth_id LIKE '%' || ? || '%'`,
			raw,
			raw,
		).Scan(&matches); err != nil {
			t.Fatalf("search session affinity rows for raw value: %v", err)
		}
		if matches != 0 {
			t.Fatalf("session affinity table contains raw value %q", raw)
		}
	}
}

func TestUserStoreSessionAffinityCASRebindConverges(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()
	digests := digestSessionAffinitySignals("user:usr_1", []sessionAffinitySignal{
		{Kind: sessionAffinitySignalSessionID, Value: "session-1"},
		{Kind: sessionAffinitySignalConversationID, Value: "conversation-1"},
	})
	initial, err := store.BindSessionAffinity(ctx, digests, "account:acct_a")
	if err != nil {
		t.Fatalf("BindSessionAffinity returned error: %v", err)
	}
	lookup, ok, err := store.LookupSessionAffinity(ctx, digests[1:])
	if err != nil {
		t.Fatalf("LookupSessionAffinity by alias returned error: %v", err)
	}
	if !ok || len(lookup.Digests) != 2 {
		t.Fatalf("alias lookup = %#v ok=%t, want complete two-digest group", lookup, ok)
	}
	initial = lookup

	start := make(chan struct{})
	results := make(chan SessionAffinityBinding, 2)
	errs := make(chan error, 2)
	for _, candidate := range []string{"account:acct_b", "account:acct_c"} {
		go func(candidate string) {
			<-start
			binding, errRebind := store.RebindSessionAffinity(ctx, initial, candidate)
			results <- binding
			errs <- errRebind
		}(candidate)
	}
	close(start)

	first := <-results
	second := <-results
	if err = <-errs; err != nil {
		t.Fatalf("first RebindSessionAffinity error: %v", err)
	}
	if err = <-errs; err != nil {
		t.Fatalf("second RebindSessionAffinity error: %v", err)
	}
	if first.AuthID == initial.AuthID || second.AuthID == initial.AuthID {
		t.Fatalf("CAS rebind stayed on initial auth: first=%q second=%q", first.AuthID, second.AuthID)
	}
	if first.AuthID != second.AuthID {
		t.Fatalf("CAS rebind did not converge: first=%q second=%q", first.AuthID, second.AuthID)
	}

	stored, ok, err := store.LookupSessionAffinity(ctx, digests)
	if err != nil {
		t.Fatalf("LookupSessionAffinity returned error: %v", err)
	}
	if !ok || stored.AuthID != first.AuthID {
		t.Fatalf("stored binding = %#v ok=%t, want auth %q", stored, ok, first.AuthID)
	}
	var distinctAuths int
	if err = store.db.QueryRow(
		`SELECT COUNT(DISTINCT auth_id)
		 FROM session_affinity_bindings WHERE binding_digest = ?`,
		stored.BindingDigest,
	).Scan(&distinctAuths); err != nil {
		t.Fatalf("count alias auths after CAS: %v", err)
	}
	if distinctAuths != 1 {
		t.Fatalf("alias group has %d auths after CAS, want 1", distinctAuths)
	}
}

func TestUserStoreSessionAffinityMergesExistingAliasGroups(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()
	sessionDigest := digestSessionAffinitySignals("user:usr_1", []sessionAffinitySignal{
		{Kind: sessionAffinitySignalSessionID, Value: "session-1"},
	})
	conversationDigest := digestSessionAffinitySignals("user:usr_1", []sessionAffinitySignal{
		{Kind: sessionAffinitySignalConversationID, Value: "conversation-1"},
	})
	if _, err := store.BindSessionAffinity(ctx, sessionDigest, "account:acct_a"); err != nil {
		t.Fatalf("bind session alias: %v", err)
	}
	if _, err := store.BindSessionAffinity(ctx, conversationDigest, "account:acct_a"); err != nil {
		t.Fatalf("bind conversation alias: %v", err)
	}

	combined := append(slices.Clone(sessionDigest), conversationDigest...)
	binding, ok, err := store.LookupSessionAffinity(ctx, combined)
	if err != nil {
		t.Fatalf("LookupSessionAffinity returned error: %v", err)
	}
	if !ok || len(binding.Digests) != 2 {
		t.Fatalf("merged binding = %#v ok=%t, want two aliases", binding, ok)
	}
	var bindingGroups int
	if err = store.db.QueryRow(
		`SELECT COUNT(DISTINCT binding_digest)
		 FROM session_affinity_bindings WHERE session_digest IN (?, ?)`,
		sessionDigest[0],
		conversationDigest[0],
	).Scan(&bindingGroups); err != nil {
		t.Fatalf("count merged binding groups: %v", err)
	}
	if bindingGroups != 1 {
		t.Fatalf("merged aliases use %d binding groups, want 1", bindingGroups)
	}
}

func TestUserStoreSessionAffinityRenewalExpiryCleanupAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "users.db")
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store, err := OpenUserStore(ctx, path)
	if err != nil {
		t.Fatalf("OpenUserStore returned error: %v", err)
	}
	store.now = func() time.Time { return now }
	digests := digestSessionAffinitySignals("user:usr_1", []sessionAffinitySignal{
		{Kind: sessionAffinitySignalSessionID, Value: "session-1"},
		{Kind: sessionAffinitySignalPromptCacheKey, Value: "cache-1"},
	})
	bound, err := store.BindSessionAffinity(ctx, digests, "account:acct_a")
	if err != nil {
		t.Fatalf("BindSessionAffinity returned error: %v", err)
	}
	createdAt, updatedAt, expiresAt := readSessionAffinityTimes(t, store, digests[0])
	if !updatedAt.Equal(now) || !expiresAt.Equal(now.Add(sessionAffinityExpiry)) {
		t.Fatalf("initial times = created %s updated %s expires %s", createdAt, updatedAt, expiresAt)
	}

	now = now.Add(sessionAffinityRenewalThreshold - time.Second)
	if _, ok, errLookup := store.LookupSessionAffinity(ctx, digests[:1]); errLookup != nil || !ok {
		t.Fatalf("pre-threshold lookup binding ok=%t error=%v", ok, errLookup)
	}
	_, unchangedUpdatedAt, unchangedExpiresAt := readSessionAffinityTimes(t, store, digests[0])
	if !unchangedUpdatedAt.Equal(updatedAt) || !unchangedExpiresAt.Equal(expiresAt) {
		t.Fatalf("pre-threshold lookup wrote renewal: updated=%s expires=%s", unchangedUpdatedAt, unchangedExpiresAt)
	}

	now = now.Add(time.Second)
	if renewed, ok, errLookup := store.LookupSessionAffinity(ctx, digests[:1]); errLookup != nil || !ok {
		t.Fatalf("threshold lookup binding ok=%t error=%v", ok, errLookup)
	} else if len(renewed.Digests) != 2 {
		t.Fatalf("renewed binding aliases = %d, want 2", len(renewed.Digests))
	}
	renewedCreatedAt, renewedUpdatedAt, renewedExpiresAt := readSessionAffinityTimes(t, store, digests[0])
	if !renewedCreatedAt.Equal(createdAt) {
		t.Fatalf("renewal changed created_at: got %s want %s", renewedCreatedAt, createdAt)
	}
	if !renewedUpdatedAt.Equal(now) || !renewedExpiresAt.Equal(now.Add(sessionAffinityExpiry)) {
		t.Fatalf("renewed times = updated %s expires %s, want %s and %s", renewedUpdatedAt, renewedExpiresAt, now, now.Add(sessionAffinityExpiry))
	}
	_, aliasUpdatedAt, aliasExpiresAt := readSessionAffinityTimes(t, store, digests[1])
	if !aliasUpdatedAt.Equal(now) || !aliasExpiresAt.Equal(now.Add(sessionAffinityExpiry)) {
		t.Fatalf("alias renewal times = updated %s expires %s, want group renewal", aliasUpdatedAt, aliasExpiresAt)
	}

	if err = store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	store, err = OpenUserStore(ctx, path)
	if err != nil {
		t.Fatalf("restart OpenUserStore returned error: %v", err)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Fatalf("close restarted store: %v", errClose)
		}
	})
	store.now = func() time.Time { return now }
	restarted, ok, err := store.LookupSessionAffinity(ctx, digests)
	if err != nil {
		t.Fatalf("restart LookupSessionAffinity returned error: %v", err)
	}
	if !ok || restarted.AuthID != bound.AuthID {
		t.Fatalf("restart binding = %#v ok=%t, want auth %q", restarted, ok, bound.AuthID)
	}

	expiredAt := formatDBTime(now.Add(-time.Second))
	for _, digest := range []string{"expired-1", "expired-2", "expired-3"} {
		if _, err = store.db.Exec(
			`INSERT INTO session_affinity_bindings
				(session_digest, binding_digest, auth_id, created_at, updated_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			digest,
			digest,
			"account:expired",
			expiredAt,
			expiredAt,
			expiredAt,
		); err != nil {
			t.Fatalf("insert expired binding: %v", err)
		}
	}
	deleted, err := store.CleanupExpiredSessionAffinity(ctx, 2)
	if err != nil {
		t.Fatalf("CleanupExpiredSessionAffinity returned error: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("cleanup deleted %d rows, want 2", deleted)
	}
	var remaining int
	if err = store.db.QueryRow(`SELECT COUNT(*) FROM session_affinity_bindings WHERE auth_id = 'account:expired'`).Scan(&remaining); err != nil {
		t.Fatalf("count remaining expired rows: %v", err)
	}
	if remaining != 1 {
		t.Fatalf("remaining expired rows = %d, want 1 after bounded cleanup", remaining)
	}
}

func TestSessionAffinityDatabaseNeverContainsRawSignalValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users.db")
	store, err := OpenUserStore(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenUserStore returned error: %v", err)
	}
	raw := "raw-session-value-that-must-not-be-persisted"
	digests := digestSessionAffinitySignals("user:usr_1", []sessionAffinitySignal{
		{Kind: sessionAffinitySignalSessionID, Value: raw},
	})
	if _, err = store.BindSessionAffinity(context.Background(), digests, "account:acct_a"); err != nil {
		t.Fatalf("BindSessionAffinity returned error: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	databaseBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database: %v", err)
	}
	if strings.Contains(string(databaseBytes), raw) {
		t.Fatalf("SQLite database contains raw session value %q", raw)
	}
}

func readSessionAffinityTimes(t *testing.T, store *UserStore, digest SessionAffinityDigest) (time.Time, time.Time, time.Time) {
	t.Helper()
	var createdRaw string
	var updatedRaw string
	var expiresRaw string
	if err := store.db.QueryRow(
		`SELECT created_at, updated_at, expires_at
		 FROM session_affinity_bindings WHERE session_digest = ?`,
		digest,
	).Scan(&createdRaw, &updatedRaw, &expiresRaw); err != nil {
		t.Fatalf("read session affinity times: %v", err)
	}
	createdAt, err := parseDBTime(createdRaw)
	if err != nil {
		t.Fatalf("parse created_at: %v", err)
	}
	updatedAt, err := parseDBTime(updatedRaw)
	if err != nil {
		t.Fatalf("parse updated_at: %v", err)
	}
	expiresAt, err := parseDBTime(expiresRaw)
	if err != nil {
		t.Fatalf("parse expires_at: %v", err)
	}
	return createdAt, updatedAt, expiresAt
}

func quotedJSON(value string) string {
	return `"` + value + `"`
}

func largeSessionAffinityJSON(sessionID string) string {
	var body strings.Builder
	body.WriteString(`{"input":[`)
	for i := 0; i < 4096; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		body.WriteString(`{"type":"input_text","text":"padding"}`)
	}
	body.WriteString(`],"session_id":`)
	body.WriteString(quotedJSON(sessionID))
	body.WriteByte('}')
	return body.String()
}

type singleErrorReadCloser struct {
	prefix []byte
	err    error
	suffix []byte
	stage  int
}

func (r *singleErrorReadCloser) Read(p []byte) (int, error) {
	switch r.stage {
	case 0:
		r.stage++
		return copy(p, r.prefix), r.err
	case 1:
		r.stage++
		return copy(p, r.suffix), io.EOF
	default:
		return 0, io.EOF
	}
}

func (*singleErrorReadCloser) Close() error {
	return nil
}
