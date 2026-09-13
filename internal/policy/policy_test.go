package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func testPolicy() Policy {
	return Policy{
		Version: 1,
		Default: ActionInclude,
		Rules: []Rule{
			{Name: "no-video", Priority: 10, Action: ActionSkip,
				Match: Match{Ext: []string{"mp4", "mkv", "mov"}}},
			{Name: "hard-size-cap", Priority: 10, Action: ActionSkip,
				Match: Match{MinSize: 2 << 30}},
			{Name: "keep-slides", Priority: 100, Action: ActionInclude,
				Match: Match{Ext: []string{"pdf", "pptx"}}},
		},
	}
}

func TestEvaluate(t *testing.T) {
	p := testPolicy()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		c    Candidate
		want Action
		rule string
	}{
		{"plain md included by default", Candidate{Path: "notes.md", Size: 100}, ActionInclude, ""},
		{"video skipped", Candidate{Path: "Week 1/lecture.mp4", Size: 1 << 20}, ActionSkip, "no-video"},
		{"video uppercase ext", Candidate{Path: "Week 1/lecture.MP4", Size: 1 << 20}, ActionSkip, "no-video"},
		{"huge file skipped", Candidate{Path: "data.zip", Size: 3 << 30}, ActionSkip, "hard-size-cap"},
		{"pdf wins over size cap", Candidate{Path: "textbook.pdf", Size: 3 << 30}, ActionInclude, "keep-slides"},
		{"no extension", Candidate{Path: "LICENSE", Size: 10}, ActionInclude, ""},
		{"dotfile has no ext", Candidate{Path: ".gitignore", Size: 10}, ActionInclude, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := p.Evaluate(c.c)
			if d.Action != c.want {
				t.Fatalf("action = %q want %q (reason %q)", d.Action, c.want, d.Reason)
			}
			if d.Rule != c.rule {
				t.Fatalf("rule = %q want %q", d.Rule, c.rule)
			}
			if d.Reason == "" {
				t.Fatal("every decision must carry a reason, the UI shows it to the user")
			}
		})
	}
}

// Ties break by document order so a rules file reads top to bottom.
func TestPriorityTieBreaksByOrder(t *testing.T) {
	p := Policy{Version: 1, Default: ActionInclude, Rules: []Rule{
		{Name: "first", Priority: 5, Action: ActionSkip, Match: Match{Ext: []string{"pdf"}}},
		{Name: "second", Priority: 5, Action: ActionInclude, Match: Match{Ext: []string{"pdf"}}},
	}}
	d := p.Evaluate(Candidate{Path: "a.pdf"})
	if d.Rule != "first" {
		t.Fatalf("tie should go to document order, got %q", d.Rule)
	}
}

func TestGlobAndCourseScope(t *testing.T) {
	p := Policy{Version: 1, Default: ActionInclude, Rules: []Rule{
		{Name: "drop-solutions", Priority: 10, Action: ActionSkip,
			Match: Match{Glob: []string{"*/solutions/*"}, CourseIDs: []int64{101}}},
	}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if d := p.Evaluate(Candidate{Path: "cs/solutions/a.pdf", CourseID: 101}); d.Action != ActionSkip {
		t.Fatal("expected skip in scoped course")
	}
	if d := p.Evaluate(Candidate{Path: "cs/solutions/a.pdf", CourseID: 202}); d.Action != ActionInclude {
		t.Fatal("rule should not apply outside its course scope")
	}
}

func TestValidateRejectsCatchAll(t *testing.T) {
	p := Policy{Version: 1, Default: ActionInclude, Rules: []Rule{
		{Name: "everything", Priority: 1, Action: ActionSkip},
	}}
	if err := p.Validate(); err == nil {
		t.Fatal("a rule with an empty match silently swallows everything, must be rejected")
	}
}

func TestValidateRejectsDuplicateNames(t *testing.T) {
	p := Policy{Version: 1, Default: ActionInclude, Rules: []Rule{
		{Name: "dup", Priority: 1, Action: ActionSkip, Match: Match{Ext: []string{"a"}}},
		{Name: "dup", Priority: 2, Action: ActionSkip, Match: Match{Ext: []string{"b"}}},
	}}
	if err := p.Validate(); err == nil {
		t.Fatal("duplicate rule names make manifest Reason fields ambiguous")
	}
}

// Hash must ignore cosmetic reordering, or every rules edit forces a pointless
// re-evaluation of every skipped entry in the store.
func TestHashIsOrderInsensitive(t *testing.T) {
	a := testPolicy()
	b := testPolicy()
	b.Rules[0], b.Rules[2] = b.Rules[2], b.Rules[0]
	if a.Hash() != b.Hash() {
		t.Fatal("hash changed on reorder alone")
	}
	b.Rules[0].Priority += 1
	if a.Hash() == b.Hash() {
		t.Fatal("hash did not change on a real edit")
	}
}

// The golden file is the contract with plugin/src/policy.ts. Both sides load
// it and must produce identical decisions. If you change the engine, change the
// golden file, and run both test suites.
func TestGoldenFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "golden.json"))
	if err != nil {
		t.Skip("no golden fixture yet")
	}
	var g struct {
		Policy Policy `json:"policy"`
		Cases  []struct {
			Candidate Candidate `json:"candidate"`
			Want      Action    `json:"want"`
			WantRule  string    `json:"want_rule"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	if err := g.Policy.Validate(); err != nil {
		t.Fatal(err)
	}
	for i, c := range g.Cases {
		d := g.Policy.Evaluate(c.Candidate)
		if d.Action != c.Want || d.Rule != c.WantRule {
			t.Errorf("case %d (%s): got %s/%s want %s/%s",
				i, c.Candidate.Path, d.Action, d.Rule, c.Want, c.WantRule)
		}
	}
}
