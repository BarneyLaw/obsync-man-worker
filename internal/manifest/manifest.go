// Package manifest defines the only contract between the worker and any
// consumer. Everything a consumer needs in order to decide is in here.
//
// The central design choice: the manifest is a complete CATALOGUE of what
// Canvas has, not a list of what was fetched. Skipped files are still listed,
// with the rule that excluded them. That makes the plugin's "what will be
// pulled" preview a pure function over data the plugin already has, with no
// server round trip, and it means nothing Canvas offers is ever invisible.
//
// Pure: no I/O beyond encoding.
package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

const SchemaVersion = 1

type State string

const (
	// StateStored means the blob is in the store and SHA256 is valid.
	StateStored State = "stored"
	// StateSkipped means catalogued but deliberately not fetched. Reason and
	// RuleName say why.
	StateSkipped State = "skipped"
	// StateLocked means Canvas reports the file as not yet downloadable.
	// Distinct from Failed so it does not generate alerts every week before a
	// lecture drops.
	StateLocked State = "locked"
	// StateFailed means a fetch was attempted and did not succeed.
	StateFailed State = "failed"
	// StateDeleted is a tombstone. The path was live in a previous run and is
	// absent from Canvas now.
	StateDeleted State = "deleted"
)

type Entry struct {
	Path  string `json:"path"`
	State State  `json:"state"`

	Size       int64     `json:"size"`
	MIME       string    `json:"mime,omitempty"`
	CanvasID   int64     `json:"canvas_id"`
	CanvasUUID string    `json:"canvas_uuid,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
	ModifiedAt time.Time `json:"modified_at"`

	// SHA256 is set only when State is StateStored. BlobKey is derived from it
	// rather than stored, so the two can never disagree.
	SHA256 string `json:"sha256,omitempty"`

	// Reason is human-readable and surfaced directly in the plugin UI.
	Reason   string `json:"reason,omitempty"`
	RuleName string `json:"rule_name,omitempty"`

	UnlockAt  *time.Time `json:"unlock_at,omitempty"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

type Manifest struct {
	SchemaVersion int       `json:"schema_version"`
	CourseID      int64     `json:"course_id"`
	CourseName    string    `json:"course_name"`
	RunID         string    `json:"run_id"`
	PrevRunID     string    `json:"prev_run_id,omitempty"`
	GeneratedAt   time.Time `json:"generated_at"`

	// RulesHash identifies the worker rule set that produced this manifest.
	// When it changes, previously skipped entries must be re-evaluated.
	RulesHash string `json:"rules_hash"`

	// Entries is always sorted by Path. Not cosmetic: it makes two manifests
	// diffable with plain text tools, which you will want at 2am.
	Entries []Entry `json:"entries"`
}

// BlobKey is the content-addressed location of an entry's bytes.
// Two-level fanout is for humans listing the bucket, not for Garage.
func BlobKey(sha256hex string) string {
	if len(sha256hex) < 4 {
		return ""
	}
	return fmt.Sprintf("blobs/sha256/%s/%s/%s", sha256hex[0:2], sha256hex[2:4], sha256hex)
}

func ManifestKey(courseID int64, runID string) string {
	return fmt.Sprintf("manifests/%d/%s.json", courseID, runID)
}

// LatestKey is the ONLY mutable key in the entire store. Everything else is
// write-once. Publishing it is the commit point of a run.
func LatestKey(courseID int64) string {
	return fmt.Sprintf("manifests/%d/latest", courseID)
}

// Live returns entries that are not tombstones, which is what a consumer
// actually iterates.
func (m *Manifest) Live() []Entry {
	out := make([]Entry, 0, len(m.Entries))
	for _, e := range m.Entries {
		if e.State != StateDeleted {
			out = append(out, e)
		}
	}
	return out
}

func (m *Manifest) ByPath() map[string]Entry {
	out := make(map[string]Entry, len(m.Entries))
	for _, e := range m.Entries {
		out[e.Path] = e
	}
	return out
}

// Normalize enforces the sort invariant. Call before encoding, always.
func (m *Manifest) Normalize() {
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
}

func (m *Manifest) Validate() error {
	if m.SchemaVersion != SchemaVersion {
		return fmt.Errorf("manifest: unsupported schema version %d (this build understands %d)",
			m.SchemaVersion, SchemaVersion)
	}
	if m.RunID == "" {
		return fmt.Errorf("manifest: empty run id")
	}
	// An empty course is [], never absent: absent means a truncated or foreign
	// document, and treating it as "Canvas has nothing" would tombstone every
	// file in the consumer's vault.
	if m.Entries == nil {
		return fmt.Errorf("manifest: entries missing")
	}
	seen := map[string]bool{}
	for i, e := range m.Entries {
		if e.Path == "" {
			return fmt.Errorf("manifest: entry %d has empty path", i)
		}
		switch e.State {
		case StateStored, StateSkipped, StateLocked, StateFailed, StateDeleted:
		default:
			return fmt.Errorf("manifest: %q has unknown state %q", e.Path, e.State)
		}
		if seen[e.Path] {
			return fmt.Errorf("manifest: duplicate path %q", e.Path)
		}
		seen[e.Path] = true
		if e.State == StateStored && e.SHA256 == "" {
			return fmt.Errorf("manifest: %q is stored but has no hash", e.Path)
		}
		if e.State != StateStored && e.SHA256 != "" {
			return fmt.Errorf("manifest: %q is %s but carries a hash", e.Path, e.State)
		}
		if (e.State == StateSkipped || e.State == StateFailed) && e.Reason == "" {
			return fmt.Errorf("manifest: %q is %s with no reason, the UI has nothing to show", e.Path, e.State)
		}
	}
	return nil
}

func Encode(w io.Writer, m *Manifest) error {
	m.Normalize()
	if err := m.Validate(); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// Counts tallies entries by state, plus the bytes held in stored entries.
func (m *Manifest) Counts() (map[State]int, int64) {
	counts := map[State]int{}
	var stored int64
	for _, e := range m.Entries {
		counts[e.State]++
		if e.State == StateStored {
			stored += e.Size
		}
	}
	return counts, stored
}

type ChangeKind string

const (
	ChangeAdded   ChangeKind = "added"   // became live
	ChangeRemoved ChangeKind = "removed" // stopped being live
	ChangeContent ChangeKind = "content" // stored in both, different bytes
	ChangeState   ChangeKind = "state"   // live in both, different state
)

type Change struct {
	Path       string
	Kind       ChangeKind
	From, To   State
	FromSHA256 string
	ToSHA256   string
}

// Diff lists what changed between two manifests of the same course, sorted by
// path. a may be nil, meaning everything live in b was added.
func Diff(a, b *Manifest) []Change {
	var before map[string]Entry
	if a != nil {
		before = a.ByPath()
	}
	after := b.ByPath()
	live := func(e Entry, ok bool) bool { return ok && e.State != StateDeleted }

	var out []Change
	for p, eb := range after {
		ea, okA := before[p]
		switch {
		case !live(ea, okA) && live(eb, true):
			out = append(out, Change{Path: p, Kind: ChangeAdded, From: ea.State, To: eb.State, ToSHA256: eb.SHA256})
		case live(ea, okA) && !live(eb, true):
			out = append(out, Change{Path: p, Kind: ChangeRemoved, From: ea.State, To: eb.State, FromSHA256: ea.SHA256})
		case live(ea, okA) && ea.State != eb.State:
			out = append(out, Change{Path: p, Kind: ChangeState, From: ea.State, To: eb.State,
				FromSHA256: ea.SHA256, ToSHA256: eb.SHA256})
		case live(ea, okA) && eb.State == StateStored && ea.SHA256 != eb.SHA256:
			out = append(out, Change{Path: p, Kind: ChangeContent, From: ea.State, To: eb.State,
				FromSHA256: ea.SHA256, ToSHA256: eb.SHA256})
		}
	}
	for p, ea := range before {
		if _, ok := after[p]; !ok && ea.State != StateDeleted {
			out = append(out, Change{Path: p, Kind: ChangeRemoved, From: ea.State, FromSHA256: ea.SHA256})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Decode refuses unknown schema versions rather than ignoring fields it does
// not understand. A consumer that silently drops fields will corrupt a vault
// the first time the schema grows.
func Decode(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}
