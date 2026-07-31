package codexonly

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const DefaultAuthParseErrorGrace = 5 * time.Second

const (
	accountAuthIDPrefix = "account:"
	pathAuthIDPrefix    = "path:"
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
	Disabled     bool
	SourcePaths  []string
	codexCLI     bool
	relativePath string
	sourceIDs    []string
	renameFile   func(string, string) error
}

func (a *Auth) LastRefreshAt() time.Time {
	if a == nil {
		return time.Time{}
	}
	return timeField(a.Metadata, "last_refresh", "last_refresh_at", "lastRefreshAt")
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

func (a *Auth) Identified() bool {
	return a != nil && strings.TrimSpace(a.AccountID) != ""
}

func (a *Auth) ManageableAccountID() (string, bool) {
	if !a.Identified() {
		return "", false
	}
	return a.AccountID, true
}

func (a *Auth) SourceCount() int {
	if a == nil {
		return 0
	}
	return len(a.SourcePaths)
}

func (a *Auth) ReparseIdentity() {
	if a == nil {
		return
	}
	idClaims := tokenIdentityClaims(a.IDToken)
	accessClaims := tokenIdentityClaims(a.AccessToken)
	if accessClaims.AccountID != "" {
		a.AccountID = accessClaims.AccountID
	} else if idClaims.AccountID != "" {
		a.AccountID = idClaims.AccountID
	}
	if accessClaims.Email != "" {
		a.Email = accessClaims.Email
	} else if idClaims.Email != "" {
		a.Email = idClaims.Email
	}
	a.ID = stableAuthID(a.AccountID, a.relativePath)
}

func (a *Auth) Save() error {
	if a == nil {
		return fmt.Errorf("auth is nil")
	}
	if strings.TrimSpace(a.Path) == "" {
		return fmt.Errorf("auth path is empty")
	}
	data := cloneMap(a.Metadata)
	data["disabled"] = a.Disabled
	if a.codexCLI || mapField(data, "tokens") != nil {
		for _, key := range []string{
			"access_token", "accessToken",
			"refresh_token", "refreshToken",
			"id_token", "idToken",
			"account_id", "accountID",
		} {
			delete(data, key)
		}
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
		delete(tokens, "accountID")
		if a.AccountID != "" {
			tokens["account_id"] = a.AccountID
		} else {
			delete(tokens, "account_id")
		}
	} else {
		data["type"] = "codex"
		data["access_token"] = a.AccessToken
		data["refresh_token"] = a.RefreshToken
		if a.IDToken != "" {
			data["id_token"] = a.IDToken
		}
		delete(data, "accountID")
		if a.AccountID != "" {
			data["account_id"] = a.AccountID
		} else {
			delete(data, "account_id")
		}
		if a.Email != "" {
			data["email"] = a.Email
		} else {
			delete(data, "email")
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
	temp, err := os.CreateTemp(filepath.Dir(a.Path), "."+filepath.Base(a.Path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create auth temp file: %w", err)
	}
	tempPath := temp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tempPath)
		}
	}()
	if err = temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return fmt.Errorf("set auth temp permissions: %w", err)
	}
	if _, err = temp.Write(append(raw, '\n')); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write auth temp file: %w", err)
	}
	if err = temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync auth temp file: %w", err)
	}
	if err = temp.Close(); err != nil {
		return fmt.Errorf("close auth temp file: %w", err)
	}
	renameFile := os.Rename
	if a.renameFile != nil {
		renameFile = a.renameFile
	}
	if err = renameFile(tempPath, a.Path); err != nil {
		return fmt.Errorf("replace auth file: %w", err)
	}
	renamed = true
	a.Metadata = data
	a.codexCLI = mapField(data, "tokens") != nil
	return nil
}

type AuthChangeKind string

const (
	AuthAdded   AuthChangeKind = "added"
	AuthUpdated AuthChangeKind = "updated"
	AuthRemoved AuthChangeKind = "removed"
)

type AuthChange struct {
	Kind               AuthChangeKind
	ID                 string
	AccountID          string
	Identified         bool
	CredentialsChanged bool
	MetadataChanged    bool
	EligibilityChanged bool
	SourcesChanged     bool
}

type AuthFileProblemKind string

const (
	AuthFileReadProblem  AuthFileProblemKind = "read_error"
	AuthFileParseProblem AuthFileProblemKind = "parse_error"
)

type AuthFileProblem struct {
	Path       string
	Kind       AuthFileProblemKind
	GraceUntil time.Time
	Excluded   bool
}

type AuthReconcileResult struct {
	Auths        []*Auth
	Active       []*Auth
	Changes      []AuthChange
	FileProblems []AuthFileProblem
}

type FileAuthStore struct {
	Dir             string
	Now             func() time.Time
	ParseErrorGrace time.Duration

	mu          sync.Mutex
	mutationMu  sync.Mutex
	renameFile  func(string, string) error
	files       map[string]authFileState
	auths       map[string]*Auth
	initialized bool
}

type authFileState struct {
	auth            *Auth
	parseErrorSince time.Time
}

func NewFileAuthStore(dir string) *FileAuthStore {
	return &FileAuthStore{Dir: dir}
}

func (s *FileAuthStore) Load(ctx context.Context) ([]*Auth, error) {
	result, err := s.Reconcile(ctx)
	if err != nil {
		return nil, err
	}
	return result.Active, nil
}

func (s *FileAuthStore) Reconcile(ctx context.Context) (AuthReconcileResult, error) {
	if s == nil {
		return AuthReconcileResult{}, fmt.Errorf("auth store is nil")
	}
	dir, err := ResolveAuthDir(s.Dir)
	if err != nil {
		return AuthReconcileResult{}, err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	grace := s.ParseErrorGrace
	if grace == 0 {
		grace = DefaultAuthParseErrorGrace
	}
	if grace < 0 {
		grace = 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = make(map[string]authFileState)
	}
	if s.auths == nil {
		s.auths = make(map[string]*Auth)
	}

	files := make(map[string]authFileState)
	entries := make([]authFileEntry, 0)
	problems := make([]AuthFileProblem, 0)
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
		relativePath := normalizedRelativeAuthPath(dir, path)
		auth, errRead := readAuthFile(path, dir)
		if errRead != nil {
			kind := AuthFileReadProblem
			var parseErr *authFileParseError
			if errors.As(errRead, &parseErr) {
				kind = AuthFileParseProblem
			}
			previous, hadPrevious := s.files[relativePath]
			if kind == AuthFileParseProblem && hadPrevious && previous.auth != nil {
				if previous.parseErrorSince.IsZero() {
					previous.parseErrorSince = now()
				}
				excluded := !now().Before(previous.parseErrorSince.Add(grace))
				files[relativePath] = previous
				problem := AuthFileProblem{
					Path:       relativePath,
					Kind:       kind,
					GraceUntil: previous.parseErrorSince.Add(grace),
					Excluded:   excluded,
				}
				problems = append(problems, problem)
				if !excluded {
					entries = append(entries, authFileEntry{
						relativePath: relativePath,
						auth:         cloneAuth(previous.auth),
					})
				}
				return nil
			}
			problems = append(problems, AuthFileProblem{
				Path:     relativePath,
				Kind:     kind,
				Excluded: true,
			})
			return nil
		}
		if auth == nil {
			return nil
		}
		auth.relativePath = relativePath
		auth.SourcePaths = []string{path}
		auth.sourceIDs = []string{relativePath}
		files[relativePath] = authFileState{auth: cloneAuth(auth)}
		entries = append(entries, authFileEntry{
			relativePath: relativePath,
			auth:         auth,
		})
		return nil
	})
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return AuthReconcileResult{}, fmt.Errorf("load auths: %w", err)
	}
	logical := groupAuthFiles(entries)
	nextAuths := make(map[string]*Auth, len(logical))
	for _, auth := range logical {
		nextAuths[auth.ID] = cloneAuth(auth)
	}
	changes := reconcileAuthChanges(s.auths, nextAuths, s.initialized)
	s.files = files
	s.auths = nextAuths
	s.initialized = true

	result := AuthReconcileResult{
		Auths:        cloneAuths(logical),
		Changes:      changes,
		FileProblems: problems,
	}
	for _, auth := range logical {
		if !auth.Disabled {
			result.Active = append(result.Active, cloneAuth(auth))
		}
	}
	return result, nil
}

func (s *FileAuthStore) SetAccountDisabled(ctx context.Context, accountID string, disabled bool) (*Auth, error) {
	if s == nil {
		return nil, fmt.Errorf("auth store is nil")
	}
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, ErrInvalidInput
	}
	s.mutationMu.Lock()
	defer s.mutationMu.Unlock()

	result, err := s.Reconcile(ctx)
	if err != nil {
		return nil, err
	}
	target := authByManageableAccountID(result.Auths, accountID)
	if target == nil {
		for _, auth := range result.Auths {
			if auth != nil && !auth.Identified() && auth.ID == accountID {
				return nil, ErrUnidentifiedAuth
			}
		}
		return nil, ErrAuthNotFound
	}
	dir, err := ResolveAuthDir(s.Dir)
	if err != nil {
		return nil, err
	}
	originals := make([]*Auth, 0, len(target.SourcePaths))
	updates := make([]*Auth, 0, len(target.SourcePaths))
	for _, path := range target.SourcePaths {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
		}
		source, errRead := readAuthFile(path, dir)
		if errRead != nil || source == nil || source.AccountID != accountID {
			return nil, ErrAuthFilesInvalid
		}
		originals = append(originals, cloneAuth(source))
		update := cloneAuth(source)
		update.Disabled = disabled
		update.Metadata["disabled"] = disabled
		update.renameFile = s.renameFile
		originals[len(originals)-1].renameFile = s.renameFile
		updates = append(updates, update)
	}
	for index, update := range updates {
		if err = update.Save(); err != nil {
			var rollbackErr error
			for rollbackIndex := index - 1; rollbackIndex >= 0; rollbackIndex-- {
				rollbackErr = errors.Join(rollbackErr, originals[rollbackIndex].Save())
			}
			return nil, errors.Join(fmt.Errorf("save auth disabled state: %w", err), rollbackErr)
		}
	}
	result, err = s.Reconcile(ctx)
	if err != nil {
		return nil, err
	}
	updated := authByManageableAccountID(result.Auths, accountID)
	if updated == nil {
		return nil, ErrAuthNotFound
	}
	return updated, nil
}

func readAuthFile(path string, baseDir string) (*Auth, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, &authFileParseError{err: fmt.Errorf("auth file is empty")}
	}
	var meta map[string]any
	if err = json.Unmarshal(raw, &meta); err != nil {
		return nil, &authFileParseError{err: err}
	}
	provider, _ := meta["type"].(string)
	tokens := mapField(meta, "tokens")
	if strings.TrimSpace(provider) != "" && !strings.EqualFold(strings.TrimSpace(provider), "codex") {
		return nil, nil
	}
	if strings.TrimSpace(provider) == "" && tokens == nil {
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
	email := stringField(meta, "email")
	idClaims := tokenIdentityClaims(idToken)
	accessClaims := tokenIdentityClaims(accessToken)
	if accountID == "" {
		if idClaims.AccountID != "" {
			accountID = idClaims.AccountID
		} else {
			accountID = accessClaims.AccountID
		}
	}
	if email == "" {
		if idClaims.Email != "" {
			email = idClaims.Email
		} else {
			email = accessClaims.Email
		}
	}
	relativePath := normalizedRelativeAuthPath(baseDir, path)
	disabled, _ := meta["disabled"].(bool)
	auth := &Auth{
		ID:           stableAuthID(accountID, relativePath),
		Path:         path,
		Email:        email,
		AccountID:    accountID,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		IDToken:      idToken,
		ExpiresAt:    expiresAt,
		Metadata:     meta,
		Disabled:     disabled,
		SourcePaths:  []string{path},
		codexCLI:     tokens != nil,
		relativePath: relativePath,
		sourceIDs:    []string{relativePath},
	}
	return auth, nil
}

type authFileParseError struct {
	err error
}

func (e *authFileParseError) Error() string {
	return e.err.Error()
}

func (e *authFileParseError) Unwrap() error {
	return e.err
}

type authFileEntry struct {
	relativePath string
	auth         *Auth
}

func groupAuthFiles(entries []authFileEntry) []*Auth {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].relativePath < entries[j].relativePath
	})
	grouped := make(map[string][]authFileEntry)
	for _, entry := range entries {
		grouped[entry.auth.ID] = append(grouped[entry.auth.ID], entry)
	}
	ids := make([]string, 0, len(grouped))
	for id := range grouped {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	auths := make([]*Auth, 0, len(ids))
	for _, id := range ids {
		group := grouped[id]
		representative := group[0]
		for _, entry := range group {
			if !entry.auth.Disabled {
				representative = entry
				break
			}
		}
		auth := cloneAuth(representative.auth)
		auth.Disabled = true
		auth.SourcePaths = auth.SourcePaths[:0]
		auth.sourceIDs = auth.sourceIDs[:0]
		for _, entry := range group {
			auth.SourcePaths = append(auth.SourcePaths, entry.auth.Path)
			auth.sourceIDs = append(auth.sourceIDs, entry.relativePath)
			if !entry.auth.Disabled {
				auth.Disabled = false
			}
		}
		auths = append(auths, auth)
	}
	return auths
}

func reconcileAuthChanges(previous map[string]*Auth, current map[string]*Auth, initialized bool) []AuthChange {
	ids := make([]string, 0, len(previous)+len(current))
	seen := make(map[string]struct{}, len(previous)+len(current))
	for id := range previous {
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for id := range current {
		if _, ok := seen[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	changes := make([]AuthChange, 0)
	for _, id := range ids {
		before, hadBefore := previous[id]
		after, hasAfter := current[id]
		switch {
		case !hadBefore && hasAfter:
			if initialized {
				changes = append(changes, newAuthChange(AuthAdded, after))
			}
		case hadBefore && !hasAfter:
			changes = append(changes, newAuthChange(AuthRemoved, before))
		case hadBefore && hasAfter:
			change := newAuthChange(AuthUpdated, after)
			change.CredentialsChanged = authCredentialsChanged(before, after)
			change.MetadataChanged = before.Email != after.Email
			change.EligibilityChanged = before.Disabled != after.Disabled
			change.SourcesChanged = !slices.Equal(before.sourceIDs, after.sourceIDs)
			if change.CredentialsChanged || change.MetadataChanged || change.EligibilityChanged || change.SourcesChanged {
				changes = append(changes, change)
			}
		}
	}
	return changes
}

func newAuthChange(kind AuthChangeKind, auth *Auth) AuthChange {
	return AuthChange{
		Kind:       kind,
		ID:         auth.ID,
		AccountID:  auth.AccountID,
		Identified: auth.Identified(),
	}
}

func authCredentialsChanged(before *Auth, after *Auth) bool {
	return before.AccessToken != after.AccessToken ||
		before.RefreshToken != after.RefreshToken ||
		before.IDToken != after.IDToken ||
		!before.ExpiresAt.Equal(after.ExpiresAt)
}

func stableAuthID(accountID string, relativePath string) string {
	if accountID = strings.TrimSpace(accountID); accountID != "" {
		return accountAuthIDPrefix + accountID
	}
	return pathAuthIDPrefix + normalizedAuthPath(relativePath)
}

func authByManageableAccountID(auths []*Auth, accountID string) *Auth {
	accountID = strings.TrimSpace(accountID)
	for _, auth := range auths {
		if auth != nil && auth.Identified() && auth.AccountID == accountID {
			return auth
		}
	}
	return nil
}

func normalizedRelativeAuthPath(baseDir string, path string) string {
	relativePath, err := filepath.Rel(baseDir, path)
	if err != nil || relativePath == "" {
		relativePath = path
	}
	return normalizedAuthPath(relativePath)
}

func normalizedAuthPath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	path = filepath.ToSlash(path)
	return strings.TrimPrefix(path, "./")
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

func cloneMap(meta map[string]any) map[string]any {
	cloned := make(map[string]any, len(meta))
	for key, value := range meta {
		switch typed := value.(type) {
		case map[string]any:
			cloned[key] = cloneMap(typed)
		case []any:
			cloned[key] = cloneSlice(typed)
		default:
			cloned[key] = typed
		}
	}
	return cloned
}

func cloneSlice(values []any) []any {
	cloned := make([]any, len(values))
	for index, value := range values {
		switch typed := value.(type) {
		case map[string]any:
			cloned[index] = cloneMap(typed)
		case []any:
			cloned[index] = cloneSlice(typed)
		default:
			cloned[index] = typed
		}
	}
	return cloned
}

func cloneAuth(auth *Auth) *Auth {
	if auth == nil {
		return nil
	}
	cloned := *auth
	cloned.Metadata = cloneMap(auth.Metadata)
	cloned.SourcePaths = slices.Clone(auth.SourcePaths)
	cloned.sourceIDs = slices.Clone(auth.sourceIDs)
	return &cloned
}

func copyAuth(target *Auth, source *Auth) {
	if target == nil || source == nil {
		return
	}
	*target = *cloneAuth(source)
}

func cloneAuths(auths []*Auth) []*Auth {
	cloned := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		cloned = append(cloned, cloneAuth(auth))
	}
	return cloned
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

type identityClaims struct {
	AccountID string
	Email     string
}

func tokenIdentityClaims(token string) identityClaims {
	claims, ok := decodeJWTClaims(token)
	if !ok {
		return identityClaims{}
	}
	result := identityClaims{
		AccountID: stringField(claims, "account_id", "accountID", "chatgpt_account_id"),
		Email:     stringField(claims, "email"),
	}
	if profile := mapField(claims, "https://api.openai.com/profile"); result.Email == "" && profile != nil {
		result.Email = stringField(profile, "email")
	}
	if authClaims := mapField(claims, "https://api.openai.com/auth"); authClaims != nil {
		if result.AccountID == "" {
			result.AccountID = stringField(authClaims, "chatgpt_account_id", "account_id", "accountID")
		}
		if result.Email == "" {
			result.Email = stringField(authClaims, "email")
		}
	}
	return result
}

func decodeJWTClaims(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if err = json.Unmarshal(payload, &claims); err != nil {
		return nil, false
	}
	return claims, true
}

func tokenExpiry(token string) time.Time {
	claims, ok := decodeJWTClaims(token)
	if !ok {
		return time.Time{}
	}
	expiry, ok := claims["exp"].(float64)
	if !ok || expiry <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(expiry), 0).UTC()
}
