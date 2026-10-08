// Package store keeps buckets and objects in a plain directory tree.
//
//	<root>/<bucket>/.bucket.json      bucket metadata (creation time, policy)
//	<root>/<bucket>/<sha256(key)>.bin object bytes
//	<root>/<bucket>/<sha256(key)>.json object metadata (key, size, etag, headers)
//	<root>/.uploads/<uploadId>/...    in-progress multipart uploads
//
// Object files are named by key hash because S3 keys go up to 1024 bytes and may contain both "a" and
// "a/b" — neither fits a filesystem path directly. An in-memory index answers listings.
package store

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrNoSuchBucket     = errors.New("NoSuchBucket")
	ErrBucketExists     = errors.New("BucketAlreadyOwnedByYou")
	ErrBucketNotEmpty   = errors.New("BucketNotEmpty")
	ErrNoSuchKey        = errors.New("NoSuchKey")
	ErrInvalidBucket    = errors.New("InvalidBucketName")
	ErrNoSuchUpload     = errors.New("NoSuchUpload")
	ErrInvalidPart      = errors.New("InvalidPart")
	ErrInvalidPartOrder = errors.New("InvalidPartOrder")
)

var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

type ObjectMeta struct {
	Key          string            `json:"key"`
	Size         int64             `json:"size"`
	ETag         string            `json:"etag"` // quoted, as S3 returns it
	LastModified time.Time         `json:"lastModified"`
	ContentType  string            `json:"contentType,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`  // Content-Disposition, Cache-Control, ...
	UserMeta     map[string]string `json:"userMeta,omitempty"` // x-amz-meta-* without the prefix, lowercased
}

type BucketMeta struct {
	Name    string    `json:"name"`
	Created time.Time `json:"created"`
	Policy  string    `json:"policy,omitempty"`
}

// PublicRead reports whether the bucket policy grants anonymous s3:GetObject — what `mc anonymous set
// download` writes.
func (b BucketMeta) PublicRead() bool {
	return b.Policy != "" && strings.Contains(b.Policy, "s3:GetObject") &&
		(strings.Contains(b.Policy, `"*"`) || strings.Contains(b.Policy, `"AWS":["*"]`))
}

type Store struct {
	root    string
	mu      sync.RWMutex
	buckets map[string]*bucket
}

type bucket struct {
	meta    BucketMeta
	objects map[string]ObjectMeta
}

func Open(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, ".uploads"), 0o755); err != nil {
		return nil, err
	}
	s := &Store{root: root, buckets: map[string]*bucket{}}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b := &bucket{objects: map[string]ObjectMeta{}}
		if err := readJSON(filepath.Join(root, e.Name(), ".bucket.json"), &b.meta); err != nil {
			continue
		}
		files, _ := os.ReadDir(filepath.Join(root, e.Name()))
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") || f.Name() == ".bucket.json" {
				continue
			}
			var m ObjectMeta
			if readJSON(filepath.Join(root, e.Name(), f.Name()), &m) == nil {
				b.objects[m.Key] = m
			}
		}
		s.buckets[e.Name()] = b
	}
	return s, nil
}

func (s *Store) bucketDir(name string) string { return filepath.Join(s.root, name) }

func objectBase(bucketDir, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(bucketDir, hex.EncodeToString(sum[:]))
}

func (s *Store) CreateBucket(name string) error {
	if !bucketNameRe.MatchString(name) || strings.Contains(name, "..") {
		return ErrInvalidBucket
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.buckets[name]; ok {
		return ErrBucketExists
	}
	meta := BucketMeta{Name: name, Created: time.Now().UTC()}
	if err := os.MkdirAll(s.bucketDir(name), 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(s.bucketDir(name), ".bucket.json"), meta); err != nil {
		return err
	}
	s.buckets[name] = &bucket{meta: meta, objects: map[string]ObjectMeta{}}
	return nil
}

func (s *Store) DeleteBucket(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return ErrNoSuchBucket
	}
	if len(b.objects) > 0 {
		return ErrBucketNotEmpty
	}
	delete(s.buckets, name)
	return os.RemoveAll(s.bucketDir(name))
}

func (s *Store) Bucket(name string) (BucketMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.buckets[name]
	if !ok {
		return BucketMeta{}, ErrNoSuchBucket
	}
	return b.meta, nil
}

func (s *Store) ListBuckets() []BucketMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]BucketMeta, 0, len(s.buckets))
	for _, b := range s.buckets {
		out = append(out, b.meta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) SetPolicy(name, policy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return ErrNoSuchBucket
	}
	b.meta.Policy = policy
	return writeJSON(filepath.Join(s.bucketDir(name), ".bucket.json"), b.meta)
}

// PutObject streams body to disk, then publishes the metadata. meta.Key/ContentType/Headers/UserMeta come
// from the caller; size, ETag and LastModified are computed here.
func (s *Store) PutObject(bucketName string, meta ObjectMeta, body io.Reader) (ObjectMeta, error) {
	if _, err := s.Bucket(bucketName); err != nil {
		return ObjectMeta{}, err
	}
	dir := s.bucketDir(bucketName)
	tmp, err := os.CreateTemp(dir, ".put-*")
	if err != nil {
		return ObjectMeta{}, err
	}
	defer os.Remove(tmp.Name())
	h := md5.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return ObjectMeta{}, err
	}
	meta.Size = n
	meta.ETag = `"` + hex.EncodeToString(h.Sum(nil)) + `"`
	return s.publish(bucketName, meta, tmp.Name())
}

func (s *Store) publish(bucketName string, meta ObjectMeta, dataPath string) (ObjectMeta, error) {
	meta.LastModified = time.Now().UTC().Truncate(time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return ObjectMeta{}, ErrNoSuchBucket
	}
	base := objectBase(s.bucketDir(bucketName), meta.Key)
	if err := os.Rename(dataPath, base+".bin"); err != nil {
		return ObjectMeta{}, err
	}
	if err := writeJSON(base+".json", meta); err != nil {
		return ObjectMeta{}, err
	}
	b.objects[meta.Key] = meta
	return meta, nil
}

func (s *Store) HeadObject(bucketName, key string) (ObjectMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return ObjectMeta{}, ErrNoSuchBucket
	}
	m, ok := b.objects[key]
	if !ok {
		return ObjectMeta{}, ErrNoSuchKey
	}
	return m, nil
}

// OpenObject returns the metadata and an open file; the caller closes it.
func (s *Store) OpenObject(bucketName, key string) (ObjectMeta, *os.File, error) {
	m, err := s.HeadObject(bucketName, key)
	if err != nil {
		return m, nil, err
	}
	f, err := os.Open(objectBase(s.bucketDir(bucketName), key) + ".bin")
	if err != nil {
		return m, nil, err
	}
	return m, f, nil
}

// DeleteObject is idempotent like S3: deleting a missing key succeeds.
func (s *Store) DeleteObject(bucketName, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return ErrNoSuchBucket
	}
	if _, ok := b.objects[key]; !ok {
		return nil
	}
	base := objectBase(s.bucketDir(bucketName), key)
	os.Remove(base + ".bin")
	os.Remove(base + ".json")
	delete(b.objects, key)
	return nil
}

// CopyObject copies bytes server-side. When replace is nil the source metadata is kept (COPY directive).
func (s *Store) CopyObject(srcBucket, srcKey, dstBucket, dstKey string, replace *ObjectMeta) (ObjectMeta, error) {
	src, f, err := s.OpenObject(srcBucket, srcKey)
	if err != nil {
		return ObjectMeta{}, err
	}
	defer f.Close()
	meta := src
	if replace != nil {
		meta = *replace
	}
	meta.Key = dstKey
	return s.PutObject(dstBucket, meta, f)
}

type ListResult struct {
	Objects        []ObjectMeta
	CommonPrefixes []string
	Truncated      bool
	NextMarker     string // last key or prefix returned; continue after it
}

// List returns keys after `after` that start with prefix, rolling keys up to the delimiter into common
// prefixes. Objects and prefixes both count towards max, as in S3.
func (s *Store) List(bucketName, prefix, delimiter, after string, max int) (ListResult, error) {
	s.mu.RLock()
	b, ok := s.buckets[bucketName]
	if !ok {
		s.mu.RUnlock()
		return ListResult{}, ErrNoSuchBucket
	}
	metas := make([]ObjectMeta, 0, len(b.objects))
	for k, m := range b.objects {
		if strings.HasPrefix(k, prefix) && k > after {
			metas = append(metas, m)
		}
	}
	s.mu.RUnlock()
	sort.Slice(metas, func(i, j int) bool { return metas[i].Key < metas[j].Key })

	var res ListResult
	seen := map[string]bool{}
	for _, m := range metas {
		k := m.Key
		if delimiter != "" {
			if i := strings.Index(k[len(prefix):], delimiter); i >= 0 {
				cp := k[:len(prefix)+i+len(delimiter)]
				if seen[cp] || cp <= after {
					continue
				}
				if len(res.Objects)+len(res.CommonPrefixes) >= max {
					res.Truncated = true
					break
				}
				seen[cp] = true
				res.CommonPrefixes = append(res.CommonPrefixes, cp)
				res.NextMarker = cp
				continue
			}
		}
		if len(res.Objects)+len(res.CommonPrefixes) >= max {
			res.Truncated = true
			break
		}
		res.Objects = append(res.Objects, m)
		res.NextMarker = k
	}
	// A truncated listing that ended on a common prefix must resume after every key under that prefix.
	if res.Truncated && strings.HasSuffix(res.NextMarker, delimiter) && delimiter != "" {
		res.NextMarker += "\xff"
	}
	return res, nil
}

// --- multipart ---

type upload struct {
	Bucket  string     `json:"bucket"`
	Meta    ObjectMeta `json:"meta"`
	Created time.Time  `json:"created"`
}

type Part struct {
	Number int
	ETag   string
	Size   int64
}

func (s *Store) uploadDir(id string) string { return filepath.Join(s.root, ".uploads", id) }

func (s *Store) CreateUpload(bucketName string, meta ObjectMeta) (string, error) {
	if _, err := s.Bucket(bucketName); err != nil {
		return "", err
	}
	buf := make([]byte, 16)
	rand.Read(buf)
	id := hex.EncodeToString(buf)
	if err := os.MkdirAll(s.uploadDir(id), 0o755); err != nil {
		return "", err
	}
	return id, writeJSON(filepath.Join(s.uploadDir(id), "upload.json"), upload{Bucket: bucketName, Meta: meta, Created: time.Now().UTC()})
}

func (s *Store) loadUpload(bucketName, key, id string) (upload, error) {
	var u upload
	if strings.ContainsAny(id, `/\.`) || readJSON(filepath.Join(s.uploadDir(id), "upload.json"), &u) != nil {
		return u, ErrNoSuchUpload
	}
	if u.Bucket != bucketName || u.Meta.Key != key {
		return u, ErrNoSuchUpload
	}
	return u, nil
}

func (s *Store) PutPart(bucketName, key, id string, number int, body io.Reader) (string, error) {
	if _, err := s.loadUpload(bucketName, key, id); err != nil {
		return "", err
	}
	f, err := os.Create(filepath.Join(s.uploadDir(id), fmt.Sprintf("part-%05d", number)))
	if err != nil {
		return "", err
	}
	h := md5.New()
	_, err = io.Copy(io.MultiWriter(f, h), body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`, nil
}

func (s *Store) ListParts(bucketName, key, id string) ([]Part, error) {
	if _, err := s.loadUpload(bucketName, key, id); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.uploadDir(id))
	if err != nil {
		return nil, err
	}
	var parts []Part
	for _, e := range entries {
		num, ok := strings.CutPrefix(e.Name(), "part-")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(num)
		etag, size, err := fileMD5(filepath.Join(s.uploadDir(id), e.Name()))
		if err != nil {
			return nil, err
		}
		parts = append(parts, Part{Number: n, ETag: etag, Size: size})
	}
	return parts, nil
}

// CompleteUpload concatenates the requested parts in order. The resulting ETag follows S3:
// md5(concat(md5(part)...)) + "-" + count.
func (s *Store) CompleteUpload(bucketName, key, id string, requested []Part) (ObjectMeta, error) {
	u, err := s.loadUpload(bucketName, key, id)
	if err != nil {
		return ObjectMeta{}, err
	}
	tmp, err := os.CreateTemp(s.bucketDir(bucketName), ".mpu-*")
	if err != nil {
		return ObjectMeta{}, err
	}
	defer os.Remove(tmp.Name())
	etags := md5.New()
	var size int64
	last := 0
	for _, p := range requested {
		if p.Number <= last {
			tmp.Close()
			return ObjectMeta{}, ErrInvalidPartOrder
		}
		last = p.Number
		path := filepath.Join(s.uploadDir(id), fmt.Sprintf("part-%05d", p.Number))
		etag, _, err := fileMD5(path)
		if err != nil || strings.Trim(etag, `"`) != strings.Trim(p.ETag, `"`) {
			tmp.Close()
			return ObjectMeta{}, ErrInvalidPart
		}
		raw, _ := hex.DecodeString(strings.Trim(etag, `"`))
		etags.Write(raw)
		f, err := os.Open(path)
		if err != nil {
			tmp.Close()
			return ObjectMeta{}, err
		}
		n, err := io.Copy(tmp, f)
		f.Close()
		if err != nil {
			tmp.Close()
			return ObjectMeta{}, err
		}
		size += n
	}
	if err := tmp.Close(); err != nil {
		return ObjectMeta{}, err
	}
	meta := u.Meta
	meta.Size = size
	meta.ETag = fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(etags.Sum(nil)), len(requested))
	out, err := s.publish(bucketName, meta, tmp.Name())
	if err != nil {
		return ObjectMeta{}, err
	}
	os.RemoveAll(s.uploadDir(id))
	return out, nil
}

func (s *Store) AbortUpload(bucketName, key, id string) error {
	if _, err := s.loadUpload(bucketName, key, id); err != nil {
		return err
	}
	return os.RemoveAll(s.uploadDir(id))
}

// --- helpers ---

func fileMD5(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := md5.New()
	n, err := io.Copy(h, f)
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`, n, err
}

func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
