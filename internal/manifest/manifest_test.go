package manifest

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func valid() *Manifest {
	return &Manifest{
		SchemaVersion: SchemaVersion, CourseID: 1, RunID: "r1",
		Entries: []Entry{
			{Path: "b.pdf", State: StateStored, SHA256: "aa"},
			{Path: "a.mp4", State: StateSkipped, Reason: "no video"},
		},
	}
}

func TestEncodeSortsAndRoundTrips(t *testing.T) {
	var buf bytes.Buffer
	if err := Encode(&buf, valid()); err != nil {
		t.Fatal(err)
	}
	m, err := Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if m.Entries[0].Path != "a.mp4" {
		t.Fatalf("entries not sorted: %+v", m.Entries)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]func(m *Manifest){
		"schema":         func(m *Manifest) { m.SchemaVersion = 99 },
		"run id":         func(m *Manifest) { m.RunID = "" },
		"empty path":     func(m *Manifest) { m.Entries[0].Path = "" },
		"duplicate":      func(m *Manifest) { m.Entries[1].Path = "b.pdf" },
		"stored no hash": func(m *Manifest) { m.Entries[0].SHA256 = "" },
		"hash on skip":   func(m *Manifest) { m.Entries[1].SHA256 = "aa" },
		"skip no reason": func(m *Manifest) { m.Entries[1].Reason = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := valid()
			mutate(m)
			if err := m.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestDecodeRefusesUnknownSchema(t *testing.T) {
	_, err := Decode(strings.NewReader(`{"schema_version": 2, "run_id": "x"}`))
	if err == nil {
		t.Fatal("unknown schema version must be refused")
	}
}

func TestDiff(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a := &Manifest{Entries: []Entry{
		{Path: "same.pdf", State: StateStored, SHA256: "11"},
		{Path: "edited.pdf", State: StateStored, SHA256: "22"},
		{Path: "gone.pdf", State: StateStored, SHA256: "33"},
		{Path: "unlocked.pdf", State: StateLocked},
		{Path: "old-tomb.pdf", State: StateDeleted, DeletedAt: &at},
	}}
	b := &Manifest{Entries: []Entry{
		{Path: "same.pdf", State: StateStored, SHA256: "11"},
		{Path: "edited.pdf", State: StateStored, SHA256: "99"},
		{Path: "gone.pdf", State: StateDeleted, DeletedAt: &at},
		{Path: "unlocked.pdf", State: StateStored, SHA256: "44"},
		{Path: "old-tomb.pdf", State: StateDeleted, DeletedAt: &at},
		{Path: "new.pdf", State: StateStored, SHA256: "55"},
	}}
	got := map[string]ChangeKind{}
	for _, c := range Diff(a, b) {
		got[c.Path] = c.Kind
	}
	want := map[string]ChangeKind{
		"edited.pdf":   ChangeContent,
		"gone.pdf":     ChangeRemoved,
		"unlocked.pdf": ChangeState,
		"new.pdf":      ChangeAdded,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for p, k := range want {
		if got[p] != k {
			t.Fatalf("%s: got %q want %q (all: %v)", p, got[p], k, got)
		}
	}
}

func TestDiffFromNothing(t *testing.T) {
	b := valid()
	if n := len(Diff(nil, b)); n != 2 {
		t.Fatalf("first manifest should report every live entry as added, got %d", n)
	}
}
