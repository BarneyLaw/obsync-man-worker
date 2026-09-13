// Package policy is the exclusion rule engine.
//
// The same rule schema is evaluated in two places with different defaults:
// the worker decides what enters the store (irreversible, so keep it
// permissive), the plugin decides what enters a given vault (reversible, so it
// can be as aggressive as the user likes).
//
// The TypeScript implementation in plugin/src/policy.ts must stay behaviourally
// identical. Both sides test against schema/policy-golden.json.
//
// Pure: no I/O, no clock.
package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

type Action string

const (
	ActionInclude Action = "include"
	ActionSkip    Action = "skip"
)

type Match struct {
	// Ext matches file extensions without the dot, case-insensitive.
	Ext []string `json:"ext,omitempty" yaml:"ext,omitempty"`
	// Glob matches the full vault-relative path.
	Glob []string `json:"glob,omitempty" yaml:"glob,omitempty"`
	// MinSize matches files at or above this many bytes.
	MinSize int64 `json:"min_size,omitempty" yaml:"min_size,omitempty"`
	// MaxSize matches files at or below this many bytes. Zero means unset.
	MaxSize int64 `json:"max_size,omitempty" yaml:"max_size,omitempty"`
	// CourseIDs restricts the rule to specific courses. Empty means all.
	CourseIDs []int64 `json:"course_ids,omitempty" yaml:"course_ids,omitempty"`
}

type Rule struct {
	Name     string `json:"name" yaml:"name"`
	Priority int    `json:"priority" yaml:"priority"`
	Match    Match  `json:"match" yaml:"match"`
	Action   Action `json:"action" yaml:"action"`
	// Comment is free text for humans. Excluded from Hash.
	Comment string `json:"_comment,omitempty" yaml:"_comment,omitempty"`
}

type Policy struct {
	Version int    `json:"version" yaml:"version"`
	Default Action `json:"default" yaml:"default"`
	Rules   []Rule `json:"rules" yaml:"rules"`
	// Comment is free text for humans. Excluded from Hash.
	Comment string `json:"_comment,omitempty" yaml:"_comment,omitempty"`
}

// Parse decodes and validates a rules file. Unknown fields are an error: a
// typo like "max_szie" would otherwise be silently dropped, leaving a rule far
// broader than the one you wrote.
func Parse(b []byte) (*Policy, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("policy: parse: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Candidate is the subset of a Canvas file a rule can see. Deliberately narrow:
// if a rule needs a field that is not here, that is a schema change with a
// version bump, not a quiet addition.
type Candidate struct {
	Path     string
	Size     int64
	MIME     string
	CourseID int64
}

type Decision struct {
	Action Action
	// Rule is the name of the rule that decided, or "" when Default applied.
	Rule string
	// Reason is human-readable and ends up in the manifest so the plugin can
	// tell the user why a file they can see in Canvas is not in their vault.
	Reason string
}

func (d Decision) Included() bool { return d.Action == ActionInclude }

// Validate catches the mistakes that would otherwise silently include or
// exclude everything.
func (p *Policy) Validate() error {
	if p.Version != 1 {
		return fmt.Errorf("policy: unsupported version %d", p.Version)
	}
	if p.Default != ActionInclude && p.Default != ActionSkip {
		return fmt.Errorf("policy: default must be include or skip, got %q", p.Default)
	}
	seen := map[string]bool{}
	for i, r := range p.Rules {
		if r.Name == "" {
			return fmt.Errorf("policy: rule %d has no name", i)
		}
		if seen[r.Name] {
			return fmt.Errorf("policy: duplicate rule name %q", r.Name)
		}
		seen[r.Name] = true
		if r.Action != ActionInclude && r.Action != ActionSkip {
			return fmt.Errorf("policy: rule %q has invalid action %q", r.Name, r.Action)
		}
		if isEmptyMatch(r.Match) {
			return fmt.Errorf("policy: rule %q matches everything, which is what Default is for", r.Name)
		}
		for _, g := range r.Match.Glob {
			if _, err := path.Match(g, "probe"); err != nil {
				return fmt.Errorf("policy: rule %q has bad glob %q: %w", r.Name, g, err)
			}
		}
	}
	return nil
}

// Evaluate returns the decision for one candidate.
//
// Highest priority wins. Ties break by document order, so a rules file is read
// top to bottom the way a human expects. Nothing matching falls through to
// Default.
func (p *Policy) Evaluate(c Candidate) Decision {
	best := -1
	bestPri := 0
	for i, r := range p.Rules {
		if !matches(r.Match, c) {
			continue
		}
		if best == -1 || r.Priority > bestPri {
			best, bestPri = i, r.Priority
		}
	}
	if best == -1 {
		return Decision{Action: p.Default, Reason: "default policy"}
	}
	r := p.Rules[best]
	return Decision{
		Action: r.Action,
		Rule:   r.Name,
		Reason: describe(r, c),
	}
}

// Hash identifies a rule set. It is recorded on every manifest so a run can be
// traced back to the rules that produced it.
//
// Rules stay in document order: priority ties break by document order, so
// reordering rules can change decisions and must change the hash. The lists
// inside a match are sets, so those are sorted, on copies, never in place:
// sorting the caller's slices would be a data race once courses run
// concurrently. Comments are excluded.
func (p *Policy) Hash() string {
	c := Policy{Version: p.Version, Default: p.Default, Rules: make([]Rule, len(p.Rules))}
	for i, r := range p.Rules {
		r.Comment = ""
		r.Match.Ext = sortedStrings(r.Match.Ext)
		r.Match.Glob = sortedStrings(r.Match.Glob)
		if len(r.Match.CourseIDs) > 0 {
			ids := append([]int64(nil), r.Match.CourseIDs...)
			sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
			r.Match.CourseIDs = ids
		}
		c.Rules[i] = r
	}
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func matches(m Match, c Candidate) bool {
	if len(m.CourseIDs) > 0 && !containsInt64(m.CourseIDs, c.CourseID) {
		return false
	}
	if len(m.Ext) > 0 && !containsFold(m.Ext, ext(c.Path)) {
		return false
	}
	if len(m.Glob) > 0 && !anyGlob(m.Glob, c.Path) {
		return false
	}
	if m.MinSize > 0 && c.Size < m.MinSize {
		return false
	}
	if m.MaxSize > 0 && c.Size > m.MaxSize {
		return false
	}
	return true
}

func isEmptyMatch(m Match) bool {
	return len(m.Ext) == 0 && len(m.Glob) == 0 &&
		m.MinSize == 0 && m.MaxSize == 0 && len(m.CourseIDs) == 0
}

func describe(r Rule, c Candidate) string {
	switch {
	case r.Match.MinSize > 0 && len(r.Match.Ext) == 0:
		return fmt.Sprintf("rule %q: %s is at or above %s", r.Name, humanBytes(c.Size), humanBytes(r.Match.MinSize))
	case len(r.Match.Ext) > 0:
		return fmt.Sprintf("rule %q: extension .%s", r.Name, ext(c.Path))
	default:
		return fmt.Sprintf("rule %q", r.Name)
	}
}

// ext returns the lowercase extension without the dot. A dotfile with no other
// dot has no extension.
func ext(p string) string {
	base := p
	if i := strings.LastIndex(p, "/"); i >= 0 {
		base = p[i+1:]
	}
	i := strings.LastIndex(base, ".")
	if i <= 0 {
		return ""
	}
	return strings.ToLower(base[i+1:])
}

func anyGlob(globs []string, p string) bool {
	for _, g := range globs {
		if ok, err := path.Match(g, p); err == nil && ok {
			return true
		}
	}
	return false
}

func containsFold(hay []string, needle string) bool {
	for _, h := range hay {
		if strings.EqualFold(strings.TrimPrefix(h, "."), needle) {
			return true
		}
	}
	return false
}

func sortedStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := append([]string(nil), s...)
	sort.Strings(out)
	return out
}

func containsInt64(hay []int64, n int64) bool {
	for _, h := range hay {
		if h == n {
			return true
		}
	}
	return false
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
