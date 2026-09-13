package portable

import (
	"strings"
	"testing"
)

func TestComponent(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{"plain", "Lecture 01.pdf", "Lecture 01.pdf", false},
		{"windows illegal", `Q1: what?*.pdf`, "Q1- what--.pdf", false},
		{"backslash", `notes\draft.md`, "notes-draft.md", false},
		{"trailing dot", "lecture .", "lecture", false},
		{"leading space", "  slides.pdf", "slides.pdf", false},
		{"control chars", "bad\x00name\x07.pdf", "badname.pdf", false},
		{"reserved bare", "CON", "_CON", false},
		{"reserved with ext", "con.txt", "_con.txt", false},
		{"reserved uppercase ext", "COM1.PDF", "_COM1.PDF", false},
		{"not reserved", "CONTENTS.md", "CONTENTS.md", false},
		{"empty", "", "", true},
		{"dots only", "...", "", true},
		{"dotdot", "..", "", true},
		{"unicode kept", "复习资料.pdf", "复习资料.pdf", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Component(c.in)
			if c.err {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestComponentTruncates(t *testing.T) {
	in := strings.Repeat("a", 300) + ".pdf"
	got, err := Component(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > MaxComponent {
		t.Fatalf("len %d exceeds %d", len(got), MaxComponent)
	}
}

func TestPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		err  bool
	}{
		{"nested", "Week 1/Lecture 01.pdf", "Week 1/Lecture 01.pdf", false},
		{"backslash sep", `Week 1\Lecture.pdf`, "Week 1/Lecture.pdf", false},
		{"empty segments", "Week 1//Lecture.pdf", "Week 1/Lecture.pdf", false},
		{"dot segment", "./Week 1/Lecture.pdf", "Week 1/Lecture.pdf", false},
		{"traversal", "../../etc/passwd", "", true},
		{"traversal middle", "Week 1/../../../etc/passwd", "", true},
		{"absolute", "/etc/passwd", "", true},
		{"empty", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Path(c.in)
			if c.err {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestDisambiguate(t *testing.T) {
	cases := []struct {
		in   string
		id   int64
		want string
	}{
		{"Week 1/slides.pdf", 42, "Week 1/slides (42).pdf"},
		{"README", 7, "README (7)"},
		{"Week 1/.hidden", 9, "Week 1/.hidden (9)"},
		{"a.b/c.d", 1, "a.b/c (1).d"},
	}
	for _, c := range cases {
		if got := Disambiguate(c.in, c.id); got != c.want {
			t.Fatalf("Disambiguate(%q,%d) = %q want %q", c.in, c.id, got, c.want)
		}
	}
}

// Stability is the whole point of Disambiguate: same inputs, same output,
// regardless of the order files came back from Canvas.
func TestDisambiguateIsDeterministic(t *testing.T) {
	a := Disambiguate("x/y.pdf", 100)
	b := Disambiguate("x/y.pdf", 100)
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
}
