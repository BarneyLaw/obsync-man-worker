// Package canvas is the Canvas LMS Source implementation.
package canvas

import "time"

type Course struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Code string `json:"course_code"`
}

// File mirrors the Canvas REST file object.
//
// Note what is NOT here: a content hash. Canvas does not expose one over REST.
// There is an md5/sha512 column, but it lives in the Canvas Data (DAP) dataset,
// not in this response. That single absence is why change detection is two
// stage: (size, updated_at, modified_at) is a MAYBE-CHANGED signal that
// triggers a download, and SHA-256 over the downloaded bytes decides whether a
// new blob is actually stored.
type File struct {
	ID          int64      `json:"id"`
	UUID        string     `json:"uuid"`
	FolderID    int64      `json:"folder_id"`
	DisplayName string     `json:"display_name"`
	Filename    string     `json:"filename"`
	ContentType string     `json:"content-type"`
	URL         string     `json:"url"`
	Size        int64      `json:"size"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ModifiedAt  time.Time  `json:"modified_at"`
	UnlockAt    *time.Time `json:"unlock_at"`
	Locked      bool       `json:"locked"`
	Hidden      bool       `json:"hidden"`
	// LockedForUser is the field that actually matters: a file can be visible
	// in the listing and still not downloadable.
	LockedForUser bool `json:"locked_for_user"`
	MimeClass     string `json:"mime_class"`
}

type Folder struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Name     string `json:"name"`
}
