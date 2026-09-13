package run

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/leifsen/obsync/internal/store"
)

// Summary is runs/<run_id>.json: the durable record of one worker invocation,
// including courses that failed and never produced a manifest. Write-once.
type Summary struct {
	RunID           string         `json:"run_id"`
	Command         string         `json:"command"`
	Trigger         string         `json:"trigger"`
	Host            string         `json:"host"`
	PID             int            `json:"pid"`
	DryRun          bool           `json:"dry_run"`
	CourseSelectors []string       `json:"course_selectors,omitempty"`
	Scope           []string       `json:"scope,omitempty"`
	RulesHash       string         `json:"rules_hash"`
	StartedAt       time.Time      `json:"started_at"`
	FinishedAt      time.Time      `json:"finished_at"`
	Outcome         string         `json:"outcome"`
	Error           string         `json:"error,omitempty"`
	Courses         []CourseResult `json:"courses"`
}

// Course statuses.
const (
	StatusCommitted = "committed"
	StatusDryRun    = "dry_run"
	StatusForbidden = "forbidden"
	StatusFailed    = "failed"
	StatusNotRun    = "not_run"
)

type CourseResult struct {
	CourseID int64  `json:"course_id"`
	Code     string `json:"course_code"`
	Name     string `json:"course_name"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
	Stats    Stats  `json:"stats"`
}

func SummaryKey(runID string) string { return "runs/" + runID + ".json" }

func WriteSummary(ctx context.Context, st store.Store, s Summary) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	_, err = store.PutOnce(ctx, st, SummaryKey(s.RunID), bytes.NewReader(b), int64(len(b)))
	return err
}
