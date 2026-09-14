package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/leifsen/obsync/internal/canvas"
	"github.com/leifsen/obsync/internal/manifest"
	"github.com/leifsen/obsync/internal/plan"
	"github.com/leifsen/obsync/internal/policy"
	"github.com/leifsen/obsync/internal/scope"
	"github.com/leifsen/obsync/internal/store"
)

var (
	t0     = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	now    = time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	course = canvas.Course{ID: 93794, Code: "CS3103", Name: "Computer Networks"}
)

// fakeSource serves a fixed listing. Content is keyed by URL; a URL mapped to
// an error fails the download.
type fakeSource struct {
	files   []canvas.File
	folders map[int64]canvas.Folder
	content map[string]string
	failing map[string]error
	onOpen  func(canvas.File)
}

func (s *fakeSource) Courses(context.Context) ([]canvas.Course, error) {
	return []canvas.Course{course}, nil
}
func (s *fakeSource) Files(context.Context, int64) ([]canvas.File, error) {
	return s.files, nil
}
func (s *fakeSource) Folders(context.Context, int64) (map[int64]canvas.Folder, error) {
	return s.folders, nil
}
func (s *fakeSource) Open(_ context.Context, f canvas.File) (io.ReadCloser, error) {
	if s.onOpen != nil {
		s.onOpen(f)
	}
	if err := s.failing[f.URL]; err != nil {
		return nil, err
	}
	body, ok := s.content[f.URL]
	if !ok {
		return nil, fmt.Errorf("no content for %q", f.URL)
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func newSource() *fakeSource {
	return &fakeSource{
		folders: map[int64]canvas.Folder{
			1: {ID: 1, FullName: "course files"},
			2: {ID: 2, FullName: "course files/Week 1"},
			3: {ID: 3, FullName: "course files/Week 2"},
		},
		content: map[string]string{},
		failing: map[string]error{},
	}
}

func (s *fakeSource) add(id, folder int64, name, body string) {
	url := fmt.Sprintf("u%d", id)
	s.files = append(s.files, canvas.File{
		ID: id, FolderID: folder, DisplayName: name, URL: url,
		Size: int64(len(body)), UpdatedAt: t0, ModifiedAt: t0,
	})
	s.content[url] = body
}

func permissive() *policy.Policy {
	return &policy.Policy{Version: 1, Default: policy.ActionInclude, Rules: []policy.Rule{
		{Name: "no-video", Priority: 10, Action: policy.ActionSkip, Match: policy.Match{Ext: []string{"mp4"}}},
	}}
}

func runner(src Source, st store.Store, logs io.Writer) *Runner {
	if logs == nil {
		logs = io.Discard
	}
	return &Runner{
		Source: src, Store: st, Policy: permissive(),
		Log: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now: func() time.Time { return now }, TempDir: "",
	}
}

func latest(t *testing.T, st store.Store) *manifest.Manifest {
	t.Helper()
	rc, err := st.Get(context.Background(), manifest.LatestKey(course.ID))
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	id, _ := io.ReadAll(rc)
	rc.Close()
	mrc, err := st.Get(context.Background(), manifest.ManifestKey(course.ID, string(id)))
	if err != nil {
		t.Fatal(err)
	}
	defer mrc.Close()
	m, err := manifest.Decode(mrc)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func blob(t *testing.T, st store.Store, sha string) string {
	t.Helper()
	rc, err := st.Get(context.Background(), manifest.BlobKey(sha))
	if err != nil {
		t.Fatalf("blob %s: %v", sha, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

// Bug 1: collisions were looked up by the renamed path, found nothing, and
// both files failed to download.
func TestCollidingNamesBothDownload(t *testing.T) {
	src := newSource()
	src.add(100, 1, "Q1: what.pdf", "first")
	src.add(200, 1, "Q1? what.pdf", "second")
	st := store.NewMemory()

	s, err := runner(src, st, nil).Course(context.Background(), course, Options{RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Fetched != 2 || s.Failed != 0 {
		t.Fatalf("stats = %+v", s)
	}
	got := map[string]string{}
	for _, e := range latest(t, st).Entries {
		got[e.Path] = blob(t, st, e.SHA256)
	}
	if got["Q1- what (100).pdf"] != "first" || got["Q1- what (200).pdf"] != "second" {
		t.Fatalf("entries = %v", got)
	}
}

// The plugin names each course's folder from the code, so it must reach the
// manifest.
func TestManifestCarriesCourseCode(t *testing.T) {
	src := newSource()
	src.add(1, 2, "a.pdf", "x")
	st := store.NewMemory()
	if _, err := runner(src, st, nil).Course(context.Background(), course, Options{RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if m := latest(t, st); m.CourseCode != "CS3103" || m.CourseName != course.Name {
		t.Fatalf("course code %q name %q", m.CourseCode, m.CourseName)
	}
}

// Bug 2: unportable names were dropped with only a log line.
func TestUnportableNameIsStoredUnderStandIn(t *testing.T) {
	src := newSource()
	src.add(300, 2, "...", "mystery")
	st := store.NewMemory()

	s, err := runner(src, st, nil).Course(context.Background(), course, Options{RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Renamed != 1 || s.Fetched != 1 {
		t.Fatalf("stats = %+v", s)
	}
	e, ok := latest(t, st).ByPath()["Week 1/canvas-file-300"]
	if !ok || e.State != manifest.StateStored || !strings.Contains(e.Reason, "could not be made portable") {
		t.Fatalf("entry = %+v (ok=%v)", e, ok)
	}
}

func TestUnlistedFolderIsNotDropped(t *testing.T) {
	src := newSource()
	src.add(1, 999, "hidden.pdf", "x")
	st := store.NewMemory()
	if _, err := runner(src, st, nil).Course(context.Background(), course, Options{RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := latest(t, st).ByPath()["_unlisted-folder-999/hidden.pdf"]; !ok {
		t.Fatalf("entries = %+v", latest(t, st).Entries)
	}
}

// Bug 6: a failed refresh replaced a good stored entry with "failed".
func TestFailedRefreshKeepsPreviousVersion(t *testing.T) {
	src := newSource()
	src.add(1, 2, "slides.pdf", "v1")
	st := store.NewMemory()
	r := runner(src, st, nil)
	if _, err := r.Course(context.Background(), course, Options{RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	v1 := latest(t, st).ByPath()["Week 1/slides.pdf"]

	src.files[0].UpdatedAt = now
	src.files[0].Size = 2
	src.failing["u1"] = errors.New("connection reset")
	s, err := r.Course(context.Background(), course, Options{RunID: "r2"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Failed != 1 || s.KeptPrevious != 1 {
		t.Fatalf("stats = %+v", s)
	}
	e := latest(t, st).ByPath()["Week 1/slides.pdf"]
	if e.State != manifest.StateStored || e.SHA256 != v1.SHA256 || !e.UpdatedAt.Equal(t0) {
		t.Fatalf("previous version not kept verbatim: %+v", e)
	}

	// Next run succeeds and replaces it: the stale metadata guaranteed a retry.
	delete(src.failing, "u1")
	src.content["u1"] = "v2"
	if _, err := r.Course(context.Background(), course, Options{RunID: "r3"}); err != nil {
		t.Fatal(err)
	}
	if e := latest(t, st).ByPath()["Week 1/slides.pdf"]; blob(t, st, e.SHA256) != "v2" || e.Reason != "" {
		t.Fatalf("retry did not refresh: %+v", e)
	}
}

func TestNewFileFailureIsCatalogued(t *testing.T) {
	src := newSource()
	src.add(1, 2, "a.pdf", "x")
	src.failing["u1"] = errors.New("boom")
	st := store.NewMemory()
	if _, err := runner(src, st, nil).Course(context.Background(), course, Options{RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if e := latest(t, st).ByPath()["Week 1/a.pdf"]; e.State != manifest.StateFailed || e.Reason == "" {
		t.Fatalf("entry = %+v", e)
	}
}

// A clean but short read must never become the authoritative content.
func TestSizeMismatchIsAFailure(t *testing.T) {
	src := newSource()
	src.add(1, 2, "a.pdf", "full body")
	src.content["u1"] = "full"
	st := store.NewMemory()
	s, err := runner(src, st, nil).Course(context.Background(), course, Options{RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	e := latest(t, st).ByPath()["Week 1/a.pdf"]
	if s.Failed != 1 || e.State != manifest.StateFailed || !strings.Contains(e.Reason, "size mismatch") {
		t.Fatalf("stats=%+v entry=%+v", s, e)
	}
}

func TestPublishFailureLeavesPreviousLive(t *testing.T) {
	src := newSource()
	src.add(1, 2, "a.pdf", "x")
	st := store.NewMemory()
	r := runner(src, st, nil)
	if _, err := r.Course(context.Background(), course, Options{RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	st.PutErr = func(key string) error {
		if key == manifest.LatestKey(course.ID) {
			return errors.New("disk full")
		}
		return nil
	}
	src.add(2, 2, "b.pdf", "y")
	if _, err := r.Course(context.Background(), course, Options{RunID: "r2"}); err == nil {
		t.Fatal("expected publish failure")
	}
	st.PutErr = nil
	if m := latest(t, st); m.RunID != "r1" {
		t.Fatalf("latest moved to %s despite failed publish", m.RunID)
	}
}

func TestInterruptedRunDoesNotCommit(t *testing.T) {
	src := newSource()
	src.add(1, 2, "a.pdf", "x")
	src.add(2, 2, "b.pdf", "y")
	st := store.NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	src.onOpen = func(canvas.File) { cancel() }

	_, err := runner(src, st, nil).Course(ctx, course, Options{RunID: "r1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
	if ok, _ := st.Exists(context.Background(), manifest.LatestKey(course.ID)); ok {
		t.Fatal("interrupted run published latest")
	}
}

type failingGuard struct{}

func (failingGuard) Verify(context.Context) error { return errors.New("stolen") }

func TestLostLeaseBlocksCommit(t *testing.T) {
	src := newSource()
	src.add(1, 2, "a.pdf", "x")
	st := store.NewMemory()
	r := runner(src, st, nil)
	r.Guard = failingGuard{}
	if _, err := r.Course(context.Background(), course, Options{RunID: "r1"}); err == nil {
		t.Fatal("expected lease failure")
	}
	if ok, _ := st.Exists(context.Background(), manifest.LatestKey(course.ID)); ok {
		t.Fatal("published latest without the lease")
	}
}

func TestManifestsAreWriteOnce(t *testing.T) {
	src := newSource()
	src.add(1, 2, "a.pdf", "x")
	st := store.NewMemory()
	r := runner(src, st, nil)
	if _, err := r.Course(context.Background(), course, Options{RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Course(context.Background(), course, Options{RunID: "r1"}); !errors.Is(err, store.ErrExists) {
		t.Fatalf("reusing a run id must not overwrite its manifest: %v", err)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	src := newSource()
	src.add(1, 2, "a.pdf", "x")
	st := store.NewMemory()
	r := runner(src, st, nil)
	r.DryRun = true
	opened := false
	src.onOpen = func(canvas.File) { opened = true }
	if _, err := r.Course(context.Background(), course, Options{RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	n := 0
	_ = st.List(context.Background(), "", func(store.ObjectInfo) error { n++; return nil })
	if n != 0 || opened {
		t.Fatalf("dry run wrote %d objects, opened=%v", n, opened)
	}
}

func TestScopedPull(t *testing.T) {
	src := newSource()
	src.add(1, 2, "w1.pdf", "one")
	src.add(2, 3, "w2.pdf", "two")
	st := store.NewMemory()
	r := runner(src, st, nil)
	sc, err := scope.Parse([]string{"Week 1", "Week 9"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Course(context.Background(), course, Options{RunID: "r1", Scope: sc})
	if err != nil {
		t.Fatal(err)
	}
	if s.Fetched != 1 || s.OutOfScope != 1 {
		t.Fatalf("stats = %+v", s)
	}
	if len(s.UnmatchedScope) != 1 || s.UnmatchedScope[0] != "Week 9" {
		t.Fatalf("unmatched = %v", s.UnmatchedScope)
	}
	e := latest(t, st).ByPath()["Week 2/w2.pdf"]
	if e.State != manifest.StateSkipped || e.RuleName != plan.ScopeRule {
		t.Fatalf("out-of-scope file must still be catalogued: %+v", e)
	}

	// The next full pull picks up what the scoped one deferred.
	s, err = r.Course(context.Background(), course, Options{RunID: "r2"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Fetched != 1 || latest(t, st).ByPath()["Week 2/w2.pdf"].State != manifest.StateStored {
		t.Fatalf("deferred file not fetched by full pull: %+v", s)
	}
}

// Treating a dangling latest as a first run would lose tombstones.
func TestDanglingLatestRecoversNewestManifest(t *testing.T) {
	src := newSource()
	src.add(1, 2, "a.pdf", "x")
	src.add(2, 2, "b.pdf", "y")
	st := store.NewMemory()
	r := runner(src, st, nil)
	if _, err := r.Course(context.Background(), course, Options{RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	_ = st.Put(context.Background(), manifest.LatestKey(course.ID), strings.NewReader("r0-missing"), -1)

	src.files = src.files[:1] // b.pdf deleted in Canvas
	s, err := r.Course(context.Background(), course, Options{RunID: "r2"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Tombstoned != 1 {
		t.Fatalf("previous manifest not recovered, tombstone lost: %+v", s)
	}
}

func TestSkippedFilesAreCataloguedWithReason(t *testing.T) {
	src := newSource()
	src.add(1, 2, "lecture.mp4", "video")
	st := store.NewMemory()
	if _, err := runner(src, st, nil).Course(context.Background(), course, Options{RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	e := latest(t, st).ByPath()["Week 1/lecture.mp4"]
	if e.State != manifest.StateSkipped || e.RuleName != "no-video" {
		t.Fatalf("entry = %+v", e)
	}
}

// The audit log must let you reconstruct what happened without the store.
func TestAuditTrail(t *testing.T) {
	src := newSource()
	src.add(1, 2, "a.pdf", "x")
	src.add(2, 2, "v.mp4", "video")
	st := store.NewMemory()
	var logs bytes.Buffer
	r := runner(src, st, &logs)
	r.Store = store.WithLogging(st, r.Log)
	if _, err := r.Course(context.Background(), course, Options{RunID: "r1"}); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	for _, event := range []string{
		"course.start", "course.first_run", "plan.computed", "plan.fetch", "plan.skip",
		"file.fetch_start", "file.fetched", "store.put", "manifest.written",
		"latest.published", "course.done",
	} {
		if !strings.Contains(out, `"msg":"`+event+`"`) {
			t.Errorf("audit log missing %s", event)
		}
	}
	if !strings.Contains(out, `"course_id":93794`) {
		t.Error("course events must carry course_id")
	}
}
