package portable

import (
	"strings"
	"testing"
	"unicode/utf8"
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

func TestDisambiguateRespectsComponentCap(t *testing.T) {
	long := strings.Repeat("s", MaxComponent-4) + ".pdf"
	got := Disambiguate("dir/"+long, 123456789)
	last := got[strings.LastIndex(got, "/")+1:]
	if len(last) > MaxComponent {
		t.Fatalf("component %d bytes exceeds %d", len(last), MaxComponent)
	}
	if !strings.HasSuffix(got, " (123456789).pdf") {
		t.Fatalf("suffix or extension lost: %q", got)
	}
}

// macOS hands out NFD. Without NFC the same lecture is two paths.
func TestComponentNormalizesToNFC(t *testing.T) {
	nfd := "Résumé.pdf"
	nfc := "Résumé.pdf"
	got, err := Component(nfd)
	if err != nil {
		t.Fatal(err)
	}
	if got != nfc {
		t.Fatalf("got %q want NFC %q", got, nfc)
	}
}

// Cutting ".pdf" off stops extension rules matching and stops the OS opening it.
func TestTruncationKeepsExtension(t *testing.T) {
	got, err := Component(strings.Repeat("a", 300) + ".pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, ".pdf") || len(got) > MaxComponent {
		t.Fatalf("got %q (%d bytes)", got, len(got))
	}
}

func TestTruncationDoesNotSplitRunes(t *testing.T) {
	got, err := Component(strings.Repeat("复", 100) + ".pdf")
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(got) || len(got) > MaxComponent {
		t.Fatalf("invalid truncation %q (%d bytes)", got, len(got))
	}
}

func TestCollisionKeyFoldsCase(t *testing.T) {
	if CollisionKey("Week 1/Slides.pdf") != CollisionKey("week 1/slides.PDF") {
		t.Fatal("case-only differences must collide")
	}
}

func TestFallback(t *testing.T) {
	cases := []struct {
		orig string
		want string
	}{
		{"...", "canvas-file-7"},
		{"???.pdf", "canvas-file-7.pdf"},
		{"weird", "canvas-file-7"},
		{"x.a very long not-an-extension", "canvas-file-7"},
	}
	for _, c := range cases {
		got := Fallback(c.orig, 7)
		if got != c.want {
			t.Fatalf("Fallback(%q) = %q want %q", c.orig, got, c.want)
		}
		if _, err := Path(got); err != nil {
			t.Fatalf("fallback %q is not itself portable: %v", got, err)
		}
	}
}
