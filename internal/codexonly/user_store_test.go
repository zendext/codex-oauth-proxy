package codexonly

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestUserStoreCreatesUserWithInitialAPIKey(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()

	created, err := store.CreateUser(ctx, CreateUserParams{Name: " Alice "})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	if created.User.ID == "" {
		t.Fatal("created user ID is empty")
	}
	if created.User.Name != "Alice" {
		t.Fatalf("created user name = %q, want Alice", created.User.Name)
	}
	if !created.User.Enabled {
		t.Fatal("created user is disabled, want enabled")
	}
	if created.APIKey.ID == "" {
		t.Fatal("created API key ID is empty")
	}
	if created.APIKey.UserID != created.User.ID {
		t.Fatalf("created API key user ID = %q, want %q", created.APIKey.UserID, created.User.ID)
	}
	if created.PlaintextAPIKey == "" || !strings.HasPrefix(created.PlaintextAPIKey, "cop_") {
		t.Fatalf("plaintext API key = %q, want cop_ prefix", created.PlaintextAPIKey)
	}
	if created.APIKey.KeyHash == "" || created.APIKey.KeyHash == created.PlaintextAPIKey {
		t.Fatalf("stored key hash = %q, want non-empty hash distinct from plaintext", created.APIKey.KeyHash)
	}
	if strings.Contains(created.APIKey.MaskedKey, created.PlaintextAPIKey) {
		t.Fatalf("masked key %q contains plaintext key", created.APIKey.MaskedKey)
	}

	authenticated, err := store.AuthenticateAPIKey(ctx, created.PlaintextAPIKey)
	if err != nil {
		t.Fatalf("AuthenticateAPIKey returned error: %v", err)
	}
	if authenticated.User.ID != created.User.ID {
		t.Fatalf("authenticated user ID = %q, want %q", authenticated.User.ID, created.User.ID)
	}
	if authenticated.APIKey.ID != created.APIKey.ID {
		t.Fatalf("authenticated API key ID = %q, want %q", authenticated.APIKey.ID, created.APIKey.ID)
	}

	users, err := store.ListUsers(ctx, nil)
	if err != nil {
		t.Fatalf("ListUsers returned error: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("user count = %d, want 1", len(users))
	}
	if users[0].APIKey == nil {
		t.Fatal("listed user current API key is nil")
	}
	if users[0].APIKey.MaskedKey == "" {
		t.Fatal("listed user masked key is empty")
	}
}

func TestUserStoreRejectsDuplicateNamesCaseInsensitively(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()

	if _, err := store.CreateUser(ctx, CreateUserParams{Name: "Alice"}); err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	_, err := store.CreateUser(ctx, CreateUserParams{Name: " alice "})
	if !errors.Is(err, ErrDuplicateUserName) {
		t.Fatalf("CreateUser duplicate error = %v, want ErrDuplicateUserName", err)
	}
}

func TestUserStoreResetDisablesOldAPIKey(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()

	created, err := store.CreateUser(ctx, CreateUserParams{Name: "Alice"})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	reset, err := store.ResetUserAPIKey(ctx, created.User.ID)
	if err != nil {
		t.Fatalf("ResetUserAPIKey returned error: %v", err)
	}
	if reset.PlaintextAPIKey == "" || reset.PlaintextAPIKey == created.PlaintextAPIKey {
		t.Fatalf("reset plaintext key = %q, want new non-empty key", reset.PlaintextAPIKey)
	}

	_, err = store.AuthenticateAPIKey(ctx, created.PlaintextAPIKey)
	if !errors.Is(err, ErrInvalidAPIKey) {
		t.Fatalf("old key auth error = %v, want ErrInvalidAPIKey", err)
	}
	authenticated, err := store.AuthenticateAPIKey(ctx, reset.PlaintextAPIKey)
	if err != nil {
		t.Fatalf("new key AuthenticateAPIKey returned error: %v", err)
	}
	if authenticated.APIKey.ID == created.APIKey.ID {
		t.Fatalf("authenticated API key ID = %q, want new key ID", authenticated.APIKey.ID)
	}
}

func TestUserStoreDisabledUserCannotAuthenticate(t *testing.T) {
	store := openTestUserStore(t)
	ctx := context.Background()

	created, err := store.CreateUser(ctx, CreateUserParams{Name: "Alice"})
	if err != nil {
		t.Fatalf("CreateUser returned error: %v", err)
	}
	enabled := false
	if _, err = store.UpdateUser(ctx, created.User.ID, UpdateUserParams{Enabled: &enabled}); err != nil {
		t.Fatalf("UpdateUser returned error: %v", err)
	}
	_, err = store.AuthenticateAPIKey(ctx, created.PlaintextAPIKey)
	if !errors.Is(err, ErrDisabledCredential) {
		t.Fatalf("disabled user auth error = %v, want ErrDisabledCredential", err)
	}
}

func openTestUserStore(t *testing.T) *UserStore {
	t.Helper()
	store, err := OpenUserStore(context.Background(), t.TempDir()+"/users.db")
	if err != nil {
		t.Fatalf("OpenUserStore returned error: %v", err)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Fatalf("Close returned error: %v", errClose)
		}
	})
	return store
}
