package runnerproto

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// UploadKind is the kind of an upload.
type UploadKind string

// The upload kinds.
const (
	// UploadArtifact is a file that the agent made.
	UploadArtifact UploadKind = "artifact"
	// UploadSnapshot is a part of a snapshot.
	UploadSnapshot UploadKind = "snapshot"
	// UploadTranscript is the raw transcript of the harness.
	UploadTranscript UploadKind = "transcript"
)

// Valid reports whether k is a known upload kind.
func (k UploadKind) Valid() bool {
	switch k {
	case UploadArtifact, UploadSnapshot, UploadTranscript:
		return true
	}
	return false
}

// UploadRequest is the body of the upload request.
type UploadRequest struct {
	// Kind is the kind of the upload.
	Kind UploadKind `json:"kind"`
	// Name is the file name, or for a snapshot the path in the part.
	Name string `json:"name"`
	// Bytes is the size of the file.
	Bytes int64 `json:"bytes"`
	// SHA256 is the SHA-256 hash of the file as 64 lower-case hex characters.
	SHA256 string `json:"sha256"`
	// ContentType is the media type of the file.
	ContentType string `json:"content_type,omitempty"`
	// Part is workspace or harness_state. It is set only for a snapshot.
	Part string `json:"part,omitempty"`
}

// Validate checks the rules of an upload request.
func (r UploadRequest) Validate() error {
	if !r.Kind.Valid() {
		return fmt.Errorf("invalid kind %q", r.Kind)
	}
	if r.Name == "" {
		return errors.New("name is empty")
	}
	if strings.HasPrefix(r.Name, "/") {
		return errors.New("name starts with /")
	}
	for _, el := range strings.Split(r.Name, "/") {
		if el == ".." {
			return errors.New("name has a .. path element")
		}
	}
	if r.Bytes < 0 {
		return errors.New("bytes is negative")
	}
	if !validSHA256(r.SHA256) {
		return errors.New("sha256 is not 64 lower-case hex characters")
	}
	if r.Kind == UploadSnapshot {
		if r.Part != "workspace" && r.Part != "harness_state" {
			return fmt.Errorf("invalid part %q for a snapshot", r.Part)
		}
	} else if r.Part != "" {
		return fmt.Errorf("part must be empty for kind %q", r.Kind)
	}
	return nil
}

func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// UploadResponse is the body of the response to the upload request.
type UploadResponse struct {
	// UploadID is the ID that an event names after the upload.
	UploadID string `json:"upload_id"`
	// Method is the HTTP method for the upload, for example PUT.
	Method string `json:"method"`
	// URL is the URL to send the file to.
	URL string `json:"url"`
	// Headers are headers to send with the upload.
	Headers map[string]string `json:"headers"`
	// ExpiresAt is the time when the URL expires.
	ExpiresAt time.Time `json:"expires_at"`
}

// DownloadsResponse is the body of the response to the downloads request.
type DownloadsResponse struct {
	// Files are the files to download.
	Files []DownloadFile `json:"files"`
}

// DownloadFile is one file to download.
type DownloadFile struct {
	// Part is the snapshot part, if the file belongs to a snapshot.
	Part string `json:"part,omitempty"`
	// Name is the file name or the path in the part.
	Name string `json:"name"`
	// URL is the presigned URL to get the file.
	URL string `json:"url"`
	// Bytes is the size of the file.
	Bytes int64 `json:"bytes"`
	// ExpiresAt is the time when the URL expires.
	ExpiresAt time.Time `json:"expires_at"`
}

// GitCredentialRequest is the body of the git credential request.
type GitCredentialRequest struct {
	// Protocol is the protocol, for example https.
	Protocol string `json:"protocol"`
	// Host is the host name of the forge.
	Host string `json:"host"`
	// Path is the path of the repository.
	Path string `json:"path"`
}

// GitCredentialResponse is the body of the response to the git credential request.
type GitCredentialResponse struct {
	// Username is the user name for git.
	Username string `json:"username"`
	// Password is the password or token for git.
	Password string `json:"password"`
	// ExpiresAt is the time when the credential expires.
	ExpiresAt time.Time `json:"expires_at"`
}

// CloudTokenRequest is the body of the cloud token request.
type CloudTokenRequest struct {
	// Audience is the audience of the token.
	Audience string `json:"audience"`
	// TTLSeconds is the requested life of the token.
	TTLSeconds int `json:"ttl_seconds"`
}

// CloudTokenResponse is the body of the response to the cloud token request.
type CloudTokenResponse struct {
	// Token is the cloud identity token.
	Token string `json:"token"`
	// ExpiresAt is the time when the token expires.
	ExpiresAt time.Time `json:"expires_at"`
}
