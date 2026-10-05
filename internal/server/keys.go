package server

import (
	"context"
	"net/http"

	"github.com/codemug/shhttp/v2/internal/id"
	"github.com/codemug/shhttp/v2/pkg/api"
	"github.com/danielgtaylor/huma/v2"
)

// KeyPath is the {id} of key operations. It is exported because huma can only
// fill fields of exported embedded structs.
type KeyPath struct {
	ID string `path:"id" doc:"Key id" example:"key_01j9z3k5q8m2x7v4w6t0b1c3d5"`
}

// valid answers 404 for anything that is not a key id.
func (k KeyPath) valid() error {
	if !id.Valid(id.Key, k.ID) {
		return huma.Error404NotFound("not found")
	}
	return nil
}

type createKeyInput struct {
	Body api.CreateKeyRequest
}

type keyWithSecretOutput struct {
	Location string `header:"Location"`
	Body     api.KeyWithSecret
}

type keyOutput struct{ Body api.Key }

type keyListOutput struct{ Body api.KeyList }

type updateKeyInput struct {
	KeyPath
	Body api.UpdateKeyRequest
}

type rotateKeyInput struct {
	KeyPath
	Body *api.RotateKeyRequest `required:"false"`
}

type revokeKeyInput struct {
	KeyPath
	KillSessions bool `query:"kill_sessions" doc:"Also kill the key's running sessions."`
}

type revokeKeyOutput struct{ Body api.RevokeKeyResponse }

func (s *Server) registerKeys() {
	master := access{master: true}

	op := operation("create-key", http.MethodPost, "/v2/keys", "Create an API key", "Keys", master)
	op.Description = "The response contains the full key in `key`. It is shown only once."
	op.DefaultStatus = http.StatusCreated
	op.MaxBodyBytes = maxJSONBody
	op.Errors = append(op.Errors, http.StatusBadRequest)
	huma.Register(s.api, op, func(ctx context.Context, in *createKeyInput) (*keyWithSecretOutput, error) {
		k, err := s.auth.CreateKey(ctx, in.Body)
		if err != nil {
			return nil, s.apiError(err)
		}
		s.log.Info("key created", "audit", true, "key", k.ID, "name", k.Name, "scopes", k.Scopes)
		return &keyWithSecretOutput{Location: "/v2/keys/" + k.ID, Body: k}, nil
	})

	op = operation("list-keys", http.MethodGet, "/v2/keys", "List API keys", "Keys", master)
	op.Description = "Includes revoked and expired keys. Secrets are never returned."
	huma.Register(s.api, op, func(ctx context.Context, _ *struct{}) (*keyListOutput, error) {
		keys, err := s.auth.ListKeys(ctx)
		if err != nil {
			return nil, s.apiError(err)
		}
		if keys == nil {
			keys = []api.Key{}
		}
		return &keyListOutput{Body: api.KeyList{Keys: keys}}, nil
	})

	op = operation("get-key", http.MethodGet, "/v2/keys/{id}", "Get an API key", "Keys", master)
	op.Errors = append(op.Errors, http.StatusNotFound)
	huma.Register(s.api, op, func(ctx context.Context, in *KeyPath) (*keyOutput, error) {
		if err := in.valid(); err != nil {
			return nil, err
		}
		k, err := s.auth.GetKey(ctx, in.ID)
		if err != nil {
			return nil, s.apiError(err)
		}
		return &keyOutput{Body: k}, nil
	})

	op = operation("update-key", http.MethodPatch, "/v2/keys/{id}", "Update an API key", "Keys", master)
	op.Description = "Changes the name, scopes, policy or expiry. Omitted fields are left unchanged."
	op.MaxBodyBytes = maxJSONBody
	op.Errors = append(op.Errors, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict)
	huma.Register(s.api, op, func(ctx context.Context, in *updateKeyInput) (*keyOutput, error) {
		if err := in.valid(); err != nil {
			return nil, err
		}
		k, err := s.auth.UpdateKey(ctx, in.ID, in.Body)
		if err != nil {
			return nil, s.apiError(err)
		}
		s.log.Info("key updated", "audit", true, "key", k.ID, "scopes", k.Scopes)
		return &keyOutput{Body: k}, nil
	})

	op = operation("rotate-key", http.MethodPost, "/v2/keys/{id}/rotate", "Issue a new secret for an API key", "Keys", master)
	op.Description = "Returns the new full key once. With `grace`, the previous secret keeps working for that long; otherwise it stops working immediately."
	op.MaxBodyBytes = maxJSONBody
	op.Errors = append(op.Errors, http.StatusNotFound, http.StatusConflict)
	huma.Register(s.api, op, func(ctx context.Context, in *rotateKeyInput) (*keyWithSecretOutput, error) {
		if err := in.valid(); err != nil {
			return nil, err
		}
		var grace api.Duration
		if in.Body != nil {
			grace = in.Body.Grace
		}
		k, err := s.auth.RotateKey(ctx, in.ID, grace.Std())
		if err != nil {
			return nil, s.apiError(err)
		}
		s.log.Info("key rotated", "audit", true, "key", k.ID, "grace", grace.Std().String())
		return &keyWithSecretOutput{Body: k}, nil
	})

	op = operation("revoke-key", http.MethodDelete, "/v2/keys/{id}", "Revoke an API key", "Keys", master)
	op.Description = "Revocation is permanent. The key's running sessions keep running unless `kill_sessions=true`."
	op.Errors = append(op.Errors, http.StatusNotFound)
	huma.Register(s.api, op, func(ctx context.Context, in *revokeKeyInput) (*revokeKeyOutput, error) {
		if err := in.valid(); err != nil {
			return nil, err
		}
		k, err := s.auth.RevokeKey(ctx, in.ID)
		if err != nil {
			return nil, s.apiError(err)
		}
		resp := api.RevokeKeyResponse{Key: k}
		if in.KillSessions {
			resp.KilledSessions = s.sessions.KillByKey(in.ID)
		}
		s.log.Info("key revoked", "audit", true, "key", k.ID, "killed_sessions", resp.KilledSessions)
		return &revokeKeyOutput{Body: resp}, nil
	})
}
