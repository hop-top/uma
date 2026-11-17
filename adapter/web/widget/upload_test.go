package widget_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// uploadRig is the widget behind Runtime.Webhook over an engine with a
// blob store, a webhook body limit of 512 bytes and an attachment limit of
// 2 KiB, PNGs only.
type uploadRig struct {
	*rig
	store  *memory.Store
	client uma.Client
}

func newUploadRig(t *testing.T) *uploadRig {
	t.Helper()
	ctx := t.Context()
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
	st := memory.New(memory.Options{Clock: clock})
	f, err := widget.New(widget.Options{Verifier: headerVerifier})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{f},
		Credentials: newKeyHost(),
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(0xf11e),
		Blobs:       &portstest.MemoryBlobs{},
		// Smaller than any upload limit a webhook body limit would allow.
		WebhookBodyLimit:     512,
		AttachmentSizeLimit:  2048,
		AttachmentMediaTypes: []string{"image/png"},
	})
	if err != nil {
		t.Fatal(err)
	}
	lock := true
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(ctx, model.Channel{
			Ref: chanA, Type: widget.Type, Configuration: widget.Configuration("support"), CredentialRef: "widget-key", Revision: "r1",
		}, ""); err != nil {
			return err
		}
		return tx.PutInbox(ctx, model.Inbox{
			Ref: inboxA, Channel: chanA, Name: "support", Destinations: []string{"support"}, Revision: "r1",
			Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
		}, "")
	}); err != nil {
		t.Fatal(err)
	}
	hd, err := engine.Runtime().Webhook(ctx, service(), chanA)
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/widget/", hd)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		f.Close()
		srv.Close()
		_ = engine.Close(context.Background())
	})
	client, err := engine.For(model.Principal{Tenant: tenant, Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	return &uploadRig{rig: &rig{t: t, ch: chanA, clock: clock, f: f, srv: srv}, store: st, client: client}
}

// upload posts body as a file of type typ and returns the status and
// handle.
func (r *uploadRig) upload(token, id, typ string, body []byte) (int, string) {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.t.Context(), http.MethodPost, r.srv.URL+"/widget/uploads?id="+id+"&name=shot.png", bytes.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", typ)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := r.srv.Client().Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out widget.UploadResponse
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out.Attachment
}

func (r *uploadRig) messages() []model.Message {
	r.t.Helper()
	var out []model.Message
	if err := r.store.View(r.t.Context(), tenant, func(rd store.Reader) error {
		p, err := rd.Messages(r.t.Context(), model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inboxA}}, Page: model.PageRequest{Limit: 10}})
		out = p.Items
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// A visitor uploads a file with its session token: the bytes are staged
// through the engine under the attachments configuration (larger than the
// webhook body limit, within the attachment limit), and the message that
// lists the returned handle references the staged attachment.
func TestVisitorUploadStagedAndReferenced(t *testing.T) {
	r := newUploadRig(t)
	alice := r.session("alice", widget.AssuranceVerified)
	png := bytes.Repeat([]byte("p"), 1500)
	code, handle := r.upload(alice.Token, "u-1", "image/png", png)
	if code != http.StatusCreated || handle == "" {
		t.Fatalf("upload = %d %q, want 201 with a handle", code, handle)
	}
	if code, again := r.upload(alice.Token, "u-1", "image/png", png); code != http.StatusCreated || again != handle {
		t.Fatalf("retried upload = %d %q, want the same handle %q", code, again, handle)
	}
	if code, _ := r.post("/widget/messages", alice.Token, widget.MessageRequest{ID: "c-1", Text: "see attached", Attachments: []string{handle}}); code != http.StatusAccepted {
		t.Fatalf("message = %d", code)
	}
	msgs := r.messages()
	if len(msgs) != 1 || len(msgs[0].Content.Parts) != 2 || msgs[0].Content.Parts[1].Attachment == nil {
		t.Fatalf("messages = %+v, want one with text and the attachment", msgs)
	}
	att, rc, err := r.client.Attachments().Open(t.Context(), model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "open"}, Ref: *msgs[0].Content.Parts[1].Attachment})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, png) || att.Metadata.MediaType != "image/png" || att.Metadata.Name != "shot.png" {
		t.Fatalf("attachment %+v bytes %q", att, got)
	}
}

// Uploads keep the attachments configuration and the session: a file over
// the attachment limit, of a refused type or without a token is refused,
// and a handle is the uploader's alone.
func TestVisitorUploadLimitsAndOwnership(t *testing.T) {
	r := newUploadRig(t)
	alice := r.session("alice", widget.AssuranceVerified)
	bob := r.session("bob", widget.AssuranceVerified)
	if code, _ := r.upload(alice.Token, "u-big", "image/png", bytes.Repeat([]byte("p"), 2049)); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload = %d, want 413", code)
	}
	if code, _ := r.upload(alice.Token, "u-gif", "image/gif", []byte("gif")); code != http.StatusUnprocessableEntity {
		t.Fatalf("refused type = %d, want 422", code)
	}
	if code, _ := r.upload("", "u-anon", "image/png", []byte("png")); code != http.StatusUnauthorized {
		t.Fatalf("upload without a token = %d, want 401", code)
	}
	code, handle := r.upload(alice.Token, "u-1", "image/png", []byte("png"))
	if code != http.StatusCreated {
		t.Fatalf("upload = %d", code)
	}
	if code, _ := r.post("/widget/messages", bob.Token, widget.MessageRequest{ID: "c-1", Attachments: []string{handle}}); code != http.StatusBadRequest {
		t.Fatalf("another visitor's handle = %d, want 400", code)
	}
	forged := handle[:len(handle)-2] + "AA"
	if code, _ := r.post("/widget/messages", alice.Token, widget.MessageRequest{ID: "c-2", Attachments: []string{forged}}); code != http.StatusBadRequest {
		t.Fatalf("forged handle = %d, want 400", code)
	}
	if n := len(r.messages()); n != 0 {
		t.Fatalf("messages = %d, want none", n)
	}
}
