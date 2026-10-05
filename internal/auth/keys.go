package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codemug/shhttp/v2/internal/id"
	"github.com/codemug/shhttp/v2/internal/policy"
	"github.com/codemug/shhttp/v2/internal/store"
	"github.com/codemug/shhttp/v2/pkg/api"
)

// InvalidError reports a malformed key management request.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return e.Reason }

// ErrRevoked is returned when changing a revoked key.
var ErrRevoked = errors.New("the key is revoked")

func invalid(format string, args ...any) error {
	return &InvalidError{Reason: fmt.Sprintf(format, args...)}
}

func validateName(name string) error {
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 100 {
		return invalid("name must be 1 to 100 characters")
	}
	return nil
}

func normalizeScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return nil, invalid("at least one scope is required; known scopes: %s", strings.Join(api.AllScopes, ", "))
	}
	out := slices.Clone(scopes)
	for _, s := range out {
		if !slices.Contains(api.AllScopes, s) {
			return nil, invalid("unknown scope %q; known scopes: %s", s, strings.Join(api.AllScopes, ", "))
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func validatePolicy(p api.Policy) error {
	if err := policy.Validate(p); err != nil {
		return invalid("%v", err)
	}
	return nil
}

// CreateKey creates an API key and returns it with its secret.
func (a *Authenticator) CreateKey(ctx context.Context, req api.CreateKeyRequest) (api.KeyWithSecret, error) {
	if err := validateName(req.Name); err != nil {
		return api.KeyWithSecret{}, err
	}
	scopes, err := normalizeScopes(req.Scopes)
	if err != nil {
		return api.KeyWithSecret{}, err
	}
	if err := validatePolicy(req.Policy); err != nil {
		return api.KeyWithSecret{}, err
	}
	now := a.now().UTC()
	k := store.KeyRecord{Key: api.Key{
		ID:        id.New(id.Key),
		Name:      req.Name,
		Scopes:    scopes,
		Policy:    req.Policy,
		CreatedAt: now,
	}}
	if req.ExpiresIn > 0 {
		t := now.Add(req.ExpiresIn.Std())
		k.ExpiresAt = &t
	}
	token, h := newSecret(k.ID)
	k.SecretHash = h
	if err := a.store.InsertKey(ctx, k); err != nil {
		return api.KeyWithSecret{}, err
	}
	return api.KeyWithSecret{Key: k.Key, Secret: token}, nil
}

// GetKey returns a key's metadata.
func (a *Authenticator) GetKey(ctx context.Context, keyID string) (api.Key, error) {
	k, err := a.store.GetKey(ctx, keyID)
	return k.Key, err
}

// ListKeys returns every key, including revoked ones.
func (a *Authenticator) ListKeys(ctx context.Context) ([]api.Key, error) {
	recs, err := a.store.ListKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]api.Key, len(recs))
	for i, r := range recs {
		out[i] = r.Key
	}
	return out, nil
}

// UpdateKey changes a key's name, scopes, policy or expiry.
func (a *Authenticator) UpdateKey(ctx context.Context, keyID string, req api.UpdateKeyRequest) (api.Key, error) {
	k, err := a.store.GetKey(ctx, keyID)
	if err != nil {
		return api.Key{}, err
	}
	if k.RevokedAt != nil {
		return api.Key{}, ErrRevoked
	}
	if req.Name != nil {
		if err := validateName(*req.Name); err != nil {
			return api.Key{}, err
		}
		k.Name = *req.Name
	}
	if req.Scopes != nil {
		if k.Scopes, err = normalizeScopes(*req.Scopes); err != nil {
			return api.Key{}, err
		}
	}
	if req.Policy != nil {
		if err := validatePolicy(*req.Policy); err != nil {
			return api.Key{}, err
		}
		k.Policy = *req.Policy
	}
	if req.ExpiresAt != nil && req.ClearExpiry {
		return api.Key{}, invalid("expires_at and clear_expiry cannot both be set")
	}
	if req.ExpiresAt != nil {
		t := req.ExpiresAt.UTC()
		k.ExpiresAt = &t
	}
	if req.ClearExpiry {
		k.ExpiresAt = nil
	}
	if err := a.store.UpdateKey(ctx, k); err != nil {
		return api.Key{}, err
	}
	return k.Key, nil
}

// RotateKey issues a new secret. With a grace period the previous secret
// keeps working until it ends; otherwise it stops working immediately.
func (a *Authenticator) RotateKey(ctx context.Context, keyID string, grace time.Duration) (api.KeyWithSecret, error) {
	k, err := a.store.GetKey(ctx, keyID)
	if err != nil {
		return api.KeyWithSecret{}, err
	}
	if k.RevokedAt != nil {
		return api.KeyWithSecret{}, ErrRevoked
	}
	token, h := newSecret(k.ID)
	k.PrevSecretHash, k.PreviousSecretValidUntil = "", nil
	if grace > 0 {
		t := a.now().UTC().Add(grace)
		k.PrevSecretHash, k.PreviousSecretValidUntil = k.SecretHash, &t
	}
	k.SecretHash = h
	if err := a.store.UpdateKey(ctx, k); err != nil {
		return api.KeyWithSecret{}, err
	}
	return api.KeyWithSecret{Key: k.Key, Secret: token}, nil
}

// RevokeKey permanently disables a key. Revoking twice is not an error.
func (a *Authenticator) RevokeKey(ctx context.Context, keyID string) (api.Key, error) {
	k, err := a.store.GetKey(ctx, keyID)
	if err != nil {
		return api.Key{}, err
	}
	if k.RevokedAt == nil {
		t := a.now().UTC()
		k.RevokedAt = &t
		k.PrevSecretHash, k.PreviousSecretValidUntil = "", nil
		if err := a.store.UpdateKey(ctx, k); err != nil {
			return api.Key{}, err
		}
	}
	return k.Key, nil
}

// ErrKeyInactive is returned by Policy for a revoked, expired or deleted key.
var ErrKeyInactive = errors.New("the key that owns this job is revoked or expired")

// Policy returns the current policy of an active key. Jobs use it to start
// each step with the key's policy at that moment.
func (a *Authenticator) Policy(ctx context.Context, keyID string) (api.Policy, error) {
	k, err := a.store.GetKey(ctx, keyID)
	if errors.Is(err, store.ErrNotFound) {
		return api.Policy{}, ErrKeyInactive
	}
	if err != nil {
		return api.Policy{}, err
	}
	if k.RevokedAt != nil || (k.ExpiresAt != nil && !a.now().Before(*k.ExpiresAt)) {
		return api.Policy{}, ErrKeyInactive
	}
	return k.Policy, nil
}
