package mime

import (
	"strings"

	"golang.org/x/net/html"
)

// htmlText renders an HTML body as plain text: text nodes in order, script
// and style dropped, block elements and <br> as line breaks, runs of blank
// lines collapsed.
func htmlText(src string) string {
	z := html.NewTokenizer(strings.NewReader(src))
	var b strings.Builder
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return collapse(b.String())
		case html.TextToken:
			if skip == 0 {
				b.WriteString(strings.Join(strings.Fields(string(z.Text())), " "))
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			switch tag := string(name); {
			case tag == "script" || tag == "style":
				skip++
			case breaks[tag]:
				b.WriteByte('\n')
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			switch tag := string(name); {
			case (tag == "script" || tag == "style") && skip > 0:
				skip--
			case breaks[tag]:
				b.WriteByte('\n')
			}
		}
	}
}

var breaks = map[string]bool{
	"br": true, "p": true, "div": true, "li": true, "tr": true, "table": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"blockquote": true, "pre": true, "hr": true,
}

func collapse(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
