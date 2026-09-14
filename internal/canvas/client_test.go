package canvas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok")
	c.Limiter().Stall = 5 * time.Millisecond
	return c, srv
}

func TestPaginationFollowsLinksWithToken(t *testing.T) {
	var srvURL string
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing token on %s", r.URL)
		}
		page := r.URL.Query().Get("page")
		switch page {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/v1/courses/1/files?page=2&per_page=100>; rel="next"`, srvURL))
			json.NewEncoder(w).Encode([]File{{ID: 1}})
		case "2":
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/v1/courses/1/files?page=1>; rel="first", <%s/api/v1/courses/1/files?page=3&per_page=100>; rel="next"`, srvURL, srvURL))
			json.NewEncoder(w).Encode([]File{{ID: 2}})
		default:
			json.NewEncoder(w).Encode([]File{{ID: 3}})
		}
	})
	srvURL = srv.URL
	files, err := c.Files(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("files = %+v", files)
	}
}

func TestPaginationLoopIsDetected(t *testing.T) {
	var srvURL string
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/api/v1/courses/1/files?per_page=100>; rel="next"`, srvURL))
		w.Write([]byte("[]"))
	})
	srvURL = srv.URL
	_, err := c.Files(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "loop") {
		t.Fatalf("got %v, want pagination loop error", err)
	}
}

// A Link header is server-controlled; the token must never follow it offsite.
func TestPaginationRefusesForeignHost(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://evil.example/api/v1/steal>; rel="next"`)
		w.Write([]byte("[]"))
	})
	_, err := c.Files(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "refusing to send token") {
		t.Fatalf("got %v", err)
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		header map[string]string
		want   error
	}{
		{"bad token", 401, `{"errors":[{"message":"Invalid access token."}]}`, nil, ErrUnauthorized},
		{"401 empty body is the token", 401, ``, nil, ErrUnauthorized},
		{"401 permission", 401, `{"status":"unauthorized","errors":[{"message":"user not authorized to perform that action"}]}`, nil, ErrForbidden},
		{"403 files tab hidden (NUS)", 403, `{"status":"unauthorized","errors":[{"message":"user not authorised to perform that action"}]}`, nil, ErrForbidden},
		{"404", 404, `{"errors":[{"message":"The specified resource does not exist."}]}`, nil, ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			})
			_, err := c.Files(context.Background(), 1)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if hits.Load() != 1 {
				t.Fatalf("non-throttle errors must not be retried, got %d requests", hits.Load())
			}
		})
	}
}

func TestThrottleIsRetried(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte("[]"))
	})
	if _, err := c.Files(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 || c.Limiter().Throttles() != 1 {
		t.Fatalf("hits=%d throttles=%d", hits.Load(), c.Limiter().Throttles())
	}
}

// The old limiter looped forever once remaining dipped below Floor, because
// only a request could raise it again.
func TestLowQuotaDoesNotHang(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Rate-Limit-Remaining", "5")
		w.Write([]byte("[]"))
	})
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 4; i++ {
			if _, err := c.Files(context.Background(), 1); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("limiter hung below floor")
	}
	if c.Limiter().Stalls() == 0 {
		t.Fatal("expected at least one stall")
	}
}

func TestStallHonoursCancellation(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Rate-Limit-Remaining", "1")
		w.Write([]byte("[]"))
	})
	c.Limiter().Stall = time.Hour
	if _, err := c.Files(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.Files(ctx, 1); err == nil {
		t.Fatal("expected cancellation")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("stall ignored context cancellation")
	}
}

// The token must never reach the storage host, including across redirects.
func TestDownloadCarriesNoToken(t *testing.T) {
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Authorization header reached storage host")
		}
		w.Write([]byte("pdf-bytes"))
	}))
	defer storage.Close()
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("download request to canvas carried the token")
		}
		http.Redirect(w, r, storage.URL+"/blob?X-Amz-Signature=abc", http.StatusFound)
	})
	rc, err := c.Open(context.Background(), File{ID: 9, URL: srv.URL + "/files/9/download?verifier=secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "pdf-bytes" {
		t.Fatalf("got %q", b)
	}
}

func TestRedactURL(t *testing.T) {
	got := redactURL("https://canvas.nus.edu.sg/files/9/download?download_frd=1&verifier=s3cret&page=2")
	if strings.Contains(got, "s3cret") {
		t.Fatalf("verifier leaked: %s", got)
	}
	if !strings.Contains(got, "page=2") {
		t.Fatalf("safe params should survive: %s", got)
	}
}

func TestRestrictedCoursesAreSkipped(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":1,"name":"CS3103","course_code":"CS3103"},{"id":2,"access_restricted_by_date":true}]`))
	})
	courses, err := c.Courses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(courses) != 1 || courses[0].ID != 1 {
		t.Fatalf("courses = %+v", courses)
	}
}

func TestSelectCourses(t *testing.T) {
	all := []Course{
		{ID: 77826, Code: "CS2103/CS2103T"},
		{ID: 93794, Code: "CS3103"},
		{ID: 40630, Code: "THE1001/RC1000A"},
	}
	cases := []struct {
		want    []string
		ids     []int64
		wantErr string
	}{
		{nil, []int64{77826, 93794, 40630}, ""},
		{[]string{"cs3103"}, []int64{93794}, ""},
		{[]string{"CS2103T"}, []int64{77826}, ""},
		{[]string{"77826", "CS2103"}, []int64{77826}, ""},
		{[]string{"CS9999"}, nil, "no active course"},
	}
	for _, tc := range cases {
		got, err := SelectCourses(all, tc.want)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%v: got err %v", tc.want, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(tc.ids) {
			t.Fatalf("%v: got %+v", tc.want, got)
		}
		for i, id := range tc.ids {
			if got[i].ID != id {
				t.Fatalf("%v: got %+v", tc.want, got)
			}
		}
	}
	amb := []Course{{ID: 1, Code: "X/A"}, {ID: 2, Code: "Y/A"}}
	if _, err := SelectCourses(amb, []string{"A"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous selector: %v", err)
	}
}

func TestResolveCoursesKeepsGoing(t *testing.T) {
	all := []Course{
		{ID: 93794, Code: "CS3103"},
		{ID: 94846, Code: "LAG1201"},
		{ID: 1, Code: "X/A"},
		{ID: 2, Code: "Y/A"},
	}
	got, problems := ResolveCourses(all, []string{"CS9999", "lag1201", "A", "CS3103"})
	if len(got) != 2 || got[0].ID != 94846 || got[1].ID != 93794 {
		t.Fatalf("resolved courses: got %+v", got)
	}
	if len(problems) != 2 ||
		!strings.Contains(problems[0].Error(), "no active course matches \"CS9999\"") ||
		!strings.Contains(problems[1].Error(), "ambiguous") {
		t.Fatalf("problems: got %v", problems)
	}
	if got, problems := ResolveCourses(all, nil); len(got) != len(all) || problems != nil {
		t.Fatalf("no selectors must select every course: got %+v, %v", got, problems)
	}
}
