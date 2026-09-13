// Package scope restricts a manual pull to specific directories and files.
//
// A pattern is matched against the vault path, the path `obsync ls` prints,
// case-insensitively. Vaults commonly live on case-insensitive filesystems,
// and the planner already guarantees paths are unique under case folding, so
// folding here cannot make one pattern select two different files by accident.
//
// Pattern forms:
//
//	Week 1                 a directory: everything beneath it
//	Week 1/slides.pdf      a single file
//	Week */*.pdf           glob, one path segment per *
//	**/*.pdf               ** spans any number of segments
//
// A glob that matches a directory selects everything beneath it, the same as a
// literal directory does.
//
// Pure: no I/O.
package scope

import (
	"fmt"
	"path"
	"strings"

	"github.com/leifsen/obsync/internal/portable"
)

// Scope is a set of patterns. The zero value selects everything.
type Scope struct {
	patterns []pattern
}

type pattern struct {
	raw  string
	norm string
	segs []string
	glob bool
}

// Parse validates patterns. An empty list is the everything-scope.
func Parse(raw []string) (Scope, error) {
	var s Scope
	for _, r := range raw {
		n := normalize(r)
		if n == "" {
			return Scope{}, fmt.Errorf("scope: pattern %q is empty (to pull everything, omit -path)", r)
		}
		segs := strings.Split(n, "/")
		for _, seg := range segs {
			if seg == ".." {
				return Scope{}, fmt.Errorf("scope: pattern %q escapes the course root", r)
			}
			if _, err := path.Match(seg, ""); err != nil {
				return Scope{}, fmt.Errorf("scope: pattern %q: %w", r, err)
			}
		}
		s.patterns = append(s.patterns, pattern{
			raw:  r,
			norm: n,
			segs: segs,
			glob: strings.ContainsAny(n, "*?["),
		})
	}
	return s, nil
}

// All reports whether this scope selects everything.
func (s Scope) All() bool { return len(s.patterns) == 0 }

// Patterns returns the patterns as the user typed them, for logs and summaries.
func (s Scope) Patterns() []string {
	out := make([]string, len(s.patterns))
	for i, p := range s.patterns {
		out[i] = p.raw
	}
	return out
}

// Contains reports whether a vault path is selected.
func (s Scope) Contains(p string) bool {
	if s.All() {
		return true
	}
	lp := portable.CollisionKey(p)
	for _, pat := range s.patterns {
		if pat.matches(lp) {
			return true
		}
	}
	return false
}

// Unmatched returns the patterns, as typed, that select none of paths. A
// pattern that matches nothing is almost always a typo, and a pull that
// silently does nothing is the failure mode worth shouting about.
func (s Scope) Unmatched(paths []string) []string {
	var out []string
	for _, pat := range s.patterns {
		hit := false
		for _, p := range paths {
			if pat.matches(portable.CollisionKey(p)) {
				hit = true
				break
			}
		}
		if !hit {
			out = append(out, pat.raw)
		}
	}
	return out
}

// matches takes an already case-folded path. Literal comparison runs first so
// that a real filename containing '[' is not misread as a glob.
func (p pattern) matches(lp string) bool {
	if lp == p.norm || strings.HasPrefix(lp, p.norm+"/") {
		return true
	}
	return p.glob && matchSegs(p.segs, strings.Split(lp, "/"))
}

// matchSegs has prefix semantics: once the pattern is exhausted, anything
// beneath the matched directory is selected.
func matchSegs(pat, segs []string) bool {
	if len(pat) == 0 {
		return true
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegs(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	if ok, err := path.Match(pat[0], segs[0]); err != nil || !ok {
		return false
	}
	return matchSegs(pat[1:], segs[1:])
}

// normalize puts a user-typed pattern into vault-path form: NFC, forward
// slashes, no leading "./" or "/", no trailing "/", no synthetic "course
// files" root (users copy that from the Canvas UI), then case-folded.
func normalize(r string) string {
	n := portable.Normalize(strings.TrimSpace(r))
	n = strings.ReplaceAll(n, "\\", "/")
	parts := strings.Split(n, "/")
	kept := parts[:0]
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		kept = append(kept, part)
	}
	if len(kept) > 0 && strings.EqualFold(kept[0], "course files") {
		kept = kept[1:]
		if len(kept) == 0 {
			return "**"
		}
	}
	return strings.ToLower(strings.Join(kept, "/"))
}
