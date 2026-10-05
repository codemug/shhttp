package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codemug/shhttp/v2/internal/store"
	"github.com/codemug/shhttp/v2/pkg/api"
)

func newAuth(t *testing.T) (*Authenticator, string, *time.Time) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	master := GenerateMasterKey()
	a, err := New(st, master)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	return a, master, &now
}

func mustAuth(t *testing.T, a *Authenticator, token string) Principal {
	t.Helper()
	p, err := a.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	return p
}

func mustFail(t *testing.T, a *Authenticator, token string) {
	t.Helper()
	if _, err := a.Authenticate(context.Background(), token); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Authenticate(%q) = %v, want ErrUnauthorized", token, err)
	}
}

func TestMasterKey(t *testing.T) {
	a, master, _ := newAuth(t)
	p := mustAuth(t, a, master)
	if !p.Master || p.Has(api.ScopeSessionsRun) {
		t.Fatalf("master principal = %+v", p)
	}
	mustFail(t, a, master+"x")
	mustFail(t, a, GenerateMasterKey())
	if _, err := New(nil, "too-short"); err == nil {
		t.Fatal("weak master key accepted")
	}
}

func TestCreateAndAuthenticate(t *testing.T) {
	a, _, _ := newAuth(t)
	ctx := context.Background()
	k, err := a.CreateKey(ctx, api.CreateKeyRequest{Name: "ci", Scopes: []string{"sessions:run", "sessions:read", "sessions:run"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Secret, "shh_") || len(k.Scopes) != 2 {
		t.Fatalf("created key = %+v", k)
	}
	p := mustAuth(t, a, k.Secret)
	if p.KeyID() != k.ID || !p.Has(api.ScopeSessionsRun) || p.Has(api.ScopeAdminRead) {
		t.Fatalf("principal = %+v", p)
	}
	got, err := a.GetKey(ctx, k.ID)
	if err != nil || got.LastUsedAt == nil {
		t.Fatalf("last_used_at not recorded: %+v, %v", got, err)
	}
	// Tampered or malformed tokens.
	mustFail(t, a, k.Secret[:len(k.Secret)-1]+"A")
	mustFail(t, a, "shh_nope_secret")
	mustFail(t, a, "")
	mustFail(t, a, "Bearer "+k.Secret)
}

func TestCreateValidation(t *testing.T) {
	a, _, _ := newAuth(t)
	for _, req := range []api.CreateKeyRequest{
		{Name: "", Scopes: []string{"sessions:run"}},
		{Name: "x", Scopes: nil},
		{Name: "x", Scopes: []string{"root"}},
		{Name: "x", Scopes: []string{"sessions:run"}, Policy: api.Policy{Commands: []string{"("}}},
	} {
		_, err := a.CreateKey(context.Background(), req)
		var inv *InvalidError
		if !errors.As(err, &inv) {
			t.Errorf("CreateKey(%+v) = %v, want InvalidError", req, err)
		}
	}
}

func TestExpiry(t *testing.T) {
	a, _, now := newAuth(t)
	k, err := a.CreateKey(context.Background(), api.CreateKeyRequest{Name: "x", Scopes: []string{"sessions:read"}, ExpiresIn: api.Duration(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	mustAuth(t, a, k.Secret)
	*now = now.Add(time.Hour)
	mustFail(t, a, k.Secret)
	if _, err := a.UpdateKey(context.Background(), k.ID, api.UpdateKeyRequest{ClearExpiry: true}); err != nil {
		t.Fatal(err)
	}
	mustAuth(t, a, k.Secret)
}

func TestRotateWithGrace(t *testing.T) {
	a, _, now := newAuth(t)
	ctx := context.Background()
	k, _ := a.CreateKey(ctx, api.CreateKeyRequest{Name: "x", Scopes: []string{"sessions:read"}})
	r, err := a.RotateKey(ctx, k.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mustAuth(t, a, r.Secret)
	mustAuth(t, a, k.Secret)
	*now = now.Add(time.Minute)
	mustFail(t, a, k.Secret)
	mustAuth(t, a, r.Secret)

	// Without grace the old secret stops working at once.
	r2, err := a.RotateKey(ctx, k.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	mustFail(t, a, r.Secret)
	mustAuth(t, a, r2.Secret)
}

func TestRevoke(t *testing.T) {
	a, _, _ := newAuth(t)
	ctx := context.Background()
	k, _ := a.CreateKey(ctx, api.CreateKeyRequest{Name: "x", Scopes: []string{"sessions:read"}})
	if _, err := a.RevokeKey(ctx, k.ID); err != nil {
		t.Fatal(err)
	}
	mustFail(t, a, k.Secret)
	if _, err := a.RotateKey(ctx, k.ID, 0); !errors.Is(err, ErrRevoked) {
		t.Fatalf("rotate revoked key: %v", err)
	}
	if _, err := a.RevokeKey(ctx, k.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if _, err := a.RevokeKey(ctx, "key_missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoke missing key: %v", err)
	}
}

func TestLoadMasterKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "master.key")
	k1, created, err := LoadMasterKey("", path)
	if err != nil || !created {
		t.Fatalf("first load: %v, created=%v", err, created)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("master key file mode: %v, %v", info.Mode(), err)
	}
	k2, created, err := LoadMasterKey("", path)
	if err != nil || created || k1 != k2 {
		t.Fatalf("second load: %q vs %q, created=%v, %v", k1, k2, created, err)
	}
	k3, _, _ := LoadMasterKey("shhm_from_env", path)
	if k3 != "shhm_from_env" {
		t.Fatalf("env value ignored: %q", k3)
	}
}
