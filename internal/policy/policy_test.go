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

// Priority ties break by document order, so swapping two equal-priority rules
// can flip a decision. The hash must see that.
func TestHashIsRuleOrderSensitive(t *testing.T) {
	a := Policy{Version: 1, Default: ActionInclude, Rules: []Rule{
		{Name: "first", Priority: 5, Action: ActionSkip, Match: Match{Ext: []string{"pdf"}}},
		{Name: "second", Priority: 5, Action: ActionInclude, Match: Match{Ext: []string{"pdf"}}},
	}}
	b := a
	b.Rules = []Rule{a.Rules[1], a.Rules[0]}
	if a.Evaluate(Candidate{Path: "x.pdf"}).Action == b.Evaluate(Candidate{Path: "x.pdf"}).Action {
		t.Fatal("test premise broken: reorder should change the decision")
	}
	if a.Hash() == b.Hash() {
		t.Fatal("reorder changed behaviour but not the hash")
	}
}

func TestHashIgnoresSetOrderAndComments(t *testing.T) {
	a := testPolicy()
	b := testPolicy()
	b.Rules[0].Match.Ext = []string{"mov", "mp4", "mkv"}
	b.Comment = "edited the comment"
	b.Rules[1].Comment = "and this one"
	if a.Hash() != b.Hash() {
		t.Fatal("hash changed on ext order or comments alone")
	}
	b.Rules[0].Priority++
	if a.Hash() == b.Hash() {
		t.Fatal("hash did not change on a real edit")
	}
}

// Hash used to sort the caller's slices in place.
func TestHashDoesNotMutatePolicy(t *testing.T) {
	p := testPolicy()
	p.Rules[0].Match.Ext = []string{"mp4", "avi", "mkv"}
	p.Hash()
	if p.Rules[0].Match.Ext[0] != "mp4" {
		t.Fatalf("Hash reordered the caller's ext list: %v", p.Rules[0].Match.Ext)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	_, err := Parse([]byte(`{"version":1,"default":"include","rules":[
		{"name":"cap","priority":1,"action":"skip","match":{"ext":["mp4"],"max_szie":10}}]}`))
	if err == nil {
		t.Fatal("a typo in a match key silently broadens the rule and must be rejected")
	}
}

func TestParseAcceptsComments(t *testing.T) {
	p, err := Parse([]byte(`{"_comment":"hi","version":1,"default":"include","rules":[
		{"_comment":"why","name":"cap","priority":1,"action":"skip","match":{"ext":["mp4"]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rules) != 1 {
		t.Fatalf("rules = %+v", p.Rules)
	}
}

func TestDeployedWorkerRulesParse(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "apps", "obsync-worker", "rules.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(raw); err != nil {
		t.Fatalf("deploy/apps/obsync-worker/rules.json does not parse: %v", err)
	}
}

// The golden file is the contract with plugin/src/policy.ts. Both sides load
// the SAME file, schema/policy-golden.json, and must produce identical
// decisions and reject the same invalid policies. If you change the engine,
// change the golden file, and run both test suites. A missing fixture is a
// failure, not a skip.
func TestGoldenFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "schema", "policy-golden.json"))
	if err != nil {
		t.Fatalf("golden fixture missing: %v", err)
	}
	var g struct {
		Policy json.RawMessage `json:"policy"`
		Cases  []struct {
			Why       string    `json:"_why"`
			Candidate Candidate `json:"candidate"`
			Want      Action    `json:"want"`
			WantRule  string    `json:"want_rule"`
		} `json:"cases"`
		Invalid []struct {
			Why    string          `json:"_why"`
			Policy json.RawMessage `json:"policy"`
		} `json:"invalid"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	// Strict parse, the same path the deployed worker rules take.
	p, err := Parse(g.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Cases) == 0 || len(g.Invalid) == 0 {
		t.Fatal("golden fixture must have both cases and invalid policies")
	}
	for i, c := range g.Cases {
		d := p.Evaluate(c.Candidate)
		if d.Action != c.Want || d.Rule != c.WantRule {
			t.Errorf("case %d (%s, %s): got %s/%s want %s/%s",
				i, c.Candidate.Path, c.Why, d.Action, d.Rule, c.Want, c.WantRule)
		}
		if d.Reason == "" {
			t.Errorf("case %d (%s): empty reason", i, c.Candidate.Path)
		}
	}
	for i, c := range g.Invalid {
		if _, err := Parse(c.Policy); err == nil {
			t.Errorf("invalid policy %d accepted: %s", i, c.Why)
		}
	}
}
