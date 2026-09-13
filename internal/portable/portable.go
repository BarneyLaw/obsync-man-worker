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
)

const (
	MaxComponent = 120
	MaxTotal     = 400
)

// Normalize is applied to every component before sanitisation.
//
// macOS hands you NFD, Windows and Linux hand you NFC, and iOS is inconsistent.
// Without normalisation the same file becomes two different paths depending on
// which device the worker happened to run on.
//
// TODO: wire this to golang.org/x/text/unicode/norm.NFC.String once you are
// happy taking the dependency. Left as a variable so tests can swap it and so
// the core package stays dependency-free.
var Normalize = func(s string) string { return s }

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
		out = strings.TrimRight(truncateBytes(out, MaxComponent), ". ")
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
// has to come from the file itself.
func Disambiguate(path string, canvasID int64) string {
	slash := strings.LastIndex(path, "/")
	dot := strings.LastIndex(path, ".")
	if dot > slash+1 {
		return fmt.Sprintf("%s (%d)%s", path[:dot], canvasID, path[dot:])
	}
	return fmt.Sprintf("%s (%d)", path, canvasID)
}

func stem(s string) string {
	if i := strings.Index(s, "."); i > 0 {
		return s[:i]
	}
	return s
}

func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && len(string(r)) > n {
		r = r[:len(r)-1]
	}
	return string(r)
}
