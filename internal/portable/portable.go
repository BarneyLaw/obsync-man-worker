// Package portable turns arbitrary Canvas filenames into paths that are safe on
// Windows, macOS, Linux, Android and iOS.
//
// Pure: no I/O, no clock, no randomness. Everything here is table-testable, and
// every bug you will hit in production originates in this file.
package portable

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	MaxComponent = 120
	MaxTotal     = 400

	// maxExt bounds what counts as an extension worth preserving on
	// truncation. "lecture.pdf" has one; "notes.from the tutor about week 3"
	// does not.
	maxExt = 16
)

// Normalize is applied to every component before sanitisation.
//
// macOS hands you NFD, Windows and Linux hand you NFC, and iOS is inconsistent.
// Without normalisation the same file becomes two different paths depending on
// which device the worker happened to run on. Left as a variable so tests can
// swap it.
var Normalize = norm.NFC.String

// reserved holds Windows device names. Illegal as a whole component with or
// without an extension: "CON", "con.txt" and "CON.PDF" all collide.
var reserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// illegal on Windows, awkward on iOS. Slashes are handled separately because
// they are separator candidates rather than content.
const illegal = `<>:"|?*`

// Component sanitises a single path segment.
func Component(s string) (string, error) {
	s = Normalize(s)

	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '/' || r == '\\':
			b.WriteRune('-')
		case strings.ContainsRune(illegal, r):
			b.WriteRune('-')
		case unicode.IsControl(r):
			// dropped
		default:
			b.WriteRune(r)
		}
	}

	// Windows silently strips trailing dots and spaces, turning "lecture ."
	// into "lecture" behind your back. Do it ourselves so the manifest matches
	// what actually lands on disk.
	out := strings.TrimRight(b.String(), ". ")
	out = strings.TrimLeft(out, " ")

	if out == "" || out == "." || out == ".." {
		return "", fmt.Errorf("portable: component %q is empty after sanitisation", s)
	}
	if reserved[strings.ToUpper(stem(out))] {
		out = "_" + out
	}
	if len(out) > MaxComponent {
		out = truncateKeepExt(out, MaxComponent)
		if out == "" {
			return "", fmt.Errorf("portable: component %q empty after truncation", s)
		}
	}
	return out, nil
}

// Path sanitises each component of a slash-separated path and refuses anything
// that tries to escape the vault root.
func Path(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("portable: absolute path %q", p)
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return "", fmt.Errorf("portable: traversal in %q", p)
		}
		c, err := Component(part)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("portable: path %q is empty", p)
	}
	joined := strings.Join(out, "/")
	if len(joined) > MaxTotal {
		return "", fmt.Errorf("portable: path %q exceeds %d bytes", joined, MaxTotal)
	}
	return joined, nil
}

// Disambiguate appends a deterministic suffix derived from the Canvas file id.
//
// Two distinct Canvas files can sanitise to the same path. Resolving that by
// iteration order would make the manifest unstable across runs, so the suffix
// has to come from the file itself. The stem is shortened if the suffix would
// push the final component past MaxComponent.
func Disambiguate(p string, canvasID int64) string {
	dir, name := "", p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		dir, name = p[:i+1], p[i+1:]
	}
	suffix := fmt.Sprintf(" (%d)", canvasID)
	stemPart, ext := name, ""
	if i := strings.LastIndex(name, "."); i > 0 {
		stemPart, ext = name[:i], name[i:]
	}
	if over := len(stemPart) + len(suffix) + len(ext) - MaxComponent; over > 0 {
		keep := len(stemPart) - over
		if keep < 0 {
			keep = 0
		}
		stemPart = strings.TrimRight(truncateBytes(stemPart, keep), ". ")
	}
	return dir + stemPart + suffix + ext
}

// CollisionKey is the identity two paths share if they would land on the same
// file on a case-insensitive filesystem (Windows, default macOS). Vaults live
// on those, so "Slides.pdf" and "slides.pdf" are a collision even though the
// strings differ.
func CollisionKey(p string) string {
	return strings.ToLower(Normalize(p))
}

// Fallback is a portable stand-in name for a Canvas file whose real name
// cannot be made portable at all. The extension is kept when it survives
// sanitisation so the file still opens and extension rules still apply.
func Fallback(original string, canvasID int64) string {
	name := fmt.Sprintf("canvas-file-%d", canvasID)
	if i := strings.LastIndex(original, "."); i > 0 && len(original)-i-1 <= maxExt {
		if ext, err := Component(original[i+1:]); err == nil && !strings.ContainsAny(ext, " .") {
			return name + "." + ext
		}
	}
	return name
}

func stem(s string) string {
	if i := strings.Index(s, "."); i > 0 {
		return s[:i]
	}
	return s
}

// truncateKeepExt shortens s to at most n bytes. A short extension is
// preserved: cutting ".pdf" off would stop extension rules matching and stop
// the OS knowing how to open the file.
func truncateKeepExt(s string, n int) string {
	stemPart, ext := s, ""
	if i := strings.LastIndex(s, "."); i > 0 && len(s)-i-1 <= maxExt {
		stemPart, ext = s[:i], s[i:]
	}
	if len(ext) >= n {
		return strings.TrimRight(truncateBytes(s, n), ". ")
	}
	stemPart = strings.TrimRight(truncateBytes(stemPart, n-len(ext)), ". ")
	if stemPart == "" {
		return ""
	}
	return stemPart + ext
}

// truncateBytes cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
