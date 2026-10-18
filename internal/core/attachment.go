package core

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"
)

// MaxAttachmentBytes is the size limit of one uploaded attachment.
const MaxAttachmentBytes = 25 << 20

// MaxAttachmentsPerMessage is the most attachments one message can carry.
const MaxAttachmentsPerMessage = 20

// Attachment is a file that a person uploaded for the messages of a run.
// The control plane keeps the file in object storage and the metadata in the
// key/value store (see AttachmentKVKey).
type Attachment struct {
	ID          string    `json:"attachment_id"`
	TeamID      string    `json:"team_id"`
	Name        string    `json:"name"`
	ContentType string    `json:"content_type"`
	Bytes       int64     `json:"bytes"`
	Key         string    `json:"-"` // object key
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// AttachmentKVKey is the key/value store key of the metadata of an attachment.
func AttachmentKVKey(id string) string { return "attachment:" + id }

// NormalizeAttachmentName returns the base name of an uploaded file name. It
// replaces backslashes with slashes first. The name must not be empty, "." or
// "..", and must have at most 255 bytes (ErrInvalid otherwise).
func NormalizeAttachmentName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("%w: attachment name is required", ErrInvalid)
	}
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "" || name == "." || name == ".." || name == "/" {
		return "", fmt.Errorf("%w: invalid attachment name %q", ErrInvalid, raw)
	}
	if len(name) > 255 {
		return "", fmt.Errorf("%w: attachment name is longer than 255 bytes", ErrInvalid)
	}
	return name, nil
}

// attachmentRecord is the stored form of an Attachment. It adds the object
// key, which the JSON form of the API hides.
type attachmentRecord struct {
	Attachment
	ObjectKey string `json:"object_key"`
}

// MarshalAttachment encodes an Attachment for the key/value store. Unlike
// the API form, it includes the object key.
func MarshalAttachment(a Attachment) (string, error) {
	b, err := json.Marshal(attachmentRecord{Attachment: a, ObjectKey: a.Key})
	return string(b), err
}

// UnmarshalAttachment decodes the output of MarshalAttachment.
func UnmarshalAttachment(s string) (Attachment, error) {
	var r attachmentRecord
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return Attachment{}, err
	}
	r.Key = r.ObjectKey
	return r.Attachment, nil
}
