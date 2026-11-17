package webhookauth

import (
	"strings"
)

// field names a part of the request a signed payload can contain.
type field int

const (
	fieldLiteral field = iota
	fieldBody
	fieldTimestamp
	fieldID
)

type segment struct {
	lit   string
	field field
}

// layout is a parsed Config.Payload: what the signature covers, in order.
type layout []segment

// parseLayout parses a payload template. Literal text is signed as is;
// {body} is the raw request body. "{{" and "}}" are literal braces. Every
// placeholder must be one the configuration enables, and {body} must appear
// exactly once: a payload that ignores the body would authenticate nothing.
func parseLayout(s string, enabled map[string]field) (layout, error) {
	var (
		out  layout
		lit  strings.Builder
		body int
	)
	flush := func() {
		if lit.Len() > 0 {
			out = append(out, segment{lit: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '{':
			if i+1 < len(s) && s[i+1] == '{' {
				lit.WriteByte('{')
				i++
				continue
			}
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, cfgErr("payload %q: unclosed placeholder", s)
			}
			name := s[i+1 : i+end]
			f, ok := enabled[name]
			if !ok {
				return nil, cfgErr("payload %q: placeholder {%s} is not available", s, name)
			}
			if f == fieldBody {
				body++
			}
			flush()
			out = append(out, segment{field: f})
			i += end
		case '}':
			if i+1 < len(s) && s[i+1] == '}' {
				lit.WriteByte('}')
				i++
				continue
			}
			return nil, cfgErr("payload %q: unmatched }", s)
		default:
			lit.WriteByte(c)
		}
	}
	flush()
	if body != 1 {
		return nil, cfgErr("payload %q: {body} must appear exactly once", s)
	}
	return out, nil
}

// uses reports whether the layout contains f.
func (l layout) uses(f field) bool {
	for _, s := range l {
		if s.field == f && f != fieldLiteral {
			return true
		}
	}
	return false
}

// fields are the request values a layout is filled from.
type fields struct {
	body      []byte
	timestamp string
	id        string
}

// bytes returns the signed payload.
func (l layout) bytes(f fields) []byte {
	var out []byte
	for _, s := range l {
		switch s.field {
		case fieldLiteral:
			out = append(out, s.lit...)
		case fieldBody:
			out = append(out, f.body...)
		case fieldTimestamp:
			out = append(out, f.timestamp...)
		case fieldID:
			out = append(out, f.id...)
		}
	}
	return out
}
