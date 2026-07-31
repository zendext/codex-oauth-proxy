package codexonly

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFileAuthStoreDeduplicatesAccountsForRoundRobin(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "a.json", flatAuthJSON("acct_a", "access-a1", false))
	writeAuthFile(t, dir, "duplicate.json", flatAuthJSON("acct_a", "access-a2", true))
	writeAuthFile(t, dir, "b.json", flatAuthJSON("acct_b", "access-b", false))

	store := NewFileAuthStore(dir)
	auths, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(auths) != 2 {
		t.Fatalf("auth count = %d, want 2", len(auths))
	}
	if got := []string{auths[0].ID, auths[1].ID}; !slices.Equal(got, []string{"account:acct_a", "account:acct_b"}) {
		t.Fatalf("auth IDs = %v, want stable account order", got)
	}
	if auths[0].SourceCount() != 2 {
		t.Fatalf("acct_a source count = %d, want 2", auths[0].SourceCount())
	}

	manager := &AuthManager{Store: store}
	var selected []string
	for range 4 {
		auth, errSelect := manager.Select(context.Background())
		if errSelect != nil {
			t.Fatalf("Select returned error: %v", errSelect)
		}
		selected = append(selected, auth.ID)
	}
	want := []string{"account:acct_a", "account:acct_b", "account:acct_a", "account:acct_b"}
	if !slices.Equal(selected, want) {
		t.Fatalf("selected IDs = %v, want %v", selected, want)
	}
}

func TestFileAuthStoreDetectsNewAccountWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "a.json", flatAuthJSON("acct_a", "access-a", false))
	store := NewFileAuthStore(dir)
	if _, err := store.Reconcile(context.Background()); err != nil {
		t.Fatalf("initial Reconcile returned error: %v", err)
	}

	writeAuthFile(t, dir, "b.json", flatAuthJSON("acct_b", "access-b", false))
	added, err := store.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("addition Reconcile returned error: %v", err)
	}
	requireSingleChange(t, added.Changes, AuthAdded, "account:acct_b")
	if got := authIDs(added.Active); !slices.Equal(got, []string{"account:acct_a", "account:acct_b"}) {
		t.Fatalf("active IDs = %v, want both accounts", got)
	}
}

func TestFileAuthStoreRecoversIdentityFromTokenClaims(t *testing.T) {
	dir := t.TempDir()
	idToken := jwtWithClaims(t, map[string]any{
		"email": "id@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_id",
		},
	})
	accessToken := jwtWithClaims(t, map[string]any{
		"https://api.openai.com/profile": map[string]any{
			"email": "access@example.com",
		},
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_access",
		},
	})
	writeAuthFile(t, dir, "id.json", fmt.Sprintf(`{
		"type": "codex",
		"access_token": "access-id",
		"refresh_token": "refresh-id",
		"id_token": %q
	}`, idToken))
	writeAuthFile(t, dir, "access.json", fmt.Sprintf(`{
		"type": "codex",
		"access_token": %q,
		"refresh_token": "refresh-access",
		"id_token": "malformed"
	}`, accessToken))
	writeAuthFile(t, dir, "fallback.json", `{
		"type": "codex",
		"access_token": "malformed",
		"refresh_token": "refresh-fallback",
		"id_token": "also-malformed"
	}`)

	auths, err := NewFileAuthStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if len(auths) != 3 {
		t.Fatalf("auth count = %d, want 3", len(auths))
	}

	byID := authsByID(auths)
	if auth := byID["account:acct_id"]; auth == nil || auth.AccountID != "acct_id" || auth.Email != "id@example.com" {
		t.Fatalf("ID-token auth = %#v, want recovered account and email", auth)
	}
	if auth := byID["account:acct_access"]; auth == nil || auth.AccountID != "acct_access" || auth.Email != "access@example.com" {
		t.Fatalf("access-token auth = %#v, want recovered account and email", auth)
	}
	fallback := byID["path:fallback.json"]
	if fallback == nil {
		t.Fatalf("fallback auth IDs = %v, want path:fallback.json", authIDs(auths))
	}
	if fallback.Identified() {
		t.Fatal("fallback auth is identified, want unidentified compatibility auth")
	}
	if _, ok := fallback.ManageableAccountID(); ok {
		t.Fatal("fallback auth is account-manageable, want read-only unidentified auth")
	}
}

func TestFileAuthStoreIdentityAndOrderingSurviveRenameAndRestart(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "z.json", flatAuthJSON("acct_z", "access-z", false))
	writeAuthFile(t, dir, "a.json", flatAuthJSON("acct_a", "access-a", false))

	store := NewFileAuthStore(dir)
	initial, err := store.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("initial Reconcile returned error: %v", err)
	}
	if got := authIDs(initial.Active); !slices.Equal(got, []string{"account:acct_a", "account:acct_z"}) {
		t.Fatalf("initial IDs = %v, want stable account order", got)
	}

	movedDir := filepath.Join(dir, "nested")
	if err = os.MkdirAll(movedDir, 0o700); err != nil {
		t.Fatalf("create nested dir: %v", err)
	}
	if err = os.Rename(filepath.Join(dir, "a.json"), filepath.Join(movedDir, "renamed.json")); err != nil {
		t.Fatalf("rename auth file: %v", err)
	}

	renamed, err := store.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("rename Reconcile returned error: %v", err)
	}
	change := requireSingleChange(t, renamed.Changes, AuthUpdated, "account:acct_a")
	if !change.SourcesChanged || change.CredentialsChanged || change.EligibilityChanged {
		t.Fatalf("rename change = %#v, want source-only update", change)
	}

	restarted, err := NewFileAuthStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("restart Load returned error: %v", err)
	}
	if got := authIDs(restarted); !slices.Equal(got, []string{"account:acct_a", "account:acct_z"}) {
		t.Fatalf("restart IDs = %v, want stable account order", got)
	}
}

func TestFileAuthStoreReconcilesCredentialReplacementAndDisable(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "auth.json", flatAuthJSON("acct_a", "access-old", false))

	store := NewFileAuthStore(dir)
	if _, err := store.Reconcile(context.Background()); err != nil {
		t.Fatalf("initial Reconcile returned error: %v", err)
	}

	writeAuthFile(t, dir, "auth.json", flatAuthJSON("acct_a", "access-new", false))
	replaced, err := store.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("token replacement Reconcile returned error: %v", err)
	}
	change := requireSingleChange(t, replaced.Changes, AuthUpdated, "account:acct_a")
	if !change.CredentialsChanged || change.EligibilityChanged || change.SourcesChanged {
		t.Fatalf("token replacement change = %#v, want credential-only update", change)
	}
	if strings.Contains(fmt.Sprintf("%#v", replaced.Changes), "access-new") {
		t.Fatal("reconciliation change exposed token content")
	}

	writeAuthFile(t, dir, "auth.json", flatAuthJSON("acct_a", "access-new", true))
	disabled, err := store.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("disable Reconcile returned error: %v", err)
	}
	change = requireSingleChange(t, disabled.Changes, AuthUpdated, "account:acct_a")
	if !change.EligibilityChanged || change.CredentialsChanged {
		t.Fatalf("disable change = %#v, want eligibility-only update", change)
	}
	if len(disabled.Active) != 0 {
		t.Fatalf("active auth count = %d, want 0 after disable", len(disabled.Active))
	}
	if len(disabled.Auths) != 1 || !disabled.Auths[0].Disabled {
		t.Fatalf("logical auths = %#v, want retained disabled auth", disabled.Auths)
	}
}

func TestFileAuthStoreReconcilesAccountReplacementAdditionAndRemoval(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "auth.json", flatAuthJSON("acct_old", "access-old", false))

	store := NewFileAuthStore(dir)
	if _, err := store.Reconcile(context.Background()); err != nil {
		t.Fatalf("initial Reconcile returned error: %v", err)
	}

	writeAuthFile(t, dir, "auth.json", flatAuthJSON("acct_new", "access-new", false))
	replaced, err := store.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("account replacement Reconcile returned error: %v", err)
	}
	requireChange(t, replaced.Changes, AuthRemoved, "account:acct_old")
	requireChange(t, replaced.Changes, AuthAdded, "account:acct_new")

	writeAuthFile(t, dir, "duplicate.json", flatAuthJSON("acct_new", "access-new", false))
	addedDuplicate, err := store.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("duplicate addition Reconcile returned error: %v", err)
	}
	change := requireSingleChange(t, addedDuplicate.Changes, AuthUpdated, "account:acct_new")
	if !change.SourcesChanged || change.CredentialsChanged {
		t.Fatalf("duplicate addition change = %#v, want source-only update", change)
	}
	if len(addedDuplicate.Active) != 1 || addedDuplicate.Active[0].SourceCount() != 2 {
		t.Fatalf("active auths = %#v, want one auth with two sources", addedDuplicate.Active)
	}

	if err = os.Remove(filepath.Join(dir, "auth.json")); err != nil {
		t.Fatalf("remove first source: %v", err)
	}
	oneSource, err := store.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("source removal Reconcile returned error: %v", err)
	}
	change = requireSingleChange(t, oneSource.Changes, AuthUpdated, "account:acct_new")
	if !change.SourcesChanged || change.CredentialsChanged {
		t.Fatalf("source removal change = %#v, want source-only update", change)
	}

	if err = os.Remove(filepath.Join(dir, "duplicate.json")); err != nil {
		t.Fatalf("remove final source: %v", err)
	}
	removed, err := store.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("final removal Reconcile returned error: %v", err)
	}
	requireSingleChange(t, removed.Changes, AuthRemoved, "account:acct_new")
	if len(removed.Auths) != 0 || len(removed.Active) != 0 {
		t.Fatalf("remaining auths = %#v, want none", removed.Auths)
	}
}

func TestFileAuthStoreKeepsLastGoodAuthDuringParseErrorGrace(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "auth.json", flatAuthJSON("acct_a", "access-old", false))
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	store := NewFileAuthStore(dir)
	store.Now = func() time.Time { return now }
	store.ParseErrorGrace = 5 * time.Second

	if _, err := store.Reconcile(context.Background()); err != nil {
		t.Fatalf("initial Reconcile returned error: %v", err)
	}
	writeAuthFile(t, dir, "auth.json", `{"type":"codex","access_token":`)

	grace, err := store.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("grace Reconcile returned error: %v", err)
	}
	if len(grace.Active) != 1 || grace.Active[0].AccessToken != "access-old" {
		t.Fatalf("grace active auths = %#v, want last good representation", grace.Active)
	}
	if len(grace.FileProblems) != 1 || grace.FileProblems[0].Excluded {
		t.Fatalf("grace file problems = %#v, want retained parse problem", grace.FileProblems)
	}
	if len(grace.Changes) != 0 {
		t.Fatalf("grace changes = %#v, want no logical auth change", grace.Changes)
	}

	now = now.Add(6 * time.Second)
	expired, err := store.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("expired grace Reconcile returned error: %v", err)
	}
	if len(expired.Active) != 0 {
		t.Fatalf("active auth count = %d, want 0 after grace expiry", len(expired.Active))
	}
	if len(expired.FileProblems) != 1 || !expired.FileProblems[0].Excluded {
		t.Fatalf("expired file problems = %#v, want excluded parse problem", expired.FileProblems)
	}
	requireSingleChange(t, expired.Changes, AuthRemoved, "account:acct_a")

	writeAuthFile(t, dir, "auth.json", flatAuthJSON("acct_a", "access-new", false))
	recovered, err := store.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("recovery Reconcile returned error: %v", err)
	}
	requireSingleChange(t, recovered.Changes, AuthAdded, "account:acct_a")
	if len(recovered.FileProblems) != 0 {
		t.Fatalf("recovery file problems = %#v, want none", recovered.FileProblems)
	}
}

func TestFileAuthStoreReportsNewMalformedFileWithoutServingIt(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "broken.json", `{"access_token":"secret"`)

	result, err := NewFileAuthStore(dir).Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if len(result.Active) != 0 {
		t.Fatalf("active auth count = %d, want 0", len(result.Active))
	}
	if len(result.FileProblems) != 1 || !result.FileProblems[0].Excluded {
		t.Fatalf("file problems = %#v, want excluded malformed file", result.FileProblems)
	}
	if strings.Contains(fmt.Sprintf("%#v", result.FileProblems), "secret") {
		t.Fatal("file problem exposed token content")
	}
}

func TestRefresherReparsesIdentityMetadata(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	writeAuthFile(t, dir, "auth.json", flatAuthJSON("acct_old", "access-old", false))
	idToken := jwtWithClaims(t, map[string]any{
		"email": "new@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_new",
		},
	})
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-new",
			"id_token":     idToken,
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	auth := &Auth{
		ID:           "account:acct_old",
		Path:         authPath,
		Email:        "old@example.com",
		AccountID:    "acct_old",
		AccessToken:  "access-old",
		RefreshToken: "refresh-old",
		Metadata: map[string]any{
			"type":          "codex",
			"account_id":    "acct_old",
			"email":         "old@example.com",
			"access_token":  "access-old",
			"refresh_token": "refresh-old",
		},
	}
	refresher := &Refresher{
		Client:   tokenServer.Client(),
		TokenURL: tokenServer.URL,
	}

	if err := refresher.Refresh(context.Background(), auth); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if auth.ID != "account:acct_new" || auth.AccountID != "acct_new" || auth.Email != "new@example.com" {
		t.Fatalf("refreshed identity = ID %q account %q email %q", auth.ID, auth.AccountID, auth.Email)
	}

	raw, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read refreshed auth: %v", err)
	}
	var saved map[string]any
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("unmarshal refreshed auth: %v", err)
	}
	if saved["account_id"] != "acct_new" || saved["email"] != "new@example.com" {
		t.Fatalf("saved identity = account %#v email %#v", saved["account_id"], saved["email"])
	}
}

func TestRefresherPrefersNewAccessTokenIdentityOverStaleIDToken(t *testing.T) {
	dir := t.TempDir()
	oldIDToken := jwtWithClaims(t, map[string]any{
		"email": "old@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_old",
		},
	})
	newAccessToken := jwtWithClaims(t, map[string]any{
		"email": "new@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct_new",
		},
	})
	writeAuthFile(t, dir, "auth.json", fmt.Sprintf(`{
		"type": "codex",
		"account_id": "acct_old",
		"email": "old@example.com",
		"access_token": "access-old",
		"refresh_token": "refresh-old",
		"id_token": %q
	}`, oldIDToken))
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": newAccessToken,
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	auths, err := NewFileAuthStore(dir).Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	auth := auths[0]
	refresher := &Refresher{
		Client:   tokenServer.Client(),
		TokenURL: tokenServer.URL,
	}
	if err = refresher.Refresh(context.Background(), auth); err != nil {
		t.Fatalf("Refresh returned error: %v", err)
	}
	if auth.ID != "account:acct_new" || auth.AccountID != "acct_new" || auth.Email != "new@example.com" {
		t.Fatalf("refreshed identity = ID %q account %q email %q", auth.ID, auth.AccountID, auth.Email)
	}
}

func flatAuthJSON(accountID string, accessToken string, disabled bool) string {
	return fmt.Sprintf(`{
		"type": "codex",
		"account_id": %q,
		"access_token": %q,
		"refresh_token": "refresh-%s",
		"disabled": %t
	}`, accountID, accessToken, accountID, disabled)
}

func jwtWithClaims(t *testing.T, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal JWT header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal JWT payload: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("signature"))
}

func authsByID(auths []*Auth) map[string]*Auth {
	result := make(map[string]*Auth, len(auths))
	for _, auth := range auths {
		result[auth.ID] = auth
	}
	return result
}

func authIDs(auths []*Auth) []string {
	ids := make([]string, 0, len(auths))
	for _, auth := range auths {
		ids = append(ids, auth.ID)
	}
	return ids
}

func requireSingleChange(t *testing.T, changes []AuthChange, kind AuthChangeKind, id string) AuthChange {
	t.Helper()
	if len(changes) != 1 {
		t.Fatalf("changes = %#v, want one %s for %s", changes, kind, id)
	}
	if changes[0].Kind != kind || changes[0].ID != id {
		t.Fatalf("change = %#v, want %s for %s", changes[0], kind, id)
	}
	return changes[0]
}

func requireChange(t *testing.T, changes []AuthChange, kind AuthChangeKind, id string) AuthChange {
	t.Helper()
	for _, change := range changes {
		if change.Kind == kind && change.ID == id {
			return change
		}
	}
	t.Fatalf("changes = %#v, want %s for %s", changes, kind, id)
	return AuthChange{}
}
