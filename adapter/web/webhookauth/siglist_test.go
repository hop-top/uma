package webhookauth_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter/web/webhookauth"
)

// oldSecret is the previous secret of a rotation; longSecret is the new one.
const oldSecret = "fedcba9876543210fedcba9876543210"

func sigWith(secret, body string) string {
	return macHex(sha256New, []byte(secret), []byte(body))
}

// listCfg is the comma-delimited rotation header: "v1=<hex>,v1=<hex>".
func listCfg(max int) webhookauth.Config {
	return webhookauth.Config{
		Scheme: webhookauth.SchemeHMAC,
		Prefix: "v1=",
		SignatureList: webhookauth.SignatureList{
			Separator: webhookauth.SeparatorComma, Max: max,
		},
	}
}

func v1(sig string) string { return "v1=" + sig }

// --- default behaviour, pinned ------------------------------------------

// With the option off, the store key of a delivery is the digest of scope,
// source and decoded signature, exactly as before the option existed, so
// records an earlier build wrote still match. The key below was produced by
// the build without the option.
func TestDefaultReplayKeyUnchanged(t *testing.T) {
	rs := &recordingStore{ReplayStore: webhookauth.NewMemoryReplayStore(0, clock{now})}
	v := mustNew(t, bySignature(webhookauth.Config{}), secrets(longSecret), webhookauth.Deps{Replay: rs, Scope: "ch1"})
	if err := verify(t, v, hdr("X-Webhook-Signature", sigOf("b")), "b"); err != nil {
		t.Fatal(err)
	}
	const want = "1fa36c93c49cbae22178ffd0511616d706fe77fa441fc5f3a16f784af277b53a"
	if len(rs.keys) != 1 || rs.keys[0] != want {
		t.Fatalf("reserved %v, want [%s]", rs.keys, want)
	}
}

// Off, a list is not parsed: the whole value is one signature and does not
// decode, with the same error text as ever.
func TestListOffLeavesHeaderASingleSignature(t *testing.T) {
	good := sigOf("b")
	for _, tc := range []struct {
		name, header, want string
	}{
		{"comma list", good + "," + good, "webhookauth: request malformed: X-Webhook-Signature is not hex"},
		{"space list", good + " " + good, "webhookauth: request malformed: X-Webhook-Signature is not hex"},
		{"prefixed comma list", v1(good) + "," + v1(good), "webhookauth: request malformed: X-Webhook-Signature is not hex"},
		{"one signature twice as long", good + good, "webhookauth: request malformed: X-Webhook-Signature is 64 bytes, want 32"},
		{"wrong signature", strings.Repeat("00", 32), "webhookauth: credential invalid: X-Webhook-Signature"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, secrets(longSecret), webhookauth.Deps{})
			err := verify(t, v, hdr("X-Webhook-Signature", tc.header), "b")
			if err == nil || err.Error() != tc.want {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
	v := mustNew(t, webhookauth.Config{Scheme: webhookauth.SchemeHMAC}, secrets(longSecret), webhookauth.Deps{})
	if err := verify(t, v, hdr("X-Webhook-Signature", good), "b"); err != nil {
		t.Fatalf("single signature: %v", err)
	}
}

// --- documented vector ----------------------------------------------------

// svixConfig is the Standard Webhooks / Svix header convention: a list of
// space-delimited "v1,<base64>" entries over "{id}.{timestamp}.{body}".
func svixConfig() webhookauth.Config {
	return webhookauth.Config{
		Scheme:      webhookauth.SchemeHMAC,
		Header:      "Svix-Signature",
		Prefix:      "v1,",
		Encoding:    webhookauth.EncodingBase64,
		KeyEncoding: webhookauth.EncodingBase64,
		Payload:     "{id}.{timestamp}.{body}",
		Timestamp:   webhookauth.Timestamp{Header: "Svix-Timestamp"},
		Replay:      webhookauth.Replay{Source: webhookauth.ReplayByHeader, Header: "Svix-Id"},
		SignatureList: webhookauth.SignatureList{
			Separator: webhookauth.SeparatorSpace,
		},
	}
}

// The worked example of Svix's "verifying payloads manually" page
// (docs.svix.com/receiving/verifying-payloads/how-manual): secret
// whsec_plJ3nmyCDGBKInavdOK15jsl, id msg_loFOjxBNrRLzqYUf, timestamp
// 1731705121, payload {"event_type":"ping","data":{"success":true}}, and the
// signature it publishes. The vector was also recomputed independently. The
// extra list entries are constructed: the page documents the space-delimited
// list but publishes no second signature.
func TestSvixDocumentedVector(t *testing.T) {
	const (
		docKey = "plJ3nmyCDGBKInavdOK15jsl" // the part after "whsec_"
		id     = "msg_loFOjxBNrRLzqYUf"
		ts     = "1731705121"
		body   = `{"event_type":"ping","data":{"success":true}}`
		doc    = "v1,rAvfW3dJ/X/qxhsaXPOyyCGmRKsaKWcsNccKXlIktD0="
	)
	at := time.Unix(1731705121, 0)
	// A second, constructed secret and its signature over the same message.
	other := base64.StdEncoding.EncodeToString([]byte(oldSecret))
	otherKey, _ := base64.StdEncoding.DecodeString(other)
	m := macBase64(otherKey, id+"."+ts+"."+body)
	constructed := "v1," + m
	forged := "v1," + base64.StdEncoding.EncodeToString(make([]byte, 32))

	h := func(sig string) http.Header {
		return hdr("Svix-Id", id, "Svix-Timestamp", ts, "Svix-Signature", sig)
	}
	for _, tc := range []struct {
		name    string
		secrets []string
		sig     string
		want    error
	}{
		{"documented signature alone", []string{docKey}, doc, nil},
		{"documented signature after a stale one", []string{docKey}, forged + " " + doc, nil},
		{"documented signature before a stale one", []string{docKey}, doc + " " + forged, nil},
		{"rotation: the other secret's entry verifies", []string{docKey, other}, constructed + " " + forged, nil},
		{"rotation: both verify", []string{docKey, other}, doc + " " + constructed, nil},
		{"entry for a secret the host no longer holds", []string{docKey}, constructed + " " + doc, nil},
		{"only an unknown secret's entry", []string{docKey}, constructed, webhookauth.ErrInvalid},
		{"nothing verifies", []string{docKey}, forged + " " + forged, webhookauth.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := mustNew(t, svixConfig(), secrets(tc.secrets...), webhookauth.Deps{
				Clock: clock{at}, Replay: webhookauth.NewMemoryReplayStore(0, clock{at}),
			})
			_, err := v.Verify(context.Background(), webhookauth.Request{Header: h(tc.sig), Body: []byte(body)})
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				return
			}
			wantErr(t, err, tc.want)
		})
	}
}

// --- list parsing ---------------------------------------------------------

func TestSignatureListParsing(t *testing.T) {
	good, other := sigWith(longSecret, "b"), sigWith(oldSecret, "b")
	wrong := strings.Repeat("00", 32)
	b64 := base64.StdEncoding.EncodeToString(hexBytes(t, good))
	// A base64 value whose last character carries non-zero trailing bits:
	// it decodes loosely to the same bytes, and must not strictly.
	loose := []byte(b64)
	loose[len(loose)-2] = looseChar(loose[len(loose)-2])
	// One byte of the valid signature, appended: 33 bytes.
	long := good + "00"

	comma := listCfg(0)
	space := listCfg(0)
	space.SignatureList.Separator = webhookauth.SeparatorSpace

	type tc struct {
		name   string
		cfg    webhookauth.Config
		header string
		want   error // nil: accepted
	}
	cases := []tc{
		// delimiters
		{"comma: one entry", comma, v1(good), nil},
		{"comma: two entries", comma, v1(wrong) + "," + v1(good), nil},
		{"comma: valid first", comma, v1(good) + "," + v1(wrong), nil},
		{"space: one entry", space, v1(good), nil},
		{"space: two entries", space, v1(wrong) + " " + v1(good), nil},
		{"comma list under space delimiter", space, v1(wrong) + "," + v1(good), webhookauth.ErrMalformed},
		{"space list under comma delimiter", comma, v1(wrong) + " " + v1(good), webhookauth.ErrMalformed},
		{"tab is not a delimiter", space, v1(wrong) + "\t" + v1(good), webhookauth.ErrMalformed},
		// mixed valid and invalid entries
		{"all well-formed, none verifies", comma, v1(wrong) + "," + v1(wrong), webhookauth.ErrInvalid},
		{"the other secret's entry", comma, v1(other) + "," + v1(wrong), nil}, // both secrets are configured below
		{"valid hidden behind a malformed one", comma, v1("zz") + "," + v1(good), webhookauth.ErrMalformed},
		{"malformed after a valid one", comma, v1(good) + "," + v1("zz"), webhookauth.ErrMalformed},
		// duplicates
		{"same valid entry twice", comma, v1(good) + "," + v1(good), nil},
		{"same wrong entry twice", comma, v1(wrong) + "," + v1(wrong), webhookauth.ErrInvalid},
		{"same entry in two cases", comma, v1(good) + "," + v1(strings.ToUpper(good)), nil},
		// empty entries
		{"empty list entry first", comma, "," + v1(good), webhookauth.ErrMalformed},
		{"empty list entry last", comma, v1(good) + ",", webhookauth.ErrMalformed},
		{"empty list entry between", comma, v1(good) + ",," + v1(good), webhookauth.ErrMalformed},
		{"only a delimiter", comma, ",", webhookauth.ErrMalformed},
		{"two spaces", space, v1(good) + "  " + v1(good), webhookauth.ErrMalformed},
		{"empty after the prefix", comma, v1(good) + ",v1=", webhookauth.ErrMalformed},
		// the prefix applies to every entry
		{"entry without the prefix", comma, v1(good) + "," + good, webhookauth.ErrMalformed},
		{"entry of another scheme", comma, v1(good) + ",v0=" + good, webhookauth.ErrMalformed},
		{"prefix twice in an entry", comma, v1(good) + ",v1=v1=" + good, webhookauth.ErrMalformed},
		// whitespace is never trimmed from an entry
		{"space after a comma", comma, v1(good) + ", " + v1(good), webhookauth.ErrMalformed},
		{"space before a comma", comma, v1(good) + " ," + v1(good), webhookauth.ErrMalformed},
		{"outer whitespace is the transport's", comma, " \t" + v1(good) + "\t ", nil},
		// non-canonical encodings in the list are refused, never normalized
		{"wrong size in the list", comma, v1(good) + "," + v1(long), webhookauth.ErrMalformed},
		{"truncated in the list", comma, v1(good) + "," + v1(good[:32]), webhookauth.ErrMalformed},
		{"odd hex in the list", comma, v1(good) + "," + v1(good[:63]), webhookauth.ErrMalformed},
		{"base64 in a hex list", comma, v1(good) + "," + v1(b64), webhookauth.ErrMalformed},
		{"line break in an entry", comma, v1(good) + ",v1=" + good[:10] + "\n" + good[10:], webhookauth.ErrMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := mustNew(t, c.cfg, secrets(longSecret, oldSecret), webhookauth.Deps{})
			err := verify(t, v, hdr("X-Webhook-Signature", c.header), "b")
			if c.want == nil {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				return
			}
			wantErr(t, err, c.want)
		})
	}

	// Base64: the strict decoder holds per entry, for every alphabet.
	t.Run("base64 entries", func(t *testing.T) {
		raw := hexBytes(t, good)
		cfg := listCfg(0)
		cfg.Prefix = ""
		cfg.Encoding = webhookauth.EncodingBase64
		v := mustNew(t, cfg, secrets(longSecret), webhookauth.Deps{})
		hd := func(s string) error { return verify(t, v, hdr("X-Webhook-Signature", s), "b") }
		std := base64.StdEncoding.EncodeToString(raw)
		if err := hd(std + "," + std); err != nil {
			t.Fatalf("canonical list: %v", err)
		}
		for name, bad := range map[string]string{
			"non-zero trailing bits": string(loose),
			"unpadded":               base64.RawStdEncoding.EncodeToString(raw),
			"url alphabet":           strings.NewReplacer("+", "-", "/", "_").Replace(base64.StdEncoding.EncodeToString(append([]byte{0xfb, 0xff}, raw[2:]...))),
			"hex":                    good,
		} {
			if err := hd(std + "," + bad); !errors.Is(err, webhookauth.ErrMalformed) {
				t.Errorf("%s after a valid entry: %v, want malformed", name, err)
			}
			if err := hd(bad + "," + std); !errors.Is(err, webhookauth.ErrMalformed) {
				t.Errorf("%s before a valid entry: %v, want malformed", name, err)
			}
		}
		// The loose form alone would decode under a forgiving decoder and
		// verify; it is refused instead.
		wantErr(t, hd(string(loose)), webhookauth.ErrMalformed)
	})

	t.Run("optional prefix", func(t *testing.T) {
		cfg := listCfg(0)
		cfg.PrefixOptional = true
		v := mustNew(t, cfg, secrets(longSecret), webhookauth.Deps{})
		if err := verify(t, v, hdr("X-Webhook-Signature", good+","+v1(good)), "b"); err != nil {
			t.Fatalf("mixed prefixed and bare entries: %v", err)
		}
		wantErr(t, verify(t, v, hdr("X-Webhook-Signature", "v1=v1="+good+","+good), "b"), webhookauth.ErrMalformed)
	})

	// A list is one header line: a repeated header is still refused.
	t.Run("repeated header", func(t *testing.T) {
		v := mustNew(t, listCfg(0), secrets(longSecret), webhookauth.Deps{})
		h := hdr("X-Webhook-Signature", v1(good), "X-Webhook-Signature", v1(good))
		wantErr(t, verify(t, v, h, "b"), webhookauth.ErrMalformed)
	})
}

// A header that carries a timestamp element and a signature of another
// scheme beside the v1 signatures ("t=<ts>,v1=<sig>,v0=<sig>") is not
// supported: the embedded timestamp is not read and an element that is not a
// signature of the configured shape is not discarded, so the header is
// malformed (see docs/webhook-auth.md). The values are made up; they
// exercise the parser, not a MAC.
func TestMixedElementHeaderIsRefused(t *testing.T) {
	const header = "t=1492774577,v1=1111111111111111111111111111111111111111111111111111111111111111," +
		"v0=2222222222222222222222222222222222222222222222222222222222222222"
	v := mustNew(t, listCfg(0), secrets(longSecret), webhookauth.Deps{})
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", header), "{}"), webhookauth.ErrMalformed)
	// The v1 elements alone are the supported shape.
	v1s := "v1=1111111111111111111111111111111111111111111111111111111111111111"
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", v1s+","+v1s), "{}"), webhookauth.ErrInvalid)
}

// --- the cap --------------------------------------------------------------

func TestSignatureListCap(t *testing.T) {
	good, wrong := sigOf("b"), strings.Repeat("00", 32)
	list := func(n int, validAt int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = v1(wrong)
			if i == validAt {
				parts[i] = v1(good)
			}
		}
		return strings.Join(parts, ",")
	}
	for _, tc := range []struct {
		name string
		max  int
		n    int
		want error
	}{
		{"default cap, at the cap", 0, 8, nil},
		{"default cap, one over", 0, 9, webhookauth.ErrMalformed},
		{"explicit cap, at the cap", 3, 3, nil},
		{"explicit cap, one over", 3, 4, webhookauth.ErrMalformed},
		{"cap of one", 1, 1, nil},
		{"cap of one, two entries", 1, 2, webhookauth.ErrMalformed},
		{"highest cap, at the cap", 32, 32, nil},
		{"highest cap, one over", 32, 33, webhookauth.ErrMalformed},
		{"far over", 0, 5000, webhookauth.ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := mustNew(t, listCfg(tc.max), secrets(longSecret), webhookauth.Deps{})
			// The valid entry is the last one, within the cap or not: past the
			// cap a genuine signature is refused too.
			err := verify(t, v, hdr("X-Webhook-Signature", list(tc.n, tc.n-1)), "b")
			if tc.want == nil && err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if tc.want != nil {
				wantErr(t, err, tc.want)
			}
		})
	}
	// Over the cap is refused before anything is compared.
	var compares int
	restore := webhookauth.SpyEqual(func(a, b []byte) { compares++ })
	defer restore()
	v := mustNew(t, listCfg(2), secrets(longSecret), webhookauth.Deps{})
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", list(3, 0)), "b"), webhookauth.ErrMalformed)
	if compares != 0 {
		t.Fatalf("%d comparisons for an over-long list", compares)
	}
	// Entries are counted by delimiter, so the cap holds for any length.
	wantErr(t, verify(t, v, hdr("X-Webhook-Signature", strings.Repeat(",", 100)), "b"), webhookauth.ErrMalformed)
}

// --- constant time, no early exit ----------------------------------------

// Every entry is compared against every secret, whichever entry matches and
// wherever it sits: the comparisons made do not depend on the outcome.
func TestSignatureListComparesEveryCandidate(t *testing.T) {
	newSig, oldSig, wrong := sigWith(longSecret, "b"), sigWith(oldSecret, "b"), strings.Repeat("11", 32)
	for _, tc := range []struct {
		name   string
		header string
		ok     bool
	}{
		{"first matches", v1(newSig) + "," + v1(wrong) + "," + v1(wrong), true},
		{"last matches", v1(wrong) + "," + v1(wrong) + "," + v1(newSig), true},
		{"all match", v1(newSig) + "," + v1(oldSig) + "," + v1(newSig), true},
		{"none matches", v1(wrong) + "," + v1(wrong) + "," + v1(wrong), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type pair struct{ sig, mac string }
			var seen []pair
			restore := webhookauth.SpyEqual(func(a, b []byte) { seen = append(seen, pair{hex.EncodeToString(a), hex.EncodeToString(b)}) })
			defer restore()
			v := mustNew(t, listCfg(0), secrets(longSecret, oldSecret), webhookauth.Deps{})
			err := verify(t, v, hdr("X-Webhook-Signature", tc.header), "b")
			if (err == nil) != tc.ok {
				t.Fatalf("Verify: %v, want ok=%v", err, tc.ok)
			}
			// 3 entries x 2 secrets.
			if len(seen) != 6 {
				t.Fatalf("%d comparisons, want 6", len(seen))
			}
			macs := map[string]int{}
			for _, p := range seen {
				macs[p.mac]++
			}
			if macs[newSig] != 3 || macs[oldSig] != 3 {
				t.Fatalf("every entry must meet each recomputed MAC: %v", macs)
			}
		})
	}
}

// A signature of every entry is verified under every key for Ed25519 too,
// and an entry of one key does not stand in for another.
func TestSignatureListEd25519(t *testing.T) {
	seed := func(b byte) ed25519.PrivateKey {
		return ed25519.NewKeyFromSeed([]byte(strings.Repeat(string([]byte{b}), 32)))
	}
	k1, k2, k3 := seed(1), seed(2), seed(3)
	pub := func(k ed25519.PrivateKey) string { return hex.EncodeToString(k.Public().(ed25519.PublicKey)) }
	sign := func(k ed25519.PrivateKey, msg string) string {
		return "v1a," + base64.StdEncoding.EncodeToString(ed25519.Sign(k, []byte(msg)))
	}
	cfg := webhookauth.Config{
		Scheme: webhookauth.SchemeEd25519, Prefix: "v1a,", Encoding: webhookauth.EncodingBase64,
		KeyEncoding:   webhookauth.EncodingHex,
		SignatureList: webhookauth.SignatureList{Separator: webhookauth.SeparatorSpace},
	}
	v := mustNew(t, cfg, secrets(pub(k1), pub(k2)), webhookauth.Deps{})
	body := "payload"
	for _, tc := range []struct {
		name   string
		header string
		want   error
	}{
		{"first key", sign(k1, body), nil},
		{"second key", sign(k3, body) + " " + sign(k2, body), nil},
		{"both keys", sign(k1, body) + " " + sign(k2, body), nil},
		{"a key the host does not hold", sign(k3, body), webhookauth.ErrInvalid},
		{"signed another body", sign(k1, "other") + " " + sign(k2, "other"), webhookauth.ErrInvalid},
		{"truncated entry", sign(k1, body) + " " + sign(k2, body)[:50], webhookauth.ErrMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verify(t, v, hdr("X-Webhook-Signature", tc.header), body)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				return
			}
			wantErr(t, err, tc.want)
		})
	}
}

// --- timestamp window -----------------------------------------------------

// The window applies to the request, not to an entry: every entry covers
// the same timestamp, so a stale request is stale whichever entry verified,
// and nothing is reserved for it.
func TestSignatureListTimestampWindow(t *testing.T) {
	cfg := listCfg(0)
	cfg.Payload = "{timestamp}.{body}"
	cfg.Timestamp = webhookauth.Timestamp{Header: "X-Relay-Timestamp", Tolerance: time.Minute}
	cfg.Replay = webhookauth.Replay{Source: webhookauth.ReplayBySignature}
	body := "b"
	signed := func(at time.Time) string {
		ts := unix(at)
		return v1(sigWith(oldSecret, ts+"."+body)) + "," + v1(sigWith(longSecret, ts+"."+body))
	}
	for _, tc := range []struct {
		name string
		at   time.Time
		want error
	}{
		{"fresh", now, nil},
		{"edge past", now.Add(-time.Minute), nil},
		{"edge future", now.Add(time.Minute), nil},
		{"stale", now.Add(-time.Minute - time.Second), webhookauth.ErrStale},
		{"too far ahead", now.Add(time.Minute + time.Second), webhookauth.ErrStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := &recordingStore{ReplayStore: webhookauth.NewMemoryReplayStore(0, clock{now})}
			v := mustNew(t, cfg, secrets(longSecret, oldSecret), webhookauth.Deps{Replay: rs})
			h := hdr("X-Webhook-Signature", signed(tc.at), "X-Relay-Timestamp", unix(tc.at))
			err := verify(t, v, h, body)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Verify: %v", err)
				}
				if len(rs.keys) != 2 {
					t.Fatalf("reserved %d keys, want 2", len(rs.keys))
				}
				return
			}
			wantErr(t, err, tc.want)
			if len(rs.keys) != 0 {
				t.Fatalf("a stale request reserved %d keys", len(rs.keys))
			}
		})
	}
	// The list is not a way around the signed timestamp: another timestamp
	// header is another message, and no entry verifies.
	v := mustNew(t, cfg, secrets(longSecret, oldSecret), webhookauth.Deps{Replay: webhookauth.NewMemoryReplayStore(0, clock{now})})
	h := hdr("X-Webhook-Signature", signed(now.Add(-time.Hour)), "X-Relay-Timestamp", unix(now))
	wantErr(t, verify(t, v, h, body), webhookauth.ErrInvalid)
	// A malformed timestamp is refused before the list is looked at.
	h = hdr("X-Webhook-Signature", signed(now), "X-Relay-Timestamp", "yesterday")
	wantErr(t, verify(t, v, h, body), webhookauth.ErrMalformed)
}

// --- replay ---------------------------------------------------------------

// Replay identifies a delivery by the signatures that verified. The list is
// not signed, so a replayed request can drop entries: every verifying
// signature is a reservation, and a delivery with any of them taken before
// is a replay. A forged entry reserves nothing.
func TestSignatureListReplay(t *testing.T) {
	newSig, oldSig := sigWith(longSecret, "b"), sigWith(oldSecret, "b")
	wrong := strings.Repeat("11", 32)
	cfg := listCfg(0)
	cfg.Replay.Source = webhookauth.ReplayBySignature
	setup := func() (*webhookauth.Verifier, *recordingStore) {
		rs := &recordingStore{ReplayStore: webhookauth.NewMemoryReplayStore(0, clock{now})}
		return mustNew(t, cfg, secrets(longSecret, oldSecret), webhookauth.Deps{Replay: rs, Scope: "ch1"}), rs
	}
	send := func(v *webhookauth.Verifier, body string, entries ...string) error {
		return verify(t, v, hdr("X-Webhook-Signature", strings.Join(entries, ",")), body)
	}

	t.Run("a copy with another subset of entries is a replay", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			second []string
		}{
			{"same list", []string{v1(oldSig), v1(newSig)}},
			{"reordered", []string{v1(newSig), v1(oldSig)}},
			{"only the new entry", []string{v1(newSig)}},
			{"only the old entry", []string{v1(oldSig)}},
			{"with a forged entry added", []string{v1(wrong), v1(newSig)}},
			{"with a duplicate", []string{v1(newSig), v1(newSig)}},
		} {
			v, _ := setup()
			if err := send(v, "b", v1(oldSig), v1(newSig)); err != nil {
				t.Fatalf("%s: first delivery: %v", tc.name, err)
			}
			wantErr(t, send(v, "b", tc.second...), webhookauth.ErrReplay)
		}
	})

	t.Run("every verifying signature is reserved once, forged ones never", func(t *testing.T) {
		v, rs := setup()
		if err := send(v, "b", v1(wrong), v1(oldSig), v1(newSig), v1(oldSig), v1(wrong)); err != nil {
			t.Fatal(err)
		}
		if len(rs.keys) != 2 {
			t.Fatalf("reserved %d keys, want 2 (the distinct verifying signatures)", len(rs.keys))
		}
		if !sort.StringsAreSorted(rs.keys) {
			t.Fatalf("keys reserved out of order: %v", rs.keys)
		}
		// Another message is another delivery.
		if err := send(v, "c", v1(sigWith(oldSecret, "c")), v1(sigWith(longSecret, "c"))); err != nil {
			t.Fatalf("other message: %v", err)
		}
	})

	t.Run("a forged request reserves nothing", func(t *testing.T) {
		v, rs := setup()
		wantErr(t, send(v, "b", v1(wrong), v1(wrong)), webhookauth.ErrInvalid)
		wantErr(t, send(v, "b", v1(newSig), "v1=zz"), webhookauth.ErrMalformed)
		if len(rs.keys) != 0 {
			t.Fatalf("reserved %d keys", len(rs.keys))
		}
		if err := send(v, "b", v1(newSig)); err != nil {
			t.Fatalf("genuine delivery after forged ones: %v", err)
		}
	})

	// Identity is the signature: the same message signed under a secret the
	// first delivery did not carry a signature for is not recognised. This
	// is the rule that already held for two configured secrets, and a host
	// that needs message-level identity keys on a signed id.
	t.Run("a signature never seen is a new delivery", func(t *testing.T) {
		v, _ := setup()
		if err := send(v, "b", v1(oldSig)); err != nil {
			t.Fatal(err)
		}
		if err := send(v, "b", v1(newSig)); err != nil {
			t.Fatalf("same message, signature not seen before: %v", err)
		}
		wantErr(t, send(v, "b", v1(oldSig), v1(newSig)), webhookauth.ErrReplay)
	})

	t.Run("release frees every key", func(t *testing.T) {
		v, rs := setup()
		res, err := v.Verify(context.Background(), webhookauth.Request{
			Header: hdr("X-Webhook-Signature", v1(oldSig)+","+v1(newSig)), Body: []byte("b"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := res.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := res.Release(context.Background()); err != nil {
			t.Fatalf("second release: %v", err)
		}
		if len(rs.released) != 2 {
			t.Fatalf("released %d keys, want 2", len(rs.released))
		}
		if err := send(v, "b", v1(newSig)); err != nil {
			t.Fatalf("retry after release: %v", err)
		}
	})

	t.Run("a store failure midway keeps nothing", func(t *testing.T) {
		rs := &failSecond{recordingStore: recordingStore{ReplayStore: webhookauth.NewMemoryReplayStore(0, clock{now})}}
		v := mustNew(t, cfg, secrets(longSecret, oldSecret), webhookauth.Deps{Replay: rs, Scope: "ch1"})
		wantErr(t, send(v, "b", v1(oldSig), v1(newSig)), webhookauth.ErrUnavailable)
		if len(rs.released) != 1 {
			t.Fatalf("released %d keys, want the one reserved before the failure", len(rs.released))
		}
		rs.fail = false
		if err := send(v, "b", v1(oldSig), v1(newSig)); err != nil {
			t.Fatalf("the sender's retry: %v", err)
		}
	})

	t.Run("concurrent copies: one wins", func(t *testing.T) {
		for round := 0; round < 40; round++ {
			store := webhookauth.NewMemoryReplayStore(0, clock{now})
			v := mustNew(t, cfg, secrets(longSecret, oldSecret), webhookauth.Deps{Replay: store, Scope: "ch1"})
			var wg sync.WaitGroup
			var mu sync.Mutex
			wins := 0
			// Copies of one delivery as a sender retries it: the same
			// signatures in any order. A copy carrying only one of them is a
			// delivery of its own, as the subtests above settle.
			lists := [][]string{{v1(oldSig), v1(newSig)}, {v1(newSig), v1(oldSig)}, {v1(oldSig), v1(newSig)}, {v1(newSig), v1(oldSig)}}
			for _, l := range lists {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if send(v, "b", l...) == nil {
						mu.Lock()
						wins++
						mu.Unlock()
					}
				}()
			}
			wg.Wait()
			if wins != 1 {
				t.Fatalf("round %d: %d deliveries accepted, want 1", round, wins)
			}
		}
	})

	t.Run("replay by header id is one key whatever the list", func(t *testing.T) {
		hc := listCfg(0)
		hc.Payload = "{id}.{body}"
		hc.Replay = webhookauth.Replay{Source: webhookauth.ReplayByHeader, Header: "X-Delivery-Id"}
		rs := &recordingStore{ReplayStore: webhookauth.NewMemoryReplayStore(0, clock{now})}
		v := mustNew(t, hc, secrets(longSecret, oldSecret), webhookauth.Deps{Replay: rs})
		both := v1(sigWith(oldSecret, "d1.b")) + "," + v1(sigWith(longSecret, "d1.b"))
		h := hdr("X-Webhook-Signature", both, "X-Delivery-Id", "d1")
		if err := verify(t, v, h, "b"); err != nil {
			t.Fatal(err)
		}
		if len(rs.keys) != 1 {
			t.Fatalf("reserved %d keys, want 1", len(rs.keys))
		}
		h = hdr("X-Webhook-Signature", v1(sigWith(longSecret, "d1.b")), "X-Delivery-Id", "d1")
		wantErr(t, verify(t, v, h, "b"), webhookauth.ErrReplay)
	})
}

// failSecond fails its second reservation once.
type failSecond struct {
	recordingStore
	n    int
	fail bool
}

func (s *failSecond) Reserve(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if s.n++; s.n == 2 && !s.fail {
		s.fail = true
		return false, errors.New("store down")
	}
	return s.recordingStore.Reserve(ctx, key, ttl)
}

// With the option off, replay by signature still reserves exactly one key,
// even with two secrets configured.
func TestSingleSignatureReservesOneKey(t *testing.T) {
	rs := &recordingStore{ReplayStore: webhookauth.NewMemoryReplayStore(0, clock{now})}
	v := mustNew(t, bySignature(webhookauth.Config{}), secrets(longSecret, oldSecret), webhookauth.Deps{Replay: rs})
	if err := verify(t, v, hdr("X-Webhook-Signature", sigOf("b")), "b"); err != nil {
		t.Fatal(err)
	}
	if len(rs.keys) != 1 {
		t.Fatalf("reserved %d keys", len(rs.keys))
	}
}

// --- configuration --------------------------------------------------------

func TestSignatureListConfigErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  webhookauth.Config
	}{
		{"unknown separator", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, SignatureList: webhookauth.SignatureList{Separator: ";"}}},
		{"two-character separator", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, SignatureList: webhookauth.SignatureList{Separator: ", "}}},
		{"max without separator", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, SignatureList: webhookauth.SignatureList{Max: 4}}},
		{"negative max", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, SignatureList: webhookauth.SignatureList{Separator: ",", Max: -1}}},
		{"max over the ceiling", webhookauth.Config{Scheme: webhookauth.SchemeHMAC, SignatureList: webhookauth.SignatureList{Separator: ",", Max: 33}}},
		{"bearer", webhookauth.Config{Scheme: webhookauth.SchemeBearer, SignatureList: webhookauth.SignatureList{Separator: ","}}},
		{"token", webhookauth.Config{Scheme: webhookauth.SchemeToken, SignatureList: webhookauth.SignatureList{Separator: ","}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := webhookauth.New(tc.cfg, secrets(longSecret), webhookauth.Deps{})
			wantErr(t, err, webhookauth.ErrConfig)
		})
	}
}

// --- helpers --------------------------------------------------------------

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func macBase64(key []byte, msg string) string {
	sum, _ := hex.DecodeString(macHex(sha256.New, key, []byte(msg)))
	return base64.StdEncoding.EncodeToString(sum)
}

// looseChar returns a base64 character for the last data character of a
// 32-byte value that sets the unused trailing bits.
func looseChar(c byte) byte {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	return alphabet[strings.IndexByte(alphabet, c)|1]
}
