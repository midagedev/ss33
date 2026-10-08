package server

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"hash"
	"io"
	"net/http"
	"strings"
)

var (
	errBadDigest     = errors.New("BadDigest")     // the body does not match its Content-MD5 or x-amz-checksum-*
	errInvalidDigest = errors.New("InvalidDigest") // Content-MD5 is not a base64 MD5
)

// digestReader computes the checksums a request declares while its body streams to disk. At EOF it compares
// them with what the client sent, Content-MD5 and an x-amz-checksum-* header or aws-chunked trailer, and
// fails the read on a mismatch, so the store discards the upload as S3 does.
type digestReader struct {
	r       io.Reader
	md5     hash.Hash
	wantMD5 []byte
	alg     string    // requested x-amz-checksum algorithm, or ""
	sum     hash.Hash // its running value, answered in the response
	want    func() string
}

func newDigestReader(r *http.Request) (*digestReader, error) {
	body := requestBody(r)
	d := &digestReader{r: body, alg: requestedChecksum(r)}
	if v := r.Header.Get("Content-MD5"); v != "" {
		raw, err := base64.StdEncoding.DecodeString(v)
		if err != nil || len(raw) != md5.Size {
			return nil, errInvalidDigest
		}
		d.md5, d.wantMD5 = md5.New(), raw
	}
	if newHash, ok := checksumHashes[d.alg]; ok {
		d.sum = newHash()
		header := "X-Amz-Checksum-" + d.alg
		d.want = func() string {
			if v := r.Header.Get(header); v != "" {
				return v
			}
			if c, ok := body.(*chunkedReader); ok {
				return c.trailers[strings.ToLower(header)]
			}
			return ""
		}
	}
	return d, nil
}

func (d *digestReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if d.md5 != nil {
		d.md5.Write(p[:n])
	}
	if d.sum != nil {
		d.sum.Write(p[:n])
	}
	if err == io.EOF {
		if d.md5 != nil && !bytes.Equal(d.md5.Sum(nil), d.wantMD5) {
			return n, errBadDigest
		}
		if want := d.checksumWanted(); want != "" && want != d.checksum() {
			return n, errBadDigest
		}
	}
	return n, err
}

func (d *digestReader) checksumWanted() string {
	if d.want == nil {
		return ""
	}
	return d.want()
}

// checksum is the base64 x-amz-checksum value of what was read, or "" when none was requested.
func (d *digestReader) checksum() string {
	if d.sum == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(d.sum.Sum(nil))
}

// setChecksumHeader answers the requested checksum, computed over the stored bytes.
func (d *digestReader) setChecksumHeader(w http.ResponseWriter) {
	if v := d.checksum(); v != "" {
		w.Header().Set("X-Amz-Checksum-"+d.alg, v)
	}
}
