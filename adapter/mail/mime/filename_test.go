package mime_test

import (
	"math/rand"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"hop.top/uma/adapter/mail/mime"
)

// Ordinary names pass through unchanged.
func TestSanitizeFilenameKeepsOrdinaryNames(t *testing.T) {
	for _, name := range []string{
		"invoice.pdf",
		"Q3 report (final).xlsx",
		"archive.tar.gz",
		"a.b.c.d.txt",
		"no-extension",
		".bashrc",
		"..hidden",
		"note...txt",
		"résumé.pdf",
		"Café au lait – menu.docx",
		"請求書.pdf",
		"счёт №5.pdf",
		"مستند.pdf",
		"קובץ.txt",
		"emoji 📎.png",
		"a;b,c=d&e.txt",
		"it's.txt",
		"report: Q3?.pdf",
		"ज़िंदगी.txt",                     // composed already
		"\u0645\u200c\u0633.txt",          // ZWNJ is meaningful in Persian
		strings.Repeat("a", 251) + ".txt", // exactly 255 bytes
	} {
		if got := mime.SanitizeFilename(name); got != name {
			t.Errorf("SanitizeFilename(%q) = %q, want it unchanged", name, got)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	long := strings.Repeat("x", 400)
	for _, tc := range []struct{ name, in, want string }{
		// Path components: the base name stays, either separator.
		{"unix traversal", "../../etc/passwd", "passwd"},
		{"windows traversal", `..\..\x`, "x"},
		{"mixed separators", `..\../a/..\b.txt`, "b.txt"},
		{"absolute unix", "/etc/passwd", "passwd"},
		{"absolute windows", `C:\Users\jane\doc.pdf`, "doc.pdf"},
		{"unc", `\\server\share\doc.pdf`, "doc.pdf"},
		{"trailing separator", "dir/sub/", "sub"},
		{"only separators", "///", ""},
		{"dot dot", "..", ""},
		{"dot", ".", ""},
		{"dot dot slash", "../", ""},
		{"dots only", "....", ""},
		{"dot slash", "./a.txt", "a.txt"},
		{"traversal then dots", "a/..", ""},
		// Control characters are dropped, and so cannot hide a separator.
		{"nul", "a\x00b.txt", "ab.txt"},
		{"nul inside dots", ".\x00./x", "x"},
		{"nul before separator", "..\x00/..\x00/passwd", "passwd"},
		{"crlf", "a\r\nX-Evil: 1.txt", "aX-Evil: 1.txt"},
		{"tab and escape", "a\tb\x1b[31m.txt", "ab[31m.txt"},
		{"del and c1", "a\x7fb\u0085c.txt", "abc.txt"},
		{"line separator", "a\u2028b\u2029c.txt", "abc.txt"},
		{"only controls", "\x00\r\n\t", ""},
		// Bidi and invisible overrides.
		{"rlo spoof", "invoice\u202egpj.exe", "invoicegpj.exe"},
		{"embeddings and isolates", "a\u202a\u202b\u202c\u202d\u2066\u2067\u2068\u2069b.txt", "ab.txt"},
		{"marks", "a\u200e\u200f\u061cb.txt", "ab.txt"},
		{"bom", "\ufeffa.txt", "a.txt"},
		{"zero width space", "invoice\u200b.pdf", "invoice.pdf"},
		{"soft hyphen and word joiner", "a\u00ad\u2060b.txt", "ab.txt"},
		// Whitespace and dots at the ends.
		{"surrounding space", "  a.txt  ", "a.txt"},
		{"trailing dots and spaces", "a.txt. . ", "a.txt"},
		{"nbsp ends", "\u00a0a.txt\u00a0", "a.txt"},
		// Reserved Windows device names.
		{"con", "CON", "_CON"},
		{"con ext", "con.txt", "_con.txt"},
		{"aux ext", "Aux.tar.gz", "_Aux.tar.gz"},
		{"nul ext", "NUL.", "_NUL"},
		{"com1", "COM1.txt", "_COM1.txt"},
		{"lpt9", "lpt9", "_lpt9"},
		{"device in dir", "C:\\x\\PRN.txt", "_PRN.txt"},
		{"not a device", "CONSOLE.txt", "CONSOLE.txt"},
		{"com10 is not a device", "COM10.txt", "COM10.txt"},
		// Unicode form.
		{"nfd to nfc", "re\u0301sume\u0301.pdf", "résumé.pdf"},
		{"invalid utf-8", "a\xffb.txt", "a\ufffdb.txt"},
		// Length: 255 bytes, extension kept, no split rune.
		{"long ascii", long + ".pdf", strings.Repeat("x", 251) + ".pdf"},
		{"long no ext", long, strings.Repeat("x", 255)},
		{"long multibyte", strings.Repeat("é", 200) + ".txt", strings.Repeat("é", 125) + ".txt"},
		{"long extension is cut with the name", "a." + long, "a." + strings.Repeat("x", 253)},
		{"empty", "", ""},
		{"spaces only", "   ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mime.SanitizeFilename(tc.in); got != tc.want {
				t.Errorf("SanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Whatever the input, the result holds the guarantees of SanitizeFilename
// and sanitising it again changes nothing.
func TestSanitizeFilenameGuarantees(t *testing.T) {
	alphabet := []rune("ab.. /\\:\x00\r\n\t\u202e\u200e\u2066\ufeff\u00e9\u0301\u0301con COM1\u00a0\u2028\u200b\u200c\U0001F4CE\xff")
	rng := rand.New(rand.NewSource(1)) // #nosec G404 -- seeded for a reproducible property test
	for range 20000 {
		var b strings.Builder
		for range rng.Intn(24) {
			r := alphabet[rng.Intn(len(alphabet))]
			if r == utf8.RuneError {
				b.WriteByte(0xff)
				continue
			}
			b.WriteRune(r)
		}
		in := b.String()
		if rng.Intn(50) == 0 {
			in = strings.Repeat(in+"é", 120)
		}
		out := mime.SanitizeFilename(in)
		if again := mime.SanitizeFilename(out); again != out {
			t.Fatalf("not idempotent: %q -> %q -> %q", in, out, again)
		}
		if out == "" {
			continue
		}
		switch {
		case out == "." || out == "..":
			t.Fatalf("%q -> %q", in, out)
		case len(out) > 255:
			t.Fatalf("%q -> %d bytes", in, len(out))
		case !utf8.ValidString(out):
			t.Fatalf("%q -> invalid UTF-8 %q", in, out)
		case strings.ContainsAny(out, `/\`):
			t.Fatalf("%q -> %q has a separator", in, out)
		case out != strings.TrimSpace(out) || strings.HasSuffix(out, "."):
			t.Fatalf("%q -> %q has trailing space or dot", in, out)
		}
		for _, r := range out {
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) && r != '\u200c' && r != '\u200d' {
				t.Fatalf("%q -> %q holds %U", in, out, r)
			}
		}
	}
}

func filenames(t *testing.T, disposition string) []string {
	t.Helper()
	m := parse(t, "MIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: application/octet-stream\nContent-Disposition: "+disposition+"\n\nx\n--b--\n", mime.Limits{})
	var out []string
	for _, a := range m.Attachments {
		out = append(out, a.Filename)
	}
	return out
}

// Parse hands out sanitised names whichever way the sender wrote them: a
// quoted string, an RFC 2231 extended value or continuation, or an RFC 2047
// encoded word.
func TestParseSanitizesFilenames(t *testing.T) {
	for _, tc := range []struct{ name, disposition, want string }{
		{"plain", `attachment; filename=invoice.pdf`, "invoice.pdf"},
		{"quoted with spaces", `attachment; filename="Q3 report.pdf"`, "Q3 report.pdf"},
		{"2231 utf-8", `attachment; filename*=UTF-8''r%C3%A9sum%C3%A9.pdf`, "résumé.pdf"},
		{"2231 utf-8 with language", `attachment; filename*=UTF-8'fr'caf%C3%A9.txt`, "café.txt"},
		{"2231 continuation", `attachment; filename*0="annual "; filename*1="report.pdf"`, "annual report.pdf"},
		{"2231 extended continuation", `attachment; filename*0*=UTF-8''r%C3%A9; filename*1*=sum%C3%A9.pdf`, "résumé.pdf"},
		{"2047 word", `attachment; filename="=?utf-8?q?caf=C3=A9.txt?="`, "café.txt"},
		{"2047 base64", `attachment; filename="=?utf-8?b?5Y+W5byV44CC?=.pdf"`, "取引。.pdf"},
		{"multiple dots", `attachment; filename="a.b.c.tar.gz"`, "a.b.c.tar.gz"},
		{"traversal", `attachment; filename="../../etc/passwd"`, "passwd"},
		{"windows traversal", `attachment; filename="..\\..\\x"`, "x"},
		{"absolute", `attachment; filename="/etc/passwd"`, "passwd"},
		{"2231 traversal", `attachment; filename*=UTF-8''%2E%2E%2F%2E%2E%2Fetc%2Fpasswd`, "passwd"},
		{"2231 nul", `attachment; filename*=UTF-8''a%00b.txt`, "ab.txt"},
		{"2231 crlf", `attachment; filename*=UTF-8''a%0D%0AX-Evil%3A%201.txt`, "aX-Evil: 1.txt"},
		{"2231 rlo", `attachment; filename*=UTF-8''invoice%E2%80%AEgpj.exe`, "invoicegpj.exe"},
		{"2047 traversal", `attachment; filename="=?utf-8?q?..=2F..=2Fx?="`, "x"},
		{"2047 nul", `attachment; filename="=?utf-8?q?a=00b?="`, "ab"},
		{"2231 continuation traversal", `attachment; filename*0*=UTF-8''..%2F; filename*1*=..%2Fx.txt`, "x.txt"},
		{"reserved", `attachment; filename="CON.txt"`, "_CON.txt"},
		{"long", `attachment; filename="` + strings.Repeat("x", 1000) + `.pdf"`, strings.Repeat("x", 251) + ".pdf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := filenames(t, tc.disposition)
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("Filename = %q, want [%q]", got, tc.want)
			}
		})
	}
}

// The Content-Type name parameter is the fallback source and is sanitised
// alike.
func TestParseSanitizesContentTypeName(t *testing.T) {
	m := parse(t, "MIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: application/pdf; name=\"../../x.pdf\"\n\nx\n--b--\n", mime.Limits{})
	if len(m.Attachments) != 1 || m.Attachments[0].Filename != "x.pdf" {
		t.Errorf("attachments = %+v", m.Attachments)
	}
}

// A name with nothing usable left is reported as no name, so a consumer
// falls back to its generated one; the part is still an attachment.
func TestParseNameWithNothingLeft(t *testing.T) {
	for _, cd := range []string{`attachment; filename="../"`, `attachment; filename=".."`, `attachment; filename*=UTF-8''%00%0A`} {
		m := parse(t, "MIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\nContent-Disposition: "+cd+"\n\nx\n--b--\n", mime.Limits{})
		if len(m.Attachments) != 1 || m.Attachments[0].Filename != "" {
			t.Errorf("%s: attachments = %+v, want one unnamed attachment", cd, m.Attachments)
		}
	}
}
