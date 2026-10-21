package codexonly

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const grokAuthIDPrefix = "grok:"

type GrokAuthStatus struct {
	ID          string    `json:"id"`
	AccountID   string    `json:"account_id"`
	Email       string    `json:"email,omitempty"`
	SourceCount int       `json:"source_count"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`
	Refreshable bool      `json:"refreshable"`
	Expired     bool      `json:"expired"`
}

type GrokCredential struct {
	ID           string
	Path         string
	Scope        string
	AccountID    string
	Email        string
	AccessToken  string
	RefreshToken string
	Issuer       string
	ClientID     string
	ExpiresAt    time.Time
	CreateTime   string
	SourceCount  int
}

func (c *GrokCredential) Expired(now time.Time) bool {
	return c == nil || (!c.ExpiresAt.IsZero() && !c.ExpiresAt.After(now.Add(5*time.Minute)))
}

func (c *GrokCredential) canRefresh() bool {
	return c != nil && c.RefreshToken != "" && c.Issuer != "" && c.ClientID != ""
}

type GrokAuthStore struct {
	paths     []string
	next      atomic.Uint64
	now       func() time.Time
	mu        sync.Mutex
	refreshes map[string]*grokRefreshCall
	writeMu   sync.Mutex
}

type grokRefreshCall struct {
	done       chan struct{}
	credential *GrokCredential
	err        error
}

func NewGrokAuthStore(paths []string) *GrokAuthStore {
	return &GrokAuthStore{paths: append([]string(nil), paths...), now: time.Now}
}

func (s *GrokAuthStore) Select(ctx context.Context, excluded map[string]struct{}) (*GrokCredential, error) {
	credentials, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	eligible := make([]*GrokCredential, 0, len(credentials))
	for _, credential := range credentials {
		if _, skip := excluded[credential.ID]; skip || credential.AccessToken == "" || (credential.Expired(s.now()) && !credential.canRefresh()) {
			continue
		}
		eligible = append(eligible, credential)
	}
	if len(eligible) == 0 {
		return nil, fmt.Errorf("no active Grok auth available")
	}
	return eligible[int(s.next.Add(1)-1)%len(eligible)], nil
}

func (s *GrokAuthStore) Load(ctx context.Context) ([]*GrokCredential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	grouped := make(map[string]*GrokCredential)
	var firstErr error
	readable := false
	for _, path := range s.paths {
		credentials, err := readGrokAuthFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("read Grok auth %s: %w", path, err)
			}
			continue
		}
		readable = true
		for _, credential := range credentials {
			current := grouped[credential.ID]
			if current == nil || credential.newerThan(current) {
				if current != nil {
					credential.SourceCount += current.SourceCount
				}
				grouped[credential.ID] = credential
				continue
			}
			current.SourceCount++
		}
	}
	if !readable && firstErr != nil {
		return nil, firstErr
	}
	ids := make([]string, 0, len(grouped))
	for id := range grouped {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	credentials := make([]*GrokCredential, 0, len(ids))
	for _, id := range ids {
		credentials = append(credentials, grouped[id])
	}
	return credentials, nil
}

func (s *GrokAuthStore) Refresh(ctx context.Context, id string, client *http.Client, tokenURL string) (*GrokCredential, error) {
	return s.refresh(ctx, id, nil, client, tokenURL)
}

func (s *GrokAuthStore) refresh(ctx context.Context, id string, expected *GrokCredential, client *http.Client, tokenURL string) (*GrokCredential, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("Grok auth id is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultOAuthRefreshTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if call := s.refreshes[id]; call != nil {
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-call.done:
			return call.credential, call.err
		}
	}
	if s.refreshes == nil {
		s.refreshes = make(map[string]*grokRefreshCall)
	}
	call := &grokRefreshCall{done: make(chan struct{})}
	s.refreshes[id] = call
	s.mu.Unlock()

	credential, err := s.refreshCurrent(ctx, id, expected, client, tokenURL)
	s.mu.Lock()
	call.credential = credential
	call.err = err
	delete(s.refreshes, id)
	close(call.done)
	s.mu.Unlock()
	return credential, err
}

func (s *GrokAuthStore) refreshCurrent(ctx context.Context, id string, expected *GrokCredential, client *http.Client, tokenURL string) (*GrokCredential, error) {
	credentials, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	var current *GrokCredential
	for _, credential := range credentials {
		if credential.ID == id {
			current = credential
			break
		}
	}
	if current == nil {
		return nil, fmt.Errorf("Grok auth not found")
	}
	if expected != nil {
		changed := current.generation() != expected.generation() || !current.ExpiresAt.Equal(expected.ExpiresAt) ||
			current.Path != expected.Path || current.Scope != expected.Scope
		if changed {
			return current, nil
		}
	}
	if !current.canRefresh() {
		return nil, fmt.Errorf("Grok auth cannot be refreshed")
	}
	generation := current.generation()
	if strings.TrimSpace(tokenURL) == "" {
		tokenURL, err = discoverGrokTokenURL(ctx, client, current.Issuer)
		if err != nil {
			return nil, err
		}
	}
	if latest, err := grokAuthGenerationAt(current.Path, current.Scope); err != nil || latest != generation {
		return nil, fmt.Errorf("Grok auth changed before refresh completed")
	}
	refreshed, err := refreshGrokToken(ctx, client, tokenURL, current)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC().Truncate(time.Second)
	s.writeMu.Lock()
	err = updateGrokAuthRecord(current.Path, current.Scope, generation, refreshed, now)
	s.writeMu.Unlock()
	if err != nil {
		return nil, err
	}
	current.AccessToken = refreshed.AccessToken
	if refreshed.RefreshToken != "" {
		current.RefreshToken = refreshed.RefreshToken
	}
	if refreshed.ExpiresIn > 0 {
		current.ExpiresAt = now.Add(time.Duration(refreshed.ExpiresIn) * time.Second)
	}
	return current, nil
}

func (s *GrokAuthStore) SubscriptionUsage(ctx context.Context, id string, client *http.Client, baseURL string) (map[string]any, error) {
	credentials, err := s.Load(ctx)
	if err != nil {
		return nil, err
	}
	var current *GrokCredential
	for _, credential := range credentials {
		if credential.ID == id {
			current = credential
			break
		}
	}
	if current == nil {
		return nil, fmt.Errorf("Grok auth not found")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/billing?format=credits", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+current.AccessToken)
	applyGrokIdentityHeaders(req.Header, "")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err = json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode Grok subscription usage: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Grok subscription usage returned status %d", resp.StatusCode)
	}
	return payload, nil
}

func discoverGrokTokenURL(ctx context.Context, client *http.Client, issuer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var payload struct {
		TokenEndpoint string `json:"token_endpoint"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if err = validateDiscoveredGrokTokenURL(payload.TokenEndpoint, issuer); err != nil {
		return "", err
	}
	return payload.TokenEndpoint, nil
}

func validateDiscoveredGrokTokenURL(raw string, issuer string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	issuerURL, issuerErr := url.Parse(strings.TrimSpace(issuer))
	if err != nil || issuerErr != nil || parsed.Hostname() == "" || issuerURL.Hostname() == "" {
		return fmt.Errorf("Grok token endpoint is not allowed")
	}
	host := strings.ToLower(parsed.Hostname())
	sameIssuer := strings.EqualFold(host, issuerURL.Hostname())
	if !sameIssuer && host != "x.ai" && !strings.HasSuffix(host, ".x.ai") {
		return fmt.Errorf("Grok token endpoint host is not allowed")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if sameIssuer && parsed.Scheme == issuerURL.Scheme && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return nil
	}
	return fmt.Errorf("Grok token endpoint is not allowed")
}

func refreshGrokToken(ctx context.Context, client *http.Client, tokenURL string, credential *GrokCredential) (oauthRefreshResponse, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {credential.RefreshToken}, "client_id": {credential.ClientID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return oauthRefreshResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return oauthRefreshResponse{}, err
	}
	defer resp.Body.Close()
	var parsed oauthRefreshResponse
	if err = json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return oauthRefreshResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || strings.TrimSpace(parsed.AccessToken) == "" {
		return oauthRefreshResponse{}, fmt.Errorf("Grok token refresh failed with status %d", resp.StatusCode)
	}
	parsed.AccessToken = strings.TrimSpace(parsed.AccessToken)
	parsed.RefreshToken = strings.TrimSpace(parsed.RefreshToken)
	return parsed, nil
}

type grokAuthGeneration struct {
	accountID    string
	accessToken  string
	refreshToken string
	createTime   string
}

func (c *GrokCredential) generation() grokAuthGeneration {
	return grokAuthGeneration{
		accountID:    c.AccountID,
		accessToken:  c.AccessToken,
		refreshToken: c.RefreshToken,
		createTime:   c.CreateTime,
	}
}

func grokAuthGenerationAt(path string, scope string) (grokAuthGeneration, error) {
	record, err := grokAuthScopeRecord(path, scope)
	if err != nil {
		return grokAuthGeneration{}, err
	}
	return grokAuthGenerationFromRecord(record), nil
}

func grokAuthScopeRecord(path string, scope string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var scopes map[string]map[string]any
	if err = json.Unmarshal(raw, &scopes); err != nil {
		return nil, err
	}
	record := scopes[scope]
	if record == nil {
		return nil, fmt.Errorf("Grok auth scope not found")
	}
	return record, nil
}

func grokAuthGenerationFromRecord(record map[string]any) grokAuthGeneration {
	return grokAuthGeneration{
		accountID:    firstNonEmpty(stringField(record, "principal_id"), stringField(record, "team_id"), stringField(record, "user_id"), stringField(record, "email")),
		accessToken:  strings.TrimSpace(stringField(record, "key", "access_token")),
		refreshToken: strings.TrimSpace(stringField(record, "refresh_token")),
		createTime:   strings.TrimSpace(stringField(record, "create_time")),
	}
}

func updateGrokAuthRecord(path string, scope string, expected grokAuthGeneration, refreshed oauthRefreshResponse, now time.Time) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var scopes map[string]map[string]any
	if err = json.Unmarshal(raw, &scopes); err != nil {
		return err
	}
	record := scopes[scope]
	if record == nil {
		return fmt.Errorf("Grok auth scope not found")
	}
	if grokAuthGenerationFromRecord(record) != expected {
		return fmt.Errorf("Grok auth changed before refresh completed")
	}
	record["key"] = refreshed.AccessToken
	if refreshed.RefreshToken != "" {
		record["refresh_token"] = refreshed.RefreshToken
	}
	if refreshed.ExpiresIn > 0 {
		record["expires_at"] = now.Add(time.Duration(refreshed.ExpiresIn) * time.Second).UTC().Format(time.RFC3339)
	}
	encoded, err := json.MarshalIndent(scopes, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".grok-auth-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err = temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err = temp.Write(append(encoded, '\n')); err != nil {
		_ = temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func (c *GrokCredential) newerThan(other *GrokCredential) bool {
	if c == nil || other == nil {
		return c != nil
	}
	if !c.ExpiresAt.Equal(other.ExpiresAt) {
		return c.ExpiresAt.After(other.ExpiresAt)
	}
	return c.Path < other.Path
}

func readGrokAuthFile(path string) ([]*GrokCredential, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var scopes map[string]map[string]any
	if err = json.Unmarshal(raw, &scopes); err != nil {
		return nil, err
	}
	credentials := make([]*GrokCredential, 0, len(scopes))
	for scope, record := range scopes {
		credential, ok := grokCredentialFromRecord(path, scope, record)
		if ok {
			credentials = append(credentials, credential)
		}
	}
	return credentials, nil
}

func grokCredentialFromRecord(path string, scope string, record map[string]any) (*GrokCredential, bool) {
	mode := strings.ToLower(strings.TrimSpace(stringField(record, "auth_mode")))
	issuer := strings.TrimRight(strings.TrimSpace(stringField(record, "oidc_issuer")), "/")
	accessToken := strings.TrimSpace(stringField(record, "key", "access_token"))
	if accessToken == "" || mode == "api_key" || strings.EqualFold(scope, "xai::api_key") {
		return nil, false
	}
	legacy := scope == "https://accounts.x.ai/sign-in" || mode == "web_login" || mode == "grok"
	firstParty := issuer == "https://auth.x.ai" || issuer == "http://localhost:22255"
	if !legacy && !firstParty && !strings.HasPrefix(issuer, "http://127.0.0.1:") && !strings.HasPrefix(issuer, "http://localhost:") {
		return nil, false
	}
	accountID := firstNonEmpty(
		stringField(record, "principal_id"),
		stringField(record, "team_id"),
		stringField(record, "user_id"),
		stringField(record, "email"),
	)
	if accountID == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(strings.ToLower(accountID)))
	return &GrokCredential{
		ID:           grokAuthIDPrefix + hex.EncodeToString(sum[:]),
		Path:         path,
		Scope:        scope,
		AccountID:    accountID,
		Email:        stringField(record, "email"),
		AccessToken:  accessToken,
		RefreshToken: stringField(record, "refresh_token"),
		Issuer:       issuer,
		ClientID:     stringField(record, "oidc_client_id"),
		ExpiresAt:    grokExpiry(record, accessToken),
		CreateTime:   strings.TrimSpace(stringField(record, "create_time")),
		SourceCount:  1,
	}, true
}

func grokExpiry(record map[string]any, accessToken string) time.Time {
	if expiresAt := timeField(record, "expires_at", "expired"); !expiresAt.IsZero() {
		return expiresAt
	}
	if created := timeField(record, "create_time"); !created.IsZero() {
		return created.Add(30 * 24 * time.Hour)
	}
	return tokenExpiry(accessToken)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
