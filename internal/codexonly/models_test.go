package codexonly

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeClientVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
		want    string
		ok      bool
	}{
		{name: "stable", version: "0.146.0", want: "0.146.0", ok: true},
		{name: "trimmed", version: " 0.147.0-alpha.2 ", want: "0.147.0-alpha.2", ok: true},
		{name: "build metadata", version: "0.146.0+release.1", want: "0.146.0+release.1", ok: true},
		{name: "empty", version: "", ok: false},
		{name: "control", version: "0.146.0\nnext", ok: false},
		{name: "space", version: "0.146.0 next", ok: false},
		{name: "query delimiter", version: "0.146.0&next=1", ok: false},
		{name: "oversized", version: strings.Repeat("1", maxClientVersionBytes+1), ok: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := normalizeClientVersion(test.version)
			if got != test.want || ok != test.ok {
				t.Fatalf("normalizeClientVersion(%q) = %q, %t, want %q, %t", test.version, got, ok, test.want, test.ok)
			}
		})
	}
}

func TestRuntimeModelCatalogColdFanoutAndConcurrentMissCoalescing(t *testing.T) {
	t.Parallel()

	auths := []*Auth{
		{ID: "account:acct_a", AccountID: "acct_a"},
		{ID: "account:acct_b", AccountID: "acct_b"},
	}
	started := make(chan string, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	catalog := newRuntimeModelCatalog(context.Background(), func(_ context.Context, auth *Auth, version string) (modelCatalogFetchResult, error) {
		calls.Add(1)
		started <- auth.ID
		<-release
		return modelCatalogFetchResult{
			Models: []map[string]any{{
				"slug":           "model-" + auth.AccountID,
				"display_name":   auth.AccountID,
				"client_version": version,
			}},
		}, nil
	}, nil)
	t.Cleanup(catalog.Close)

	const requestCount = 12
	var ready sync.WaitGroup
	ready.Add(requestCount)
	begin := make(chan struct{})
	results := make(chan modelCatalogView, requestCount)
	errs := make(chan error, requestCount)
	for range requestCount {
		go func() {
			ready.Done()
			<-begin
			view, err := catalog.Catalog(context.Background(), "0.146.0", auths)
			results <- view
			errs <- err
		}()
	}
	ready.Wait()
	close(begin)

	first := <-started
	second := <-started
	if first == second {
		t.Fatalf("foreground fan-out started the same auth twice: %q", first)
	}
	if got := calls.Load(); got != int32(len(auths)) {
		t.Fatalf("fetch calls before release = %d, want %d", got, len(auths))
	}
	close(release)

	for range requestCount {
		view := <-results
		if err := <-errs; err != nil {
			t.Fatalf("Catalog returned error: %v", err)
		}
		if got := modelSlugs(view.Models); !slices.Equal(got, []string{"model-acct_a", "model-acct_b"}) {
			t.Fatalf("catalog slugs = %v, want both auth contributions", got)
		}
	}
	if got := calls.Load(); got != int32(len(auths)) {
		t.Fatalf("coalesced fetch calls = %d, want %d", got, len(auths))
	}
}

func TestRuntimeModelCatalogTTLAndVersionLRU(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	catalog := newRuntimeModelCatalog(context.Background(), func(_ context.Context, auth *Auth, version string) (modelCatalogFetchResult, error) {
		calls.Add(1)
		return modelCatalogFetchResult{
			Models: []map[string]any{{
				"slug":         "model-" + version,
				"display_name": auth.ID,
			}},
		}, nil
	}, nil)
	catalog.now = func() time.Time { return now }
	t.Cleanup(catalog.Close)
	auths := []*Auth{{ID: "account:acct_a"}}

	if _, err := catalog.Catalog(context.Background(), "0.1.0", auths); err != nil {
		t.Fatalf("initial Catalog returned error: %v", err)
	}
	now = now.Add(modelCatalogTTL - time.Second)
	if _, err := catalog.Catalog(context.Background(), "0.1.0", auths); err != nil {
		t.Fatalf("fresh Catalog returned error: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fresh cache fetch calls = %d, want 1", got)
	}
	now = now.Add(2 * time.Second)
	if _, err := catalog.Catalog(context.Background(), "0.1.0", auths); err != nil {
		t.Fatalf("expired Catalog returned error: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expired cache fetch calls = %d, want 2", got)
	}

	for index := 2; index <= maxModelCatalogVersions+1; index++ {
		version := fmt.Sprintf("0.%d.0", index)
		if _, err := catalog.Catalog(context.Background(), version, auths); err != nil {
			t.Fatalf("Catalog(%s) returned error: %v", version, err)
		}
	}

	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if got := len(catalog.versions); got != maxModelCatalogVersions {
		t.Fatalf("cached versions = %d, want %d", got, maxModelCatalogVersions)
	}
	if _, ok := catalog.versions["0.1.0"]; ok {
		t.Fatal("least-recently-used version 0.1.0 was not evicted")
	}
	if _, ok := catalog.versions[fmt.Sprintf("0.%d.0", maxModelCatalogVersions+1)]; !ok {
		t.Fatal("most recently used version was evicted")
	}
}

func TestRuntimeModelCatalogAggregatesDeterministicCompleteModelsAndSupportSets(t *testing.T) {
	t.Parallel()

	fetch := func(_ context.Context, auth *Auth, _ string) (modelCatalogFetchResult, error) {
		switch auth.ID {
		case "account:acct_a":
			return modelCatalogFetchResult{Models: []map[string]any{
				{
					"slug":         "shared",
					"display_name": "canonical-a",
					"a_only":       true,
					"priority":     2,
				},
				{
					"slug":         "only-a",
					"display_name": "only-a",
					"priority":     3,
				},
			}}, nil
		case "account:acct_b":
			return modelCatalogFetchResult{Models: []map[string]any{
				{
					"slug":         "shared",
					"display_name": "candidate-b",
					"b_only":       true,
					"priority":     1,
				},
				{
					"slug":         "only-b",
					"display_name": "only-b",
					"priority":     0,
				},
			}}, nil
		default:
			return modelCatalogFetchResult{}, fmt.Errorf("unexpected auth %q", auth.ID)
		}
	}

	firstCatalog := newRuntimeModelCatalog(context.Background(), fetch, nil)
	t.Cleanup(firstCatalog.Close)
	first, err := firstCatalog.Catalog(context.Background(), "0.146.0", []*Auth{
		{ID: "account:acct_b"},
		{ID: "account:acct_a"},
	})
	if err != nil {
		t.Fatalf("first Catalog returned error: %v", err)
	}

	secondCatalog := newRuntimeModelCatalog(context.Background(), fetch, nil)
	t.Cleanup(secondCatalog.Close)
	second, err := secondCatalog.Catalog(context.Background(), "0.146.0", []*Auth{
		{ID: "account:acct_a"},
		{ID: "account:acct_b"},
	})
	if err != nil {
		t.Fatalf("second Catalog returned error: %v", err)
	}

	if fmt.Sprint(first.Models) != fmt.Sprint(second.Models) {
		t.Fatalf("aggregation depends on auth order:\nfirst=%#v\nsecond=%#v", first.Models, second.Models)
	}
	if got := modelSlugs(first.Models); !slices.Equal(got, []string{"only-b", "shared", "only-a"}) {
		t.Fatalf("deterministic model order = %v, want priority then slug order", got)
	}
	shared := findCodexClientModel(first.Models, "shared")
	if shared["display_name"] != "canonical-a" || shared["a_only"] != true {
		t.Fatalf("shared canonical object = %#v, want complete object from account:acct_a", shared)
	}
	if _, ok := shared["b_only"]; ok {
		t.Fatalf("shared canonical object merged fields from another auth: %#v", shared)
	}
	if got := first.SupportingAuthIDs("shared"); !slices.Equal(got, []string{"account:acct_a", "account:acct_b"}) {
		t.Fatalf("shared supporting auths = %v, want sorted complete support set", got)
	}
	if !first.CapabilityKnown {
		t.Fatal("CapabilityKnown = false, want true with usable per-auth snapshots")
	}
}

func TestRuntimeModelCatalogStaleFallbackBackgroundRetryBudgetAndHealthyReopen(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	var healthyGeneration atomic.Uint64
	var waitsMu sync.Mutex
	var waits []time.Duration
	catalog := newRuntimeModelCatalog(context.Background(), func(_ context.Context, _ *Auth, _ string) (modelCatalogFetchResult, error) {
		call := calls.Add(1)
		switch call {
		case 1:
			return modelCatalogFetchResult{
				Models: []map[string]any{{"slug": "stale-model", "display_name": "stale"}},
			}, nil
		case 6:
			return modelCatalogFetchResult{
				Models: []map[string]any{{"slug": "fresh-model", "display_name": "fresh"}},
			}, nil
		default:
			return modelCatalogFetchResult{}, &modelCatalogFetchError{
				RetryAt: now.Add(10 * time.Second),
				Err:     errors.New("temporary model fetch failure"),
			}
		}
	}, func(string) uint64 {
		return healthyGeneration.Load()
	})
	catalog.now = func() time.Time { return now }
	catalog.waitRetry = func(_ context.Context, delay time.Duration) error {
		waitsMu.Lock()
		waits = append(waits, delay)
		waitsMu.Unlock()
		return nil
	}
	catalog.retryJitter = func(time.Duration) time.Duration { return 0 }
	t.Cleanup(catalog.Close)
	auths := []*Auth{{ID: "account:acct_a"}}

	if _, err := catalog.Catalog(context.Background(), "0.146.0", auths); err != nil {
		t.Fatalf("initial Catalog returned error: %v", err)
	}
	now = now.Add(modelCatalogTTL + time.Second)
	stale, err := catalog.Catalog(context.Background(), "0.146.0", auths)
	if err != nil {
		t.Fatalf("stale Catalog returned error: %v", err)
	}
	if got := modelSlugs(stale.Models); !slices.Equal(got, []string{"stale-model"}) {
		t.Fatalf("stale fallback models = %v, want stale-model", got)
	}
	waitForModelCatalogCondition(t, func() bool {
		return calls.Load() == 2+modelCatalogBackgroundRetries
	})
	if _, err = catalog.Catalog(context.Background(), "0.146.0", auths); err != nil {
		t.Fatalf("exhausted Catalog returned error: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 2+modelCatalogBackgroundRetries {
		t.Fatalf("repeated request multiplied exhausted background work: calls=%d", got)
	}

	waitsMu.Lock()
	gotWaits := slices.Clone(waits)
	waitsMu.Unlock()
	if len(gotWaits) != modelCatalogBackgroundRetries {
		t.Fatalf("background waits = %v, want %d waits", gotWaits, modelCatalogBackgroundRetries)
	}
	for _, delay := range gotWaits {
		if delay < 10*time.Second {
			t.Fatalf("Retry-After wait = %s, want at least 10s", delay)
		}
	}

	healthyGeneration.Add(1)
	fresh, err := catalog.Catalog(context.Background(), "0.146.0", auths)
	if err != nil {
		t.Fatalf("healthy-reopened Catalog returned error: %v", err)
	}
	if got := modelSlugs(fresh.Models); !slices.Equal(got, []string{"fresh-model"}) {
		t.Fatalf("healthy-reopened models = %v, want fresh-model", got)
	}
	if got := calls.Load(); got != 6 {
		t.Fatalf("healthy reopen fetch calls = %d, want 6", got)
	}
}

func TestRuntimeModelCatalogReopensExhaustedCycleForNextPeriodAndNewVersion(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	catalog := newRuntimeModelCatalog(context.Background(), func(_ context.Context, _ *Auth, _ string) (modelCatalogFetchResult, error) {
		calls.Add(1)
		return modelCatalogFetchResult{}, errors.New("unavailable")
	}, nil)
	catalog.now = func() time.Time { return now }
	catalog.waitRetry = func(context.Context, time.Duration) error { return nil }
	catalog.retryJitter = func(time.Duration) time.Duration { return 0 }
	t.Cleanup(catalog.Close)
	auths := []*Auth{{ID: "account:acct_a"}}

	first, err := catalog.Catalog(context.Background(), "0.146.0", auths)
	if err != nil {
		t.Fatalf("first Catalog returned error: %v", err)
	}
	if !first.EmbeddedFallback {
		t.Fatal("first all-auth failure did not use embedded fallback")
	}
	waitForModelCatalogCondition(t, func() bool {
		return calls.Load() == 1+modelCatalogBackgroundRetries
	})
	if _, err = catalog.Catalog(context.Background(), "0.146.0", auths); err != nil {
		t.Fatalf("same-period Catalog returned error: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 1+modelCatalogBackgroundRetries {
		t.Fatalf("same period reopened exhausted cycle: calls=%d", got)
	}

	now = now.Add(modelCatalogTTL)
	if _, err = catalog.Catalog(context.Background(), "0.146.0", auths); err != nil {
		t.Fatalf("next-period Catalog returned error: %v", err)
	}
	waitForModelCatalogCondition(t, func() bool {
		return calls.Load() == 2*(1+modelCatalogBackgroundRetries)
	})

	if _, err = catalog.Catalog(context.Background(), "0.147.0", auths); err != nil {
		t.Fatalf("new-version Catalog returned error: %v", err)
	}
	waitForModelCatalogCondition(t, func() bool {
		return calls.Load() == 3*(1+modelCatalogBackgroundRetries)
	})
}

func modelSlugs(models []map[string]any) []string {
	slugs := make([]string, 0, len(models))
	for _, model := range models {
		slug, _ := model["slug"].(string)
		slugs = append(slugs, slug)
	}
	return slugs
}

func waitForModelCatalogCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for model catalog condition")
}
