package codexonly

import (
	"context"
	"crypto/tls"
	"database/sql"
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
	if cfg.Usage.FiveHourReferenceTokens != 0 {
		t.Fatalf("five-hour reference = %d, want 0", cfg.Usage.FiveHourReferenceTokens)
	}
	if cfg.Usage.WeeklyReferenceTokens != 0 {
		t.Fatalf("weekly reference = %d, want 0", cfg.Usage.WeeklyReferenceTokens)
	}
	if got := usageAlertThreshold(cfg.Usage); got != 0.8 {
		t.Fatalf("alert threshold = %v, want 0.8", got)
	}
	if got := usageEventRetentionDays(cfg.Usage); got != 30 {
		t.Fatalf("event retention days = %d, want 30", got)
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

func TestUserStoreRecordsUsageBucketsAndThresholdEvents(t *testing.T) {
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
		Enabled:                 &enabled,
		FiveHourReferenceTokens: 100,
		WeeklyReferenceTokens:   100,
		AlertThreshold:          0.5,
		EventRetentionDays:      30,
		DebugOpenAIResponse:     false,
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
	if fiveHour.Ratio == nil || *fiveHour.Ratio != 0.7 {
		t.Fatalf("5h ratio = %#v, want 0.7", fiveHour.Ratio)
	}
	if !fiveHour.OverThreshold {
		t.Fatal("5h over_threshold = false, want true")
	}

	events, err := store.ListUsageEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ListUsageEvents returned error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}
	if events[0].Window != "7d" || events[1].Window != "5h" {
		t.Fatalf("event windows = %s/%s, want newest 7d then 5h", events[0].Window, events[1].Window)
	}
	if events[1].TotalTokens != 60 || events[1].RequestCount != 2 {
		t.Fatalf("5h event totals = tokens %d requests %d, want 60/2", events[1].TotalTokens, events[1].RequestCount)
	}
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
			Enabled:                 &enabled,
			FiveHourReferenceTokens: 40,
			WeeklyReferenceTokens:   100,
			AlertThreshold:          0.5,
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
	fiveHour := entry.Windows["5h"]
	if fiveHour.Ratio == nil || *fiveHour.Ratio != 0.5 {
		t.Fatalf("management 5h ratio = %#v, want 0.5", fiveHour.Ratio)
	}
	if !fiveHour.OverThreshold {
		t.Fatal("management 5h over_threshold = false, want true")
	}
	assertUsageDimensions(t, entry.Models, []testUsageDimension{
		{
			Model:           "gpt-5.3-codex",
			ReasoningEffort: "high",
			Windows: map[string]UsageWindow{
				"5h": {
					UsageCounters: UsageCounters{
						RequestCount:    1,
						InputTokens:     12,
						OutputTokens:    8,
						ReasoningTokens: 3,
						TotalTokens:     20,
					},
					ReferenceTokens: 40,
					Ratio:           ptrFloat64(0.5),
					OverThreshold:   true,
				},
				"7d": {
					UsageCounters: UsageCounters{
						RequestCount:    1,
						InputTokens:     12,
						OutputTokens:    8,
						ReasoningTokens: 3,
						TotalTokens:     20,
					},
					ReferenceTokens: 100,
					Ratio:           ptrFloat64(0.2),
					OverThreshold:   false,
				},
			},
		},
	})

	eventsResp := doJSONRequest(t, handler, http.MethodGet, "/v0/management/usage/events?count=10", "", "admin-key")
	if eventsResp.Code != http.StatusOK {
		t.Fatalf("events status = %d, want 200, body: %s", eventsResp.Code, eventsResp.Body.String())
	}
	var eventsPayload struct {
		Events []UsageThresholdEvent `json:"events"`
	}
	decodeResponse(t, eventsResp, &eventsPayload)
	if len(eventsPayload.Events) != 1 {
		t.Fatalf("events count = %d, want 1", len(eventsPayload.Events))
	}
	if strings.Contains(eventsResp.Body.String(), created.PlaintextAPIKey) {
		t.Fatalf("events leaked plaintext API key: %s", eventsResp.Body.String())
	}
	if eventsPayload.Events[0].ReasoningEffort != "high" {
		t.Fatalf("event reasoning_effort = %q, want high", eventsPayload.Events[0].ReasoningEffort)
	}
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

	cfg := UsageConfig{
		FiveHourReferenceTokens: 100,
		WeeklyReferenceTokens:   100,
		AlertThreshold:          0.5,
	}
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
			Windows: map[string]UsageWindow{
				"5h": {
					UsageCounters: UsageCounters{
						RequestCount: 2,
						InputTokens:  27,
						OutputTokens: 28,
						TotalTokens:  55,
					},
					ReferenceTokens: 100,
					Ratio:           ptrFloat64(0.55),
					OverThreshold:   true,
				},
				"7d": {
					UsageCounters: UsageCounters{
						RequestCount: 2,
						InputTokens:  27,
						OutputTokens: 28,
						TotalTokens:  55,
					},
					ReferenceTokens: 100,
					Ratio:           ptrFloat64(0.55),
					OverThreshold:   true,
				},
			},
		},
		{
			Model:           "gpt-5.5",
			ReasoningEffort: "xhigh",
			Windows: map[string]UsageWindow{
				"5h": {
					UsageCounters: UsageCounters{
						RequestCount: 1,
						InputTokens:  10,
						OutputTokens: 10,
						TotalTokens:  20,
					},
					ReferenceTokens: 100,
					Ratio:           ptrFloat64(0.2),
					OverThreshold:   false,
				},
				"7d": {
					UsageCounters: UsageCounters{
						RequestCount: 1,
						InputTokens:  10,
						OutputTokens: 10,
						TotalTokens:  20,
					},
					ReferenceTokens: 100,
					Ratio:           ptrFloat64(0.2),
					OverThreshold:   false,
				},
			},
		},
	})

	events, err := store.ListUsageEvents(ctx, 10)
	if err != nil {
		t.Fatalf("ListUsageEvents returned error: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}
	for _, event := range events {
		if event.Model != "gpt-5.5" || event.ReasoningEffort != "high" {
			t.Fatalf("event dimension = %s/%s, want gpt-5.5/high", event.Model, event.ReasoningEffort)
		}
		if event.TotalTokens != 55 || event.RequestCount != 2 {
			t.Fatalf("event totals = tokens %d requests %d, want 55/2", event.TotalTokens, event.RequestCount)
		}
	}
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
		`CREATE TABLE usage_threshold_state (
			window TEXT NOT NULL,
			api_key_id TEXT NOT NULL,
			above_threshold INTEGER NOT NULL CHECK (above_threshold IN (0, 1)),
			updated_at TEXT NOT NULL,
			PRIMARY KEY (window, api_key_id)
		)`,
		`CREATE TABLE usage_threshold_events (
			id TEXT PRIMARY KEY,
			timestamp TEXT NOT NULL,
			window TEXT NOT NULL,
			user_id TEXT NOT NULL,
			api_key_id TEXT NOT NULL,
			key_hash TEXT NOT NULL,
			masked_key TEXT NOT NULL,
			ratio REAL NOT NULL,
			threshold REAL NOT NULL,
			total_tokens INTEGER NOT NULL,
			reference_tokens INTEGER NOT NULL,
			request_count INTEGER NOT NULL,
			failed_request_count INTEGER NOT NULL,
			model TEXT NOT NULL,
			auth_id TEXT NOT NULL,
			request_id TEXT NOT NULL,
			diagnostics TEXT NOT NULL
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
		`INSERT INTO usage_threshold_state (window, api_key_id, above_threshold, updated_at)
			VALUES ('5h', 'key_1', 1, '2026-06-12T10:00:00Z')`,
		`INSERT INTO usage_threshold_events (
			id, timestamp, window, user_id, api_key_id, key_hash, masked_key, ratio,
			threshold, total_tokens, reference_tokens, request_count, failed_request_count,
			model, auth_id, request_id, diagnostics
		) VALUES (
			'evt_1', '2026-06-12T10:00:00Z', '5h', 'usr_1', 'key_1', 'hash_1',
			'cop_old...old', 0.8, 0.8, 80, 100, 1, 0, 'gpt-5.5', 'auth.json',
			'req_1', '{}'
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
	assertColumnExists(t, store.db, "usage_threshold_state", "model")
	assertColumnExists(t, store.db, "usage_threshold_state", "reasoning_effort")
	assertColumnExists(t, store.db, "usage_threshold_events", "reasoning_effort")

	var effort string
	var total int64
	err = store.db.QueryRowContext(ctx, `SELECT reasoning_effort, total_tokens FROM usage_buckets WHERE user_id = 'usr_1'`).Scan(&effort, &total)
	if err != nil {
		t.Fatalf("query migrated usage bucket: %v", err)
	}
	if effort != "unknown" || total != 12 {
		t.Fatalf("migrated usage bucket effort/total = %s/%d, want unknown/12", effort, total)
	}

	var eventEffort string
	err = store.db.QueryRowContext(ctx, `SELECT reasoning_effort FROM usage_threshold_events WHERE id = 'evt_1'`).Scan(&eventEffort)
	if err != nil {
		t.Fatalf("query migrated threshold event: %v", err)
	}
	if eventEffort != "unknown" {
		t.Fatalf("migrated event effort = %q, want unknown", eventEffort)
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

func TestManagementUsageEventsRejectsNonPositiveCount(t *testing.T) {
	handler := newUserManagementTestHandler(t, &Config{AdminAPIKey: "admin-key"})

	resp := doJSONRequest(t, handler, http.MethodGet, "/v0/management/usage/events?count=0", "", "admin-key")
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body: %s", resp.Code, resp.Body.String())
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

func assertUsageDimensions(t *testing.T, got []testUsageDimension, want []testUsageDimension) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("usage dimension count = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Model != want[i].Model || got[i].ReasoningEffort != want[i].ReasoningEffort {
			t.Fatalf("usage dimension #%d = %s/%s, want %s/%s", i, got[i].Model, got[i].ReasoningEffort, want[i].Model, want[i].ReasoningEffort)
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
		if gotWindow.ReferenceTokens != wantWindow.ReferenceTokens {
			t.Fatalf("usage window %q reference tokens = %d, want %d", name, gotWindow.ReferenceTokens, wantWindow.ReferenceTokens)
		}
		if gotWindow.OverThreshold != wantWindow.OverThreshold {
			t.Fatalf("usage window %q over_threshold = %t, want %t", name, gotWindow.OverThreshold, wantWindow.OverThreshold)
		}
		switch {
		case gotWindow.Ratio == nil && wantWindow.Ratio == nil:
		case gotWindow.Ratio == nil || wantWindow.Ratio == nil:
			t.Fatalf("usage window %q ratio = %#v, want %#v", name, gotWindow.Ratio, wantWindow.Ratio)
		case *gotWindow.Ratio != *wantWindow.Ratio:
			t.Fatalf("usage window %q ratio = %v, want %v", name, *gotWindow.Ratio, *wantWindow.Ratio)
		}
	}
}

func ptrFloat64(value float64) *float64 {
	return &value
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
