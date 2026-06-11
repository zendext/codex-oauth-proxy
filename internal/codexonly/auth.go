package codexonly

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Auth struct {
	ID           string
	Path         string
	Email        string
	AccountID    string
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time
	Metadata     map[string]any
	codexCLI     bool
}

func (a *Auth) Expired(now time.Time) bool {
	if a == nil {
		return true
	}
	if a.ExpiresAt.IsZero() {
		return false
	}
	return !a.ExpiresAt.After(now.Add(5 * time.Minute))
}

func (a *Auth) Save() error {
	if a == nil {
		return fmt.Errorf("auth is nil")
	}
	if strings.TrimSpace(a.Path) == "" {
		return fmt.Errorf("auth path is empty")
	}
	data := make(map[string]any, len(a.Metadata)+8)
	for k, v := range a.Metadata {
		data[k] = v
	}
	if a.codexCLI || mapField(data, "tokens") != nil {
		tokens := mapField(data, "tokens")
		if tokens == nil {
			tokens = make(map[string]any)
			data["tokens"] = tokens
		}
		tokens["access_token"] = a.AccessToken
		tokens["refresh_token"] = a.RefreshToken
		if a.IDToken != "" {
			tokens["id_token"] = a.IDToken
		}
		if a.AccountID != "" {
			tokens["account_id"] = a.AccountID
		}
	} else {
		data["type"] = "codex"
		data["access_token"] = a.AccessToken
		data["refresh_token"] = a.RefreshToken
		if a.IDToken != "" {
			data["id_token"] = a.IDToken
		}
		if a.AccountID != "" {
			data["account_id"] = a.AccountID
		}
		if a.Email != "" {
			data["email"] = a.Email
		}
		if !a.ExpiresAt.IsZero() {
			data["expired"] = a.ExpiresAt.UTC().Format(time.RFC3339)
		}
	}
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal auth: %w", err)
	}
	if err = os.MkdirAll(filepath.Dir(a.Path), 0o700); err != nil {
		return fmt.Errorf("create auth dir: %w", err)
	}
	if err = os.WriteFile(a.Path, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("write auth: %w", err)
	}
	a.Metadata = data
	a.codexCLI = mapField(data, "tokens") != nil
	return nil
}

type FileAuthStore struct {
	Dir string
}

func NewFileAuthStore(dir string) *FileAuthStore {
	return &FileAuthStore{Dir: dir}
}

func (s *FileAuthStore) Load(ctx context.Context) ([]*Auth, error) {
	if s == nil {
		return nil, fmt.Errorf("auth store is nil")
	}
	dir, err := ResolveAuthDir(s.Dir)
	if err != nil {
		return nil, err
	}
	auths := make([]*Auth, 0)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return nil
		}
		auth, errRead := readAuthFile(path, dir)
		if errRead != nil || auth == nil {
			return nil
		}
		auths = append(auths, auth)
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load auths: %w", err)
	}
	sort.Slice(auths, func(i, j int) bool {
		return auths[i].ID < auths[j].ID
	})
	return auths, nil
}

func readAuthFile(path string, baseDir string) (*Auth, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var meta map[string]any
	if err = json.Unmarshal(raw, &meta); err != nil {
		return nil, err
	}
	provider, _ := meta["type"].(string)
	tokens := mapField(meta, "tokens")
	if strings.TrimSpace(provider) != "" && !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return nil, nil
	}
	if strings.TrimSpace(provider) == "" && tokens == nil {
		return nil, nil
	}
	if disabled, _ := meta["disabled"].(bool); disabled {
		return nil, nil
	}
	accessToken := stringField(meta, "access_token", "accessToken")
	refreshToken := stringField(meta, "refresh_token", "refreshToken")
	idToken := stringField(meta, "id_token", "idToken")
	accountID := stringField(meta, "account_id", "accountID")
	expiresAt := timeField(meta, "expired", "expire", "expires_at", "expiresAt", "expiry", "expires")
	if tokens != nil {
		if accessToken == "" {
			accessToken = stringField(tokens, "access_token", "accessToken")
		}
		if refreshToken == "" {
			refreshToken = stringField(tokens, "refresh_token", "refreshToken")
		}
		if idToken == "" {
			idToken = stringField(tokens, "id_token", "idToken")
		}
		if accountID == "" {
			accountID = stringField(tokens, "account_id", "accountID")
		}
	}
	if accessToken == "" && refreshToken == "" {
		return nil, nil
	}
	if expiresAt.IsZero() {
		expiresAt = tokenExpiry(accessToken)
	}
	id := path
	if rel, errRel := filepath.Rel(baseDir, path); errRel == nil && rel != "" {
		id = rel
	}
	auth := &Auth{
		ID:           id,
		Path:         path,
		Email:        stringField(meta, "email"),
		AccountID:    accountID,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		IDToken:      idToken,
		ExpiresAt:    expiresAt,
		Metadata:     meta,
		codexCLI:     tokens != nil,
	}
	return auth, nil
}

func mapField(meta map[string]any, key string) map[string]any {
	raw, ok := meta[key]
	if !ok {
		return nil
	}
	value, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	return value
}

func stringField(meta map[string]any, keys ...string) string {
	for _, key := range keys {
		if raw, ok := meta[key]; ok {
			if value, okString := raw.(string); okString {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

func timeField(meta map[string]any, keys ...string) time.Time {
	for _, key := range keys {
		raw, ok := meta[key]
		if !ok {
			continue
		}
		switch value := raw.(type) {
		case string:
			trimmed := strings.TrimSpace(value)
			if trimmed == "" {
				continue
			}
			if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
				return parsed
			}
		case float64:
			if value > 0 {
				return time.Unix(int64(value), 0).UTC()
			}
		}
	}
	return time.Time{}
}

func tokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims map[string]any
	if err = json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}
	}
	expiry, ok := claims["exp"].(float64)
	if !ok || expiry <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(expiry), 0).UTC()
}
