package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leifsen/obsync/internal/store"
)

func serveFixture(t *testing.T) *httptest.Server {
	t.Helper()
	fsStore := store.NewFS(t.TempDir())
	ctx := context.Background()
	for key, body := range map[string]string{
		"manifests/1/latest":            "r1",
		"manifests/1/r1.json":           `{"schema_version":1}`,
		"blobs/sha256/ab/cd/abcd":       "0123456789",
		"locks/worker.json":             `{"holder":"x"}`,
		"runs/r1.json":                  `{}`,
		"blobs/sha256/ab/cd/.tmp-12345": "partial",
	} {
		if err := fsStore.Put(ctx, key, strings.NewReader(body), -1); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(&storeHandler{
		st: fsStore, log: slog.New(slog.NewTextHandler(io.Discard, nil)), bucket: "obsync",
	})
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, srv *httptest.Server, method, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestServeReadsKeys(t *testing.T) {
	srv := serveFixture(t)
	resp, body := get(t, srv, "GET", "/manifests/1/latest", nil)
	if resp.StatusCode != 200 || body != "r1" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("latest: %d %q %q", resp.StatusCode, body, resp.Header.Get("Cache-Control"))
	}
	resp, _ = get(t, srv, "GET", "/manifests/1/r1.json", nil)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("manifest: %d %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
}

// The plugin's default Bucket setting is "obsync", so it requests
// {base}/obsync/{key}. Both URL shapes must reach the same object.
func TestServeAcceptsBucketPrefix(t *testing.T) {
	srv := serveFixture(t)
	resp, body := get(t, srv, "GET", "/obsync/manifests/1/latest", nil)
	if resp.StatusCode != 200 || body != "r1" {
		t.Fatalf("bucket-prefixed latest: %d %q", resp.StatusCode, body)
	}
	resp, body = get(t, srv, "GET", "/obsync/blobs/sha256/ab/cd/abcd", map[string]string{"Range": "bytes=0-1"})
	if resp.StatusCode != http.StatusPartialContent || body != "01" {
		t.Fatalf("bucket-prefixed range: %d %q", resp.StatusCode, body)
	}
	for _, p := range []string{"/obsync/locks/worker.json", "/other/manifests/1/latest", "/obsync/"} {
		if resp, _ := get(t, srv, "GET", p, nil); resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, resp.StatusCode)
		}
	}
}

// Multi-range requests seek more than once; each range must come back exact.
func TestServeMultiRange(t *testing.T) {
	srv := serveFixture(t)
	resp, body := get(t, srv, "GET", "/blobs/sha256/ab/cd/abcd", map[string]string{"Range": "bytes=0-1,7-9"})
	if resp.StatusCode != http.StatusPartialContent || !strings.Contains(body, "01") || !strings.Contains(body, "789") {
		t.Fatalf("multi-range: %d %q", resp.StatusCode, body)
	}
	resp, body = get(t, srv, "HEAD", "/blobs/sha256/ab/cd/abcd", nil)
	if resp.StatusCode != 200 || body != "" || resp.ContentLength != 10 {
		t.Fatalf("HEAD: %d len=%d body=%q", resp.StatusCode, resp.ContentLength, body)
	}
}

// The plugin's mobile path rests on Range. The dev server must honour it.
func TestServeHonoursRange(t *testing.T) {
	srv := serveFixture(t)
	resp, body := get(t, srv, "GET", "/blobs/sha256/ab/cd/abcd", map[string]string{"Range": "bytes=3-6"})
	if resp.StatusCode != http.StatusPartialContent || body != "3456" {
		t.Fatalf("range: %d %q", resp.StatusCode, body)
	}
}

func TestServeRefuses(t *testing.T) {
	srv := serveFixture(t)
	cases := []struct {
		method, path string
		want         int
	}{
		{"PUT", "/manifests/1/latest", http.StatusMethodNotAllowed},
		{"DELETE", "/blobs/sha256/ab/cd/abcd", http.StatusMethodNotAllowed},
		{"GET", "/locks/worker.json", http.StatusNotFound},
		{"GET", "/runs/r1.json", http.StatusNotFound},
		{"GET", "/blobs/sha256/ab/cd/.tmp-12345", http.StatusNotFound},
		{"GET", "/blobs/sha256/ab/", http.StatusNotFound},
		{"GET", "/manifests/../locks/worker.json", http.StatusNotFound},
		{"GET", "/", http.StatusNotFound},
	}
	for _, c := range cases {
		resp, _ := get(t, srv, c.method, c.path, nil)
		if resp.StatusCode != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, resp.StatusCode, c.want)
		}
	}
}
