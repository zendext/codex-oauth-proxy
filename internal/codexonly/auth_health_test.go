package codexonly

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAuthHealthRestorePreservesQuotaAcrossCredentialChanges(t *testing.T) {
	ctx := context.Background()
	store, err := OpenUserStore(ctx, filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatalf("OpenUserStore returned error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	retryAt := now.Add(20 * time.Minute)
	if err = store.SaveAuthHealthState(ctx, PersistedAuthHealthState{
		AuthID:                "account:acct_a",
		Kind:                  AuthHealthQuota,
		Reason:                "quota",
		RetryAt:               retryAt,
		CredentialFingerprint: "old-fingerprint",
		StatusCode:            429,
		ErrorCode:             "rate_limit_exceeded",
		UpdatedAt:             now,
	}); err != nil {
		t.Fatalf("SaveAuthHealthState returned error: %v", err)
	}

	registry := newAuthHealthRegistry(store)
	registry.now = func() time.Time { return now }
	auth := &Auth{
		ID:           "account:acct_a",
		AccountID:    "acct_a",
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
	}
	if err = registry.Restore(ctx, []*Auth{auth}); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}

	state, ok := registry.State(auth.ID, "")
	if !ok {
		t.Fatal("quota state was not restored")
	}
	if state.Kind != AuthHealthQuota || !state.RetryAt.Equal(retryAt) {
		t.Fatalf("restored state = %#v, want quota until %s", state, retryAt)
	}
}

func TestAuthHealthRestoreClearsCredentialStateAfterAuthMaterialChanges(t *testing.T) {
	ctx := context.Background()
	store, err := OpenUserStore(ctx, filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatalf("OpenUserStore returned error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	if err = store.SaveAuthHealthState(ctx, PersistedAuthHealthState{
		AuthID:                "account:acct_a",
		Kind:                  AuthHealthUnauthorized,
		Reason:                "unauthorized",
		CredentialFingerprint: "old-fingerprint",
		StatusCode:            401,
		ErrorCode:             "unauthorized",
		UpdatedAt:             now,
	}); err != nil {
		t.Fatalf("SaveAuthHealthState returned error: %v", err)
	}

	registry := newAuthHealthRegistry(store)
	registry.now = func() time.Time { return now }
	auth := &Auth{
		ID:           "account:acct_a",
		AccountID:    "acct_a",
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
	}
	if err = registry.Restore(ctx, []*Auth{auth}); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}

	if state, ok := registry.State(auth.ID, ""); ok {
		t.Fatalf("credential state survived changed auth material: %#v", state)
	}
	rows, err := store.LoadAuthHealthStates(ctx)
	if err != nil {
		t.Fatalf("LoadAuthHealthStates returned error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("persisted health rows = %#v, want none", rows)
	}
}

func TestAuthHealthPersistsOnlyAuthoritativeStates(t *testing.T) {
	ctx := context.Background()
	store, err := OpenUserStore(ctx, filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatalf("OpenUserStore returned error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	registry := newAuthHealthRegistry(store)
	registry.now = func() time.Time { return now }
	auth := &Auth{
		ID:           "account:acct_a",
		AccountID:    "acct_a",
		AccessToken:  "access",
		RefreshToken: "refresh",
	}

	if err = registry.MarkUnavailable(ctx, auth, AuthHealthState{
		Kind:       AuthHealthTransient,
		Reason:     "network",
		RetryAt:    now.Add(time.Minute),
		StatusCode: 502,
	}); err != nil {
		t.Fatalf("MarkUnavailable returned error: %v", err)
	}
	rows, err := store.LoadAuthHealthStates(ctx)
	if err != nil {
		t.Fatalf("LoadAuthHealthStates returned error: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("transient cooldown persisted rows = %#v, want none", rows)
	}

	if err = registry.MarkUnavailable(ctx, auth, AuthHealthState{
		Kind:       AuthHealthInvalidGrant,
		Reason:     "invalid_grant",
		StatusCode: 400,
		ErrorCode:  "invalid_grant",
	}); err != nil {
		t.Fatalf("MarkUnavailable invalid_grant returned error: %v", err)
	}
	rows, err = store.LoadAuthHealthStates(ctx)
	if err != nil {
		t.Fatalf("LoadAuthHealthStates returned error: %v", err)
	}
	if len(rows) != 1 || rows[0].Kind != AuthHealthInvalidGrant {
		t.Fatalf("persisted health rows = %#v, want one invalid_grant row", rows)
	}
	if rows[0].CredentialFingerprint != authCredentialFingerprint(auth) {
		t.Fatalf("credential fingerprint = %q, want current auth fingerprint", rows[0].CredentialFingerprint)
	}
}

func TestAuthHealthSuccessfulRequestPreservesAuthoritativeQuotaDeadline(t *testing.T) {
	ctx := context.Background()
	store, err := OpenUserStore(ctx, filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatalf("OpenUserStore returned error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	registry := newAuthHealthRegistry(store)
	registry.now = func() time.Time { return now }
	auth := &Auth{
		ID:           "account:acct_a",
		AccountID:    "acct_a",
		AccessToken:  "access",
		RefreshToken: "refresh",
	}
	retryAt := now.Add(20 * time.Minute)
	if err = registry.MarkUnavailable(ctx, auth, AuthHealthState{
		Kind:          AuthHealthQuota,
		Reason:        "quota",
		RetryAt:       retryAt,
		Authoritative: true,
		StatusCode:    http.StatusTooManyRequests,
	}); err != nil {
		t.Fatalf("MarkUnavailable returned error: %v", err)
	}
	if err = registry.MarkHealthy(ctx, auth, "gpt-test"); err != nil {
		t.Fatalf("MarkHealthy returned error: %v", err)
	}

	state, blocked := registry.Blocked(auth, "gpt-test")
	if !blocked || state.Kind != AuthHealthQuota || !state.RetryAt.Equal(retryAt) {
		t.Fatalf("health state = %#v, blocked=%t, want authoritative quota until %s", state, blocked, retryAt)
	}
	rows, err := store.LoadAuthHealthStates(ctx)
	if err != nil {
		t.Fatalf("LoadAuthHealthStates returned error: %v", err)
	}
	if len(rows) != 1 || rows[0].Kind != AuthHealthQuota || !rows[0].RetryAt.Equal(retryAt) {
		t.Fatalf("persisted health rows = %#v, want quota until %s", rows, retryAt)
	}
}

func TestAuthHealthTransientFailureDoesNotDowngradePersistedState(t *testing.T) {
	tests := []struct {
		name  string
		state AuthHealthState
	}{
		{
			name: "authoritative quota",
			state: AuthHealthState{
				Kind:          AuthHealthQuota,
				Reason:        "quota",
				RetryAt:       time.Date(2026, 7, 31, 11, 0, 0, 0, time.UTC),
				Authoritative: true,
				StatusCode:    http.StatusTooManyRequests,
			},
		},
		{
			name: "invalid grant",
			state: AuthHealthState{
				Kind:       AuthHealthInvalidGrant,
				Reason:     "invalid_grant",
				StatusCode: http.StatusBadRequest,
				ErrorCode:  "invalid_grant",
			},
		},
		{
			name: "continued unauthorized",
			state: AuthHealthState{
				Kind:       AuthHealthUnauthorized,
				Reason:     "unauthorized",
				StatusCode: http.StatusUnauthorized,
				ErrorCode:  "unauthorized",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := OpenUserStore(ctx, filepath.Join(t.TempDir(), "users.db"))
			if err != nil {
				t.Fatalf("OpenUserStore returned error: %v", err)
			}
			t.Cleanup(func() { _ = store.Close() })

			now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
			registry := newAuthHealthRegistry(store)
			registry.now = func() time.Time { return now }
			auth := &Auth{
				ID:           "account:acct_a",
				AccountID:    "acct_a",
				AccessToken:  "access",
				RefreshToken: "refresh",
			}
			if err = registry.MarkUnavailable(ctx, auth, test.state); err != nil {
				t.Fatalf("MarkUnavailable persisted state: %v", err)
			}
			weakerStates := []AuthHealthState{
				{
					Kind:       AuthHealthTransient,
					Reason:     "network",
					RetryAt:    now.Add(time.Minute),
					StatusCode: http.StatusBadGateway,
				},
				{
					Kind:       AuthHealthCredentialInvalid,
					Reason:     "credential_invalid",
					StatusCode: http.StatusServiceUnavailable,
				},
			}
			if test.state.Kind == AuthHealthQuota {
				weakerStates = append(weakerStates, AuthHealthState{
					Kind:       AuthHealthQuota,
					Reason:     "quota",
					RetryAt:    now.Add(time.Minute),
					StatusCode: http.StatusTooManyRequests,
				})
			}
			for _, weaker := range weakerStates {
				if err = registry.MarkUnavailable(ctx, auth, weaker); err != nil {
					t.Fatalf("MarkUnavailable weaker %s state: %v", weaker.Kind, err)
				}
			}

			state, ok := registry.State(auth.ID, "")
			if !ok || state.Kind != test.state.Kind {
				t.Fatalf("health state = %#v, ok=%t, want preserved %s", state, ok, test.state.Kind)
			}
			rows, err := store.LoadAuthHealthStates(ctx)
			if err != nil {
				t.Fatalf("LoadAuthHealthStates returned error: %v", err)
			}
			if len(rows) != 1 || rows[0].Kind != test.state.Kind {
				t.Fatalf("persisted health rows = %#v, want preserved %s", rows, test.state.Kind)
			}
		})
	}
}

func TestAuthHealthConcurrentTransitionsKeepPersistenceInSync(t *testing.T) {
	ctx := context.Background()
	store, err := OpenUserStore(ctx, filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatalf("OpenUserStore returned error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	registry := newAuthHealthRegistry(store)
	auth := &Auth{
		ID:           "account:acct_a",
		AccountID:    "acct_a",
		AccessToken:  "access",
		RefreshToken: "refresh",
	}
	for range 100 {
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			errs <- registry.MarkUnavailable(ctx, auth, AuthHealthState{
				Kind:       AuthHealthUnauthorized,
				Reason:     "unauthorized",
				StatusCode: http.StatusUnauthorized,
				ErrorCode:  "unauthorized",
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			errs <- registry.MarkHealthy(ctx, auth, "")
		}()
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("concurrent transition returned error: %v", err)
			}
		}

		state, ok := registry.State(auth.ID, "")
		rows, err := store.LoadAuthHealthStates(ctx)
		if err != nil {
			t.Fatalf("LoadAuthHealthStates returned error: %v", err)
		}
		if ok {
			if state.Kind != AuthHealthUnauthorized || len(rows) != 1 || rows[0].Kind != AuthHealthUnauthorized {
				t.Fatalf("memory/persistence mismatch: state=%#v rows=%#v", state, rows)
			}
		} else if len(rows) != 0 {
			t.Fatalf("memory cleared with persisted rows remaining: %#v", rows)
		}
	}
}

func TestNormalizeModelIdentifier(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  string
		ok    bool
	}{
		{name: "normalizes surrounding space", model: "  gpt-5.3-codex  ", want: "gpt-5.3-codex", ok: true},
		{name: "allows codex separators", model: "openai/gpt-5.3:codex_preview", want: "openai/gpt-5.3:codex_preview", ok: true},
		{name: "rejects empty", model: "  ", ok: false},
		{name: "rejects control", model: "gpt-5\nsecret", ok: false},
		{name: "rejects space inside", model: "gpt 5", ok: false},
		{name: "rejects oversized", model: strings.Repeat("m", maxModelIdentifierBytes+1), ok: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := normalizeModelIdentifier(test.model)
			if ok != test.ok || got != test.want {
				t.Fatalf("normalizeModelIdentifier(%q) = %q, %t, want %q, %t", test.model, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestAuthHealthRejectsUnsafeModelExclusionsWithoutLoggingThem(t *testing.T) {
	ctx := context.Background()
	store, err := OpenUserStore(ctx, filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatalf("OpenUserStore returned error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	registry := newAuthHealthRegistry(store)
	auth := &Auth{
		ID:           "account:acct_a",
		AccountID:    "acct_a",
		AccessToken:  "access",
		RefreshToken: "refresh",
	}
	var logs bytes.Buffer
	restore := captureStandardLogger(t, &logs)
	defer restore()

	for _, model := range []string{
		"gpt-5\nsecret-log-line",
		strings.Repeat("m", maxModelIdentifierBytes+1),
	} {
		err = registry.MarkModelUnsupported(ctx, auth, model, upstreamFailure{
			Kind:       upstreamFailureModelUnsupported,
			StatusCode: http.StatusBadRequest,
			ErrorCode:  "model_not_supported",
		})
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("MarkModelUnsupported(%q) error = %v, want ErrInvalidInput", model, err)
		}
	}
	if len(registry.modelExclusions) != 0 {
		t.Fatalf("unsafe models were cached: %#v", registry.modelExclusions)
	}
	if logs.Len() != 0 {
		t.Fatalf("unsafe models were logged:\n%s", logs.String())
	}
}

func TestAuthHealthModelExclusionsAreBoundedWithDeterministicEviction(t *testing.T) {
	ctx := context.Background()
	store, err := OpenUserStore(ctx, filepath.Join(t.TempDir(), "users.db"))
	if err != nil {
		t.Fatalf("OpenUserStore returned error: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	registry := newAuthHealthRegistry(store)
	registry.now = func() time.Time { return now }
	auth := &Auth{
		ID:           "account:acct_a",
		AccountID:    "acct_a",
		AccessToken:  "access",
		RefreshToken: "refresh",
	}
	var logs bytes.Buffer
	restore := captureStandardLogger(t, &logs)
	defer restore()

	total := maxModelExclusionsPerAuth + 10
	for index := range total {
		model := fmt.Sprintf("model-%03d", index)
		if err = registry.MarkModelUnsupported(ctx, auth, model, upstreamFailure{
			Kind:       upstreamFailureModelUnsupported,
			StatusCode: http.StatusBadRequest,
			ErrorCode:  "model_not_supported",
		}); err != nil {
			t.Fatalf("MarkModelUnsupported(%s): %v", model, err)
		}
		now = now.Add(time.Second)
	}

	registry.mu.RLock()
	exclusions := registry.modelExclusions[auth.ID]
	count := len(exclusions)
	registry.mu.RUnlock()
	if count != maxModelExclusionsPerAuth {
		t.Fatalf("model exclusion count = %d, want %d", count, maxModelExclusionsPerAuth)
	}
	if _, ok := registry.State(auth.ID, "model-000"); ok {
		t.Fatal("oldest model exclusion was not evicted")
	}
	latest := fmt.Sprintf("model-%03d", total-1)
	state, ok := registry.State(auth.ID, latest)
	if !ok || state.Kind != AuthHealthModelUnsupported {
		t.Fatalf("latest model state = %#v, ok=%t, want model exclusion", state, ok)
	}
}
