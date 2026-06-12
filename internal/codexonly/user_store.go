package codexonly

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const apiKeyPrefix = "cop_"

var (
	ErrInvalidInput       = errors.New("invalid input")
	ErrDuplicateUserName  = errors.New("duplicate user name")
	ErrUserNotFound       = errors.New("user not found")
	ErrInvalidAPIKey      = errors.New("invalid API key")
	ErrDisabledCredential = errors.New("disabled credential")
)

type UserStore struct {
	db  *sql.DB
	now func() time.Time
}

type CreateUserParams struct {
	Name    string
	Enabled *bool
}

type UpdateUserParams struct {
	Name    *string
	Enabled *bool
}

type UserRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type APIKeyRecord struct {
	ID         string     `json:"id"`
	UserID     string     `json:"user_id"`
	KeyHash    string     `json:"-"`
	KeyPrefix  string     `json:"key_prefix"`
	MaskedKey  string     `json:"masked_key"`
	Enabled    bool       `json:"enabled"`
	CreatedAt  time.Time  `json:"created_at"`
	RotatedAt  *time.Time `json:"rotated_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

type UserWithAPIKey struct {
	User   UserRecord    `json:"user"`
	APIKey *APIKeyRecord `json:"api_key,omitempty"`
}

type CreatedUserAPIKey struct {
	User            UserRecord   `json:"user"`
	APIKey          APIKeyRecord `json:"api_key"`
	PlaintextAPIKey string       `json:"api_key_value"`
}

type AuthenticatedAPIKey struct {
	User   UserRecord   `json:"user"`
	APIKey APIKeyRecord `json:"api_key"`
}

func OpenUserStore(ctx context.Context, path string) (*UserStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("open user store: %w", ErrInvalidInput)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &UserStore{
		db: db,
		now: func() time.Time {
			return time.Now().UTC()
		},
	}
	if err = store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *UserStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *UserStore) migrate(ctx context.Context) error {
	tableStatements := []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL COLLATE NOCASE UNIQUE,
			enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			key_hash TEXT NOT NULL UNIQUE,
			key_prefix TEXT NOT NULL,
			masked_key TEXT NOT NULL,
			enabled INTEGER NOT NULL CHECK (enabled IN (0, 1)),
			created_at TEXT NOT NULL,
			rotated_at TEXT,
			last_used_at TEXT,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS usage_buckets (
			bucket_start TEXT NOT NULL,
			user_id TEXT NOT NULL,
			api_key_id TEXT NOT NULL,
			key_hash TEXT NOT NULL,
			masked_key TEXT NOT NULL,
			model TEXT NOT NULL,
			reasoning_effort TEXT NOT NULL DEFAULT 'unknown',
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
			PRIMARY KEY (bucket_start, user_id, api_key_id, model, reasoning_effort, auth_id),
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
			FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS usage_threshold_state (
			window TEXT NOT NULL,
			api_key_id TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT 'unknown',
			reasoning_effort TEXT NOT NULL DEFAULT 'unknown',
			above_threshold INTEGER NOT NULL CHECK (above_threshold IN (0, 1)),
			updated_at TEXT NOT NULL,
			PRIMARY KEY (window, api_key_id, model, reasoning_effort),
			FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS usage_threshold_events (
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
			reasoning_effort TEXT NOT NULL DEFAULT 'unknown',
			auth_id TEXT NOT NULL,
			request_id TEXT NOT NULL,
			diagnostics TEXT NOT NULL,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
			FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE CASCADE
		)`,
	}
	for _, statement := range tableStatements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate user store: %w", err)
		}
	}
	if err := s.migrateUsageReasoningEffort(ctx); err != nil {
		return err
	}
	indexStatements := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_api_keys_one_active_per_user
			ON api_keys(user_id) WHERE enabled = 1`,
		`CREATE INDEX IF NOT EXISTS idx_api_keys_user_id ON api_keys(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_buckets_user_key_time
			ON usage_buckets(user_id, api_key_id, bucket_start)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_buckets_time ON usage_buckets(bucket_start)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_threshold_events_time
			ON usage_threshold_events(timestamp)`,
	}
	for _, statement := range indexStatements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate user store: %w", err)
		}
	}
	return nil
}

func (s *UserStore) migrateUsageReasoningEffort(ctx context.Context) error {
	if err := s.migrateUsageBucketsReasoningEffort(ctx); err != nil {
		return err
	}
	if err := s.migrateUsageThresholdStateReasoningEffort(ctx); err != nil {
		return err
	}
	if err := s.migrateUsageThresholdEventsReasoningEffort(ctx); err != nil {
		return err
	}
	return nil
}

func (s *UserStore) migrateUsageBucketsReasoningEffort(ctx context.Context) error {
	hasColumn, err := tableColumnExists(ctx, s.db, "usage_buckets", "reasoning_effort")
	if err != nil {
		return err
	}
	if hasColumn {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin usage_buckets reasoning_effort migration: %w", err)
	}
	defer rollbackUnlessCommitted(tx)

	statements := []string{
		`DROP TABLE IF EXISTS usage_buckets_before_reasoning_effort`,
		`ALTER TABLE usage_buckets RENAME TO usage_buckets_before_reasoning_effort`,
		`CREATE TABLE usage_buckets (
			bucket_start TEXT NOT NULL,
			user_id TEXT NOT NULL,
			api_key_id TEXT NOT NULL,
			key_hash TEXT NOT NULL,
			masked_key TEXT NOT NULL,
			model TEXT NOT NULL,
			reasoning_effort TEXT NOT NULL DEFAULT 'unknown',
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
			PRIMARY KEY (bucket_start, user_id, api_key_id, model, reasoning_effort, auth_id),
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
			FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE CASCADE
		)`,
		`INSERT INTO usage_buckets (
			bucket_start, user_id, api_key_id, key_hash, masked_key, model, reasoning_effort, auth_id,
			request_count, failed_request_count, input_tokens, output_tokens, reasoning_tokens,
			cached_input_tokens, cache_read_tokens, cache_creation_tokens, total_tokens, updated_at
		)
		SELECT
			bucket_start, user_id, api_key_id, MAX(key_hash), MAX(masked_key), model, 'unknown', auth_id,
			COALESCE(SUM(request_count), 0),
			COALESCE(SUM(failed_request_count), 0),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0),
			COALESCE(SUM(cached_input_tokens), 0),
			COALESCE(SUM(cache_read_tokens), 0),
			COALESCE(SUM(cache_creation_tokens), 0),
			COALESCE(SUM(total_tokens), 0),
			MAX(updated_at)
		FROM usage_buckets_before_reasoning_effort
		GROUP BY bucket_start, user_id, api_key_id, model, auth_id`,
		`DROP TABLE usage_buckets_before_reasoning_effort`,
	}
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate usage_buckets reasoning_effort: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit usage_buckets reasoning_effort migration: %w", err)
	}
	return nil
}

func (s *UserStore) migrateUsageThresholdStateReasoningEffort(ctx context.Context) error {
	hasModel, err := tableColumnExists(ctx, s.db, "usage_threshold_state", "model")
	if err != nil {
		return err
	}
	hasEffort, err := tableColumnExists(ctx, s.db, "usage_threshold_state", "reasoning_effort")
	if err != nil {
		return err
	}
	if hasModel && hasEffort {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin usage_threshold_state reasoning_effort migration: %w", err)
	}
	defer rollbackUnlessCommitted(tx)

	statements := []string{
		`DROP TABLE IF EXISTS usage_threshold_state_before_reasoning_effort`,
		`ALTER TABLE usage_threshold_state RENAME TO usage_threshold_state_before_reasoning_effort`,
		`CREATE TABLE usage_threshold_state (
			window TEXT NOT NULL,
			api_key_id TEXT NOT NULL,
			model TEXT NOT NULL DEFAULT 'unknown',
			reasoning_effort TEXT NOT NULL DEFAULT 'unknown',
			above_threshold INTEGER NOT NULL CHECK (above_threshold IN (0, 1)),
			updated_at TEXT NOT NULL,
			PRIMARY KEY (window, api_key_id, model, reasoning_effort),
			FOREIGN KEY (api_key_id) REFERENCES api_keys(id) ON DELETE CASCADE
		)`,
		`INSERT INTO usage_threshold_state (window, api_key_id, model, reasoning_effort, above_threshold, updated_at)
		SELECT window, api_key_id, 'unknown', 'unknown', above_threshold, updated_at
		FROM usage_threshold_state_before_reasoning_effort`,
		`DROP TABLE usage_threshold_state_before_reasoning_effort`,
	}
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate usage_threshold_state reasoning_effort: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit usage_threshold_state reasoning_effort migration: %w", err)
	}
	return nil
}

func (s *UserStore) migrateUsageThresholdEventsReasoningEffort(ctx context.Context) error {
	hasColumn, err := tableColumnExists(ctx, s.db, "usage_threshold_events", "reasoning_effort")
	if err != nil {
		return err
	}
	if hasColumn {
		return nil
	}
	if _, err = s.db.ExecContext(ctx, `ALTER TABLE usage_threshold_events ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT 'unknown'`); err != nil {
		return fmt.Errorf("migrate usage_threshold_events reasoning_effort: %w", err)
	}
	return nil
}

func tableColumnExists(ctx context.Context, db *sql.DB, table string, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, fmt.Errorf("read table info for %s: %w", table, err)
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
			return false, fmt.Errorf("scan table info for %s: %w", table, err)
		}
		if name == column {
			return true, nil
		}
	}
	if err = rows.Err(); err != nil {
		return false, fmt.Errorf("read table info rows for %s: %w", table, err)
	}
	return false, nil
}

func (s *UserStore) CreateUser(ctx context.Context, params CreateUserParams) (CreatedUserAPIKey, error) {
	name, err := normalizeUserName(params.Name)
	if err != nil {
		return CreatedUserAPIKey{}, err
	}
	enabled := true
	if params.Enabled != nil {
		enabled = *params.Enabled
	}
	plainKey, err := generateAPIKey()
	if err != nil {
		return CreatedUserAPIKey{}, err
	}
	userID, err := randomID("usr")
	if err != nil {
		return CreatedUserAPIKey{}, err
	}
	apiKeyID, err := randomID("key")
	if err != nil {
		return CreatedUserAPIKey{}, err
	}
	now := s.now().UTC()
	user := UserRecord{
		ID:        userID,
		Name:      name,
		Enabled:   enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	key := APIKeyRecord{
		ID:        apiKeyID,
		UserID:    userID,
		KeyHash:   hashAPIKey(plainKey),
		KeyPrefix: displayKeyPrefix(plainKey),
		MaskedKey: maskAPIKey(plainKey),
		Enabled:   true,
		CreatedAt: now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreatedUserAPIKey{}, fmt.Errorf("begin create user: %w", err)
	}
	defer rollbackUnlessCommitted(tx)

	_, err = tx.ExecContext(ctx,
		`INSERT INTO users (id, name, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		user.ID, user.Name, boolInt(user.Enabled), formatDBTime(user.CreatedAt), formatDBTime(user.UpdatedAt),
	)
	if err != nil {
		return CreatedUserAPIKey{}, mapSQLiteError(err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO api_keys (id, user_id, key_hash, key_prefix, masked_key, enabled, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		key.ID, key.UserID, key.KeyHash, key.KeyPrefix, key.MaskedKey, boolInt(key.Enabled), formatDBTime(key.CreatedAt),
	)
	if err != nil {
		return CreatedUserAPIKey{}, mapSQLiteError(err)
	}
	if err = tx.Commit(); err != nil {
		return CreatedUserAPIKey{}, fmt.Errorf("commit create user: %w", err)
	}
	return CreatedUserAPIKey{User: user, APIKey: key, PlaintextAPIKey: plainKey}, nil
}

func (s *UserStore) ListUsers(ctx context.Context, enabled *bool) ([]UserWithAPIKey, error) {
	query := userWithKeySelect() + ` ORDER BY u.name COLLATE NOCASE ASC`
	args := []any{}
	if enabled != nil {
		query = userWithKeySelect() + ` WHERE u.enabled = ? ORDER BY u.name COLLATE NOCASE ASC`
		args = append(args, boolInt(*enabled))
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []UserWithAPIKey
	for rows.Next() {
		user, errScan := scanUserWithAPIKey(rows)
		if errScan != nil {
			return nil, errScan
		}
		users = append(users, user)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("list users rows: %w", err)
	}
	return users, nil
}

func (s *UserStore) GetUser(ctx context.Context, id string) (UserWithAPIKey, error) {
	row := s.db.QueryRowContext(ctx, userWithKeySelect()+` WHERE u.id = ?`, strings.TrimSpace(id))
	user, err := scanUserWithAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return UserWithAPIKey{}, ErrUserNotFound
	}
	return user, err
}

func (s *UserStore) UpdateUser(ctx context.Context, id string, params UpdateUserParams) (UserWithAPIKey, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return UserWithAPIKey{}, ErrUserNotFound
	}
	current, err := s.GetUser(ctx, id)
	if err != nil {
		return UserWithAPIKey{}, err
	}
	name := current.User.Name
	if params.Name != nil {
		name, err = normalizeUserName(*params.Name)
		if err != nil {
			return UserWithAPIKey{}, err
		}
	}
	enabled := current.User.Enabled
	if params.Enabled != nil {
		enabled = *params.Enabled
	}
	_, err = s.db.ExecContext(ctx,
		`UPDATE users SET name = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		name, boolInt(enabled), formatDBTime(s.now().UTC()), id,
	)
	if err != nil {
		return UserWithAPIKey{}, mapSQLiteError(err)
	}
	return s.GetUser(ctx, id)
}

func (s *UserStore) ResetUserAPIKey(ctx context.Context, userID string) (CreatedUserAPIKey, error) {
	user, err := s.GetUser(ctx, userID)
	if err != nil {
		return CreatedUserAPIKey{}, err
	}
	plainKey, err := generateAPIKey()
	if err != nil {
		return CreatedUserAPIKey{}, err
	}
	apiKeyID, err := randomID("key")
	if err != nil {
		return CreatedUserAPIKey{}, err
	}
	now := s.now().UTC()
	key := APIKeyRecord{
		ID:        apiKeyID,
		UserID:    user.User.ID,
		KeyHash:   hashAPIKey(plainKey),
		KeyPrefix: displayKeyPrefix(plainKey),
		MaskedKey: maskAPIKey(plainKey),
		Enabled:   true,
		CreatedAt: now,
		RotatedAt: &now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CreatedUserAPIKey{}, fmt.Errorf("begin reset api key: %w", err)
	}
	defer rollbackUnlessCommitted(tx)

	_, err = tx.ExecContext(ctx, `UPDATE api_keys SET enabled = 0 WHERE user_id = ? AND enabled = 1`, user.User.ID)
	if err != nil {
		return CreatedUserAPIKey{}, fmt.Errorf("disable previous api keys: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO api_keys (id, user_id, key_hash, key_prefix, masked_key, enabled, created_at, rotated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		key.ID, key.UserID, key.KeyHash, key.KeyPrefix, key.MaskedKey, boolInt(key.Enabled), formatDBTime(key.CreatedAt), formatDBTime(*key.RotatedAt),
	)
	if err != nil {
		return CreatedUserAPIKey{}, mapSQLiteError(err)
	}
	if err = tx.Commit(); err != nil {
		return CreatedUserAPIKey{}, fmt.Errorf("commit reset api key: %w", err)
	}
	return CreatedUserAPIKey{User: user.User, APIKey: key, PlaintextAPIKey: plainKey}, nil
}

func (s *UserStore) AuthenticateAPIKey(ctx context.Context, key string) (AuthenticatedAPIKey, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return AuthenticatedAPIKey{}, ErrInvalidAPIKey
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT
			u.id, u.name, u.enabled, u.created_at, u.updated_at,
			k.id, k.user_id, k.key_hash, k.key_prefix, k.masked_key, k.enabled, k.created_at, k.rotated_at, k.last_used_at
		FROM api_keys k
		JOIN users u ON u.id = k.user_id
		WHERE k.key_hash = ?`,
		hashAPIKey(key),
	)
	user, apiKey, err := scanAuthRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthenticatedAPIKey{}, ErrInvalidAPIKey
	}
	if err != nil {
		return AuthenticatedAPIKey{}, err
	}
	if !apiKey.Enabled {
		return AuthenticatedAPIKey{}, ErrInvalidAPIKey
	}
	if !user.Enabled {
		return AuthenticatedAPIKey{}, ErrDisabledCredential
	}
	now := s.now().UTC()
	_, err = s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, formatDBTime(now), apiKey.ID)
	if err != nil {
		return AuthenticatedAPIKey{}, fmt.Errorf("update api key last used: %w", err)
	}
	apiKey.LastUsedAt = &now
	return AuthenticatedAPIKey{User: user, APIKey: apiKey}, nil
}

type userKeyScanner interface {
	Scan(dest ...any) error
}

func userWithKeySelect() string {
	return `SELECT
		u.id, u.name, u.enabled, u.created_at, u.updated_at,
		k.id, k.user_id, k.key_hash, k.key_prefix, k.masked_key, k.enabled, k.created_at, k.rotated_at, k.last_used_at
	FROM users u
	LEFT JOIN api_keys k ON k.user_id = u.id AND k.enabled = 1`
}

func scanUserWithAPIKey(scanner userKeyScanner) (UserWithAPIKey, error) {
	user, apiKey, err := scanJoinedUserAPIKey(scanner)
	if err != nil {
		return UserWithAPIKey{}, err
	}
	return UserWithAPIKey{User: user, APIKey: apiKey}, nil
}

func scanJoinedUserAPIKey(scanner userKeyScanner) (UserRecord, *APIKeyRecord, error) {
	var user UserRecord
	var userEnabled int
	var userCreated string
	var userUpdated string
	var keyID sql.NullString
	var keyUserID sql.NullString
	var keyHash sql.NullString
	var keyPrefix sql.NullString
	var maskedKey sql.NullString
	var keyEnabled sql.NullInt64
	var keyCreated sql.NullString
	var rotatedAt sql.NullString
	var lastUsedAt sql.NullString
	err := scanner.Scan(
		&user.ID,
		&user.Name,
		&userEnabled,
		&userCreated,
		&userUpdated,
		&keyID,
		&keyUserID,
		&keyHash,
		&keyPrefix,
		&maskedKey,
		&keyEnabled,
		&keyCreated,
		&rotatedAt,
		&lastUsedAt,
	)
	if err != nil {
		return UserRecord{}, nil, err
	}
	user.Enabled = userEnabled == 1
	user.CreatedAt, err = parseDBTime(userCreated)
	if err != nil {
		return UserRecord{}, nil, err
	}
	user.UpdatedAt, err = parseDBTime(userUpdated)
	if err != nil {
		return UserRecord{}, nil, err
	}
	if !keyID.Valid {
		return user, nil, nil
	}
	apiKey := &APIKeyRecord{
		ID:        keyID.String,
		UserID:    keyUserID.String,
		KeyHash:   keyHash.String,
		KeyPrefix: keyPrefix.String,
		MaskedKey: maskedKey.String,
		Enabled:   keyEnabled.Int64 == 1,
	}
	apiKey.CreatedAt, err = parseDBTime(keyCreated.String)
	if err != nil {
		return UserRecord{}, nil, err
	}
	if rotatedAt.Valid {
		parsed, errParse := parseDBTime(rotatedAt.String)
		if errParse != nil {
			return UserRecord{}, nil, errParse
		}
		apiKey.RotatedAt = &parsed
	}
	if lastUsedAt.Valid {
		parsed, errParse := parseDBTime(lastUsedAt.String)
		if errParse != nil {
			return UserRecord{}, nil, errParse
		}
		apiKey.LastUsedAt = &parsed
	}
	return user, apiKey, nil
}

func scanAuthRow(scanner userKeyScanner) (UserRecord, APIKeyRecord, error) {
	user, apiKey, err := scanJoinedUserAPIKey(scanner)
	if err != nil {
		return UserRecord{}, APIKeyRecord{}, err
	}
	if apiKey == nil {
		return UserRecord{}, APIKeyRecord{}, sql.ErrNoRows
	}
	return user, *apiKey, nil
}

func normalizeUserName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrInvalidInput
	}
	return name, nil
}

func randomID(prefix string) (string, error) {
	token, err := randomToken(16)
	if err != nil {
		return "", err
	}
	return prefix + "_" + token, nil
}

func generateAPIKey() (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	return apiKeyPrefix + token, nil
}

func randomToken(byteCount int) (string, error) {
	buf := make([]byte, byteCount)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func displayKeyPrefix(key string) string {
	if len(key) <= 12 {
		return key
	}
	return key[:12]
}

func maskAPIKey(key string) string {
	if len(key) <= 18 {
		return displayKeyPrefix(key) + "..."
	}
	return key[:11] + "..." + key[len(key)-6:]
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func formatDBTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseDBTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse database time %q: %w", value, err)
	}
	return parsed, nil
}

func rollbackUnlessCommitted(tx *sql.Tx) {
	_ = tx.Rollback()
}

func mapSQLiteError(err error) error {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "unique") && strings.Contains(message, "users.name") {
		return ErrDuplicateUserName
	}
	if strings.Contains(message, "constraint failed") || strings.Contains(message, "constraint") {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return err
}
