package widget

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// tokenPrefix versions the token format; it is part of the MAC input.
const tokenPrefix = "v1."

// maxTokenBytes bounds a presented token before any work on it.
const maxTokenBytes = 4096

// maxVisitorBytes bounds visitor IDs.
const maxVisitorBytes = 256

// claims is the signed content of a session token.
type claims struct {
	Tenant    model.TenantID `json:"tn"`
	Channel   model.ID       `json:"ch"`
	Visitor   string         `json:"v"`
	Assurance string         `json:"a"`
	Site      string         `json:"s"`
	Session   string         `json:"sid"`
	Expires   int64          `json:"exp"`
}

// namespace is the identity namespace of the claims' visitor.
func (c claims) namespace() string {
	if c.Assurance == AssuranceVerified {
		return NamespaceUser
	}
	return NamespaceVisitor
}

func (c claims) kind() string {
	if c.Assurance == AssuranceVerified {
		return KindUser
	}
	return KindVisitor
}

func (c claims) key(ch model.ChannelRef) LogKey {
	return LogKey{Channel: ch, Namespace: c.namespace(), Visitor: c.Visitor}
}

// sender is the visitor's sender reference.
func (c claims) sender(ch model.ChannelRef) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: c.namespace(), Kind: c.kind(), ID: c.Visitor}
}

// room is the session's reference: kind room, the session ID, and the
// visitor as parent.
func (c claims) room(ch model.ChannelRef) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: c.namespace(), Kind: model.RefKindRoom, ID: c.Session, ParentID: c.Visitor}
}

// sign returns the token for c under key.
func sign(key []byte, c claims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", model.NewError(model.Permanent, "session claims do not encode")
	}
	body := tokenPrefix + base64.RawURLEncoding.EncodeToString(payload)
	return body + "." + base64.RawURLEncoding.EncodeToString(mac(key, body)), nil
}

func mac(key []byte, body string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(body))
	return m.Sum(nil)
}

// verify checks a token against key for channel at now: the MAC first, in
// constant time, then the claims. Every failure is the same
// model.Unauthorized error.
func verify(key []byte, token string, ch model.ChannelRef, now time.Time) (claims, error) {
	refused := model.NewError(model.Unauthorized, "invalid session token")
	if len(token) > maxTokenBytes || !strings.HasPrefix(token, tokenPrefix) {
		return claims{}, refused
	}
	dot := strings.LastIndexByte(token, '.')
	if dot <= len(tokenPrefix) {
		return claims{}, refused
	}
	body, sig := token[:dot], token[dot+1:]
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, mac(key, body)) {
		return claims{}, refused
	}
	payload, err := base64.RawURLEncoding.DecodeString(body[len(tokenPrefix):])
	if err != nil {
		return claims{}, refused
	}
	var c claims
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return claims{}, refused
	}
	switch {
	case c.Tenant != ch.Tenant || c.Channel != ch.ID:
		return claims{}, refused
	case !now.Before(time.Unix(c.Expires, 0)):
		return claims{}, refused
	case c.Assurance != AssuranceAnonymous && c.Assurance != AssuranceVerified:
		return claims{}, refused
	case c.Visitor == "" || c.Session == "" || c.Site == "":
		return claims{}, refused
	}
	return c, nil
}

// resolveKey resolves the channel's session key for one request or call.
func resolveKey(ctx context.Context, creds ports.Credentials, ch model.Channel) ([]byte, error) {
	if creds == nil {
		return nil, model.NewError(model.Unavailable, "no credentials port")
	}
	cred, err := creds.Resolve(ctx, ports.CredentialRequest{
		Channel: ch.Ref, Reference: ch.CredentialRef, Purpose: CredentialPurpose,
		Accept: []ports.CredentialKind{ports.KindSecret},
	})
	if err != nil {
		return nil, err
	}
	s, ok := cred.(ports.Secret)
	if !ok || len(s.Bytes) < MinKeyBytes {
		return nil, &model.Error{Code: model.Permanent, SafeMessage: "session key is not a secret of at least 32 bytes", Retry: model.NoRetry}
	}
	return s.Bytes, nil
}

// randomID returns prefix followed by 128 random bits in hex.
func randomID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", model.NewError(model.Unavailable, "no randomness")
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// isMessageID reports whether id has the shape of a widget message ID.
func isMessageID(id string) bool {
	if len(id) != 33 || id[0] != 'm' {
		return false
	}
	_, err := hex.DecodeString(id[1:])
	return err == nil
}
