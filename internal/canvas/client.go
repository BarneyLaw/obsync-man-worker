package canvas

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// ErrUnauthorized is fatal, never retried. Surface it loudly.
var ErrUnauthorized = fmt.Errorf("canvas: token rejected (401)")

// Client uses TWO http clients on purpose.
//
// api carries the bearer token and does NOT follow redirects. files carries no
// credentials at all, because Canvas file URLs are presigned storage URLs and
// sending an Authorization header alongside a presigned signature gets
// rejected. Go strips Authorization on cross-host redirects, but relying on
// that when you can just separate the clients is asking for a confusing
// afternoon.
type Client struct {
	BaseURL string
	Token   string

	api     *http.Client
	files   *http.Client
	limiter *Limiter
}

func New(baseURL, token string) *Client {
	lim := NewLimiter(nil)
	return &Client{
		BaseURL: baseURL,
		Token:   token,
		limiter: lim,
		api: &http.Client{
			Transport: lim,
			Timeout:   60 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		files: &http.Client{Timeout: 30 * time.Minute},
	}
}

func (c *Client) RateLimitRemaining() float64 { return c.limiter.Remaining() }

func (c *Client) Courses(ctx context.Context) ([]Course, error) {
	var out []Course
	err := c.paginate(ctx, "/api/v1/courses?enrollment_state=active&per_page=100",
		func(b []byte) error {
			var page []Course
			if err := json.Unmarshal(b, &page); err != nil {
				return err
			}
			out = append(out, page...)
			return nil
		})
	return out, err
}

// Files returns the COMPLETE listing for a course.
//
// Not streamed: the differ needs the full set in order to compute tombstones,
// so streaming would buy nothing and complicate error handling. A course has
// hundreds of files, not millions.
func (c *Client) Files(ctx context.Context, courseID int64) ([]File, error) {
	var out []File
	err := c.paginate(ctx, fmt.Sprintf("/api/v1/courses/%d/files?per_page=100", courseID),
		func(b []byte) error {
			var page []File
			if err := json.Unmarshal(b, &page); err != nil {
				return err
			}
			out = append(out, page...)
			return nil
		})
	return out, err
}

// Folders is needed to build human paths: the file object only carries
// folder_id, so you need the folder tree to turn that into "Week 1/Lectures".
func (c *Client) Folders(ctx context.Context, courseID int64) (map[int64]Folder, error) {
	out := map[int64]Folder{}
	err := c.paginate(ctx, fmt.Sprintf("/api/v1/courses/%d/folders?per_page=100", courseID),
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
	return out, err
}

// Open streams a file's bytes. Uses the credential-free client.
func (c *Client) Open(ctx context.Context, f File) (io.ReadCloser, error) {
	if f.URL == "" {
		return nil, fmt.Errorf("canvas: file %d has no url (locked?)", f.ID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.files.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("canvas: download %d returned %s", f.ID, resp.Status)
	}
	return resp.Body, nil
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func (c *Client) paginate(ctx context.Context, path string, fn func([]byte) error) error {
	next := c.BaseURL + path
	for next != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Accept", "application/json")

		resp, err := c.api.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return err
		}
		switch resp.StatusCode {
		case http.StatusOK:
		case http.StatusUnauthorized:
			return ErrUnauthorized
		default:
			return fmt.Errorf("canvas: %s returned %s", next, resp.Status)
		}
		if err := fn(body); err != nil {
			return err
		}
		next = parseNext(resp.Header.Get("Link"))
	}
	return nil
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
