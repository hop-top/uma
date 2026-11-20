package mime

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	stdmime "mime"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"
	"golang.org/x/text/encoding/htmlindex"
)

// Default parse limits. RFC 2045 and RFC 2046 cap neither the number of parts
// nor the nesting of multiparts, so the defaults are headroom over the mail
// seen in practice, not protocol bounds: clients nest a body three to six
// multiparts deep (mixed, related, alternative, signed), and a message
// inside message/rfc822 is a part of its own and does not nest, so 16 admits
// them with room to spare; a newsletter or digest of a few hundred parts is
// plausible, so 256. What the limits bound is the work done for a message the
// size bound already admitted: a message past them is a bomb, or broken.
const (
	DefaultMaxParts       = 256
	DefaultMaxDepth       = 16
	DefaultMaxTextBytes   = 1 << 20
	DefaultMaxHeaderBytes = 256 << 10
)

// Limits bounds Parse. A zero field takes its default.
type Limits struct {
	// MaxParts is the number of MIME parts walked, multipart containers
	// included; further parts end the parse with ErrLimit.
	MaxParts int
	// MaxDepth is the multipart nesting depth; deeper parts end the parse
	// with ErrLimit.
	MaxDepth int
	// MaxTextBytes is the decoded text kept; longer text is truncated at a
	// UTF-8 boundary and Message.TextTruncated is set.
	MaxTextBytes int
	// MaxHeaderBytes bounds each header block.
	MaxHeaderBytes int64
	// KeepBytes keeps the decoded bytes of an attachment of at most this
	// size in Attachment.Data; zero keeps none. Larger attachments are
	// described only.
	KeepBytes int64
	// KeepParts is the number of attachments whose bytes are kept, in
	// message order; an attachment over KeepBytes uses none. Zero keeps
	// none.
	KeepParts int
}

func (l Limits) withDefaults() Limits {
	if l.MaxParts <= 0 {
		l.MaxParts = DefaultMaxParts
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultMaxDepth
	}
	if l.MaxTextBytes <= 0 {
		l.MaxTextBytes = DefaultMaxTextBytes
	}
	if l.MaxHeaderBytes <= 0 {
		l.MaxHeaderBytes = DefaultMaxHeaderBytes
	}
	return l
}

// ErrMalformed is wrapped by Parse errors caused by the message itself
// (syntax, limits), as opposed to read errors.
var ErrMalformed = errors.New("mime: malformed message")

// ErrLimit is wrapped by Parse errors caused by the structure limits
// (Limits.MaxParts, Limits.MaxDepth). It wraps ErrMalformed, so a caller that
// refuses a malformed message refuses these too. Unlike other Parse errors it
// comes with a message: the header, the text and the parts read before the
// limit, with Message.PartsOmitted set. A caller that can deliver a partial
// message may use it; the parts after the limit are not read.
var ErrLimit = fmt.Errorf("%w: structure limit", ErrMalformed)

// Address is one mailbox: a decoded display name and a bare address.
type Address struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// Header holds the header fields of one message. Threading fields keep their
// raw values: normalizing them is the caller's (model.EmailHeaders).
type Header struct {
	From    []Address
	Sender  []Address
	ReplyTo []Address
	To      []Address
	Cc      []Address
	Subject string
	// Date is the parsed Date field; zero when absent or invalid.
	Date          time.Time
	MessageID     string
	InReplyTo     string
	References    string
	ListID        string
	AutoSubmitted string
	// Fields is every header field in order, raw (unfolded, undecoded).
	Fields []Field
}

// Field is one raw header field.
type Field struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Values returns the raw values of every field named key, in order,
// case-insensitively.
func (h Header) Values(key string) []string {
	var out []string
	for _, f := range h.Fields {
		if strings.EqualFold(f.Key, key) {
			out = append(out, f.Value)
		}
	}
	return out
}

// Attachment describes one non-text part. Its bytes are in Message.Data
// when Limits asked for them.
type Attachment struct {
	// Filename is the sender's suggested name, decoded and run through
	// SanitizeFilename: a base name for display, empty when the part has
	// no name or none is usable. It never locates the staged bytes.
	Filename  string `json:"filename,omitempty"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	// Digest is "sha256:" and the lowercase hex SHA-256 of the decoded bytes.
	Digest    string `json:"digest"`
	ContentID string `json:"content_id,omitempty"`
	Inline    bool   `json:"inline,omitempty"`
	// Staged reports that the bytes were handed to the host's blob store
	// and the message's content names them in an attachment part. The
	// caller sets it; Parse never does.
	Staged bool `json:"staged,omitempty"`
}

// Message is one parsed message.
type Message struct {
	Header Header
	// Text is the body text, UTF-8, possibly empty.
	Text string
	// TextTruncated reports that Text was cut at Limits.MaxTextBytes.
	TextTruncated bool
	// FromHTML reports that Text was rendered from a text/html part.
	FromHTML    bool
	Attachments []Attachment
	// PartsOmitted reports that a structure limit ended the parse: the
	// parts after it, the text among them, were not read. Parse sets it
	// only on the message it returns alongside ErrLimit.
	PartsOmitted bool
	// Data holds the decoded bytes of Attachments, index for index, for
	// those kept within Limits.KeepBytes and Limits.KeepParts: nil for an
	// attachment not kept, and for an empty one. It is nil when Limits
	// keeps nothing.
	Data [][]byte
}

// Parse reads one RFC 5322 message from r. When a structure limit ends the
// parse it returns the message read so far, PartsOmitted set, with an error
// wrapping ErrLimit; any other error comes with no message.
func Parse(r io.Reader, l Limits) (*Message, error) {
	l = l.withDefaults()
	lr := &headerLimit{r: r, n: l.MaxHeaderBytes}
	br := bufio.NewReader(lr)
	th, err := textproto.ReadHeader(br)
	if err != nil {
		return nil, fmt.Errorf("%w: header: %w", ErrMalformed, err)
	}
	lr.n = math.MaxInt64
	p := &parser{l: l, msg: &Message{Header: decodeHeader(th)}}
	ent, err := message.New(message.Header{Header: th}, br)
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if err := p.walk(ent, 0); err != nil {
		if !errors.Is(err, ErrLimit) {
			return nil, err
		}
		p.msg.PartsOmitted = true
		p.finish()
		return p.msg, err
	}
	p.finish()
	return p.msg, nil
}

// headerLimit bounds the bytes read while n is finite: the header block.
type headerLimit struct {
	r io.Reader
	n int64
}

var errHeaderTooBig = fmt.Errorf("%w: header too large", ErrMalformed)

func (h *headerLimit) Read(p []byte) (int, error) {
	if h.n <= 0 {
		return 0, errHeaderTooBig
	}
	if int64(len(p)) > h.n {
		p = p[:h.n]
	}
	n, err := h.r.Read(p)
	h.n -= int64(n)
	return n, err
}

type parser struct {
	l     Limits
	msg   *Message
	parts int
	kept  int
	plain *string
	html  *string
}

func (p *parser) walk(ent *message.Entity, depth int) error {
	p.parts++
	if p.parts > p.l.MaxParts {
		return fmt.Errorf("%w: more than %d parts", ErrLimit, p.l.MaxParts)
	}
	mediaType, params, _ := ent.Header.ContentType()
	mediaType = strings.ToLower(mediaType)
	if mr := ent.MultipartReader(); mr != nil {
		if depth >= p.l.MaxDepth {
			return fmt.Errorf("%w: multipart nested deeper than %d", ErrLimit, p.l.MaxDepth)
		}
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
				return fmt.Errorf("%w: %w", ErrMalformed, err)
			}
			if part == nil {
				return fmt.Errorf("%w: unreadable part", ErrMalformed)
			}
			if err := p.walk(part, depth+1); err != nil {
				return err
			}
		}
	}
	disp, dparams, _ := ent.Header.ContentDisposition()
	disp = strings.ToLower(disp)
	filename := dparams["filename"]
	if filename == "" {
		filename = params["name"]
	}
	textual := mediaType == "text/plain" || mediaType == "text/html"
	if textual && disp != "attachment" && filename == "" {
		if mediaType == "text/plain" && p.plain == nil {
			s, err := p.text(ent, params["charset"])
			if err != nil {
				return err
			}
			p.plain = &s
			return nil
		}
		if mediaType == "text/html" && p.html == nil {
			s, err := p.text(ent, params["charset"])
			if err != nil {
				return err
			}
			p.html = &s
			return nil
		}
	}
	a, data, err := p.describe(ent, mediaType, filename, disp)
	if err != nil {
		return err
	}
	p.msg.Attachments = append(p.msg.Attachments, a)
	if p.l.KeepBytes > 0 && p.l.KeepParts > 0 {
		p.msg.Data = append(p.msg.Data, data)
	}
	return nil
}

// text reads a text part's body as UTF-8. The body is bounded by the
// message size the caller accepted; only MaxTextBytes are kept.
func (p *parser) text(ent *message.Entity, charset string) (string, error) {
	body, err := decodeCharset(ent.Body, charset)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	return strings.ToValidUTF8(string(b), "�"), nil
}

func (p *parser) finish() {
	var s string
	switch {
	case p.plain != nil && strings.TrimSpace(*p.plain) != "":
		s = *p.plain
	case p.html != nil:
		s, p.msg.FromHTML = htmlText(*p.html), true
	case p.plain != nil:
		s = *p.plain
	}
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if len(s) > p.l.MaxTextBytes {
		cut := p.l.MaxTextBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s, p.msg.TextTruncated = s[:cut], true
	}
	p.msg.Text = s
}

func (p *parser) describe(ent *message.Entity, mediaType, filename, disp string) (Attachment, []byte, error) {
	h := sha256.New()
	var w io.Writer = h
	var keep *keepBuffer
	if p.l.KeepBytes > 0 && p.kept < p.l.KeepParts {
		keep = &keepBuffer{max: p.l.KeepBytes}
		w = io.MultiWriter(h, keep)
	}
	n, err := io.Copy(w, ent.Body)
	if err != nil {
		return Attachment{}, nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	var data []byte
	if keep != nil && !keep.over && n > 0 {
		data = keep.b
		p.kept++
	}
	return Attachment{
		Filename:  SanitizeFilename(decodeWords(filename)),
		MediaType: mediaType,
		Size:      n,
		Digest:    "sha256:" + hex.EncodeToString(h.Sum(nil)),
		ContentID: strings.Trim(strings.TrimSpace(ent.Header.Get("Content-Id")), "<>"),
		Inline:    disp == "inline",
	}, data, nil
}

// keepBuffer collects the bytes written to it until they exceed max, then
// drops them and ignores the rest.
type keepBuffer struct {
	b    []byte
	max  int64
	over bool
}

func (k *keepBuffer) Write(p []byte) (int, error) {
	switch {
	case k.over:
	case int64(len(k.b))+int64(len(p)) > k.max:
		k.b, k.over = nil, true
	default:
		k.b = append(k.b, p...)
	}
	return len(p), nil
}

// decodeCharset converts r from charset to UTF-8. go-message already
// converted utf-8 and us-ascii, and any charset when a host installed a
// global message.CharsetReader; an unknown label passes bytes through.
func decodeCharset(r io.Reader, charset string) (io.Reader, error) {
	cs := strings.ToLower(strings.TrimSpace(charset))
	switch cs {
	case "", "utf-8", "utf8", "us-ascii":
		return r, nil
	}
	if message.CharsetReader != nil {
		return r, nil
	}
	enc, err := htmlindex.Get(cs)
	if err != nil {
		return r, nil
	}
	return enc.NewDecoder().Reader(r), nil
}

// charsetReader is the encoded-word charset hook: WHATWG labels, no global
// registration.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(strings.ToLower(charset))
	if err != nil {
		return nil, fmt.Errorf("mime: unknown charset %q", charset)
	}
	return enc.NewDecoder().Reader(input), nil
}

var wordDecoder = &stdmime.WordDecoder{CharsetReader: charsetReader}

// decodeWords decodes RFC 2047 encoded words; undecodable input is kept.
func decodeWords(s string) string {
	d, err := wordDecoder.DecodeHeader(s)
	if err != nil {
		return s
	}
	return d
}

func decodeHeader(h textproto.Header) Header {
	out := Header{
		Subject:       decodeWords(h.Get("Subject")),
		MessageID:     h.Get("Message-Id"),
		InReplyTo:     h.Get("In-Reply-To"),
		References:    h.Get("References"),
		ListID:        h.Get("List-Id"),
		AutoSubmitted: h.Get("Auto-Submitted"),
		From:          addresses(h.Get("From")),
		Sender:        addresses(h.Get("Sender")),
		ReplyTo:       addresses(h.Get("Reply-To")),
		To:            addresses(h.Get("To")),
		Cc:            addresses(h.Get("Cc")),
	}
	if d := h.Get("Date"); d != "" {
		if t, err := mail.ParseDate(d); err == nil {
			out.Date = t
		}
	}
	fields := h.Fields()
	for fields.Next() {
		out.Fields = append(out.Fields, Field{Key: fields.Key(), Value: fields.Value()})
	}
	return out
}

var addressParser = &mail.AddressParser{WordDecoder: wordDecoder}

// addresses parses an address list; groups are flattened and invalid lists
// yield none.
func addresses(v string) []Address {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	list, err := addressParser.ParseList(v)
	if err != nil {
		return nil
	}
	out := make([]Address, 0, len(list))
	for _, a := range list {
		// Encoded words inside a quoted display name are not RFC 2047, but
		// common; net/mail leaves them encoded.
		out = append(out, Address{Name: decodeWords(a.Name), Address: a.Address})
	}
	return out
}

// ParseHeader reads the header block of the RFC 5322 message in r and no
// more: the decoded Header, as Parse reports it, without walking the body.
// maxBytes bounds the block (zero: DefaultMaxHeaderBytes); a longer block,
// or one that is not a header, is an error wrapping ErrMalformed.
func ParseHeader(r io.Reader, maxBytes int64) (Header, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxHeaderBytes
	}
	th, err := textproto.ReadHeader(bufio.NewReader(&headerLimit{r: r, n: maxBytes}))
	if err != nil {
		return Header{}, fmt.Errorf("%w: header: %w", ErrMalformed, err)
	}
	return decodeHeader(th), nil
}
