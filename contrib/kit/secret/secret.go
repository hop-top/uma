package secret

import (
	"context"
	"errors"
	"net/url"

	kitsecret "hop.top/kit/go/storage/secret"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// KeyFunc maps a credential request to the key of the secret holding it.
// An error that is a *model.Error is returned to the adapter unchanged; any
// other error becomes a model.Validation error with a fixed message. An
// empty key is rejected the same way.
type KeyFunc func(ports.CredentialRequest) (string, error)

// Option configures New.
type Option func(*credentials)

// WithKeyFunc replaces DefaultKey. Use it to select a key per Purpose, to
// apply the host's own naming or to reject requests a tenant may not make.
func WithKeyFunc(f KeyFunc) Option {
	return func(c *credentials) {
		c.key = f
	}
}

// New returns a ports.Credentials that reads each credential from store.
// The host builds store (any kit backend, or its own implementation) and
// owns its configuration and lifetime.
//
// Every Resolve reads the store: there is no cache, so a value rotated in
// the store is used on the next connection attempt and a Fresh request
// needs no special handling. Hosts that refresh OAuth2 tokens write the new
// access token to the store; this package never refreshes.
func New(store kitsecret.Store, opts ...Option) (ports.Credentials, error) {
	if store == nil {
		return nil, errors.New("contrib/kit/secret: nil store")
	}
	c := &credentials{store: store, key: DefaultKey}
	for _, o := range opts {
		o(c)
	}
	if c.key == nil {
		return nil, errors.New("contrib/kit/secret: nil key func")
	}
	return c, nil
}

type credentials struct {
	store kitsecret.Store
	key   KeyFunc
}

// DefaultKey is the default KeyFunc:
//
//	uma/<tenant>/<reference>
//
// where tenant is Channel.Tenant and reference is the channel's
// CredentialRef, each escaped with url.PathEscape so that a "/" inside
// either stays inside its segment. Purpose is not part of the key: every
// adapter purpose on a channel resolves the same secret. Empty segments
// and the segments "." and ".." are rejected (model.Validation), so a
// reference cannot reach another tenant's keys on path-based backends.
func DefaultKey(req ports.CredentialRequest) (string, error) {
	tenant, ok := segment(string(req.Channel.Tenant))
	if !ok {
		return "", model.NewError(model.Validation, "credential request: tenant is not a valid key segment")
	}
	ref, ok := segment(req.Reference)
	if !ok {
		return "", model.NewError(model.Validation, "credential request: reference is not a valid key segment")
	}
	return "uma/" + tenant + "/" + ref, nil
}

func segment(s string) (string, bool) {
	if s == "" || s == "." || s == ".." {
		return "", false
	}
	return url.PathEscape(s), true
}

// Resolve reads the secret named by the key function, decodes its envelope
// and returns the credential when its kind is in req.Accept.
func (c *credentials) Resolve(ctx context.Context, req ports.CredentialRequest) (ports.Credential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Accept) == 0 {
		return nil, model.NewError(model.Validation, "credential request accepts no kind")
	}
	key, err := c.key(req)
	if err != nil {
		var me *model.Error
		if errors.As(err, &me) {
			return nil, me
		}
		return nil, model.NewError(model.Validation, "credential key rejected")
	}
	if key == "" {
		return nil, model.NewError(model.Validation, "credential key is empty")
	}

	s, err := c.store.Get(ctx, key)
	if err != nil {
		return nil, storeError(ctx, err)
	}
	if s == nil {
		return nil, model.NewError(model.Unavailable, "credential store returned no secret")
	}
	// The store hands the value to the caller (kit backends return a fresh
	// slice per Get); clear it once decoded so the envelope does not
	// outlive this call.
	defer clear(s.Value)

	cred, err := decode(s.Value)
	if err != nil {
		return nil, err
	}
	if !req.Accepts(cred.Kind()) {
		zeroCredential(cred)
		return nil, model.NewError(model.Unsupported, "stored credential kind "+string(cred.Kind())+" not accepted")
	}
	return cred, nil
}

// storeError maps a store failure to a model error. The store's own error
// is dropped: backends may echo the key or more, and errors must never
// carry secret material.
func storeError(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, kitsecret.ErrNotFound):
		return model.NewError(model.NotFound, "credential not found")
	case errors.Is(err, kitsecret.ErrNotSupported):
		return model.NewError(model.Unsupported, "credential store cannot read secrets")
	}
	return model.NewError(model.Unavailable, "credential store unavailable")
}

// zeroCredential clears the secret bytes of a credential that is not
// returned. Strings (OAuth2 access token) and parsed private keys cannot be
// cleared.
func zeroCredential(c ports.Credential) {
	switch c := c.(type) {
	case ports.Secret:
		clear(c.Bytes)
	case ports.Password:
		clear(c.Secret)
	case ports.Kerberos:
		clear(c.Keytab)
		clear(c.CCache)
	}
}
