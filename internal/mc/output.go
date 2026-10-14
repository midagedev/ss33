package mc

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"
)

// s3err is an S3 XML error, or a status with an empty body (HEAD).
type s3err struct {
	Status    int
	Code      string
	Message   string
	Resource  string
	RequestID string
	Bucket    string
	Key       string
}

func (e *s3err) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return e.Code
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// s3ErrorJSON is the cause.error object official mc emits for a raw S3 error.
// Field order matches that client.
type s3ErrorJSON struct {
	Code       string `json:"Code"`
	Message    string `json:"Message"`
	BucketName string `json:"BucketName"`
	Key        string `json:"Key"`
	Resource   string `json:"Resource"`
	RequestID  string `json:"RequestID"`
	HostID     string `json:"HostID"`
	Region     string `json:"Region"`
	Server     string `json:"Server"`
}

func (e *s3err) payload() s3ErrorJSON {
	return s3ErrorJSON{
		Code: e.Code, Message: e.Message, BucketName: e.Bucket, Key: e.Key,
		Resource: e.Resource, RequestID: e.RequestID,
	}
}

func readS3Err(status int, body []byte) *s3err {
	var x struct {
		Code, Message, Resource, RequestId, BucketName, Key string
	}
	xml.Unmarshal(body, &x)
	return &s3err{Status: status, Code: x.Code, Message: x.Message, Resource: x.Resource, RequestID: x.RequestId, Bucket: x.BucketName, Key: x.Key}
}

// bucketMissing is the cause.error official mc uses for a missing bucket on ls/rb.
type bucketMissing struct {
	Bucket string `json:"Bucket"`
}

type errorLine struct {
	Status string    `json:"status"`
	Error  errorBody `json:"error"`
}

type errorBody struct {
	Message string     `json:"message"`
	Cause   errorCause `json:"cause"`
	Type    string     `json:"type"`
}

type errorCause struct {
	Message string          `json:"message"`
	Error   json.RawMessage `json:"error"`
}

type lsLine struct {
	Status         string `json:"status"`
	Type           string `json:"type"`
	LastModified   string `json:"lastModified"`
	Size           int64  `json:"size"`
	Key            string `json:"key"`
	ETag           string `json:"etag"`
	URL            string `json:"url"`
	VersionID      string `json:"versionId,omitempty"`
	VersionOrdinal int    `json:"versionOrdinal"`
	IsDeleteMarker bool   `json:"isDeleteMarker,omitempty"`
	StorageClass   string `json:"storageClass,omitempty"`
}

type statLine struct {
	Status       string            `json:"status"`
	Name         string            `json:"name"`
	LastModified string            `json:"lastModified"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag"`
	Type         string            `json:"type"`
	Metadata     map[string]string `json:"metadata"`
	VersionID    string            `json:"versionID,omitempty"`
}

// statBucketLine is the short bucket report (official mc prints a multi-page one).
type statBucketLine struct {
	Status       string `json:"status"`
	Name         string `json:"name"`
	LastModified string `json:"lastModified"`
	Size         int64  `json:"size"`
}

type mbLine struct {
	Status string `json:"status"`
	Bucket string `json:"bucket"`
	Region string `json:"region"`
}

type rbLine struct {
	Status string `json:"status"`
	Bucket string `json:"bucket"`
}

type cpLine struct {
	Status     string `json:"status"`
	Source     string `json:"source"`
	Target     string `json:"target"`
	Size       int64  `json:"size"`
	TotalCount int    `json:"totalCount"`
	TotalSize  int64  `json:"totalSize"`
}

type cpSummary struct {
	Status      string  `json:"status"`
	Total       int64   `json:"total"`
	Transferred int64   `json:"transferred"`
	Duration    int64   `json:"duration"`
	Speed       float64 `json:"speed"`
}

type rmLine struct {
	Status       string     `json:"status"`
	Key          string     `json:"key"`
	DeleteMarker bool       `json:"deleteMarker"`
	VersionID    string     `json:"versionID"`
	ModTime      *time.Time `json:"modTime"`
	DryRun       bool       `json:"dryRun"`
}

type aliasLine struct {
	Status    string `json:"status"`
	Alias     string `json:"alias"`
	URL       string `json:"URL"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	API       string `json:"api"`
	Path      string `json:"path"`
}

type anonLine struct {
	Operation  string          `json:"operation"`
	Status     string          `json:"status"`
	Bucket     string          `json:"bucket"`
	Permission string          `json:"permission"`
	Anonymous  json.RawMessage `json:"anonymous,omitempty"`
}

type anonListLine struct {
	Resource string `json:"resource"`
	Allow    string `json:"allow"`
}

type versionLine struct {
	Op         string `json:"Op"`
	Status     string `json:"status"`
	URL        string `json:"url"`
	Versioning struct {
		Status    string `json:"status"`
		MFADelete string `json:"MFADelete"`
	} `json:"versioning"`
}

type readyLine struct {
	Status          string  `json:"status"`
	Alias           string  `json:"alias"`
	Healthy         bool    `json:"healthy"`
	MaintenanceMode bool    `json:"maintenanceMode"`
	WriteQuorum     int     `json:"writeQuorum"`
	HealingDrives   int     `json:"healingDrives"`
	Error           *string `json:"error"`
}

type eventLSLine struct {
	Status string   `json:"status"`
	ARN    string   `json:"arn"`
	Event  []string `json:"event"`
	Prefix string   `json:"prefix"`
	Suffix string   `json:"suffix"`
}

type eventChangeLine struct {
	Status string `json:"status"`
	Bucket string `json:"bucket"`
	ARN    string `json:"arn"`
}

type pipeLine struct {
	Status string `json:"status"`
	Target string `json:"target"`
	Size   int64  `json:"size"`
}

type findLine struct {
	Status       string `json:"status"`
	Type         string `json:"type"`
	LastModified string `json:"lastModified"`
	Size         int64  `json:"size"`
	Key          string `json:"key"`
	ETag         string `json:"etag"`
}

type duLine struct {
	Prefix     string `json:"prefix"`
	Size       int64  `json:"size"`
	Objects    int    `json:"objects"`
	Status     string `json:"status"`
	IsVersions bool   `json:"isVersions"`
}

type tagChangeLine struct {
	Status    string `json:"status"`
	Name      string `json:"name"`
	VersionID string `json:"versionID"`
}

type tagListLine struct {
	Tagset    map[string]string `json:"tagset"`
	Status    string            `json:"status"`
	URL       string            `json:"url"`
	VersionID string            `json:"versionID"`
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func marshalRaw(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}

// formatLSTime renders an S3 timestamp in local time with millisecond precision.
func formatLSTime(s string) string {
	for _, layout := range []string{"2006-01-02T15:04:05.000Z", time.RFC3339Nano, time.RFC3339} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts.Local().Format("2006-01-02T15:04:05.000Z07:00")
		}
	}
	return s
}

func stripETag(s string) string { return strings.Trim(s, `"`) }

// writePlainStat prints the `mc stat` text block. Labels are padded to 10 columns,
// metadata keys to the longest key, sorted.
func writePlainStat(w io.Writer, name string, when time.Time, size int64, etag, versionID string, meta map[string]string) {
	fmt.Fprintf(w, "%-10s: %s\n", "Name", name)
	if !when.IsZero() {
		fmt.Fprintf(w, "%-10s: %s\n", "Date", when.Local().Format("2006-01-02 15:04:05 MST"))
	}
	fmt.Fprintf(w, "%-10s: %s\n", "Size", humanSize(size))
	if etag != "" {
		fmt.Fprintf(w, "%-10s: %s\n", "ETag", etag)
	}
	if versionID != "" {
		fmt.Fprintf(w, "%-10s: %s\n", "VersionID", versionID)
	}
	fmt.Fprintf(w, "%-10s: %s\n", "Type", "file")
	fmt.Fprintf(w, "%-10s:\n", "Metadata")
	keys := slices.Sorted(maps.Keys(meta))
	width := 0
	for _, k := range keys {
		if len(k) > width {
			width = len(k)
		}
	}
	for _, k := range keys {
		fmt.Fprintf(w, "  %-*s: %s\n", width, k, meta[k])
	}
	fmt.Fprintln(w)
}

// statMeta collects the headers `mc stat` reports. User metadata is kept in canonical header case.
func statMeta(h http.Header) map[string]string {
	meta := map[string]string{}
	for _, name := range []string{"Content-Type", "Cache-Control", "Content-Encoding", "Content-Disposition", "Content-Language", "Expires"} {
		if v := h.Get(name); v != "" {
			meta[name] = v
		}
	}
	for k, vs := range h {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-meta-") && len(vs) > 0 {
			meta[http.CanonicalHeaderKey(k)] = vs[0]
		}
	}
	return meta
}
