package operations

import (
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"strconv"

	"hop.top/uma/model"
)

// FingerprintVersion1 is the canonical operation fingerprint encoding this
// version writes. Changing the encoding or a command's field set requires a
// new version: a stored fingerprint is only compared with one of the same
// version.
const FingerprintVersion1 uint32 = 1

// fingerprintDomain separates operation fingerprints from any other SHA-256
// use of the same bytes.
const fingerprintDomain = "uma.operation.fingerprint"

// Fingerprint collects the effectful fields of one command for the version 1
// canonical encoding:
//
//	sha256( lp(domain) || u32be(version) || u32be(n) || for each field in
//	        ascending byte order of name: lp(name) || lp(value) )
//
// where lp(b) is a uvarint length followed by b. Values are themselves
// length-prefixed field sequences (see Writer), so no two distinct field
// sets share an encoding.
//
// A fingerprint covers effectful payload, destination, audience and expected
// revision. It never covers diagnostic correlation or timeout budgets, which
// may change between redeliveries of one command, nor the operation key
// fields (tenant, caller, kind, target, operation ID) beyond the kind and
// target the command names.
type Fingerprint struct {
	fields map[string][]byte
}

// NewFingerprint returns an empty field set.
func NewFingerprint() *Fingerprint { return &Fingerprint{fields: map[string][]byte{}} }

// Set records field name. Each name is set once; a command's field set is
// fixed per version.
func (f *Fingerprint) Set(name string, w *Writer) {
	if _, dup := f.fields[name]; dup {
		panic("uma: duplicate fingerprint field " + name)
	}
	f.fields[name] = w.Bytes()
}

// Sum returns the version 1 digest.
func (f *Fingerprint) Sum() []byte {
	names := make([]string, 0, len(f.fields))
	for n := range f.fields {
		names = append(names, n)
	}
	slices.Sort(names)
	var w Writer
	w.Str(fingerprintDomain)
	w.U32(FingerprintVersion1)
	w.U32(uint32(len(names))) // #nosec G115 -- a command has a handful of fields
	for _, n := range names {
		w.Str(n)
		w.Raw(f.fields[n])
	}
	d := sha256.Sum256(w.Bytes())
	return d[:]
}

// Writer builds one canonical value: a sequence of length-prefixed items.
type Writer struct{ b []byte }

// Field returns an empty value writer.
func Field() *Writer { return &Writer{} }

// Bytes returns the encoded value.
func (w *Writer) Bytes() []byte { return w.b }

// Raw writes b length-prefixed.
func (w *Writer) Raw(b []byte) *Writer {
	w.b = binary.AppendUvarint(w.b, uint64(len(b)))
	w.b = append(w.b, b...)
	return w
}

// Str writes s length-prefixed.
func (w *Writer) Str(s string) *Writer { return w.Raw([]byte(s)) }

// U32 writes v as four big-endian bytes.
func (w *Writer) U32(v uint32) *Writer {
	w.b = binary.BigEndian.AppendUint32(w.b, v)
	return w
}

// Present writes a presence marker: optional values encode as absent or as
// present followed by their items, so absent and empty differ.
func (w *Writer) Present(ok bool) *Writer {
	if ok {
		return w.Str("1")
	}
	return w.Str("0")
}

// Ref writes a reference's tenant, kind, inbox and ID.
func (w *Writer) Ref(r model.Ref) *Writer {
	return w.Str(string(r.Tenant)).Str(string(r.Kind)).Str(string(r.Inbox)).Str(string(r.ID))
}

// Audience writes an audience's visibility and policy reference.
func (w *Writer) Audience(a model.Audience) *Writer {
	return w.Str(string(a.Visibility)).Str(a.PolicyRef)
}

// Extension writes an extension's name, version and data.
func (w *Writer) Extension(e model.Extension) *Writer {
	return w.Str(e.Name).U32(e.Version).Raw(e.Data)
}

// Content encodes parts in order; part order is meaningful.
func (w *Writer) Content(c model.Content) *Writer {
	w.Str(strconv.Itoa(len(c.Parts)))
	for _, p := range c.Parts {
		w.Str(p.Kind).Str(p.Text)
		w.Present(p.Attachment != nil)
		if p.Attachment != nil {
			w.Ref(model.Ref{Tenant: p.Attachment.Inbox.Tenant, Kind: model.KindAttachment, Inbox: p.Attachment.Inbox.ID, ID: p.Attachment.ID})
		}
		w.Present(p.Structured != nil)
		if p.Structured != nil {
			w.Extension(*p.Structured)
		}
	}
	return w
}
