package server

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/midagedev/ss33/internal/sigv4"
	"github.com/midagedev/ss33/internal/store"
)

// isPostObject is a browser form upload: POST /<bucket> with a multipart/form-data body. It authenticates
// with the signed policy in the form, not with a header, so it is routed before the usual auth check.
func isPostObject(r *http.Request, bucket, key string) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return r.Method == http.MethodPost && bucket != "" && key == "" && mt == "multipart/form-data"
}

var (
	errTooLarge = errors.New("EntityTooLarge")
	errTooSmall = errors.New("EntityTooSmall")
)

// postObject implements PostObject: the form fields come first, then the file. The policy's signature,
// expiration and conditions are checked before any byte of the file is stored.
func (s *Server) postObject(w http.ResponseWriter, r *http.Request, bucket string) {
	mr, err := r.MultipartReader()
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "MalformedPOSTRequest", "The body of your POST request is not well-formed multipart/form-data.")
		return
	}
	fields := map[string]string{} // lowercased names, as S3 matches them
	for {
		part, err := mr.NextPart()
		if err != nil {
			s.fail(w, r, http.StatusBadRequest, "MalformedPOSTRequest", "The body of your POST request is not well-formed multipart/form-data.")
			return
		}
		name := strings.ToLower(part.FormName())
		if name == "bucket" {
			continue // the bucket is the one in the URL; a form field must not move the upload elsewhere
		}
		if name != "file" {
			v, _ := io.ReadAll(io.LimitReader(part, 64<<10))
			fields[name] = string(v)
			continue
		}
		fields["bucket"] = bucket
		if fields["content-type"] == "" {
			fields["content-type"] = part.Header.Get("Content-Type")
		}
		s.storePostedFile(w, r, bucket, fields, part.FileName(), part)
		return
	}
}

func (s *Server) storePostedFile(w http.ResponseWriter, r *http.Request, bucket string, fields map[string]string, filename string, file io.Reader) {
	sigV4 := fields["x-amz-signature"] != ""
	credential, sig := fields["awsaccesskeyid"], fields["signature"]
	if sigV4 {
		credential, sig = fields["x-amz-credential"], fields["x-amz-signature"]
	}
	if fields["key"] == "" || fields["policy"] == "" || sig == "" {
		s.fail(w, r, http.StatusBadRequest, "InvalidArgument", "Bucket POST must contain a key, a policy and a signature")
		return
	}
	switch err := sigv4.VerifyPolicy(s.Creds, fields["policy"], credential, sig, sigV4); {
	case errors.Is(err, sigv4.ErrUnknownAccessKey):
		s.fail(w, r, http.StatusForbidden, "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records.")
		return
	case err != nil:
		s.fail(w, r, http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.")
		return
	}
	min, max, err := checkPolicy(fields, s.now())
	if err != nil {
		s.fail(w, r, http.StatusForbidden, "AccessDenied", "Invalid according to Policy: "+err.Error())
		return
	}

	// The policy is matched against the key as posted; ${filename} is filled in afterwards.
	meta := store.ObjectMeta{Key: strings.ReplaceAll(fields["key"], "${filename}", filename), ContentType: fields["content-type"], Headers: map[string]string{}, UserMeta: map[string]string{}}
	if meta.ContentType == "" {
		meta.ContentType = "binary/octet-stream"
	}
	for name, v := range fields {
		if strings.HasPrefix(name, "x-amz-meta-") {
			meta.UserMeta[strings.TrimPrefix(name, "x-amz-meta-")] = v
		}
		for _, h := range storedHeaders {
			if name == strings.ToLower(h) {
				meta.Headers[h] = v
			}
		}
	}
	meta.Public = publicACL(fields["acl"])
	out, err := s.Store.PutObject(bucket, meta, &sizeLimit{r: file, min: min, max: max}, store.Precondition{})
	switch {
	case errors.Is(err, errTooLarge):
		s.fail(w, r, http.StatusBadRequest, "EntityTooLarge", "Your proposed upload exceeds the maximum allowed size")
		return
	case errors.Is(err, errTooSmall):
		s.fail(w, r, http.StatusBadRequest, "EntityTooSmall", "Your proposed upload is smaller than the minimum allowed size")
		return
	case err != nil:
		s.storeErr(w, r, err)
		return
	}

	location := "/" + bucket + "/" + out.Key
	w.Header().Set("ETag", out.ETag)
	w.Header().Set("Location", location)
	switch fields["success_action_status"] {
	case "200":
		w.WriteHeader(http.StatusOK)
	case "201":
		writeXML(w, http.StatusCreated, struct {
			XMLName  xml.Name `xml:"PostResponse"`
			Location string
			Bucket   string
			Key      string
			ETag     string
		}{Location: location, Bucket: bucket, Key: out.Key, ETag: out.ETag})
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// checkPolicy decodes the base64 policy, checks its expiration and every condition against the form
// fields, and returns the allowed content length range.
func checkPolicy(fields map[string]string, now time.Time) (min, max int64, err error) {
	raw, err := base64.StdEncoding.DecodeString(fields["policy"])
	if err != nil {
		return 0, 0, errors.New("policy is not valid base64")
	}
	var policy struct {
		Expiration string
		Conditions []json.RawMessage
	}
	if err := json.Unmarshal(raw, &policy); err != nil {
		return 0, 0, errors.New("policy is not valid JSON")
	}
	exp, err := time.Parse(time.RFC3339, policy.Expiration)
	if err != nil || now.After(exp) {
		return 0, 0, errors.New("Policy expired.")
	}
	min, max = 0, 5<<30
	for _, c := range policy.Conditions {
		var exact map[string]string
		if json.Unmarshal(c, &exact) == nil {
			for name, want := range exact {
				if got := fields[strings.ToLower(name)]; got != want {
					return 0, 0, fmt.Errorf("Policy Condition failed: [\"eq\", \"$%s\", %q]", name, want)
				}
			}
			continue
		}
		var cond []any
		if json.Unmarshal(c, &cond) != nil || len(cond) != 3 {
			return 0, 0, errors.New("policy condition is malformed")
		}
		op, _ := cond[0].(string)
		if strings.EqualFold(op, "content-length-range") {
			lo, _ := cond[1].(float64)
			hi, _ := cond[2].(float64)
			min, max = int64(lo), int64(hi)
			continue
		}
		field, _ := cond[1].(string)
		want, _ := cond[2].(string)
		got := fields[strings.ToLower(strings.TrimPrefix(field, "$"))]
		if (strings.EqualFold(op, "eq") && got != want) || (strings.EqualFold(op, "starts-with") && !strings.HasPrefix(got, want)) {
			return 0, 0, fmt.Errorf("Policy Condition failed: [%q, %q, %q]", op, field, want)
		}
	}
	return min, max, nil
}

// sizeLimit fails the read once more than max bytes arrive, or at EOF when fewer than min did, so the
// store discards the upload instead of publishing it.
type sizeLimit struct {
	r        io.Reader
	n        int64
	min, max int64
}

func (l *sizeLimit) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.n += int64(n)
	if l.n > l.max {
		return n, errTooLarge
	}
	if err == io.EOF && l.n < l.min {
		return n, errTooSmall
	}
	return n, err
}
