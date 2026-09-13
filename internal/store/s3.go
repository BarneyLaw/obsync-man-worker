package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// S3Config selects an S3-compatible bucket. Region is required by the signer
// but meaningless to Garage; use "garage" on both sides.
type S3Config struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
}

// S3Env names the variables S3ConfigFromEnv reads. The CronJob sets the same.
var S3Env = struct{ Endpoint, Bucket, Region, AccessKey, SecretKey string }{
	"GARAGE_ENDPOINT", "GARAGE_BUCKET", "GARAGE_REGION", "GARAGE_ACCESS_KEY", "GARAGE_SECRET_KEY",
}

func S3ConfigFromEnv(getenv func(string) string) S3Config {
	return S3Config{
		Endpoint:  getenv(S3Env.Endpoint),
		Bucket:    getenv(S3Env.Bucket),
		Region:    getenv(S3Env.Region),
		AccessKey: getenv(S3Env.AccessKey),
		SecretKey: getenv(S3Env.SecretKey),
	}
}

// S3 talks to Garage (or R2, or anything S3-compatible).
//
//   - Path-style addressing: Garage does not need virtual-host buckets, and
//     path style keeps a plain http://host:3900 endpoint working.
//   - Garage has no object versioning, so every Put to an existing key is
//     destructive. That is safe here only because every key except
//     manifests/<course>/latest and the worker lease is write-once.
//   - GetRange is the single most load-bearing S3 feature in the design (the
//     plugin's mobile path depends on it) and is the first integration test.
//   - PutIfAbsent uses If-None-Match: *, but only after proving the server
//     honours it. See ConditionalWrites. Garage v2.3.0 does not (it accepts
//     the second create), so on Garage PutIfAbsent reports ErrUnsupported and
//     the lease runs best-effort. TestS3ConditionalWrites records which.
type S3 struct {
	bucket  string
	client  *s3.Client
	presign *s3.PresignClient

	probeMu     sync.Mutex
	probed      bool
	conditional bool
}

func NewS3(cfg S3Config) (*S3, error) {
	var missing []string
	for name, v := range map[string]string{
		S3Env.Endpoint: cfg.Endpoint, S3Env.Bucket: cfg.Bucket,
		S3Env.AccessKey: cfg.AccessKey, S3Env.SecretKey: cfg.SecretKey,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("store: s3 config incomplete, missing %s", strings.Join(missing, ", "))
	}
	if cfg.Region == "" {
		cfg.Region = "garage"
	}
	client := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: aws.String(strings.TrimRight(cfg.Endpoint, "/")),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		// Recent SDKs add CRC checksums to every upload and demand them on every
		// download by default. Not every S3-compatible server speaks those
		// trailers, and content integrity is already SHA-256 end to end here.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &S3{bucket: cfg.Bucket, client: client, presign: s3.NewPresignClient(client)}, nil
}

func (s *S3) Bucket() string { return s.bucket }

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, s.wrap("get", key, err)
	}
	return out.Body, nil
}

// GetRange returns [off, off+n) of an object, truncated at its end, and an
// empty reader when off is at or past the end: the same semantics as FS and
// Memory.
func (s *S3) GetRange(ctx context.Context, key string, off, n int64) (io.ReadCloser, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	if off < 0 || n < 0 {
		return nil, fmt.Errorf("store: s3 range %s: negative offset or length", key)
	}
	if n == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	rng := fmt.Sprintf("bytes=%d-%d", off, off+n-1)
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key, Range: &rng})
	if err != nil {
		if httpStatus(err) == 416 {
			return io.NopCloser(strings.NewReader("")), nil
		}
		return nil, s.wrap("get range", key, err)
	}
	// A server that ignores Range answers 200 with the whole object. Accepting
	// that would hand callers the wrong bytes, so fail loudly instead.
	if out.ContentRange == nil {
		out.Body.Close()
		return nil, fmt.Errorf("store: s3 range GET %s: server ignored the Range header", key)
	}
	return out.Body, nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	return s.put(ctx, key, r, size, false)
}

// PutIfAbsent creates key only if it does not exist. It returns
// errors.ErrUnsupported when the server has not been proven to honour
// If-None-Match, so callers degrade to check-then-write knowingly instead of
// silently overwriting.
func (s *S3) PutIfAbsent(ctx context.Context, key string, r io.Reader, size int64) error {
	ok, err := s.ConditionalWrites(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.ErrUnsupported
	}
	return s.put(ctx, key, r, size, true)
}

func (s *S3) put(ctx context.Context, key string, r io.Reader, size int64, ifAbsent bool) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	body, length, cleanup, err := seekable(key, r, size)
	if err != nil {
		return err
	}
	defer cleanup()
	in := &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: body, ContentLength: aws.Int64(length)}
	if ifAbsent {
		in.IfNoneMatch = aws.String("*")
	}
	if _, err := s.client.PutObject(ctx, in); err != nil {
		if ifAbsent && isConflict(err) {
			return ErrExists
		}
		return s.wrap("put", key, err)
	}
	return nil
}

// ConditionalWrites reports whether the server honours If-None-Match on
// PutObject. A server that ignores the header would turn every exclusive
// create into a silent overwrite, which is exactly the failure the lease exists
// to prevent, so it is tested rather than assumed: once per process, with a
// throwaway key under locks/. Needs write access.
func (s *S3) ConditionalWrites(ctx context.Context) (bool, error) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if s.probed {
		return s.conditional, nil
	}

	var b [8]byte
	_, _ = rand.Read(b[:])
	key := "locks/.probe-" + hex.EncodeToString(b[:])
	defer func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_, _ = s.client.DeleteObject(dctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	}()

	first := s.put(ctx, key, strings.NewReader("1"), 1, true)
	if first != nil {
		if notImplemented(first) {
			s.probed, s.conditional = true, false
			return false, nil
		}
		return false, fmt.Errorf("store: s3 conditional write probe: %w", first)
	}
	switch second := s.put(ctx, key, strings.NewReader("2"), 1, true); {
	case errors.Is(second, ErrExists):
		s.probed, s.conditional = true, true
	case second == nil, notImplemented(second):
		// Accepted a second create: the header was ignored.
		s.probed, s.conditional = true, false
	default:
		return false, fmt.Errorf("store: s3 conditional write probe: %w", second)
	}
	return s.conditional, nil
}

func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.Stat(ctx, key)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *S3) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	if err := ValidKey(key); err != nil {
		return ObjectInfo{}, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return ObjectInfo{}, s.wrap("head", key, err)
	}
	return ObjectInfo{Key: key, Size: aws.ToInt64(out.ContentLength), Modified: aws.ToTime(out.LastModified)}, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	// S3 delete is idempotent: a missing key is not an error.
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key}); err != nil {
		return s.wrap("delete", key, err)
	}
	return nil
}

func (s *S3) List(ctx context.Context, prefix string, fn func(ObjectInfo) error) error {
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return s.wrap("list", prefix, err)
		}
		for _, o := range page.Contents {
			if err := fn(ObjectInfo{
				Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), Modified: aws.ToTime(o.LastModified),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *S3) Presign(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key},
		s3.WithPresignExpires(ttl))
	if err != nil {
		return "", s.wrap("presign", key, err)
	}
	return req.URL, nil
}

// wrap maps a missing key to ErrNotFound, and nothing else: a missing bucket
// or bad credentials is a configuration error that must not read as "first
// run".
func (s *S3) wrap(op, key string, err error) error {
	var ae smithy.APIError
	if errors.As(err, &ae) && ae.ErrorCode() == "NoSuchBucket" {
		return fmt.Errorf("store: s3 %s %s: bucket %q does not exist: %w", op, key, s.bucket, err)
	}
	if errors.As(err, &ae) && (ae.ErrorCode() == "NoSuchKey" || ae.ErrorCode() == "NotFound") {
		return ErrNotFound
	}
	if httpStatus(err) == 404 && !(errors.As(err, &ae) && strings.Contains(ae.ErrorCode(), "Bucket")) {
		return ErrNotFound
	}
	return fmt.Errorf("store: s3 %s %s: %w", op, key, err)
}

func httpStatus(err error) int {
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

func isConflict(err error) bool {
	switch httpStatus(err) {
	case 412, 409: // PreconditionFailed; ConditionalRequestConflict on a race
		return true
	}
	return false
}

func notImplemented(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) && ae.ErrorCode() == "NotImplemented" {
		return true
	}
	return httpStatus(err) == 501
}

// seekable returns a body the SDK can sign and retry. Seekable readers (files,
// in-memory buffers: everything the worker writes) pass straight through, with
// their length checked against size before anything is sent. Anything else is
// spooled to a temp file first.
func seekable(key string, r io.Reader, size int64) (io.ReadSeeker, int64, func(), error) {
	noop := func() {}
	if rs, ok := r.(io.ReadSeeker); ok {
		cur, err := rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, 0, noop, err
		}
		end, err := rs.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, 0, noop, err
		}
		if _, err := rs.Seek(cur, io.SeekStart); err != nil {
			return nil, 0, noop, err
		}
		if err := checkSize(key, size, end-cur); err != nil {
			return nil, 0, noop, err
		}
		return rs, end - cur, noop, nil
	}

	tmp, err := os.CreateTemp("", "obsync-s3-put-*")
	if err != nil {
		return nil, 0, noop, err
	}
	cleanup := func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}
	n, err := io.Copy(tmp, r)
	if err == nil {
		err = checkSize(key, size, n)
	}
	if err == nil {
		_, err = tmp.Seek(0, io.SeekStart)
	}
	if err != nil {
		cleanup()
		return nil, 0, noop, err
	}
	return tmp, n, cleanup, nil
}

var (
	_ Store           = (*S3)(nil)
	_ RangeReader     = (*S3)(nil)
	_ Presigner       = (*S3)(nil)
	_ ExclusivePutter = (*S3)(nil)
	_ Stater          = (*S3)(nil)
	_ Store           = (*FS)(nil)
	_ RangeReader     = (*FS)(nil)
	_ ExclusivePutter = (*FS)(nil)
	_ Stater          = (*FS)(nil)
	_ Store           = (*Memory)(nil)
	_ RangeReader     = (*Memory)(nil)
	_ ExclusivePutter = (*Memory)(nil)
	_ Stater          = (*Memory)(nil)
)
