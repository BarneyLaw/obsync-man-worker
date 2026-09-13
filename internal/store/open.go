package store

import (
	"errors"
	"path/filepath"
	"strings"
)

// Description says which backend Open chose, for logs and startup banners.
// It never contains credentials.
type Description struct {
	Kind     string // "fs" or "s3"
	Location string // absolute root, or endpoint/bucket
}

func (d Description) String() string { return d.Kind + ":" + d.Location }

// Open picks the backend. A filesystem root wins when given; otherwise the
// GARAGE_* variables select S3. Both commands and the CronJob go through here
// so they cannot disagree about precedence.
func Open(fsRoot string, getenv func(string) string) (Store, Description, error) {
	if fsRoot != "" {
		abs, err := filepath.Abs(fsRoot)
		if err != nil {
			abs = fsRoot
		}
		return NewFS(fsRoot), Description{Kind: "fs", Location: abs}, nil
	}
	cfg := S3ConfigFromEnv(getenv)
	if cfg.Endpoint == "" {
		return nil, Description{}, errors.New("no store configured: pass -fs-store DIR, or set " +
			strings.Join([]string{S3Env.Endpoint, S3Env.Bucket, S3Env.AccessKey, S3Env.SecretKey}, ", "))
	}
	s, err := NewS3(cfg)
	if err != nil {
		return nil, Description{}, err
	}
	return s, Description{Kind: "s3", Location: strings.TrimRight(cfg.Endpoint, "/") + "/" + cfg.Bucket}, nil
}
