package scope

import (
	"reflect"
	"testing"
)

var paths = []string{
	"Week 1/Lecture 01.pdf",
	"Week 1/Tutorial/q1.pdf",
	"Week 2/Lecture 02.pptx",
	"Week 10/Lecture 10.pdf",
	"syllabus.pdf",
	"Readings/[draft] notes.md",
}

func selected(t *testing.T, patterns ...string) []string {
	t.Helper()
	s, err := Parse(patterns)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range paths {
		if s.Contains(p) {
			out = append(out, p)
		}
	}
	return out
}

func TestContains(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		want     []string
	}{
		{"empty is everything", nil, paths},
		{"directory", []string{"Week 1"}, []string{"Week 1/Lecture 01.pdf", "Week 1/Tutorial/q1.pdf"}},
		{"directory trailing slash", []string{"Week 1/"}, []string{"Week 1/Lecture 01.pdf", "Week 1/Tutorial/q1.pdf"}},
		{"directory is not a string prefix", []string{"Week 1"}, []string{"Week 1/Lecture 01.pdf", "Week 1/Tutorial/q1.pdf"}},
		{"single file", []string{"syllabus.pdf"}, []string{"syllabus.pdf"}},
		{"case insensitive", []string{"week 2/LECTURE 02.PPTX"}, []string{"Week 2/Lecture 02.pptx"}},
		{"backslashes and dot", []string{`.\Week 1\Tutorial`}, []string{"Week 1/Tutorial/q1.pdf"}},
		{"course files root stripped", []string{"course files/syllabus.pdf"}, []string{"syllabus.pdf"}},
		{"course files alone is everything", []string{"course files"}, paths},
		{"glob one segment", []string{"Week */*.pdf"}, []string{"Week 1/Lecture 01.pdf", "Week 10/Lecture 10.pdf"}},
		{"glob selects directory contents", []string{"Week ?"}, []string{"Week 1/Lecture 01.pdf", "Week 1/Tutorial/q1.pdf", "Week 2/Lecture 02.pptx"}},
		{"doublestar", []string{"**/*.pdf"}, []string{"Week 1/Lecture 01.pdf", "Week 1/Tutorial/q1.pdf", "Week 10/Lecture 10.pdf", "syllabus.pdf"}},
		{"literal bracket beats glob", []string{"Readings/[draft] notes.md"}, []string{"Readings/[draft] notes.md"}},
		{"union", []string{"syllabus.pdf", "Week 2"}, []string{"Week 2/Lecture 02.pptx", "syllabus.pdf"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := selected(t, c.patterns...); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	for _, bad := range []string{"", "  ", "/", "Week 1/../..", "[unclosed"} {
		if _, err := Parse([]string{bad}); err == nil {
			t.Fatalf("pattern %q should be rejected", bad)
		}
	}
}

func TestUnmatchedReportsTypos(t *testing.T) {
	s, err := Parse([]string{"Week 1", "Wek 2", "**/*.docx"})
	if err != nil {
		t.Fatal(err)
	}
	got := s.Unmatched(paths)
	want := []string{"Wek 2", "**/*.docx"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}
