package server

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"

	"github.com/midagedev/ss33/internal/store"
)

// Server-side encryption is recorded and reported, never applied: ss33's files stay plain. What
// application tests check is the protocol, and that is kept: the headers come back on PUT, GET and HEAD,
// a bucket's default encryption applies to new objects, and an object written with a customer key (SSE-C)
// cannot be read without that key.

const (
	hdrSSE           = "X-Amz-Server-Side-Encryption"
	hdrSSEKMSKey     = "X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"
	hdrSSEBucketKey  = "X-Amz-Server-Side-Encryption-Bucket-Key-Enabled"
	hdrSSECAlgorithm = "X-Amz-Server-Side-Encryption-Customer-Algorithm"
	hdrSSECKey       = "X-Amz-Server-Side-Encryption-Customer-Key"
	hdrSSECKeyMD5    = "X-Amz-Server-Side-Encryption-Customer-Key-Md5"
	hdrStorageClass  = "X-Amz-Storage-Class"
)

var (
	errSSECKeyInvalid  = errors.New("InvalidArgument")     // the customer key does not match its MD5
	errSSECRequired    = errors.New("InvalidRequest")      // an SSE-C object read without its key
	errSSECKeyMismatch = errors.New("AccessDenied")        // an SSE-C object read with another key
	errStorageClass    = errors.New("InvalidStorageClass") // not a storage class S3 knows
	errSSEAlgorithm    = errors.New("InvalidEncryptionAlgorithmError")
)

var storageClasses = map[string]bool{
	"STANDARD": true, "REDUCED_REDUNDANCY": true, "STANDARD_IA": true, "ONEZONE_IA": true, "INTELLIGENT_TIERING": true,
	"GLACIER": true, "DEEP_ARCHIVE": true, "GLACIER_IR": true, "OUTPOSTS": true, "SNOW": true, "EXPRESS_ONEZONE": true,
}

// requestMeta is the metadata of an object a request creates: objectMetaFromRequest plus the storage class
// and encryption it asks for, or the bucket's default encryption.
func (s *Server) requestMeta(r *http.Request, bucket, key string) (store.ObjectMeta, error) {
	meta := objectMetaFromRequest(r, key)
	if sc := r.Header.Get(hdrStorageClass); sc != "" {
		if !storageClasses[sc] {
			return meta, errStorageClass
		}
		if sc != "STANDARD" { // S3 reports a storage class only when it is not STANDARD
			meta.Headers[hdrStorageClass] = sc
		}
	}
	if alg := r.Header.Get(hdrSSECAlgorithm); alg != "" {
		keyMD5, err := customerKeyMD5(r.Header, "")
		if err != nil {
			return meta, err
		}
		if alg != "AES256" {
			return meta, errSSEAlgorithm
		}
		meta.Headers[hdrSSECAlgorithm], meta.Headers[hdrSSECKeyMD5] = alg, keyMD5
		return meta, nil
	}
	if alg := r.Header.Get(hdrSSE); alg != "" {
		if alg != "AES256" && alg != "aws:kms" && alg != "aws:kms:dsse" {
			return meta, errSSEAlgorithm
		}
		for _, h := range []string{hdrSSE, hdrSSEKMSKey, hdrSSEBucketKey} {
			if v := r.Header.Get(h); v != "" {
				meta.Headers[h] = v
			}
		}
		return meta, nil
	}
	if b, err := s.Store.Bucket(bucket); err == nil && b.Configs["encryption"] != "" {
		var cfg struct {
			Rules []struct {
				Default struct {
					SSEAlgorithm   string
					KMSMasterKeyID string
				} `xml:"ApplyServerSideEncryptionByDefault"`
				BucketKeyEnabled bool
			} `xml:"Rule"`
		}
		if xml.Unmarshal([]byte(b.Configs["encryption"]), &cfg) == nil && len(cfg.Rules) > 0 && cfg.Rules[0].Default.SSEAlgorithm != "" {
			rule := cfg.Rules[0]
			meta.Headers[hdrSSE] = rule.Default.SSEAlgorithm
			if rule.Default.KMSMasterKeyID != "" {
				meta.Headers[hdrSSEKMSKey] = rule.Default.KMSMasterKeyID
			}
			if rule.BucketKeyEnabled {
				meta.Headers[hdrSSEBucketKey] = "true"
			}
		}
	}
	return meta, nil
}

// copySourcePrefix turns an SSE-C header name into its copy-source form:
// X-Amz-Copy-Source-Server-Side-Encryption-Customer-*.
const copySourcePrefix = "X-Amz-Copy-Source-"

func sseCHeader(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + strings.TrimPrefix(name, "X-Amz-")
}

// customerKeyMD5 checks an SSE-C key against its declared MD5 and returns the MD5. prefix is "" for the
// object itself and copySourcePrefix for the source of a copy.
func customerKeyMD5(h http.Header, prefix string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(h.Get(sseCHeader(prefix, hdrSSECKey)))
	if err != nil || len(key) != 32 {
		return "", errSSECKeyInvalid
	}
	sum := md5.Sum(key)
	declared, err := base64.StdEncoding.DecodeString(h.Get(sseCHeader(prefix, hdrSSECKeyMD5)))
	if err != nil || !bytes.Equal(declared, sum[:]) {
		return "", errSSECKeyInvalid
	}
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// checkCustomerKey lets a read of an SSE-C object through only with the key it was written with.
func checkCustomerKey(h http.Header, prefix string, meta store.ObjectMeta) error {
	want := meta.Headers[hdrSSECKeyMD5]
	if want == "" {
		return nil
	}
	if h.Get(sseCHeader(prefix, hdrSSECAlgorithm)) == "" {
		return errSSECRequired
	}
	got, err := customerKeyMD5(h, prefix)
	switch {
	case err != nil:
		return err
	case got != want:
		return errSSECKeyMismatch
	}
	return nil
}

// setEncryptionHeaders answers a write with the encryption the object got.
func setEncryptionHeaders(w http.ResponseWriter, meta store.ObjectMeta) {
	for _, h := range []string{hdrSSE, hdrSSEKMSKey, hdrSSEBucketKey, hdrSSECAlgorithm, hdrSSECKeyMD5} {
		if v := meta.Headers[h]; v != "" {
			w.Header().Set(h, v)
		}
	}
}
