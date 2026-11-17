package mime

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// MaxFilenameBytes is the longest name SanitizeFilename returns: the name
// limit of common filesystems.
const MaxFilenameBytes = 255

// maxExtensionBytes is the longest extension kept when a long name is cut.
const maxExtensionBytes = 32

// SanitizeFilename reduces a sender-supplied attachment filename to a
// display name that is safe to show, log, put in a header or use as the
// final component of a path:
//
//   - invalid UTF-8 is replaced by U+FFFD and the text is put in Unicode
//     normalization form C;
//   - control characters (Cc), format characters (Cf) such as the bidi
//     overrides, embeddings, isolates and marks, the zero-width space and
//     the byte order mark, and U+2028 and U+2029 are dropped, except the
//     zero-width (non-)joiner, which spells some scripts;
//   - the base name is kept: everything up to the last "/" or "\" goes,
//     so neither "../../x" nor "C:\dir\x" survives, and a name ending in a
//     separator keeps its last component;
//   - leading and trailing whitespace and trailing dots go;
//   - a Windows device name (CON, PRN, AUX, NUL, COM1 to COM9, LPT1 to
//     LPT9, with or without an extension) is prefixed with "_";
//   - a name over MaxFilenameBytes is cut at a character boundary, its
//     short last extension kept.
//
// The result is empty when nothing usable is left, as for "", "..", "///"
// or only control characters; a caller then generates a name. It is never
// "." or "..". The function is idempotent, and ordinary names, whatever
// their script, pass through unchanged.
//
// It is a name for display and for naming a file under a directory the
// caller chose. It does not make the name unique, and it keeps characters
// some filesystems refuse (such as ":", "?", "*", "<", ">", "|", '"') or a
// leading dot: a host that writes the file to disk still applies the
// naming rules of its own filesystem. The bytes of an attachment are
// never stored under this name; the staging key does not derive from it.
func SanitizeFilename(name string) string {
	// Each pass can expose work for the next (dropping a character can
	// bring two others together); a few passes reach a fixed point.
	for range 8 {
		next := sanitizeFilenameOnce(name)
		if next == name {
			return name
		}
		name = next
	}
	return name
}

func sanitizeFilenameOnce(s string) string {
	s = norm.NFC.String(strings.ToValidUTF8(s, "\uFFFD"))
	s = strings.Map(func(r rune) rune {
		if unsafeFilenameRune(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimRight(s, `/\`)
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	s = strings.TrimRightFunc(s, func(r rune) bool { return r == '.' || unicode.IsSpace(r) })
	if s == "" {
		return ""
	}
	if reservedDeviceName(s) {
		s = "_" + s
	}
	return capFilename(s)
}

// unsafeFilenameRune reports a rune SanitizeFilename drops.
func unsafeFilenameRune(r rune) bool {
	switch r {
	case '\u200c', '\u200d':
		return false
	case '\u2028', '\u2029':
		return true
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// reservedDeviceName reports a name Windows resolves to a device, whatever
// follows the first dot.
func reservedDeviceName(s string) bool {
	stem, _, _ := strings.Cut(s, ".")
	switch stem = strings.ToUpper(strings.TrimRight(stem, " ")); stem {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(stem) == 4 && (stem[:3] == "COM" || stem[:3] == "LPT") {
		return stem[3] >= '1' && stem[3] <= '9'
	}
	return false
}

// capFilename cuts s to MaxFilenameBytes at a character boundary, keeping
// a short last extension.
func capFilename(s string) string {
	if len(s) <= MaxFilenameBytes {
		return s
	}
	ext := ""
	if i := strings.LastIndexByte(s, '.'); i > 0 && len(s)-i <= maxExtensionBytes {
		ext = s[i:]
	}
	stem := s[:len(s)-len(ext)]
	n := MaxFilenameBytes - len(ext)
	for n > 0 && !runeStart(stem[n]) {
		n--
	}
	return stem[:n] + ext
}

func runeStart(b byte) bool { return b&0xC0 != 0x80 }
