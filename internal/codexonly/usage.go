package codexonly

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	usageBucketDuration      = 10 * time.Minute
	usageFiveHourBucketCount = 30
	usageWeeklyBucketCount   = 1008
)

type UsageCounters struct {
	RequestCount        int64 `json:"request_count"`
	FailedRequestCount  int64 `json:"failed_request_count"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedInputTokens   int64 `json:"cached_input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

type UsageRecordParams struct {
	Timestamp       time.Time
	User            UserRecord
	APIKey          APIKeyRecord
	Model           string
	ReasoningEffort string
	ServiceTier     string
	AuthID          string
	RequestID       string
	StatusCode      int
	Counters        UsageCounters
	Diagnostics     string
	DeltaOnly       bool
}

type UserUsageToday struct {
	UserID   string           `json:"user_id"`
	APIKeyID string           `json:"api_key_id"`
	Date     string           `json:"date"`
	Models   []UsageDimension `json:"models,omitempty"`
	UsageCounters
}

type UsageSnapshotFilter struct {
	UserID   string
	APIKeyID string
}

type UsageWindow struct {
	UsageCounters
}

type UsageDimension struct {
	Model           string                 `json:"model"`
	ReasoningEffort string                 `json:"reasoning_effort"`
	ServiceTier     string                 `json:"service_tier"`
	Windows         map[string]UsageWindow `json:"windows,omitempty"`
	UsageCounters
}

type ManagementUsageEntry struct {
	UserID    string                 `json:"user_id"`
	Name      string                 `json:"name"`
	APIKeyID  string                 `json:"api_key_id"`
	KeyHash   string                 `json:"key_hash"`
	MaskedKey string                 `json:"masked_key"`
	Windows   map[string]UsageWindow `json:"windows"`
	Models    []UsageDimension       `json:"models,omitempty"`
}

type usageWindowSpec struct {
	name        string
	bucketCount int
}

func usageTrackingEnabled(cfg *Config) bool {
	if cfg == nil {
		return true
	}
	return usageConfigTrackingEnabled(cfg.Usage)
}

func usageConfigTrackingEnabled(cfg UsageConfig) bool {
	return cfg.Enabled == nil || *cfg.Enabled
}

func (s *UserStore) RecordUsage(ctx context.Context, params UsageRecordParams, cfg UsageConfig) error {
	if s == nil || s.db == nil || !usageConfigTrackingEnabled(cfg) {
		return nil
	}
	if strings.TrimSpace(params.User.ID) == "" || strings.TrimSpace(params.APIKey.ID) == "" {
		return ErrInvalidInput
	}
	timestamp := params.Timestamp.UTC()
	if timestamp.IsZero() {
		timestamp = s.now().UTC()
	}
	params.Timestamp = timestamp
	params.Model = normalizeUsageText(params.Model, "unknown")
	params.ReasoningEffort = normalizeUsageText(params.ReasoningEffort, "unknown")
	params.ServiceTier = normalizeServiceTier(params.ServiceTier)
	params.AuthID = normalizeUsageText(params.AuthID, "unknown")
	params.RequestID = strings.TrimSpace(params.RequestID)
	if params.RequestID == "" {
		id, err := randomID("req")
		if err != nil {
			return err
		}
		params.RequestID = id
	}
	params.Counters = params.Counters.normalized()

	requestCount := int64(1)
	failedRequestCount := int64(0)
	if params.DeltaOnly {
		requestCount = 0
	} else if params.StatusCode >= 400 || params.StatusCode == 0 {
		failedRequestCount = 1
	}
	bucketStart := usageBucketStart(timestamp)
	updatedAt := formatDBTime(s.now().UTC())

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin record usage: %w", err)
	}
	defer rollbackUnlessCommitted(tx)

	_, err = tx.ExecContext(ctx,
		`INSERT INTO usage_buckets (
			bucket_start, user_id, api_key_id, key_hash, masked_key, model, reasoning_effort, service_tier, auth_id,
			request_count, failed_request_count, input_tokens, output_tokens, reasoning_tokens,
			cached_input_tokens, cache_read_tokens, cache_creation_tokens, total_tokens, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bucket_start, user_id, api_key_id, model, reasoning_effort, service_tier, auth_id) DO UPDATE SET
			key_hash = excluded.key_hash,
			masked_key = excluded.masked_key,
			request_count = usage_buckets.request_count + excluded.request_count,
			failed_request_count = usage_buckets.failed_request_count + excluded.failed_request_count,
			input_tokens = usage_buckets.input_tokens + excluded.input_tokens,
			output_tokens = usage_buckets.output_tokens + excluded.output_tokens,
			reasoning_tokens = usage_buckets.reasoning_tokens + excluded.reasoning_tokens,
			cached_input_tokens = usage_buckets.cached_input_tokens + excluded.cached_input_tokens,
			cache_read_tokens = usage_buckets.cache_read_tokens + excluded.cache_read_tokens,
			cache_creation_tokens = usage_buckets.cache_creation_tokens + excluded.cache_creation_tokens,
			total_tokens = usage_buckets.total_tokens + excluded.total_tokens,
			updated_at = excluded.updated_at`,
		formatDBTime(bucketStart),
		params.User.ID,
		params.APIKey.ID,
		params.APIKey.KeyHash,
		params.APIKey.MaskedKey,
		params.Model,
		params.ReasoningEffort,
		params.ServiceTier,
		params.AuthID,
		requestCount,
		failedRequestCount,
		params.Counters.InputTokens,
		params.Counters.OutputTokens,
		params.Counters.ReasoningTokens,
		params.Counters.CachedInputTokens,
		params.Counters.CacheReadTokens,
		params.Counters.CacheCreationTokens,
		params.Counters.TotalTokens,
		updatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert usage bucket: %w", err)
	}

	if err = pruneUsageData(ctx, tx, timestamp); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit record usage: %w", err)
	}
	return nil
}

func (s *UserStore) GetTodayUsage(ctx context.Context, userID string, apiKeyID string, now time.Time) (UserUsageToday, error) {
	if s == nil || s.db == nil {
		return UserUsageToday{}, ErrInvalidInput
	}
	now = now.UTC()
	if now.IsZero() {
		now = s.now().UTC()
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	counters, err := s.aggregateUsageRange(ctx, strings.TrimSpace(userID), strings.TrimSpace(apiKeyID), dayStart, dayEnd)
	if err != nil {
		return UserUsageToday{}, err
	}
	models, err := aggregateUsageDimensionsRange(ctx, s.db, strings.TrimSpace(userID), strings.TrimSpace(apiKeyID), dayStart, dayEnd)
	if err != nil {
		return UserUsageToday{}, err
	}
	return UserUsageToday{
		UserID:        strings.TrimSpace(userID),
		APIKeyID:      strings.TrimSpace(apiKeyID),
		Date:          dayStart.Format("2006-01-02"),
		Models:        models,
		UsageCounters: counters,
	}, nil
}

func (s *UserStore) GetUsageSnapshot(ctx context.Context, filter UsageSnapshotFilter, now time.Time, cfg UsageConfig) ([]ManagementUsageEntry, error) {
	if s == nil || s.db == nil {
		return nil, ErrInvalidInput
	}
	now = now.UTC()
	if now.IsZero() {
		now = s.now().UTC()
	}
	windowEnd := usageBucketStart(now).Add(usageBucketDuration)
	sevenDayStart := usageWindowStart(now, usageWeeklyBucketCount)

	query := `SELECT DISTINCT u.id, u.name, k.id, k.key_hash, k.masked_key
		FROM usage_buckets b
		JOIN users u ON u.id = b.user_id
		JOIN api_keys k ON k.id = b.api_key_id
		WHERE b.bucket_start >= ? AND b.bucket_start < ?`
	args := []any{formatDBTime(sevenDayStart), formatDBTime(windowEnd)}
	if strings.TrimSpace(filter.UserID) != "" {
		query += ` AND b.user_id = ?`
		args = append(args, strings.TrimSpace(filter.UserID))
	}
	if strings.TrimSpace(filter.APIKeyID) != "" {
		query += ` AND b.api_key_id = ?`
		args = append(args, strings.TrimSpace(filter.APIKeyID))
	}
	query += ` ORDER BY u.name COLLATE NOCASE ASC, k.created_at ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list usage snapshot identities: %w", err)
	}
	defer rows.Close()

	var identities []ManagementUsageEntry
	for rows.Next() {
		var entry ManagementUsageEntry
		if errScan := rows.Scan(&entry.UserID, &entry.Name, &entry.APIKeyID, &entry.KeyHash, &entry.MaskedKey); errScan != nil {
			return nil, fmt.Errorf("scan usage snapshot identity: %w", errScan)
		}
		identities = append(identities, entry)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("list usage snapshot rows: %w", err)
	}
	if err = rows.Close(); err != nil {
		return nil, fmt.Errorf("close usage snapshot rows: %w", err)
	}

	entries := make([]ManagementUsageEntry, 0, len(identities))
	for _, entry := range identities {
		entry.Windows = map[string]UsageWindow{}
		for _, spec := range usageWindowSpecs() {
			start := usageWindowStart(now, spec.bucketCount)
			counters, errAggregate := s.aggregateUsageRange(ctx, entry.UserID, entry.APIKeyID, start, windowEnd)
			if errAggregate != nil {
				return nil, errAggregate
			}
			entry.Windows[spec.name] = buildUsageWindow(counters)
		}
		models, errModels := s.usageDimensionsSnapshot(ctx, entry.UserID, entry.APIKeyID, sevenDayStart, windowEnd, now, cfg)
		if errModels != nil {
			return nil, errModels
		}
		entry.Models = models
		entries = append(entries, entry)
	}
	return entries, nil
}

func pruneUsageData(ctx context.Context, tx *sql.Tx, now time.Time) error {
	bucketCutoff := usageWindowStart(now, usageWeeklyBucketCount)
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_buckets WHERE bucket_start < ?`, formatDBTime(bucketCutoff)); err != nil {
		return fmt.Errorf("prune usage buckets: %w", err)
	}
	return nil
}

func (s *UserStore) aggregateUsageRange(ctx context.Context, userID string, apiKeyID string, start time.Time, end time.Time) (UsageCounters, error) {
	return aggregateUsageRangeDB(ctx, s.db, userID, apiKeyID, start, end, "", "", "")
}

type usageQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type usageRowsQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func aggregateUsageRangeDB(ctx context.Context, queryer usageQueryer, userID string, apiKeyID string, start time.Time, end time.Time, model string, reasoningEffort string, serviceTier string) (UsageCounters, error) {
	var counters UsageCounters
	query := `SELECT
			COALESCE(SUM(request_count), 0),
			COALESCE(SUM(failed_request_count), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(cached_input_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0),
			COALESCE(SUM(cache_creation_tokens), 0),
			COALESCE(SUM(total_tokens), 0)
		FROM usage_buckets
		WHERE user_id = ? AND api_key_id = ? AND bucket_start >= ? AND bucket_start < ?`
	args := []any{
		strings.TrimSpace(userID),
		strings.TrimSpace(apiKeyID),
		formatDBTime(start.UTC()),
		formatDBTime(end.UTC()),
	}
	if strings.TrimSpace(model) != "" {
		query += ` AND model = ?`
		args = append(args, strings.TrimSpace(model))
	}
	if strings.TrimSpace(reasoningEffort) != "" {
		query += ` AND reasoning_effort = ?`
		args = append(args, strings.TrimSpace(reasoningEffort))
	}
	if strings.TrimSpace(serviceTier) != "" {
		query += ` AND service_tier = ?`
		args = append(args, normalizeServiceTier(serviceTier))
	}
	err := queryer.QueryRowContext(ctx, query, args...).Scan(
		&counters.RequestCount,
		&counters.FailedRequestCount,
		&counters.InputTokens,
		&counters.OutputTokens,
		&counters.ReasoningTokens,
		&counters.CachedInputTokens,
		&counters.CacheReadTokens,
		&counters.CacheCreationTokens,
		&counters.TotalTokens,
	)
	if err != nil {
		return UsageCounters{}, fmt.Errorf("aggregate usage: %w", err)
	}
	return counters, nil
}

func (s *UserStore) usageDimensionsSnapshot(ctx context.Context, userID string, apiKeyID string, start time.Time, end time.Time, now time.Time, cfg UsageConfig) ([]UsageDimension, error) {
	dimensions, err := listUsageDimensionKeys(ctx, s.db, userID, apiKeyID, start, end)
	if err != nil {
		return nil, err
	}
	for i := range dimensions {
		dimensions[i].Windows = map[string]UsageWindow{}
		for _, spec := range usageWindowSpecs() {
			windowStart := usageWindowStart(now, spec.bucketCount)
			counters, errAggregate := aggregateUsageRangeDB(ctx, s.db, userID, apiKeyID, windowStart, end, dimensions[i].Model, dimensions[i].ReasoningEffort, dimensions[i].ServiceTier)
			if errAggregate != nil {
				return nil, errAggregate
			}
			dimensions[i].Windows[spec.name] = buildUsageWindow(counters)
		}
	}
	return dimensions, nil
}

func aggregateUsageDimensionsRange(ctx context.Context, queryer usageRowsQueryer, userID string, apiKeyID string, start time.Time, end time.Time) ([]UsageDimension, error) {
	rows, err := queryer.QueryContext(ctx,
		`SELECT
			model,
			reasoning_effort,
			service_tier,
			COALESCE(SUM(request_count), 0),
			COALESCE(SUM(failed_request_count), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(cached_input_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0),
			COALESCE(SUM(cache_creation_tokens), 0),
			COALESCE(SUM(total_tokens), 0)
		FROM usage_buckets
		WHERE user_id = ? AND api_key_id = ? AND bucket_start >= ? AND bucket_start < ?
		GROUP BY model, reasoning_effort, service_tier
		ORDER BY model ASC, reasoning_effort ASC, service_tier ASC`,
		strings.TrimSpace(userID),
		strings.TrimSpace(apiKeyID),
		formatDBTime(start.UTC()),
		formatDBTime(end.UTC()),
	)
	if err != nil {
		return nil, fmt.Errorf("aggregate usage dimensions: %w", err)
	}
	defer rows.Close()

	var dimensions []UsageDimension
	for rows.Next() {
		var dimension UsageDimension
		if errScan := rows.Scan(
			&dimension.Model,
			&dimension.ReasoningEffort,
			&dimension.ServiceTier,
			&dimension.RequestCount,
			&dimension.FailedRequestCount,
			&dimension.InputTokens,
			&dimension.OutputTokens,
			&dimension.ReasoningTokens,
			&dimension.CachedInputTokens,
			&dimension.CacheReadTokens,
			&dimension.CacheCreationTokens,
			&dimension.TotalTokens,
		); errScan != nil {
			return nil, fmt.Errorf("scan usage dimension: %w", errScan)
		}
		dimensions = append(dimensions, dimension)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("aggregate usage dimension rows: %w", err)
	}
	return dimensions, nil
}

func listUsageDimensionKeys(ctx context.Context, queryer usageRowsQueryer, userID string, apiKeyID string, start time.Time, end time.Time) ([]UsageDimension, error) {
	rows, err := queryer.QueryContext(ctx,
		`SELECT DISTINCT model, reasoning_effort, service_tier
		FROM usage_buckets
		WHERE user_id = ? AND api_key_id = ? AND bucket_start >= ? AND bucket_start < ?
		ORDER BY model ASC, reasoning_effort ASC, service_tier ASC`,
		strings.TrimSpace(userID),
		strings.TrimSpace(apiKeyID),
		formatDBTime(start.UTC()),
		formatDBTime(end.UTC()),
	)
	if err != nil {
		return nil, fmt.Errorf("list usage dimensions: %w", err)
	}
	defer rows.Close()

	var dimensions []UsageDimension
	for rows.Next() {
		var dimension UsageDimension
		if errScan := rows.Scan(&dimension.Model, &dimension.ReasoningEffort, &dimension.ServiceTier); errScan != nil {
			return nil, fmt.Errorf("scan usage dimension key: %w", errScan)
		}
		dimensions = append(dimensions, dimension)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("list usage dimension rows: %w", err)
	}
	return dimensions, nil
}

func usageWindowSpecs() []usageWindowSpec {
	return []usageWindowSpec{
		{
			name:        "5h",
			bucketCount: usageFiveHourBucketCount,
		},
		{
			name:        "7d",
			bucketCount: usageWeeklyBucketCount,
		},
	}
}

func buildUsageWindow(counters UsageCounters) UsageWindow {
	return UsageWindow{UsageCounters: counters}
}

func usageBucketStart(t time.Time) time.Time {
	return t.UTC().Truncate(usageBucketDuration)
}

func usageWindowStart(now time.Time, bucketCount int) time.Time {
	if bucketCount <= 1 {
		return usageBucketStart(now)
	}
	return usageBucketStart(now).Add(-time.Duration(bucketCount-1) * usageBucketDuration)
}

func normalizeUsageText(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func normalizeServiceTier(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "fast", "priority":
		return "fast"
	default:
		return "standard"
	}
}

func isFastServiceTier(value string) bool {
	return normalizeServiceTier(value) == "fast"
}

func (c UsageCounters) normalized() UsageCounters {
	if c.TotalTokens == 0 && (c.InputTokens > 0 || c.OutputTokens > 0) {
		c.TotalTokens = c.InputTokens + c.OutputTokens
	}
	return c
}

func (c UsageCounters) subtract(other UsageCounters) UsageCounters {
	return UsageCounters{
		RequestCount:        c.RequestCount - other.RequestCount,
		FailedRequestCount:  c.FailedRequestCount - other.FailedRequestCount,
		InputTokens:         c.InputTokens - other.InputTokens,
		OutputTokens:        c.OutputTokens - other.OutputTokens,
		ReasoningTokens:     c.ReasoningTokens - other.ReasoningTokens,
		CachedInputTokens:   c.CachedInputTokens - other.CachedInputTokens,
		CacheReadTokens:     c.CacheReadTokens - other.CacheReadTokens,
		CacheCreationTokens: c.CacheCreationTokens - other.CacheCreationTokens,
		TotalTokens:         c.TotalTokens - other.TotalTokens,
	}
}

func (c UsageCounters) isZero() bool {
	return c.RequestCount == 0 &&
		c.FailedRequestCount == 0 &&
		c.InputTokens == 0 &&
		c.OutputTokens == 0 &&
		c.ReasoningTokens == 0 &&
		c.CachedInputTokens == 0 &&
		c.CacheReadTokens == 0 &&
		c.CacheCreationTokens == 0 &&
		c.TotalTokens == 0
}

func extractUsageCounters(payload []byte) (UsageCounters, bool) {
	var found []UsageCounters
	if value, ok := decodeUsageJSON(payload); ok {
		collectUsageCounters(value, &found)
	}
	for _, line := range bytes.Split(payload, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		} else if !bytes.HasPrefix(line, []byte("{")) && !bytes.HasPrefix(line, []byte("[")) {
			continue
		}
		if bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		value, ok := decodeUsageJSON(line)
		if !ok {
			continue
		}
		collectUsageCounters(value, &found)
	}
	if len(found) == 0 {
		return UsageCounters{}, false
	}
	return found[len(found)-1].normalized(), true
}

func decodeUsageJSON(payload []byte) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	return value, true
}

func collectUsageCounters(value any, found *[]UsageCounters) {
	switch typed := value.(type) {
	case map[string]any:
		if usage, ok := typed["usage"]; ok {
			if counters, okCounters := usageCountersFromValue(usage); okCounters {
				*found = append(*found, counters.normalized())
			}
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if key == "usage" {
				continue
			}
			collectUsageCounters(typed[key], found)
		}
	case []any:
		for _, item := range typed {
			collectUsageCounters(item, found)
		}
	}
}

func usageCountersFromValue(value any) (UsageCounters, bool) {
	usage, ok := value.(map[string]any)
	if !ok {
		return UsageCounters{}, false
	}
	var counters UsageCounters
	var found bool
	setFirstInt64(&counters.InputTokens, &found, usage, "input_tokens", "prompt_tokens")
	setFirstInt64(&counters.OutputTokens, &found, usage, "output_tokens", "completion_tokens")
	setFirstInt64(&counters.TotalTokens, &found, usage, "total_tokens")
	setFirstInt64(&counters.ReasoningTokens, &found, usage, "reasoning_tokens")
	setFirstInt64(&counters.CachedInputTokens, &found, usage, "cached_input_tokens", "cached_tokens")
	setFirstInt64(&counters.CacheReadTokens, &found, usage, "cache_read_tokens")
	setFirstInt64(&counters.CacheCreationTokens, &found, usage, "cache_creation_tokens", "cache_creation_input_tokens")

	if details, okDetails := mapValue(usage["input_tokens_details"]); okDetails {
		setFirstInt64(&counters.CachedInputTokens, &found, details, "cached_tokens", "cached_input_tokens")
		setFirstInt64(&counters.CacheReadTokens, &found, details, "cache_read_tokens")
		setFirstInt64(&counters.CacheCreationTokens, &found, details, "cache_creation_tokens", "cache_creation_input_tokens")
	}
	if details, okDetails := mapValue(usage["prompt_tokens_details"]); okDetails {
		setFirstInt64(&counters.CachedInputTokens, &found, details, "cached_tokens", "cached_input_tokens")
		setFirstInt64(&counters.CacheReadTokens, &found, details, "cache_read_tokens")
		setFirstInt64(&counters.CacheCreationTokens, &found, details, "cache_creation_tokens", "cache_creation_input_tokens")
	}
	if details, okDetails := mapValue(usage["output_tokens_details"]); okDetails {
		setFirstInt64(&counters.ReasoningTokens, &found, details, "reasoning_tokens")
	}
	if details, okDetails := mapValue(usage["completion_tokens_details"]); okDetails {
		setFirstInt64(&counters.ReasoningTokens, &found, details, "reasoning_tokens")
	}
	if !found {
		return UsageCounters{}, false
	}
	return counters.normalized(), true
}

func setFirstInt64(target *int64, found *bool, values map[string]any, keys ...string) {
	for _, key := range keys {
		value, ok := values[key]
		if !ok {
			continue
		}
		parsed, ok := int64Value(value)
		if ok {
			*target = parsed
			*found = true
		}
		return
	}
}

func int64Value(value any) (int64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return parsed, true
		}
		floatValue, errFloat := strconv.ParseFloat(typed.String(), 64)
		if errFloat == nil {
			return int64(floatValue), true
		}
	case float64:
		return int64(typed), true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	}
	return 0, false
}

func mapValue(value any) (map[string]any, bool) {
	typed, ok := value.(map[string]any)
	return typed, ok
}
