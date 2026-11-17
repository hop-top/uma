package xrrtest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	xrr "hop.top/xrr"
	xrrhttp "hop.top/xrr/adapters/http"
)

// occurrenceParam carries the occurrence number into the fingerprint of the
// second and later identical requests. It is never sent or saved.
const occurrenceParam = "xrrtest-occurrence"

// Request is the recorded form of an outgoing request, as seen by
// NormalizeFunc hooks.
type Request struct {
	Method string
	URL    *url.URL
	// Header holds only allow-listed headers.
	Header http.Header
	Body   []byte
	// ContentType is the original Content-Type, kept even when the
	// header is not recorded.
	ContentType string
}

// Transport is an http.RoundTripper backed by an xrr cassette directory.
// It is safe for concurrent use, but concurrent identical requests get
// occurrence numbers in arrival order, which replay cannot reproduce.
type Transport struct {
	tb      testing.TB
	dir     string
	cfg     *config
	session *xrr.FileSession
	adapter *adapter

	mu   sync.Mutex
	seen map[string]int
}

// NewClient returns an *http.Client using NewTransport.
func NewClient(tb testing.TB, dir string, opts ...Option) *http.Client {
	tb.Helper()
	return &http.Client{Transport: NewTransport(tb, dir, opts...)}
}

// NewTransport returns a Transport that records to and replays from dir.
// The mode is WithMode, else XRR_MODE, else replay. Record mode creates
// dir. A replay request with no cassette fails tb with the fingerprint and
// file it looked for.
func NewTransport(tb testing.TB, dir string, opts ...Option) *Transport {
	tb.Helper()
	cfg := newConfig(opts)
	mode := cfg.mode
	if !cfg.modeSet {
		mode = Mode(os.Getenv(EnvMode))
		if mode == "" {
			mode = ModeReplay
		}
	}
	switch mode {
	case ModeRecord:
		if err := os.MkdirAll(dir, 0o750); err != nil {
			tb.Fatalf("xrrtest: create cassette dir %s: %v", dir, err)
			return nil
		}
	case ModeReplay, ModePassthrough:
	default:
		tb.Fatalf("xrrtest: %s=%q is not a valid mode (want record, replay or passthrough)", EnvMode, mode)
		return nil
	}
	return &Transport{
		tb:      tb,
		dir:     dir,
		cfg:     cfg,
		session: xrr.NewSession(mode, xrr.NewFileCassette(dir)),
		adapter: &adapter{},
		seen:    map[string]int{},
	}
}

// Mode reports the mode in effect.
func (t *Transport) Mode() Mode { return t.session.Mode() }

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := readBody(req)
	if err != nil {
		return nil, err
	}

	rec := t.recordedRequest(req, body)
	fp, err := t.fingerprint(rec)
	if err != nil {
		return nil, err
	}

	resp, err := t.session.Record(req.Context(), t.adapter, rec, func() (xrr.Response, error) {
		return t.forward(req, body)
	})
	if errors.Is(err, xrr.ErrCassetteMiss) {
		file := filepath.Join(t.dir, fmt.Sprintf("%s-%s.resp.yaml", t.adapter.ID(), fp))
		t.tb.Errorf("xrrtest: no cassette for %s %s: fingerprint %s, file %s; re-record with %s=record",
			rec.Method, rec.URL, fp, file, EnvMode)
		return nil, fmt.Errorf("xrrtest: %s %s: fingerprint %s, file %s: %w",
			rec.Method, rec.URL, fp, file, xrr.ErrCassetteMiss)
	}
	if err != nil {
		return nil, err
	}
	return t.response(req, resp)
}

func readBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	defer func() { _ = req.Body.Close() }()
	b, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("xrrtest: read request body: %w", err)
	}
	return b, nil
}

// recordedRequest builds the normalized, scrubbed request that is
// fingerprinted and saved.
func (t *Transport) recordedRequest(req *http.Request, body []byte) *request {
	ct := req.Header.Get("Content-Type")
	u := *req.URL
	u.Fragment, u.RawFragment = "", ""
	r := &Request{
		Method:      req.Method,
		URL:         &u,
		Header:      t.filterHeader(req.Header),
		Body:        append([]byte(nil), body...),
		ContentType: ct,
	}
	t.normalize(r)
	for _, fn := range t.cfg.normalize {
		fn(r)
	}

	rawURL := t.cfg.toPlaceholders(r.URL.String())
	b := []byte(t.cfg.toPlaceholders(string(r.Body)))
	b = t.scrubBody(r.ContentType, b)

	return &request{Request: xrrhttp.Request{
		Method:  r.Method,
		URL:     rawURL,
		Headers: t.flattenHeader(r.Header),
		Body:    string(b),
	}}
}

// normalize applies the built-in request normalization.
func (t *Transport) normalize(r *Request) {
	q := r.URL.Query()
	for k := range q {
		if t.cfg.dropQuery[k] {
			delete(q, k)
		}
	}
	redactValues(bodyRedactor(), q)
	r.URL.RawQuery = q.Encode() // sorted by key

	if mediaType(r.ContentType) == "application/x-www-form-urlencoded" && len(r.Body) > 0 {
		if vals, err := url.ParseQuery(string(r.Body)); err == nil {
			r.Body = []byte(vals.Encode())
		}
	}
}

func (t *Transport) scrubBody(contentType string, b []byte) []byte {
	b = ScrubSecrets(contentType, b)
	for _, fn := range t.cfg.scrub {
		b = fn(contentType, b)
	}
	return b
}

// fingerprint computes the cassette fingerprint for rec and stores it on
// rec. The host is folded into the path, which xrr's HTTP fingerprint
// omits, and the second and later identical requests on this transport get
// an occurrence number.
func (t *Transport) fingerprint(rec *request) (string, error) {
	u, err := url.Parse(rec.URL)
	if err != nil {
		return "", fmt.Errorf("xrrtest: parse recorded url: %w", err)
	}
	fpReq := &xrrhttp.Request{Method: rec.Method, Body: rec.Body}
	pathQuery := func(extra string) string {
		q := u.RawQuery
		if extra != "" {
			if q != "" {
				q += "&"
			}
			q += extra
		}
		s := "/" + u.Host + u.EscapedPath()
		if q != "" {
			s += "?" + q
		}
		return s
	}

	fpReq.URL = pathQuery("")
	base, err := xrrhttp.NewAdapter().Fingerprint(fpReq)
	if err != nil {
		return "", err
	}

	t.mu.Lock()
	t.seen[base]++
	n := t.seen[base]
	t.mu.Unlock()

	fp := base
	if n > 1 {
		fpReq.URL = pathQuery(occurrenceParam + "=" + strconv.Itoa(n))
		if fp, err = xrrhttp.NewAdapter().Fingerprint(fpReq); err != nil {
			return "", err
		}
	}
	rec.fingerprint = fp
	return fp, nil
}

// forward sends the original request to the provider and returns the
// recorded form of its response.
func (t *Transport) forward(req *http.Request, body []byte) (xrr.Response, error) {
	out := req.Clone(req.Context())
	if body != nil {
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		out.ContentLength = int64(len(body))
	}
	resp, err := t.cfg.base.RoundTrip(out)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("xrrtest: read response body: %w", err)
	}

	hdr := map[string]string{}
	for k, v := range t.flattenHeader(t.filterHeader(resp.Header)) {
		hdr[k] = t.cfg.toPlaceholders(v)
	}
	b = []byte(t.cfg.toPlaceholders(string(b)))
	b = t.scrubBody(resp.Header.Get("Content-Type"), b)
	if len(hdr) == 0 {
		hdr = nil
	}
	return &xrrhttp.Response{Status: resp.StatusCode, Headers: hdr, Body: string(b)}, nil
}

// response rebuilds an *http.Response from the typed response (record,
// passthrough) or the replayed payload (replay).
func (t *Transport) response(req *http.Request, resp xrr.Response) (*http.Response, error) {
	var (
		status int
		hdr    = http.Header{}
		body   string
	)
	switch v := resp.(type) {
	case *xrrhttp.Response:
		status, body = v.Status, v.Body
		for k, val := range v.Headers {
			hdr.Set(k, val)
		}
	case *xrr.RawResponse:
		var err error
		if status, err = payloadStatus(v.Payload["status"]); err != nil {
			return nil, err
		}
		body, _ = v.Payload["body"].(string)
		if hs, ok := v.Payload["headers"].(map[string]any); ok {
			for k, val := range hs {
				hdr.Set(k, fmt.Sprint(val))
			}
		}
	default:
		return nil, fmt.Errorf("xrrtest: unexpected response type %T", resp)
	}

	for k, vs := range hdr {
		for i, v := range vs {
			vs[i] = t.cfg.fromPlaceholders(v)
		}
		hdr[k] = vs
	}
	b := []byte(t.cfg.fromPlaceholders(body))
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        hdr,
		Body:          io.NopCloser(bytes.NewReader(b)),
		ContentLength: int64(len(b)),
		Request:       req,
	}, nil
}

func payloadStatus(v any) (int, error) {
	switch s := v.(type) {
	case int:
		return s, nil
	case int64:
		return int(s), nil
	case float64:
		return int(s), nil
	default:
		return 0, fmt.Errorf("xrrtest: replayed response has no valid status (%T %v)", v, v)
	}
}

func (t *Transport) filterHeader(h http.Header) http.Header {
	out := http.Header{}
	for k, vs := range h {
		if t.cfg.allowHeader(k) {
			out[http.CanonicalHeaderKey(k)] = append([]string(nil), vs...)
		}
	}
	return out
}

// flattenHeader joins repeated values with ", " (RFC 9110 5.3), since xrr
// stores one string per header.
func (t *Transport) flattenHeader(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, vs := range h {
		out[k] = strings.Join(vs, ", ")
	}
	return out
}

// request is xrr's HTTP request with the fingerprint precomputed, so the
// cassette payload keeps xrr's HTTP request shape.
type request struct {
	xrrhttp.Request `yaml:",inline"`
	fingerprint     string
}

// adapter is xrr's HTTP adapter with the fingerprint taken from request.
type adapter struct{ xrrhttp.Adapter }

func (a *adapter) Fingerprint(req xrr.Request) (string, error) {
	r, ok := req.(*request)
	if !ok || r.fingerprint == "" {
		return "", fmt.Errorf("xrrtest: unexpected request %T", req)
	}
	return r.fingerprint, nil
}
