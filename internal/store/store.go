// Package store keeps buckets and objects in a plain directory tree.
//
//	<root>/<bucket>/.bucket.json        bucket metadata (creation time, policy)
//	<root>/<bucket>/.meta.log           append-only journal of object metadata (one JSON record per line)
//	<root>/<bucket>/<sha256(key)>.bin   object bytes
//	<root>/<bucket>/<sha256(key)>.parts/ object bytes of a completed multipart upload, one file per part
//	<root>/.uploads/<uploadId>/         in-progress multipart uploads; parts are named p<number>-<md5>
//
// Object files are named by key hash because S3 keys go up to 1024 bytes and may contain both "a" and
// "a/b" — neither fits a filesystem path directly. All metadata lives in memory; the journal only rebuilds
// it on start, so a PUT costs one data file plus one appended line. Completing a multipart upload moves
// the part files into place instead of concatenating them.
//
// By default nothing is fsynced: ss33 is a test double and the OS page cache is the fast path. Durable
// makes every write reach the disk before it is acknowledged.
package store

import (
	"bufio"
	"bytes"
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
	ErrPrecondition     = errors.New("PreconditionFailed")
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
	Parts        []int64           `json:"parts,omitempty"`    // part sizes when the bytes live in <hash>.parts/
	Tags         map[string]string `json:"tags,omitempty"`
	Public       bool              `json:"public,omitempty"` // canned ACL public-read: anonymous GET allowed
}

type BucketMeta struct {
	Name    string            `json:"name"`
	Created time.Time         `json:"created"`
	Policy  string            `json:"policy,omitempty"`
	Configs map[string]string `json:"configs,omitempty"` // subresource (cors, lifecycle, ...) -> XML as the client sent it
}

// PublicRead reports whether the bucket policy grants anonymous s3:GetObject — what `mc anonymous set
// download` writes.
func (b BucketMeta) PublicRead() bool {
	return b.Policy != "" && strings.Contains(b.Policy, "s3:GetObject") &&
		(strings.Contains(b.Policy, `"*"`) || strings.Contains(b.Policy, `"AWS":["*"]`))
}

type Store struct {
	// Durable fsyncs object data, the journal and directory entries before a write is acknowledged.
	Durable bool

	root    string
	mu      sync.RWMutex
	buckets map[string]*bucket
}

type bucket struct {
	meta    BucketMeta
	objects map[string]ObjectMeta
	log     *os.File // .meta.log, opened for append
	records int      // lines in the journal; compaction starts when this far outgrows len(objects)
}

// record is one journal line: either a put (full metadata) or a delete.
type record struct {
	Put *ObjectMeta `json:"put,omitempty"`
	Del string      `json:"del,omitempty"`
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
		b, err := s.loadBucket(e.Name())
		if err != nil {
			return nil, fmt.Errorf("bucket %s: %w", e.Name(), err)
		}
		if b != nil {
			s.buckets[e.Name()] = b
		}
	}
	return s, nil
}

// Close releases the journals.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.buckets {
		b.log.Close()
	}
	return nil
}

func (s *Store) loadBucket(name string) (*bucket, error) {
	dir := s.bucketDir(name)
	b := &bucket{objects: map[string]ObjectMeta{}}
	if readJSON(filepath.Join(dir, ".bucket.json"), &b.meta) != nil {
		return nil, nil // not a bucket directory
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var legacy []string
	for _, f := range files {
		n := f.Name()
		switch {
		case strings.HasPrefix(n, ".put-"), strings.HasPrefix(n, ".mpu-"), strings.Contains(n, ".trash-"), n == ".meta.log.tmp":
			os.RemoveAll(filepath.Join(dir, n)) // left behind by an interrupted write
		case n != ".bucket.json" && strings.HasSuffix(n, ".json"):
			// v0.1 kept one <hash>.json per object; fold them into the journal.
			var m ObjectMeta
			if readJSON(filepath.Join(dir, n), &m) == nil {
				b.objects[m.Key] = m
			}
			legacy = append(legacy, filepath.Join(dir, n))
		}
	}
	if err := replay(filepath.Join(dir, ".meta.log"), b.objects); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := s.compact(name, b); err != nil {
		return nil, err
	}
	for _, p := range legacy {
		os.Remove(p)
	}
	return b, nil
}

// replay applies journal records in order. A torn last line (crash mid-append) is ignored.
func replay(path string, objects map[string]ObjectMeta) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var rec record
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if rec.Put != nil {
			objects[rec.Put.Key] = *rec.Put
		} else if rec.Del != "" {
			delete(objects, rec.Del)
		}
	}
	return sc.Err()
}

// compact rewrites the journal with one record per live object and reopens it for append.
// Callers hold s.mu (or own b exclusively during Open).
func (s *Store) compact(name string, b *bucket) error {
	path := filepath.Join(s.bucketDir(name), ".meta.log")
	var buf bytes.Buffer
	for _, m := range b.objects {
		line, err := json.Marshal(record{Put: &m})
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := s.writeFile(path+".tmp", buf.Bytes()); err != nil {
		return err
	}
	if b.log != nil {
		b.log.Close()
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	log, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	b.log, b.records = log, len(b.objects)
	return s.syncDir(s.bucketDir(name))
}

// appendRecord journals one change and returns the journal to sync once the lock is released, so
// concurrent durable writers share fsyncs instead of queueing behind each other. Callers hold s.mu.
func (s *Store) appendRecord(name string, b *bucket, line []byte) (*os.File, error) {
	if _, err := b.log.Write(line); err != nil {
		return nil, err
	}
	b.records++
	if b.records > 2*len(b.objects)+1024 {
		return nil, s.compact(name, b) // compact syncs the rewritten journal itself
	}
	return b.log, nil
}

// commit makes a journaled change durable after the lock is released. A journal closed by a concurrent
// compaction is fine: compaction rewrote and synced every live record.
func (s *Store) commit(log *os.File, dir string) error {
	if !s.Durable {
		return nil
	}
	if err := s.syncDir(dir); err != nil {
		return err
	}
	if log != nil {
		if err := log.Sync(); err != nil && !errors.Is(err, os.ErrClosed) {
			return err
		}
	}
	return nil
}

func journalLine(rec record) ([]byte, error) {
	line, err := json.Marshal(rec)
	return append(line, '\n'), err
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
	b := &bucket{meta: BucketMeta{Name: name, Created: time.Now().UTC()}, objects: map[string]ObjectMeta{}}
	if err := os.MkdirAll(s.bucketDir(name), 0o755); err != nil {
		return err
	}
	if err := s.writeJSON(filepath.Join(s.bucketDir(name), ".bucket.json"), b.meta); err != nil {
		return err
	}
	if err := s.compact(name, b); err != nil {
		return err
	}
	s.buckets[name] = b
	return s.syncDir(s.root)
}

func (s *Store) DeleteBucket(name string) error {
	s.mu.Lock()
	b, ok := s.buckets[name]
	if !ok {
		s.mu.Unlock()
		return ErrNoSuchBucket
	}
	if len(b.objects) > 0 {
		s.mu.Unlock()
		return ErrBucketNotEmpty
	}
	b.log.Close()
	delete(s.buckets, name)
	trash := filepath.Join(s.root, "."+name+".trash-"+randomID()) // dot-prefixed: Open skips it
	err := os.Rename(s.bucketDir(name), trash)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return os.RemoveAll(trash)
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

// UpdateBucket changes bucket metadata (policy, configurations) and persists it.
func (s *Store) UpdateBucket(name string, update func(*BucketMeta)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[name]
	if !ok {
		return ErrNoSuchBucket
	}
	update(&b.meta)
	return s.writeJSON(filepath.Join(s.bucketDir(name), ".bucket.json"), b.meta)
}

// Precondition guards a write against the object it replaces: If-Match (an ETag) and If-None-Match ("*").
type Precondition struct{ IfMatch, IfNoneMatch string }

func (p Precondition) check(old ObjectMeta, exists bool) error {
	switch {
	case p.IfNoneMatch != "" && exists:
		return ErrPrecondition
	case p.IfMatch != "" && !exists:
		return ErrNoSuchKey
	case p.IfMatch != "" && p.IfMatch != "*" && strings.Trim(p.IfMatch, `"`) != strings.Trim(old.ETag, `"`):
		return ErrPrecondition
	}
	return nil
}

// PutObject streams body to disk, then publishes the metadata. meta.Key/ContentType/Headers/UserMeta come
// from the caller; size, ETag and LastModified are computed here. cond is checked when the object is
// published, under the same lock, so two conditional writers cannot both win.
func (s *Store) PutObject(bucketName string, meta ObjectMeta, body io.Reader, cond Precondition) (ObjectMeta, error) {
	if _, err := s.Bucket(bucketName); err != nil {
		return ObjectMeta{}, err
	}
	tmp, err := os.CreateTemp(s.bucketDir(bucketName), ".put-*")
	if err != nil {
		return ObjectMeta{}, err
	}
	h := md5.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), body)
	if err == nil && s.Durable {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return ObjectMeta{}, err
	}
	meta.Size = n
	meta.ETag = `"` + hex.EncodeToString(h.Sum(nil)) + `"`
	meta.Parts = nil
	out, err := s.publish(bucketName, meta, tmp.Name(), cond)
	if err != nil {
		os.Remove(tmp.Name())
	}
	return out, err
}

// publish moves data (a file, or a directory of parts when meta.Parts is set) into place and journals meta.
// The lock covers only renames and one append; whatever the new version replaces is deleted afterwards.
func (s *Store) publish(bucketName string, meta ObjectMeta, data string, cond Precondition) (ObjectMeta, error) {
	meta.LastModified = time.Now().UTC().Truncate(time.Millisecond)
	line, err := journalLine(record{Put: &meta})
	if err != nil {
		return ObjectMeta{}, err
	}
	s.mu.Lock()
	b, ok := s.buckets[bucketName]
	if !ok {
		s.mu.Unlock()
		return ObjectMeta{}, ErrNoSuchBucket
	}
	old, exists := b.objects[meta.Key]
	if err := cond.check(old, exists); err != nil {
		s.mu.Unlock()
		return ObjectMeta{}, err
	}
	base := objectBase(s.bucketDir(bucketName), meta.Key)
	var garbage string
	if exists && (old.Parts != nil || meta.Parts != nil) {
		garbage = s.unlinkData(base, old) // a file-over-file rename replaces atomically on its own
	}
	target := base + ".bin"
	if meta.Parts != nil {
		target = base + ".parts"
	}
	if err := os.Rename(data, target); err != nil {
		s.mu.Unlock()
		return ObjectMeta{}, err
	}
	log, err := s.appendRecord(bucketName, b, line)
	b.objects[meta.Key] = meta
	s.mu.Unlock()
	if err == nil {
		err = s.commit(log, s.bucketDir(bucketName))
	}
	if garbage != "" {
		os.RemoveAll(garbage)
	}
	return meta, err
}

// unlinkData removes an object's bytes from their visible path. A plain file is unlinked in place (open
// readers keep it); a parts directory is renamed aside and returned so the caller deletes it unlocked.
func (s *Store) unlinkData(base string, m ObjectMeta) (garbage string) {
	if m.Parts == nil {
		os.Remove(base + ".bin")
		return ""
	}
	garbage = base + ".trash-" + randomID()
	if os.Rename(base+".parts", garbage) != nil {
		return ""
	}
	return garbage
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

// Object is an open object body. Single-file objects are an *os.File, so GET keeps the sendfile path.
type Object interface {
	io.ReadSeeker
	io.Closer
}

// OpenObject returns the metadata and the opened body; the caller closes it. Files are opened under the
// read lock, so a concurrent overwrite or delete cannot pull them away mid-read.
func (s *Store) OpenObject(bucketName, key string) (ObjectMeta, Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.buckets[bucketName]
	if !ok {
		return ObjectMeta{}, nil, ErrNoSuchBucket
	}
	m, ok := b.objects[key]
	if !ok {
		return ObjectMeta{}, nil, ErrNoSuchKey
	}
	base := objectBase(s.bucketDir(bucketName), key)
	if m.Parts == nil {
		f, err := os.Open(base + ".bin")
		return m, f, err
	}
	pr, err := openParts(base+".parts", m.Parts)
	return m, pr, err
}

// UpdateObject rewrites an object's metadata (tags, ACL) without touching its bytes.
func (s *Store) UpdateObject(bucketName, key string, update func(*ObjectMeta)) error {
	s.mu.Lock()
	b, ok := s.buckets[bucketName]
	if !ok {
		s.mu.Unlock()
		return ErrNoSuchBucket
	}
	m, ok := b.objects[key]
	if !ok {
		s.mu.Unlock()
		return ErrNoSuchKey
	}
	update(&m)
	line, err := journalLine(record{Put: &m})
	if err != nil {
		s.mu.Unlock()
		return err
	}
	log, err := s.appendRecord(bucketName, b, line)
	b.objects[key] = m
	s.mu.Unlock()
	if err == nil {
		err = s.commit(log, s.bucketDir(bucketName))
	}
	return err
}

// DeleteObject is idempotent like S3: deleting a missing key succeeds.
func (s *Store) DeleteObject(bucketName, key string) error {
	line, err := journalLine(record{Del: key})
	if err != nil {
		return err
	}
	s.mu.Lock()
	b, ok := s.buckets[bucketName]
	if !ok {
		s.mu.Unlock()
		return ErrNoSuchBucket
	}
	old, ok := b.objects[key]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	garbage := s.unlinkData(objectBase(s.bucketDir(bucketName), key), old)
	delete(b.objects, key)
	log, err := s.appendRecord(bucketName, b, line)
	s.mu.Unlock()
	if err == nil {
		err = s.commit(log, s.bucketDir(bucketName))
	}
	if garbage != "" {
		os.RemoveAll(garbage)
	}
	return err
}

// CopyObject copies an object server-side. When replace is nil the source metadata is kept (COPY
// directive). A single-file source is hard-linked, so the copy costs no data I/O.
func (s *Store) CopyObject(srcBucket, srcKey, dstBucket, dstKey string, replace *ObjectMeta) (ObjectMeta, error) {
	src, err := s.HeadObject(srcBucket, srcKey)
	if err != nil {
		return ObjectMeta{}, err
	}
	if _, err := s.Bucket(dstBucket); err != nil {
		return ObjectMeta{}, err
	}
	meta := src
	if replace != nil {
		meta = *replace
	}
	meta.Key = dstKey
	if src.Parts == nil {
		tmp := filepath.Join(s.bucketDir(dstBucket), ".put-"+randomID())
		if os.Link(objectBase(s.bucketDir(srcBucket), srcKey)+".bin", tmp) == nil {
			meta.Size, meta.ETag, meta.Parts = src.Size, src.ETag, nil
			out, err := s.publish(dstBucket, meta, tmp, Precondition{})
			if err != nil {
				os.Remove(tmp)
			}
			return out, err
		}
	}
	_, f, err := s.OpenObject(srcBucket, srcKey)
	if err != nil {
		return ObjectMeta{}, err
	}
	defer f.Close()
	return s.PutObject(dstBucket, meta, f, Precondition{})
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
	id := randomID()
	if err := os.MkdirAll(s.uploadDir(id), 0o755); err != nil {
		return "", err
	}
	return id, s.writeJSON(filepath.Join(s.uploadDir(id), "upload.json"), upload{Bucket: bucketName, Meta: meta, Created: time.Now().UTC()})
}

// Upload is an in-progress multipart upload as ListMultipartUploads reports it.
type Upload struct {
	ID      string
	Key     string
	Created time.Time
}

// ListUploads returns a bucket's in-progress uploads under prefix, by key then start time.
// ponytail: reads every upload.json on each call; index them in memory if a test keeps thousands open.
func (s *Store) ListUploads(bucketName, prefix string) ([]Upload, error) {
	if _, err := s.Bucket(bucketName); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.root, ".uploads"))
	if err != nil {
		return nil, err
	}
	var out []Upload
	for _, e := range entries {
		var u upload
		if readJSON(filepath.Join(s.uploadDir(e.Name()), "upload.json"), &u) != nil || u.Bucket != bucketName || !strings.HasPrefix(u.Meta.Key, prefix) {
			continue
		}
		out = append(out, Upload{ID: e.Name(), Key: u.Meta.Key, Created: u.Created})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Created.Before(out[j].Created)
	})
	return out, nil
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

// PutPart stores a part as p<number>-<md5>, so listing and completing never re-read part data.
func (s *Store) PutPart(bucketName, key, id string, number int, body io.Reader) (string, error) {
	if _, err := s.loadUpload(bucketName, key, id); err != nil {
		return "", err
	}
	dir := s.uploadDir(id)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	h := md5.New()
	_, err = io.Copy(io.MultiWriter(f, h), body)
	if err == nil && s.Durable {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	prefix := fmt.Sprintf("p%05d-", number)
	if old, _ := filepath.Glob(filepath.Join(dir, prefix+"*")); len(old) > 0 {
		for _, p := range old {
			os.Remove(p)
		}
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, prefix+sum)); err != nil {
		return "", err
	}
	return `"` + sum + `"`, s.syncDir(dir)
}

type storedPart struct {
	Part
	file string
}

func (s *Store) parts(id string) (map[int]storedPart, error) {
	entries, err := os.ReadDir(s.uploadDir(id))
	if err != nil {
		return nil, err
	}
	out := map[int]storedPart{}
	for _, e := range entries {
		num, sum, ok := strings.Cut(strings.TrimPrefix(e.Name(), "p"), "-")
		if !ok || !strings.HasPrefix(e.Name(), "p") {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // replaced while listing
		}
		out[n] = storedPart{Part{Number: n, ETag: `"` + sum + `"`, Size: info.Size()}, filepath.Join(s.uploadDir(id), e.Name())}
	}
	return out, nil
}

func (s *Store) ListParts(bucketName, key, id string) ([]Part, error) {
	if _, err := s.loadUpload(bucketName, key, id); err != nil {
		return nil, err
	}
	stored, err := s.parts(id)
	if err != nil {
		return nil, err
	}
	out := make([]Part, 0, len(stored))
	for _, p := range stored {
		out = append(out, p.Part)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}

// CompleteUpload moves the requested parts into the object's parts directory — no bytes are copied. The
// resulting ETag follows S3: md5(concat(md5(part)...)) + "-" + count.
func (s *Store) CompleteUpload(bucketName, key, id string, requested []Part) (ObjectMeta, error) {
	u, err := s.loadUpload(bucketName, key, id)
	if err != nil {
		return ObjectMeta{}, err
	}
	stored, err := s.parts(id)
	if err != nil {
		return ObjectMeta{}, err
	}
	last := 0
	for _, p := range requested {
		if p.Number <= last {
			return ObjectMeta{}, ErrInvalidPartOrder
		}
		last = p.Number
		if sp, ok := stored[p.Number]; !ok || sp.ETag != `"`+strings.Trim(p.ETag, `"`)+`"` {
			return ObjectMeta{}, ErrInvalidPart
		}
	}
	dir := filepath.Join(s.bucketDir(bucketName), ".mpu-"+id)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return ObjectMeta{}, err
	}
	etags := md5.New()
	meta := u.Meta
	meta.Size, meta.Parts = 0, make([]int64, 0, len(requested))
	for i, p := range requested {
		sp := stored[p.Number]
		if err := os.Rename(sp.file, filepath.Join(dir, fmt.Sprintf("%05d", i))); err != nil {
			os.RemoveAll(dir)
			return ObjectMeta{}, err
		}
		raw, _ := hex.DecodeString(strings.Trim(sp.ETag, `"`))
		etags.Write(raw)
		meta.Size += sp.Size
		meta.Parts = append(meta.Parts, sp.Size)
	}
	if err := s.syncDir(dir); err != nil {
		os.RemoveAll(dir)
		return ObjectMeta{}, err
	}
	meta.ETag = fmt.Sprintf(`"%s-%d"`, hex.EncodeToString(etags.Sum(nil)), len(requested))
	out, err := s.publish(bucketName, meta, dir, Precondition{})
	if err != nil {
		os.RemoveAll(dir)
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

// partsReader reads a multipart object straight from its part files.
type partsReader struct {
	files []*os.File
	ends  []int64 // cumulative end offset of each part
	off   int64
}

func openParts(dir string, sizes []int64) (*partsReader, error) {
	p := &partsReader{}
	var end int64
	for i, size := range sizes {
		f, err := os.Open(filepath.Join(dir, fmt.Sprintf("%05d", i)))
		if err != nil {
			p.Close()
			return nil, err
		}
		end += size
		p.files = append(p.files, f)
		p.ends = append(p.ends, end)
	}
	return p, nil
}

func (p *partsReader) size() int64 {
	if len(p.ends) == 0 {
		return 0
	}
	return p.ends[len(p.ends)-1]
}

func (p *partsReader) Read(b []byte) (int, error) {
	if p.off >= p.size() {
		return 0, io.EOF
	}
	i := sort.Search(len(p.ends), func(i int) bool { return p.ends[i] > p.off })
	start := int64(0)
	if i > 0 {
		start = p.ends[i-1]
	}
	if left := p.ends[i] - p.off; int64(len(b)) > left {
		b = b[:left]
	}
	n, err := p.files[i].ReadAt(b, p.off-start)
	p.off += int64(n)
	if err == io.EOF {
		if n < len(b) {
			return n, io.ErrUnexpectedEOF // part file shorter than recorded
		}
		err = nil
	}
	return n, err
}

func (p *partsReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += p.off
	case io.SeekEnd:
		offset += p.size()
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	p.off = offset
	return offset, nil
}

func (p *partsReader) Close() error {
	for _, f := range p.files {
		f.Close()
	}
	return nil
}

// --- helpers ---

func randomID() string {
	buf := make([]byte, 16)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

func (s *Store) writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := s.writeFile(path+".tmp", data); err != nil {
		return err
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return err
	}
	return s.syncDir(filepath.Dir(path))
}

func (s *Store) writeFile(path string, data []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil && s.Durable {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// syncDir makes renames and new entries in dir durable. A no-op unless Durable.
func (s *Store) syncDir(dir string) error {
	if !s.Durable {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
