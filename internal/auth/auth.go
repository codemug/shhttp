// Package auth handles the master key, API keys and scopes.
//
// API keys look like "shh_<ulid>_<secret>": the ulid part is the key id
// without its "key_" prefix and the secret is 32 random bytes in base64url.
// Only the SHA-256 of the secret is stored. The master key looks like
// "shhm_<secret>" and can only manage API keys.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/codemug/shhttp/v2/internal/id"
	"github.com/codemug/shhttp/v2/internal/store"
)

const (
	keyPrefix    = "shh_"
	masterPrefix = "shhm_"
	// touchInterval limits how often last_used_at is written.
	touchInterval = time.Minute
)

// ErrUnauthorized is returned for any invalid, expired or revoked credential.
// The reason is deliberately not distinguished.
var ErrUnauthorized = errors.New("invalid credentials")

// Principal is the authenticated caller.
type Principal struct {
	Master bool
	Key    *store.KeyRecord
}

// Has reports whether the principal holds scope. The master key holds no
// scopes: it can only manage keys.
func (p Principal) Has(scope string) bool {
	return p.Key != nil && slices.Contains(p.Key.Scopes, scope)
}

// KeyID returns the API key id, or "" for the master key.
func (p Principal) KeyID() string {
	if p.Key == nil {
		return ""
	}
	return p.Key.ID
}

// Authenticator checks bearer tokens.
type Authenticator struct {
	store      *store.Store
	masterHash []byte
	now        func() time.Time
}

// New returns an Authenticator that accepts masterKey and the keys in st.
func New(st *store.Store, masterKey string) (*Authenticator, error) {
	if !strings.HasPrefix(masterKey, masterPrefix) || len(masterKey) < len(masterPrefix)+32 {
		return nil, fmt.Errorf("master key must start with %q and contain at least 32 characters after it (generate one with `shhttpd keygen`)", masterPrefix)
	}
	return &Authenticator{store: st, masterHash: hash(masterKey), now: time.Now}, nil
}

// Authenticate resolves a bearer token to a principal.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (Principal, error) {
	if strings.HasPrefix(token, masterPrefix) {
		if subtle.ConstantTimeCompare(hash(token), a.masterHash) == 1 {
			return Principal{Master: true}, nil
		}
		return Principal{}, ErrUnauthorized
	}
	keyID, secret, ok := parseToken(token)
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	k, err := a.store.GetKey(ctx, keyID)
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, err
	}
	now := a.now()
	if k.RevokedAt != nil || (k.ExpiresAt != nil && !now.Before(*k.ExpiresAt)) {
		return Principal{}, ErrUnauthorized
	}
	h := []byte(hex.EncodeToString(hash(secret)))
	match := subtle.ConstantTimeCompare(h, []byte(k.SecretHash)) == 1
	if !match && k.PrevSecretHash != "" && k.PreviousSecretValidUntil != nil && now.Before(*k.PreviousSecretValidUntil) {
		match = subtle.ConstantTimeCompare(h, []byte(k.PrevSecretHash)) == 1
	}
	if !match {
		return Principal{}, ErrUnauthorized
	}
	if k.LastUsedAt == nil || now.Sub(*k.LastUsedAt) > touchInterval {
		if err := a.store.TouchKey(ctx, k.ID, now); err == nil {
			t := now.UTC()
			k.LastUsedAt = &t
		}
	}
	return Principal{Key: &k}, nil
}

func parseToken(token string) (keyID, secret string, ok bool) {
	rest, ok := strings.CutPrefix(token, keyPrefix)
	if !ok {
		return "", "", false
	}
	// The ulid contains no underscore; the secret may.
	ulid, secret, ok := strings.Cut(rest, "_")
	if !ok || secret == "" {
		return "", "", false
	}
	keyID = id.Key + "_" + ulid
	return keyID, secret, id.Valid(id.Key, keyID)
}

func hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func randomSecret() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// newSecret returns a bearer token for keyID and the hash to store.
func newSecret(keyID string) (token, secretHash string) {
	secret := randomSecret()
	return keyPrefix + strings.TrimPrefix(keyID, id.Key+"_") + "_" + secret, hex.EncodeToString(hash(secret))
}

// GenerateMasterKey returns a new random master key.
func GenerateMasterKey() string { return masterPrefix + randomSecret() }

// LoadMasterKey returns the master key from, in order: the value of the
// SHHTTP_MASTER_KEY environment variable (envValue), the file at path, or a
// newly generated key written to path. created reports the last case.
func LoadMasterKey(envValue, path string) (key string, created bool, err error) {
	if envValue != "" {
		return strings.TrimSpace(envValue), false, nil
	}
	b, err := os.ReadFile(path)
	if err == nil {
		return strings.TrimSpace(string(b)), false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	key = GenerateMasterKey()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", false, err
	}
	if _, err := f.WriteString(key + "\n"); err != nil {
		f.Close()
		return "", false, err
	}
	return key, true, f.Close()
}
