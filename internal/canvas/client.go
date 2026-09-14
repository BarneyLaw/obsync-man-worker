package canvas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Error kinds. Match with errors.Is; the concrete error is *APIError.
var (
	// ErrUnauthorized means the token itself was rejected. Fatal, never
	// retried, and it stops the whole run: every later request would fail the
	// same way.
	ErrUnauthorized = errors.New("canvas: access token rejected")
	// ErrForbidden means the token is fine but this user may not do this,
	// typically a course whose Files tab is hidden from students. Permanent
	// for that course, not a run failure.
	ErrForbidden = errors.New("canvas: not permitted for this user")
	ErrNotFound  = errors.New("canvas: not found")
	ErrThrottled = errors.New("canvas: throttled")
)

type APIError struct {
	What    string
	Status  int
	URL     string // redacted
	Message string
	Kind    error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("canvas: %s: %s returned %d: %s", e.What, e.URL, e.Status, e.Message)
}

func (e *APIError) Unwrap() error { return e.Kind }

// Client uses TWO http clients on purpose.
//
// api carries the bearer token and does NOT follow redirects. files carries no
// credentials at all: a file's url is a Canvas download endpoint carrying a
// verifier parameter, which redirects to presigned storage, and sending an
// Authorization header alongside a presigned signature gets rejected. Keeping
// the clients separate means the token cannot leak to the storage host.
type Client struct {
	BaseURL string
	Token   string
	// MaxPages bounds pagination so a server bug cannot spin forever.
	MaxPages int
	// MaxBody bounds a single API response.
	MaxBody int64

	log     *slog.Logger
	api     *http.Client
	files   *http.Client
	limiter *Limiter
	origin  *url.URL

	requests  atomic.Int64
	downloads atomic.Int64
}

func New(baseURL, token string) *Client {
	lim := NewLimiter(nil)
	c := &Client{
		BaseURL:  strings.TrimRight(baseURL, "/"),
		Token:    token,
		MaxPages: 1000,
		MaxBody:  32 << 20,
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		limiter:  lim,
	}
	c.origin, _ = url.Parse(c.BaseURL)
	c.api = &http.Client{
		Transport: lim,
		Timeout:   60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	c.files = &http.Client{Timeout: 30 * time.Minute, CheckRedirect: c.downloadRedirect}
	return c
}

// WithLogger routes client and limiter events to log.
func (c *Client) WithLogger(log *slog.Logger) *Client {
	c.log = log
	c.limiter.Log = log
	return c
}

func (c *Client) Limiter() *Limiter           { return c.limiter }
func (c *Client) RateLimitRemaining() float64 { return c.limiter.Remaining() }
func (c *Client) Requests() int64             { return c.requests.Load() }
func (c *Client) Downloads() int64            { return c.downloads.Load() }

func (c *Client) Courses(ctx context.Context) ([]Course, error) {
	var out []Course
	err := c.paginate(ctx, "list courses", "/api/v1/courses?enrollment_state=active&per_page=100",
		func(b []byte) error {
			var page []Course
			if err := json.Unmarshal(b, &page); err != nil {
				return err
			}
			for _, co := range page {
				// Courses outside their term dates come back as a stub with
				// only an id. There is nothing to pull from them.
				if co.AccessRestrictedByDate || co.Name == "" {
					c.log.Info("canvas.course_restricted", "course_id", co.ID)
					continue
				}
				out = append(out, co)
			}
			return nil
		})
	if err == nil {
		c.log.Info("canvas.courses_listed", "count", len(out))
	}
	return out, err
}

// Files returns the COMPLETE listing for a course.
//
// Not streamed: the differ needs the full set in order to compute tombstones,
// so streaming would buy nothing and complicate error handling. A course has
// hundreds of files, not millions.
func (c *Client) Files(ctx context.Context, courseID int64) ([]File, error) {
	var out []File
	err := c.paginate(ctx, "list files", fmt.Sprintf("/api/v1/courses/%d/files?per_page=100", courseID),
		func(b []byte) error {
			var page []File
			if err := json.Unmarshal(b, &page); err != nil {
				return err
			}
			out = append(out, page...)
			return nil
		})
	if err == nil {
		c.log.Info("canvas.files_listed", "course_id", courseID, "count", len(out))
	}
	return out, err
}

// Folders is needed to build human paths: the file object only carries
// folder_id, so you need the folder tree to turn that into "Week 1/Lectures".
func (c *Client) Folders(ctx context.Context, courseID int64) (map[int64]Folder, error) {
	out := map[int64]Folder{}
	err := c.paginate(ctx, "list folders", fmt.Sprintf("/api/v1/courses/%d/folders?per_page=100", courseID),
		func(b []byte) error {
			var page []Folder
			if err := json.Unmarshal(b, &page); err != nil {
				return err
			}
			for _, f := range page {
				out[f.ID] = f
			}
			return nil
		})
	if err == nil {
		c.log.Info("canvas.folders_listed", "course_id", courseID, "count", len(out))
	}
	return out, err
}

// ProbeFiles makes one request to find out whether a course's files are
// readable by this token, without listing them.
func (c *Client) ProbeFiles(ctx context.Context, courseID int64) error {
	_, _, err := c.do(ctx, "probe files", fmt.Sprintf("%s/api/v1/courses/%d/files?per_page=1", c.BaseURL, courseID))
	return err
}

// Open streams a file's bytes. Uses the credential-free client.
func (c *Client) Open(ctx context.Context, f File) (io.ReadCloser, error) {
	if f.URL == "" {
		return nil, fmt.Errorf("canvas: file %d has no url (locked?)", f.ID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("canvas: file %d: bad url %s", f.ID, redactURL(f.URL))
	}
	c.downloads.Add(1)
	start := time.Now()
	resp, err := c.files.Do(req)
	if err != nil {
		msg := redactErr(err)
		c.log.Warn("canvas.download_failed", "canvas_id", f.ID, "url", redactURL(f.URL), "err", msg)
		return nil, fmt.Errorf("canvas: download %d: %s", f.ID, msg)
	}
	c.log.Info("canvas.download_response", "canvas_id", f.ID, "status", resp.StatusCode,
		"content_length", resp.ContentLength, "served_by", resp.Request.URL.Host,
		"ttfb_ms", time.Since(start).Milliseconds())
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("canvas: download %d returned %s", f.ID, resp.Status)
	}
	return resp.Body, nil
}

func (c *Client) downloadRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	// Belt and braces: the files client never sets one, but nothing may carry
	// a credential to the storage host.
	req.Header.Del("Authorization")
	c.log.Debug("canvas.download_redirect", "to_host", req.URL.Host, "hop", len(via))
	return nil
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func (c *Client) paginate(ctx context.Context, what, path string, fn func([]byte) error) error {
	next := c.BaseURL + path
	seen := map[string]bool{}
	for page := 1; next != ""; page++ {
		if page > c.MaxPages {
			return fmt.Errorf("canvas: %s: more than %d pages, refusing to continue", what, c.MaxPages)
		}
		if seen[next] {
			return fmt.Errorf("canvas: %s: pagination loop, page %d repeats %s", what, page, redactURL(next))
		}
		seen[next] = true

		body, hdr, err := c.do(ctx, what, next)
		if err != nil {
			return err
		}
		if err := fn(body); err != nil {
			return fmt.Errorf("canvas: %s: decode page %d: %w", what, page, err)
		}
		next = parseNext(hdr.Get("Link"))
	}
	return nil
}

// do performs one authenticated API GET and logs it.
func (c *Client) do(ctx context.Context, what, rawURL string) ([]byte, http.Header, error) {
	if err := c.sameOrigin(rawURL); err != nil {
		c.log.Error("canvas.foreign_url_refused", "what", what, "url", redactURL(rawURL))
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")

	c.requests.Add(1)
	start := time.Now()
	resp, err := c.api.Do(req)
	if err != nil {
		c.log.Warn("canvas.request_failed", "what", what, "url", redactURL(rawURL), "err", redactErr(err))
		return nil, nil, fmt.Errorf("canvas: %s: %s", what, redactErr(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.MaxBody+1))
	c.log.Info("canvas.request", "what", what, "url", redactURL(rawURL), "status", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds(), "bytes", len(body),
		"rate_limit_remaining", resp.Header.Get("X-Rate-Limit-Remaining"),
		"request_cost", resp.Header.Get("X-Request-Cost"))
	if err != nil {
		return nil, nil, fmt.Errorf("canvas: %s: read body: %w", what, err)
	}
	if int64(len(body)) > c.MaxBody {
		return nil, nil, fmt.Errorf("canvas: %s: response exceeds %d bytes", what, c.MaxBody)
	}
	if err := classify(what, redactURL(rawURL), resp, body); err != nil {
		c.log.Warn("canvas.request_rejected", "what", what, "url", redactURL(rawURL),
			"status", resp.StatusCode, "err", err)
		return nil, nil, err
	}
	return body, resp.Header, nil
}

// sameOrigin refuses to send the token anywhere but the configured Canvas. A
// Link header is server-controlled input.
func (c *Client) sameOrigin(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("canvas: bad url: %w", err)
	}
	if c.origin == nil || u.Scheme != c.origin.Scheme || u.Host != c.origin.Host {
		return fmt.Errorf("canvas: refusing to send token to %s://%s (configured %s)", u.Scheme, u.Host, c.BaseURL)
	}
	return nil
}

// classify turns a non-200 into a typed error.
//
// Canvas answers 401 for BOTH a bad token and "user not authorized to perform
// that action". Treating every 401 as a dead token would abort the whole run
// over one course whose Files tab is hidden, so the body decides. Anything
// ambiguous is treated as the token: failing loud beats failing quiet.
func classify(what, u string, resp *http.Response, body []byte) error {
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	e := &APIError{What: what, Status: resp.StatusCode, URL: u, Message: errorMessage(body)}
	switch {
	case resp.StatusCode == http.StatusUnauthorized && permissionDenied(e.Message):
		e.Kind = ErrForbidden
	case resp.StatusCode == http.StatusUnauthorized:
		e.Kind = ErrUnauthorized
	case resp.StatusCode == http.StatusForbidden && isThrottle(resp):
		e.Kind = ErrThrottled
	case resp.StatusCode == http.StatusForbidden:
		e.Kind = ErrForbidden
	case resp.StatusCode == http.StatusNotFound:
		e.Kind = ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		e.Kind = ErrThrottled
	}
	return e
}

func permissionDenied(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "not authorized") || strings.Contains(m, "not authorised")
}

func errorMessage(body []byte) string {
	var v struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &v) == nil {
		if len(v.Errors) > 0 && v.Errors[0].Message != "" {
			return v.Errors[0].Message
		}
		if v.Message != "" {
			return v.Message
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

func parseNext(link string) string {
	m := nextLink.FindStringSubmatch(link)
	if len(m) < 2 {
		return ""
	}
	if _, err := url.Parse(m[1]); err != nil {
		return ""
	}
	return m[1]
}

// keepQuery lists query parameters safe to log. Everything else is redacted:
// download urls carry a verifier that grants access to the file.
var keepQuery = map[string]bool{"page": true, "per_page": true, "enrollment_state": true}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	u.User = nil
	q := u.Query()
	for k := range q {
		if !keepQuery[k] {
			q.Set(k, "REDACTED")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// redactErr strips the url net/http embeds in transport errors.
func redactErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Op + " " + redactURL(ue.URL) + ": " + ue.Err.Error()
	}
	return err.Error()
}

// SelectCourses resolves user-typed course selectors (numeric id, full course
// code, or one part of a cross-listed code like "CS2103T") to courses. Every
// selector must match exactly one course.
func SelectCourses(all []Course, wanted []string) ([]Course, error) {
	out, problems := ResolveCourses(all, wanted)
	if len(problems) > 0 {
		return nil, problems[0]
	}
	return out, nil
}

// ResolveCourses is SelectCourses that keeps going. A selector matching no
// course, or more than one, is reported in problems and left out; the others
// still resolve. The scheduled run uses it so that a course dropping off Canvas
// at the end of a semester does not stop the rest from syncing.
func ResolveCourses(all []Course, wanted []string) (out []Course, problems []error) {
	if len(wanted) == 0 {
		return all, nil
	}
	picked := map[int64]bool{}
	for _, w := range wanted {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		var hits []Course
		for _, c := range all {
			if courseMatches(c, w) {
				hits = append(hits, c)
			}
		}
		switch len(hits) {
		case 0:
			problems = append(problems, fmt.Errorf("no active course matches %q (available: %s)", w, describeCourses(all)))
			continue
		case 1:
		default:
			problems = append(problems, fmt.Errorf("course %q is ambiguous, it matches %s; use the numeric id", w, describeCourses(hits)))
			continue
		}
		if !picked[hits[0].ID] {
			picked[hits[0].ID] = true
			out = append(out, hits[0])
		}
	}
	return out, problems
}

func courseMatches(c Course, w string) bool {
	if strconv.FormatInt(c.ID, 10) == w || strings.EqualFold(c.Code, w) {
		return true
	}
	for _, part := range strings.Split(c.Code, "/") {
		if strings.EqualFold(strings.TrimSpace(part), w) {
			return true
		}
	}
	return false
}

func describeCourses(cs []Course) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = fmt.Sprintf("%s (%d)", c.Code, c.ID)
	}
	return strings.Join(parts, ", ")
}
