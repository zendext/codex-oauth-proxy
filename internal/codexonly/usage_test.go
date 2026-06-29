package codexonly

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestUsageConfigDefaultsAndDisable(t *testing.T) {
	cfg := &Config{}
	ApplyDefaults(cfg)

	if !usageTrackingEnabled(cfg) {
		t.Fatal("usage tracking default = false, want true")
	}

	disabled := false
	cfg.Usage.Enabled = &disabled
	if usageTrackingEnabled(cfg) {
		t.Fatal("usage tracking explicit false = true, want false")
	}
}

func TestExtractUsageCountersFromJSONAndSSE(t *testing.T) {
	jsonPayload := []byte(`{
		"id":"resp_1",
		"usage":{
			"input_tokens":21,
			"input_tokens_details":{"cached_tokens":5},
			"output_tokens":13,
			"output_tokens_details":{"reasoning_tokens":8},
			"total_tokens":34
		}
	}`)

	counters, ok := extractUsageCounters(jsonPayload)
	if !ok {
		t.Fatal("extract JSON usage returned ok=false")
	}
	assertUsageCounters(t, counters, UsageCounters{
		InputTokens:       21,
		OutputTokens:      13,
		ReasoningTokens:   8,
		CachedInputTokens: 5,
		TotalTokens:       34,
	})

	ssePayload := []byte(strings.Join([]string{
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"usage":{"prompt_tokens":10,"completion_tokens":4,"completion_tokens_details":{"reasoning_tokens":2},"prompt_tokens_details":{"cached_tokens":3},"total_tokens":14}}}`,
		"",
	}, "\n"))

	counters, ok = extractUsageCounters(ssePayload)
	if !ok {
		t.Fatal("extract SSE usage returned ok=false")
	}
	assertUsageCounters(t, counters, UsageCounters{
		InputTokens:       10,
		OutputTokens:      4,
		ReasoningTokens:   2,
		CachedInputTokens: 3,
		TotalTokens:       14,
	})
}

func TestUserStoreRecordsUsageBucketsAndWindows(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()
	fixed := time.Date(2026, 6, 12, 10, 7, 0, 0, time.UTC)
	store.now = func() time.Time { return fixed }

	created, err := store.CreateUser(ctx, CreateUserParams{Name: "Alice"})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	credential, err := store.AuthenticateAPIKey(ctx, created.PlaintextAPIKey)
	if err != nil {
		t.Fatalf("AuthenticateAPIKey returned error: %v", err)
	}

	enabled := true
	cfg := UsageConfig{
		Enabled:             &enabled,
		DebugOpenAIResponse: false,
	}

	for i, tokens := range []int64{40, 20, 10} {
		params := UsageRecordParams{
			Timestamp:  fixed.Add(time.Duration(i) * time.Minute),
			User:       credential.User,
			APIKey:     credential.APIKey,
			Model:      "gpt-5.3-codex",
			AuthID:     "auth.json",
			RequestID:  fmt.Sprintf("req_%d", i+1),
			StatusCode: http.StatusOK,
			Counters: UsageCounters{
				InputTokens:  tokens / 2,
				OutputTokens: tokens / 2,
				TotalTokens:  tokens,
			},
		}
		if err = store.RecordUsage(ctx, params, cfg); err != nil {
			t.Fatalf("RecordUsage #%d returned error: %v", i+1, err)
		}
	}

	today, err := store.GetTodayUsage(ctx, credential.User.ID, credential.APIKey.ID, fixed)
	if err != nil {
		t.Fatalf("GetTodayUsage returned error: %v", err)
	}
	if today.Date != "2026-06-12" {
		t.Fatalf("today date = %q, want 2026-06-12", today.Date)
	}
	assertUsageCounters(t, today.UsageCounters, UsageCounters{
		RequestCount: 3,
		InputTokens:  35,
		OutputTokens: 35,
		TotalTokens:  70,
	})

	snapshot, err := store.GetUsageSnapshot(ctx, UsageSnapshotFilter{}, fixed, cfg)
	if err != nil {
		t.Fatalf("GetUsageSnapshot returned error: %v", err)
	}
	if len(snapshot) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(snapshot))
	}
	entry := snapshot[0]
	if entry.UserID != credential.User.ID || entry.APIKeyID != credential.APIKey.ID {
		t.Fatalf("snapshot identity = %s/%s, want %s/%s", entry.UserID, entry.APIKeyID, credential.User.ID, credential.APIKey.ID)
	}
	fiveHour := entry.Windows["5h"]
	assertUsageCounters(t, fiveHour.UsageCounters, UsageCounters{
		RequestCount: 3,
		InputTokens:  35,
		OutputTokens: 35,
		TotalTokens:  70,
	})
}

func TestUserStorePrunesUsageBucketsAfterThirtyDays(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()
	fixed := time.Date(2026, 6, 12, 10, 7, 0, 0, time.UTC)
	store.now = func() time.Time { return fixed }

	created, err := store.CreateUser(ctx, CreateUserParams{Name: "Alice"})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	credential, err := store.AuthenticateAPIKey(ctx, created.PlaintextAPIKey)
	if err != nil {
		t.Fatalf("AuthenticateAPIKey returned error: %v", err)
	}

	enabled := true
	cfg := UsageConfig{Enabled: &enabled}
	record := func(timestamp time.Time, requestID string, totalTokens int64) {
		t.Helper()
		errRecord := store.RecordUsage(ctx, UsageRecordParams{
			Timestamp:  timestamp,
			User:       credential.User,
			APIKey:     credential.APIKey,
			Model:      "gpt-5.3-codex",
			AuthID:     "auth.json",
			RequestID:  requestID,
			StatusCode: http.StatusOK,
			Counters: UsageCounters{
				TotalTokens: totalTokens,
			},
		}, cfg)
		if errRecord != nil {
			t.Fatalf("RecordUsage %s returned error: %v", requestID, errRecord)
		}
	}

	record(fixed.Add(-29*24*time.Hour), "req_within_retention", 29)
	record(fixed.Add(-31*24*time.Hour), "req_outside_retention", 31)
	record(fixed, "req_current", 1)

	var bucketCount int
	var totalTokens int64
	if err = store.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(total_tokens), 0) FROM usage_buckets`).Scan(&bucketCount, &totalTokens); err != nil {
		t.Fatalf("query usage buckets returned error: %v", err)
	}
	if bucketCount != 2 || totalTokens != 30 {
		t.Fatalf("usage buckets count/tokens = %d/%d, want 2/30", bucketCount, totalTokens)
	}
}

func TestUserStoreUsageTimeseriesGroupsTenMinuteBucketsByUser(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 12, 10, 45, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	alice := createUsageTestCredential(t, store, "Alice")
	bob := createUsageTestCredential(t, store, "Bob")

	records := []struct {
		timestamp  time.Time
		credential AuthenticatedAPIKey
		requestID  string
		input      int64
		output     int64
		total      int64
	}{
		{
			timestamp:  time.Date(2026, 6, 12, 10, 7, 0, 0, time.UTC),
			credential: alice,
			requestID:  "req_alice_1",
			input:      12,
			output:     8,
			total:      20,
		},
		{
			timestamp:  time.Date(2026, 6, 12, 10, 12, 0, 0, time.UTC),
			credential: alice,
			requestID:  "req_alice_2",
			input:      5,
			output:     7,
			total:      12,
		},
		{
			timestamp:  time.Date(2026, 6, 12, 10, 13, 0, 0, time.UTC),
			credential: bob,
			requestID:  "req_bob_1",
			input:      30,
			output:     10,
			total:      40,
		},
	}
	for _, record := range records {
		if err := store.RecordUsage(ctx, UsageRecordParams{
			Timestamp:       record.timestamp,
			User:            record.credential.User,
			APIKey:          record.credential.APIKey,
			Model:           "gpt-5.3-codex",
			ReasoningEffort: "high",
			ServiceTier:     "standard",
			AuthID:          "auth.json",
			RequestID:       record.requestID,
			StatusCode:      http.StatusOK,
			Counters: UsageCounters{
				InputTokens:  record.input,
				OutputTokens: record.output,
				TotalTokens:  record.total,
			},
		}, UsageConfig{}); err != nil {
			t.Fatalf("RecordUsage %s returned error: %v", record.requestID, err)
		}
	}

	timeseries, err := store.GetUsageTimeseries(ctx, UsageTimeseriesParams{
		Window:  "5h",
		Step:    "10m",
		GroupBy: []string{"user"},
		Now:     now,
	}, UsageConfig{})
	if err != nil {
		t.Fatalf("GetUsageTimeseries returned error: %v", err)
	}
	if timeseries.Window != "5h" || timeseries.Step != "10m" {
		t.Fatalf("timeseries window/step = %s/%s, want 5h/10m", timeseries.Window, timeseries.Step)
	}
	if got := strings.Join(timeseries.GroupBy, ","); got != "user" {
		t.Fatalf("group_by = %q, want user", got)
	}
	if len(timeseries.Series) != 3 {
		t.Fatalf("series count = %d, want 3: %#v", len(timeseries.Series), timeseries.Series)
	}

	assertUsageTimeseriesPoint(t, timeseries.Series[0], UsageTimeseriesPoint{
		BucketStart: time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC),
		UserID:      alice.User.ID,
		Name:        "Alice",
		UsageCounters: UsageCounters{
			RequestCount: 1,
			InputTokens:  12,
			OutputTokens: 8,
			TotalTokens:  20,
		},
	})
	assertUsageTimeseriesPoint(t, timeseries.Series[1], UsageTimeseriesPoint{
		BucketStart: time.Date(2026, 6, 12, 10, 10, 0, 0, time.UTC),
		UserID:      alice.User.ID,
		Name:        "Alice",
		UsageCounters: UsageCounters{
			RequestCount: 1,
			InputTokens:  5,
			OutputTokens: 7,
			TotalTokens:  12,
		},
	})
	assertUsageTimeseriesPoint(t, timeseries.Series[2], UsageTimeseriesPoint{
		BucketStart: time.Date(2026, 6, 12, 10, 10, 0, 0, time.UTC),
		UserID:      bob.User.ID,
		Name:        "Bob",
		UsageCounters: UsageCounters{
			RequestCount: 1,
			InputTokens:  30,
			OutputTokens: 10,
			TotalTokens:  40,
		},
	})
}

func TestUserStoreUsageTimeseriesSupportsThirtyDayWindow(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 29, 10, 45, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	alice := createUsageTestCredential(t, store, "Alice")
	timestamp := now.Add(-14 * 24 * time.Hour)
	if err := store.RecordUsage(ctx, UsageRecordParams{
		Timestamp:       timestamp,
		User:            alice.User,
		APIKey:          alice.APIKey,
		Model:           "gpt-5.3-codex",
		ReasoningEffort: "high",
		ServiceTier:     "standard",
		AuthID:          "auth.json",
		RequestID:       "req_alice_30d",
		StatusCode:      http.StatusOK,
		Counters: UsageCounters{
			InputTokens:  30,
			OutputTokens: 12,
			TotalTokens:  42,
		},
	}, UsageConfig{}); err != nil {
		t.Fatalf("RecordUsage returned error: %v", err)
	}

	timeseries, err := store.GetUsageTimeseries(ctx, UsageTimeseriesParams{
		Window:  "30d",
		Step:    "1d",
		GroupBy: []string{"user"},
		Now:     now,
	}, UsageConfig{})
	if err != nil {
		t.Fatalf("GetUsageTimeseries returned error: %v", err)
	}
	if timeseries.Window != "30d" || timeseries.Step != "1d" {
		t.Fatalf("timeseries window/step = %s/%s, want 30d/1d", timeseries.Window, timeseries.Step)
	}
	if len(timeseries.Series) != 1 {
		t.Fatalf("series count = %d, want 1: %#v", len(timeseries.Series), timeseries.Series)
	}
	assertUsageTimeseriesPoint(t, timeseries.Series[0], UsageTimeseriesPoint{
		BucketStart: time.Date(timestamp.Year(), timestamp.Month(), timestamp.Day(), 0, 0, 0, 0, time.UTC),
		UserID:      alice.User.ID,
		Name:        "Alice",
		UsageCounters: UsageCounters{
			RequestCount: 1,
			InputTokens:  30,
			OutputTokens: 12,
			TotalTokens:  42,
		},
	})
}

func TestUserStoreUsageTimeseriesCanFillMissingBucketsWithZero(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 12, 10, 45, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	alice := createUsageTestCredential(t, store, "Alice")
	if err := store.RecordUsage(ctx, UsageRecordParams{
		Timestamp:       time.Date(2026, 6, 12, 10, 7, 0, 0, time.UTC),
		User:            alice.User,
		APIKey:          alice.APIKey,
		Model:           "gpt-5.3-codex",
		ReasoningEffort: "high",
		ServiceTier:     "standard",
		AuthID:          "auth.json",
		RequestID:       "req_alice_fill_zero",
		StatusCode:      http.StatusOK,
		Counters: UsageCounters{
			InputTokens:  12,
			OutputTokens: 8,
			TotalTokens:  20,
		},
	}, UsageConfig{}); err != nil {
		t.Fatalf("RecordUsage returned error: %v", err)
	}

	timeseries, err := store.GetUsageTimeseries(ctx, UsageTimeseriesParams{
		Window:  "5h",
		Step:    "10m",
		GroupBy: []string{"user"},
		Fill:    "zero",
		Now:     now,
	}, UsageConfig{})
	if err != nil {
		t.Fatalf("GetUsageTimeseries returned error: %v", err)
	}
	if len(timeseries.Series) != usageFiveHourBucketCount {
		t.Fatalf("series count = %d, want %d: %#v", len(timeseries.Series), usageFiveHourBucketCount, timeseries.Series)
	}
	assertUsageTimeseriesPoint(t, timeseries.Series[0], UsageTimeseriesPoint{
		BucketStart: time.Date(2026, 6, 12, 5, 50, 0, 0, time.UTC),
		UserID:      alice.User.ID,
		Name:        "Alice",
	})
	assertUsageTimeseriesPoint(t, timeseries.Series[25], UsageTimeseriesPoint{
		BucketStart: time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC),
		UserID:      alice.User.ID,
		Name:        "Alice",
		UsageCounters: UsageCounters{
			RequestCount: 1,
			InputTokens:  12,
			OutputTokens: 8,
			TotalTokens:  20,
		},
	})
	assertUsageTimeseriesPoint(t, timeseries.Series[26], UsageTimeseriesPoint{
		BucketStart: time.Date(2026, 6, 12, 10, 10, 0, 0, time.UTC),
		UserID:      alice.User.ID,
		Name:        "Alice",
	})
}

func TestServerRecordsProxyUsageAndExposesAPIs(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"account_id": "acct_1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/backend-api/wham/usage" {
			_, _ = w.Write([]byte(`{"plan_type":"pro"}`))
			return
		}
		w.Header().Set("OpenAI-Request-ID", "req_upstream_1")
		_, _ = w.Write([]byte(`{"id":"resp_1","usage":{"input_tokens":12,"output_tokens":8,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":20}}`))
	}))
	defer upstream.Close()

	enabled := true
	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:        authDir,
		AdminAPIKey:    "admin-key",
		Database:       DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:   upstream.URL + "/backend-api/codex",
		ChatGPTBaseURL: upstream.URL + "/backend-api",
		RequestRetry:   1,
		Usage: UsageConfig{
			Enabled: &enabled,
		},
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	created := createManagedUser(t, handler, "admin-key", "Alice")

	proxyResp := doJSONRequest(t, handler, http.MethodPost, "/v1/responses", `{"model":"gpt-5.3-codex","reasoning":{"effort":"high"},"input":"hello"}`, created.PlaintextAPIKey)
	if proxyResp.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200, body: %s", proxyResp.Code, proxyResp.Body.String())
	}

	upstreamOnlyReq := httptest.NewRequest(http.MethodGet, "/backend-api/wham/usage", nil)
	upstreamOnlyReq.Header.Set("Authorization", "Bearer access-1")
	upstreamOnlyResp := httptest.NewRecorder()
	handler.ServeHTTP(upstreamOnlyResp, upstreamOnlyReq)
	if upstreamOnlyResp.Code != http.StatusOK {
		t.Fatalf("upstream-only status = %d, want 200, body: %s", upstreamOnlyResp.Code, upstreamOnlyResp.Body.String())
	}

	todayResp := doJSONRequest(t, handler, http.MethodGet, "/v0/user/usage/today", "", created.PlaintextAPIKey)
	if todayResp.Code != http.StatusOK {
		t.Fatalf("today status = %d, want 200, body: %s", todayResp.Code, todayResp.Body.String())
	}
	var today struct {
		UserUsageToday
		Models []testUsageDimension `json:"models"`
	}
	decodeResponse(t, todayResp, &today)
	assertUsageCounters(t, today.UsageCounters, UsageCounters{
		RequestCount:    1,
		InputTokens:     12,
		OutputTokens:    8,
		ReasoningTokens: 3,
		TotalTokens:     20,
	})
	assertUsageDimensions(t, today.Models, []testUsageDimension{
		{
			Model:           "gpt-5.3-codex",
			ReasoningEffort: "high",
			ServiceTier:     "standard",
			UsageCounters: UsageCounters{
				RequestCount:    1,
				InputTokens:     12,
				OutputTokens:    8,
				ReasoningTokens: 3,
				TotalTokens:     20,
			},
		},
	})

	usageResp := doJSONRequest(t, handler, http.MethodGet, "/v0/management/usage", "", "admin-key")
	if usageResp.Code != http.StatusOK {
		t.Fatalf("management usage status = %d, want 200, body: %s", usageResp.Code, usageResp.Body.String())
	}
	var usagePayload struct {
		Usage []struct {
			ManagementUsageEntry
			Models []testUsageDimension `json:"models"`
		} `json:"usage"`
	}
	decodeResponse(t, usageResp, &usagePayload)
	if len(usagePayload.Usage) != 1 {
		t.Fatalf("management usage count = %d, want 1", len(usagePayload.Usage))
	}
	entry := usagePayload.Usage[0]
	if entry.KeyHash == "" || entry.MaskedKey == "" {
		t.Fatalf("management key metadata = hash %q masked %q, want populated", entry.KeyHash, entry.MaskedKey)
	}
	if strings.Contains(usageResp.Body.String(), created.PlaintextAPIKey) {
		t.Fatalf("management usage leaked plaintext API key: %s", usageResp.Body.String())
	}
	for _, field := range []string{`"reference_tokens"`, `"ratio"`, `"over_threshold"`} {
		if strings.Contains(usageResp.Body.String(), field) {
			t.Fatalf("management usage response includes quota field %s: %s", field, usageResp.Body.String())
		}
	}
	fiveHour := entry.Windows["5h"]
	assertUsageCounters(t, fiveHour.UsageCounters, UsageCounters{
		RequestCount:    1,
		InputTokens:     12,
		OutputTokens:    8,
		ReasoningTokens: 3,
		TotalTokens:     20,
	})
	assertUsageDimensions(t, entry.Models, []testUsageDimension{
		{
			Model:           "gpt-5.3-codex",
			ReasoningEffort: "high",
			ServiceTier:     "standard",
			Windows: map[string]UsageWindow{
				"5h": {
					UsageCounters: UsageCounters{
						RequestCount:    1,
						InputTokens:     12,
						OutputTokens:    8,
						ReasoningTokens: 3,
						TotalTokens:     20,
					},
				},
				"7d": {
					UsageCounters: UsageCounters{
						RequestCount:    1,
						InputTokens:     12,
						OutputTokens:    8,
						ReasoningTokens: 3,
						TotalTokens:     20,
					},
				},
			},
		},
	})
}

func TestServerExposesManagementUsageTimeseries(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"account_id": "acct_1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","usage":{"input_tokens":9,"output_tokens":6,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:       authDir,
		AdminAPIKey:   "admin-key",
		Database:      DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:  upstream.URL + "/backend-api/codex",
		RequestRetry:  1,
		AllowFastMode: true,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	created := createManagedUser(t, handler, "admin-key", "Alice")

	proxyResp := doJSONRequest(t, handler, http.MethodPost, "/v1/responses", `{"model":"gpt-5.3-codex","service_tier":"standard","input":"hello"}`, created.PlaintextAPIKey)
	if proxyResp.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200, body: %s", proxyResp.Code, proxyResp.Body.String())
	}
	waitForUsageTotal(t, handler, created.PlaintextAPIKey, 15)

	resp := doJSONRequest(t, handler, http.MethodGet, "/v0/management/usage/timeseries?window=5h&step=10m&group_by=user", "", "admin-key")
	if resp.Code != http.StatusOK {
		t.Fatalf("timeseries status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	var payload UsageTimeseries
	decodeResponse(t, resp, &payload)
	if payload.Window != "5h" || payload.Step != "10m" {
		t.Fatalf("timeseries window/step = %s/%s, want 5h/10m", payload.Window, payload.Step)
	}
	if got := strings.Join(payload.GroupBy, ","); got != "user" {
		t.Fatalf("group_by = %q, want user", got)
	}
	if len(payload.Series) != 1 {
		t.Fatalf("series count = %d, want 1: %#v", len(payload.Series), payload.Series)
	}
	if payload.Series[0].UserID != created.User.ID || payload.Series[0].Name != "Alice" {
		t.Fatalf("series user = %s/%s, want %s/Alice", payload.Series[0].UserID, payload.Series[0].Name, created.User.ID)
	}
	assertUsageCounters(t, payload.Series[0].UsageCounters, UsageCounters{
		RequestCount: 1,
		InputTokens:  9,
		OutputTokens: 6,
		TotalTokens:  15,
	})
}

func TestServerRecordsFastServiceTierUsage(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"account_id": "acct_1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	var upstreamReq map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&upstreamReq); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","usage":{"input_tokens":14,"output_tokens":10,"total_tokens":24}}`))
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:       authDir,
		AdminAPIKey:   "admin-key",
		Database:      DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL:  upstream.URL + "/backend-api/codex",
		RequestRetry:  1,
		AllowFastMode: true,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	created := createManagedUser(t, handler, "admin-key", "Alice")

	resp := doJSONRequest(t, handler, http.MethodPost, "/v1/responses", `{"model":"gpt-5.5","reasoning":{"effort":"xhigh"},"service_tier":"priority","input":"hello"}`, created.PlaintextAPIKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200, body: %s", resp.Code, resp.Body.String())
	}
	if upstreamReq["service_tier"] != "priority" {
		t.Fatalf("upstream service_tier = %#v, want priority", upstreamReq["service_tier"])
	}

	waitForUsageTotal(t, handler, created.PlaintextAPIKey, 24)
	todayResp := doJSONRequest(t, handler, http.MethodGet, "/v0/user/usage/today", "", created.PlaintextAPIKey)
	if todayResp.Code != http.StatusOK {
		t.Fatalf("today status = %d, want 200, body: %s", todayResp.Code, todayResp.Body.String())
	}
	var today UserUsageToday
	decodeResponse(t, todayResp, &today)
	assertUsageDimensions(t, today.Models, []testUsageDimension{
		{
			Model:           "gpt-5.5",
			ReasoningEffort: "xhigh",
			ServiceTier:     "fast",
			UsageCounters: UsageCounters{
				RequestCount: 1,
				InputTokens:  14,
				OutputTokens: 10,
				TotalTokens:  24,
			},
		},
	})
}

func TestUserStoreSeparatesUsageByReasoningEffort(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()
	fixed := time.Date(2026, 6, 12, 10, 7, 0, 0, time.UTC)
	store.now = func() time.Time { return fixed }

	created, err := store.CreateUser(ctx, CreateUserParams{Name: "Alice"})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	credential, err := store.AuthenticateAPIKey(ctx, created.PlaintextAPIKey)
	if err != nil {
		t.Fatalf("AuthenticateAPIKey returned error: %v", err)
	}

	cfg := UsageConfig{}
	records := []struct {
		effort string
		tokens int64
	}{
		{effort: "high", tokens: 40},
		{effort: "xhigh", tokens: 20},
		{effort: "high", tokens: 15},
	}
	for i, record := range records {
		err = store.RecordUsage(ctx, UsageRecordParams{
			Timestamp:       fixed.Add(time.Duration(i) * time.Minute),
			User:            credential.User,
			APIKey:          credential.APIKey,
			Model:           "gpt-5.5",
			ReasoningEffort: record.effort,
			AuthID:          "auth.json",
			RequestID:       fmt.Sprintf("req_effort_%d", i+1),
			StatusCode:      http.StatusOK,
			Counters: UsageCounters{
				InputTokens:  record.tokens / 2,
				OutputTokens: record.tokens - record.tokens/2,
				TotalTokens:  record.tokens,
			},
		}, cfg)
		if err != nil {
			t.Fatalf("RecordUsage #%d returned error: %v", i+1, err)
		}
	}

	today, err := store.GetTodayUsage(ctx, credential.User.ID, credential.APIKey.ID, fixed)
	if err != nil {
		t.Fatalf("GetTodayUsage returned error: %v", err)
	}
	assertUsageCounters(t, today.UsageCounters, UsageCounters{
		RequestCount: 3,
		InputTokens:  37,
		OutputTokens: 38,
		TotalTokens:  75,
	})
	assertUsageDimensions(t, today.Models, []testUsageDimension{
		{
			Model:           "gpt-5.5",
			ReasoningEffort: "high",
			ServiceTier:     "standard",
			UsageCounters: UsageCounters{
				RequestCount: 2,
				InputTokens:  27,
				OutputTokens: 28,
				TotalTokens:  55,
			},
		},
		{
			Model:           "gpt-5.5",
			ReasoningEffort: "xhigh",
			ServiceTier:     "standard",
			UsageCounters: UsageCounters{
				RequestCount: 1,
				InputTokens:  10,
				OutputTokens: 10,
				TotalTokens:  20,
			},
		},
	})

	snapshot, err := store.GetUsageSnapshot(ctx, UsageSnapshotFilter{}, fixed, cfg)
	if err != nil {
		t.Fatalf("GetUsageSnapshot returned error: %v", err)
	}
	if len(snapshot) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(snapshot))
	}
	assertUsageDimensions(t, snapshot[0].Models, []testUsageDimension{
		{
			Model:           "gpt-5.5",
			ReasoningEffort: "high",
			ServiceTier:     "standard",
			Windows: map[string]UsageWindow{
				"5h": {
					UsageCounters: UsageCounters{
						RequestCount: 2,
						InputTokens:  27,
						OutputTokens: 28,
						TotalTokens:  55,
					},
				},
				"7d": {
					UsageCounters: UsageCounters{
						RequestCount: 2,
						InputTokens:  27,
						OutputTokens: 28,
						TotalTokens:  55,
					},
				},
			},
		},
		{
			Model:           "gpt-5.5",
			ReasoningEffort: "xhigh",
			ServiceTier:     "standard",
			Windows: map[string]UsageWindow{
				"5h": {
					UsageCounters: UsageCounters{
						RequestCount: 1,
						InputTokens:  10,
						OutputTokens: 10,
						TotalTokens:  20,
					},
				},
				"7d": {
					UsageCounters: UsageCounters{
						RequestCount: 1,
						InputTokens:  10,
						OutputTokens: 10,
						TotalTokens:  20,
					},
				},
			},
		},
	})
}

func TestUserStoreSeparatesUsageByServiceTier(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()
	fixed := time.Date(2026, 6, 12, 10, 7, 0, 0, time.UTC)
	store.now = func() time.Time { return fixed }

	created, err := store.CreateUser(ctx, CreateUserParams{Name: "Alice"})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	credential, err := store.AuthenticateAPIKey(ctx, created.PlaintextAPIKey)
	if err != nil {
		t.Fatalf("AuthenticateAPIKey returned error: %v", err)
	}

	cfg := UsageConfig{}
	records := []struct {
		serviceTier string
		tokens      int64
	}{
		{serviceTier: "", tokens: 30},
		{serviceTier: "priority", tokens: 20},
		{serviceTier: "standard", tokens: 25},
	}
	for i, record := range records {
		err = store.RecordUsage(ctx, UsageRecordParams{
			Timestamp:       fixed.Add(time.Duration(i) * time.Minute),
			User:            credential.User,
			APIKey:          credential.APIKey,
			Model:           "gpt-5.5",
			ReasoningEffort: "high",
			ServiceTier:     record.serviceTier,
			AuthID:          "auth.json",
			RequestID:       fmt.Sprintf("req_tier_%d", i+1),
			StatusCode:      http.StatusOK,
			Counters: UsageCounters{
				InputTokens:  record.tokens / 2,
				OutputTokens: record.tokens - record.tokens/2,
				TotalTokens:  record.tokens,
			},
		}, cfg)
		if err != nil {
			t.Fatalf("RecordUsage #%d returned error: %v", i+1, err)
		}
	}

	today, err := store.GetTodayUsage(ctx, credential.User.ID, credential.APIKey.ID, fixed)
	if err != nil {
		t.Fatalf("GetTodayUsage returned error: %v", err)
	}
	assertUsageCounters(t, today.UsageCounters, UsageCounters{
		RequestCount: 3,
		InputTokens:  37,
		OutputTokens: 38,
		TotalTokens:  75,
	})
	assertUsageDimensions(t, today.Models, []testUsageDimension{
		{
			Model:           "gpt-5.5",
			ReasoningEffort: "high",
			ServiceTier:     "fast",
			UsageCounters: UsageCounters{
				RequestCount: 1,
				InputTokens:  10,
				OutputTokens: 10,
				TotalTokens:  20,
			},
		},
		{
			Model:           "gpt-5.5",
			ReasoningEffort: "high",
			ServiceTier:     "standard",
			UsageCounters: UsageCounters{
				RequestCount: 2,
				InputTokens:  27,
				OutputTokens: 28,
				TotalTokens:  55,
			},
		},
	})
}

func TestUserStoreMigratesUsageReasoningEffort(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "users.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE users (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL COLLATE NOCASE UNIQUE,
			enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE api_keys (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			key_hash TEXT NOT NULL UNIQUE,
			key_prefix TEXT NOT NULL,
			masked_key TEXT NOT NULL,
			enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
			created_at TEXT NOT NULL,
			rotated_at TEXT,
			last_used_at TEXT
		)`,
		`CREATE TABLE usage_buckets (
			bucket_start TEXT NOT NULL,
			user_id TEXT NOT NULL,
			api_key_id TEXT NOT NULL,
			key_hash TEXT NOT NULL,
			masked_key TEXT NOT NULL,
			model TEXT NOT NULL,
			auth_id TEXT NOT NULL,
			request_count INTEGER NOT NULL DEFAULT 0,
			failed_request_count INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			reasoning_tokens INTEGER NOT NULL DEFAULT 0,
			cached_input_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (bucket_start, user_id, api_key_id, model, auth_id)
		)`,
		`INSERT INTO users (id, name, enabled, created_at, updated_at)
			VALUES ('usr_1', 'Alice', 1, '2026-06-12T00:00:00Z', '2026-06-12T00:00:00Z')`,
		`INSERT INTO api_keys (id, user_id, key_hash, key_prefix, masked_key, enabled, created_at)
			VALUES ('key_1', 'usr_1', 'hash_1', 'cop_old', 'cop_old...old', 1, '2026-06-12T00:00:00Z')`,
		`INSERT INTO usage_buckets (
			bucket_start, user_id, api_key_id, key_hash, masked_key, model, auth_id,
			request_count, input_tokens, output_tokens, total_tokens, updated_at
		) VALUES (
			'2026-06-12T10:00:00Z', 'usr_1', 'key_1', 'hash_1', 'cop_old...old',
			'gpt-5.5', 'auth.json', 1, 7, 5, 12, '2026-06-12T10:00:00Z'
		)`,
	} {
		if _, err = db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("exec setup statement %q: %v", statement, err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatalf("close setup db: %v", err)
	}

	store, err := OpenUserStore(ctx, dbPath)
	if err != nil {
		t.Fatalf("OpenUserStore returned error: %v", err)
	}
	defer store.Close()

	assertColumnExists(t, store.db, "usage_buckets", "reasoning_effort")
	assertColumnExists(t, store.db, "usage_buckets", "service_tier")
	var effort string
	var serviceTier string
	var total int64
	err = store.db.QueryRowContext(ctx, `SELECT reasoning_effort, service_tier, total_tokens FROM usage_buckets WHERE user_id = 'usr_1'`).Scan(&effort, &serviceTier, &total)
	if err != nil {
		t.Fatalf("query migrated usage bucket: %v", err)
	}
	if effort != "unknown" || serviceTier != "standard" || total != 12 {
		t.Fatalf("migrated usage bucket dimension/total = %s/%s/%d, want unknown/standard/12", effort, serviceTier, total)
	}

}

func TestServerRecordsLongSSEUsageBeyondCaptureLimit(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"account_id": "acct_1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		longDelta := strings.Repeat("x", maxUsageCaptureBytes+1024)
		_, _ = fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", longDelta)
		_, _ = w.Write([]byte(`event: response.completed
data: {"type":"response.completed","response":{"usage":{"input_tokens":11,"output_tokens":5,"total_tokens":16}}}

`))
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:      authDir,
		AdminAPIKey:  "admin-key",
		Database:     DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL: upstream.URL + "/backend-api/codex",
		RequestRetry: 1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	created := createManagedUser(t, handler, "admin-key", "Alice")

	proxyResp := doJSONRequest(t, handler, http.MethodPost, "/v1/responses", `{"model":"gpt-5.3-codex","input":"hello"}`, created.PlaintextAPIKey)
	if proxyResp.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200, body: %s", proxyResp.Code, proxyResp.Body.String())
	}

	waitForUsageTotal(t, handler, created.PlaintextAPIKey, 16)
}

func TestManagementUsageEventsRouteRemoved(t *testing.T) {
	handler := newUserManagementTestHandler(t, &Config{AdminAPIKey: "admin-key"})

	resp := doJSONRequest(t, handler, http.MethodGet, "/v0/management/usage/events?count=0", "", "admin-key")
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body: %s", resp.Code, resp.Body.String())
	}
}

func TestServerRecordsFinalWebSocketUsagePerResponseAndProxiesFrames(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"account_id": "acct_1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	var sawAuthorization string
	var sawOpenAIBeta string
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuthorization = r.Header.Get("Authorization")
		sawOpenAIBeta = r.Header.Get("OpenAI-Beta")
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("upstream read failed: %v", err)
			return
		}
		if err = conn.WriteMessage(messageType, payload); err != nil {
			t.Errorf("upstream echo failed: %v", err)
			return
		}
		usageFrames := [][]byte{
			[]byte(`{"type":"response.completed","response":{"id":"resp-1","usage":{"input_tokens":7,"output_tokens":6,"total_tokens":13}}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp-1","usage":{"input_tokens":10,"output_tokens":7,"total_tokens":17}}}`),
		}
		for _, usageFrame := range usageFrames {
			if err = conn.WriteMessage(websocket.TextMessage, usageFrame); err != nil {
				t.Errorf("upstream usage write failed: %v", err)
				return
			}
		}
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:      authDir,
		AdminAPIKey:  "admin-key",
		Database:     DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL: upstream.URL + "/backend-api/codex",
		RequestRetry: 1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	created := createManagedUser(t, handler, "admin-key", "Alice")

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	wsURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/v1/responses"
	headers := http.Header{"Authorization": []string{"Bearer " + created.PlaintextAPIKey}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("dial proxy websocket: %v", err)
	}
	requestFrame := []byte(`{"type":"response.create","model":"gpt-5.5","reasoning":{"effort":"xhigh"}}`)
	if err = conn.WriteMessage(websocket.TextMessage, requestFrame); err != nil {
		t.Fatalf("write proxy websocket: %v", err)
	}
	_, echo, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read echo websocket: %v", err)
	}
	if string(echo) != string(requestFrame) {
		t.Fatalf("echo frame = %q, want request frame", string(echo))
	}
	for i := 0; i < 2; i++ {
		_, usageFrame, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read usage websocket #%d: %v", i+1, err)
		}
		if !strings.Contains(string(usageFrame), "response.completed") {
			t.Fatalf("usage frame #%d = %q, want response.completed", i+1, string(usageFrame))
		}
	}
	if err = conn.Close(); err != nil {
		t.Fatalf("close websocket: %v", err)
	}

	waitForUsageTotal(t, handler, created.PlaintextAPIKey, 17)
	todayResp := doJSONRequest(t, handler, http.MethodGet, "/v0/user/usage/today", "", created.PlaintextAPIKey)
	if todayResp.Code != http.StatusOK {
		t.Fatalf("today status = %d, want 200, body: %s", todayResp.Code, todayResp.Body.String())
	}
	var today UserUsageToday
	decodeResponse(t, todayResp, &today)
	assertUsageDimensions(t, today.Models, []testUsageDimension{
		{
			Model:           "gpt-5.5",
			ReasoningEffort: "xhigh",
			ServiceTier:     "standard",
			UsageCounters: UsageCounters{
				RequestCount: 1,
				InputTokens:  10,
				OutputTokens: 7,
				TotalTokens:  17,
			},
		},
	})

	if sawAuthorization != "Bearer access-1" {
		t.Fatalf("upstream Authorization = %q, want Bearer access-1", sawAuthorization)
	}
	if sawOpenAIBeta != websocketBetaHeader {
		t.Fatalf("upstream OpenAI-Beta = %q, want %q", sawOpenAIBeta, websocketBetaHeader)
	}
}

func TestServerRejectsFastServiceTierWebSocketFrameByDefault(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	upstreamReceived := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade failed: %v", err)
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err == nil {
			upstreamReceived <- payload
		}
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:      authDir,
		AdminAPIKey:  "admin-key",
		Database:     DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL: upstream.URL + "/backend-api/codex",
		RequestRetry: 1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	created := createManagedUser(t, handler, "admin-key", "Alice")

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	wsURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/v1/responses"
	headers := http.Header{"Authorization": []string{"Bearer " + created.PlaintextAPIKey}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("dial proxy websocket: %v", err)
	}
	defer conn.Close()

	requestFrame := []byte(`{"type":"response.create","model":"gpt-5.5","service_tier":"priority","input":"hello"}`)
	if err = conn.WriteMessage(websocket.TextMessage, requestFrame); err != nil {
		t.Fatalf("write proxy websocket: %v", err)
	}
	_, _, err = conn.ReadMessage()
	if err == nil {
		t.Fatal("read websocket returned nil error, want close")
	}
	closeErr, ok := err.(*websocket.CloseError)
	if !ok || closeErr.Code != websocket.ClosePolicyViolation {
		t.Fatalf("websocket close error = %v, want policy violation", err)
	}

	select {
	case payload := <-upstreamReceived:
		t.Fatalf("upstream received disabled Fast mode frame: %s", string(payload))
	case <-time.After(100 * time.Millisecond):
	}
}

func TestServerSkipsWebSocketPrewarmUsage(t *testing.T) {
	authDir := t.TempDir()
	writeAuthFile(t, authDir, "codex.json", `{
		"type": "codex",
		"access_token": "access-1",
		"refresh_token": "refresh-1",
		"account_id": "acct_1",
		"expired": "2099-01-01T00:00:00Z"
	}`)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upstream upgrade failed: %v", err)
			return
		}
		defer conn.Close()

		messageType, warmupPayload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("upstream warmup read failed: %v", err)
			return
		}
		if err = conn.WriteMessage(messageType, warmupPayload); err != nil {
			t.Errorf("upstream warmup echo failed: %v", err)
			return
		}
		if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"warm-1","usage":{"input_tokens":7,"output_tokens":6,"total_tokens":13}}}`)); err != nil {
			t.Errorf("upstream warmup usage write failed: %v", err)
			return
		}

		messageType, actualPayload, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("upstream actual read failed: %v", err)
			return
		}
		if err = conn.WriteMessage(messageType, actualPayload); err != nil {
			t.Errorf("upstream actual echo failed: %v", err)
			return
		}
		if err = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp-1","usage":{"input_tokens":10,"output_tokens":7,"total_tokens":17}}}`)); err != nil {
			t.Errorf("upstream actual usage write failed: %v", err)
			return
		}
	}))
	defer upstream.Close()

	handler, err := NewHandler(context.Background(), &Config{
		AuthDir:      authDir,
		AdminAPIKey:  "admin-key",
		Database:     DatabaseConfig{Path: filepath.Join(t.TempDir(), "users.db")},
		CodexBaseURL: upstream.URL + "/backend-api/codex",
		RequestRetry: 1,
	})
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	created := createManagedUser(t, handler, "admin-key", "Alice")

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	wsURL := "ws" + strings.TrimPrefix(proxyServer.URL, "http") + "/v1/responses"
	headers := http.Header{"Authorization": []string{"Bearer " + created.PlaintextAPIKey}}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("dial proxy websocket: %v", err)
	}
	warmupFrame := []byte(`{"type":"response.create","model":"gpt-5.5","reasoning":{"effort":"xhigh"},"generate":false}`)
	if err = conn.WriteMessage(websocket.TextMessage, warmupFrame); err != nil {
		t.Fatalf("write warmup websocket: %v", err)
	}
	_, warmupEcho, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read warmup echo websocket: %v", err)
	}
	if string(warmupEcho) != string(warmupFrame) {
		t.Fatalf("warmup echo frame = %q, want warmup frame", string(warmupEcho))
	}
	if _, _, err = conn.ReadMessage(); err != nil {
		t.Fatalf("read warmup usage websocket: %v", err)
	}

	actualFrame := []byte(`{"type":"response.create","model":"gpt-5.5","reasoning":{"effort":"xhigh"},"previous_response_id":"warm-1","input":[]}`)
	if err = conn.WriteMessage(websocket.TextMessage, actualFrame); err != nil {
		t.Fatalf("write actual websocket: %v", err)
	}
	_, actualEcho, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read actual echo websocket: %v", err)
	}
	if string(actualEcho) != string(actualFrame) {
		t.Fatalf("actual echo frame = %q, want actual frame", string(actualEcho))
	}
	if _, _, err = conn.ReadMessage(); err != nil {
		t.Fatalf("read actual usage websocket: %v", err)
	}
	if err = conn.Close(); err != nil {
		t.Fatalf("close websocket: %v", err)
	}

	waitForUsageTotal(t, handler, created.PlaintextAPIKey, 17)
	todayResp := doJSONRequest(t, handler, http.MethodGet, "/v0/user/usage/today", "", created.PlaintextAPIKey)
	if todayResp.Code != http.StatusOK {
		t.Fatalf("today status = %d, want 200, body: %s", todayResp.Code, todayResp.Body.String())
	}
	var today UserUsageToday
	decodeResponse(t, todayResp, &today)
	assertUsageDimensions(t, today.Models, []testUsageDimension{
		{
			Model:           "gpt-5.5",
			ReasoningEffort: "xhigh",
			ServiceTier:     "standard",
			UsageCounters: UsageCounters{
				RequestCount: 1,
				InputTokens:  10,
				OutputTokens: 7,
				TotalTokens:  17,
			},
		},
	})
}

func TestWebSocketDialerForcesHTTP11ALPN(t *testing.T) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{
			"h2",
			"http/1.1",
		},
	}
	client := &http.Client{Transport: transport}

	dialer := websocketDialer(client)

	if dialer.TLSClientConfig == nil {
		t.Fatal("dialer TLS config is nil")
	}
	if got := dialer.TLSClientConfig.NextProtos; len(got) != 1 || got[0] != "http/1.1" {
		t.Fatalf("dialer NextProtos = %#v, want only http/1.1", got)
	}
	if got := transport.TLSClientConfig.NextProtos; len(got) != 2 || got[0] != "h2" || got[1] != "http/1.1" {
		t.Fatalf("transport NextProtos mutated to %#v", got)
	}
}

func assertUsageCounters(t *testing.T, got UsageCounters, want UsageCounters) {
	t.Helper()
	if got.RequestCount != want.RequestCount ||
		got.FailedRequestCount != want.FailedRequestCount ||
		got.InputTokens != want.InputTokens ||
		got.OutputTokens != want.OutputTokens ||
		got.ReasoningTokens != want.ReasoningTokens ||
		got.CachedInputTokens != want.CachedInputTokens ||
		got.CacheReadTokens != want.CacheReadTokens ||
		got.CacheCreationTokens != want.CacheCreationTokens ||
		got.TotalTokens != want.TotalTokens {
		t.Fatalf("usage counters = %#v, want %#v", got, want)
	}
}

func waitForUsageTotal(t *testing.T, handler http.Handler, apiKey string, wantTotal int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp := doJSONRequest(t, handler, http.MethodGet, "/v0/user/usage/today", "", apiKey)
		if resp.Code == http.StatusOK {
			var today UserUsageToday
			decodeResponse(t, resp, &today)
			if today.TotalTokens == wantTotal {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp := doJSONRequest(t, handler, http.MethodGet, "/v0/user/usage/today", "", apiKey)
	t.Fatalf("usage total did not reach %d, final status=%d body=%s", wantTotal, resp.Code, resp.Body.String())
}

type testUsageDimension = UsageDimension

func createUsageTestCredential(t *testing.T, store *UserStore, name string) AuthenticatedAPIKey {
	t.Helper()
	created, err := store.CreateUser(context.Background(), CreateUserParams{Name: name})
	if err != nil {
		t.Fatalf("CreateUser %q returned error: %v", name, err)
	}
	credential, err := store.AuthenticateAPIKey(context.Background(), created.PlaintextAPIKey)
	if err != nil {
		t.Fatalf("AuthenticateAPIKey %q returned error: %v", name, err)
	}
	return credential
}

func assertUsageTimeseriesPoint(t *testing.T, got UsageTimeseriesPoint, want UsageTimeseriesPoint) {
	t.Helper()
	if !got.BucketStart.Equal(want.BucketStart) ||
		got.UserID != want.UserID ||
		got.Name != want.Name ||
		got.APIKeyID != want.APIKeyID ||
		got.Model != want.Model ||
		got.ReasoningEffort != want.ReasoningEffort ||
		got.ServiceTier != want.ServiceTier {
		t.Fatalf("timeseries point identity = %#v, want %#v", got, want)
	}
	assertUsageCounters(t, got.UsageCounters, want.UsageCounters)
}

func assertUsageDimensions(t *testing.T, got []testUsageDimension, want []testUsageDimension) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("usage dimension count = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Model != want[i].Model || got[i].ReasoningEffort != want[i].ReasoningEffort || got[i].ServiceTier != want[i].ServiceTier {
			t.Fatalf("usage dimension #%d = %s/%s/%s, want %s/%s/%s", i, got[i].Model, got[i].ReasoningEffort, got[i].ServiceTier, want[i].Model, want[i].ReasoningEffort, want[i].ServiceTier)
		}
		assertUsageCounters(t, got[i].UsageCounters, want[i].UsageCounters)
		assertUsageWindows(t, got[i].Windows, want[i].Windows)
	}
}

func assertUsageWindows(t *testing.T, got map[string]UsageWindow, want map[string]UsageWindow) {
	t.Helper()
	if len(want) == 0 {
		return
	}
	if len(got) != len(want) {
		t.Fatalf("usage window count = %d, want %d: %#v", len(got), len(want), got)
	}
	for name, wantWindow := range want {
		gotWindow, ok := got[name]
		if !ok {
			t.Fatalf("usage window %q missing from %#v", name, got)
		}
		assertUsageCounters(t, gotWindow.UsageCounters, wantWindow.UsageCounters)
	}
}

func assertColumnExists(t *testing.T, db *sql.DB, table string, column string) {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var columnType string
		var notNull int
		var defaultValue any
		var pk int
		if err = rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		if name == column {
			return
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("table_info(%s) rows: %v", table, err)
	}
	t.Fatalf("column %s.%s not found", table, column)
}
