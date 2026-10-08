// Package store keeps buckets and objects in a plain directory tree.
//
//	<root>/<bucket>/.bucket.json        bucket metadata (creation time, policy)
//	<root>/<bucket>/.meta.log           append-only journal of object metadata (one JSON record per line)
//	<root>/<bucket>/<sha256(key)>.bin   object bytes
//	<root>/<bucket>/<sha256(key)>.parts/ object bytes of a completed multipart upload, one file per part
//	<root>/<bucket>/<sha256(key)>.v-<versionId>.bin (or .parts/) bytes of a noncurrent version
//	<root>/.uploads/<uploadId>/         in-progress multipart uploads; parts are named p<number>-<md5>
//
// Object files are named by key hash because S3 keys go up to 1024 bytes and may contain both "a" and
// "a/b" — neither fits a filesystem path directly. All metadata lives in memory; the journal only rebuilds
// it on start, so a PUT costs one data file plus one appended line. Completing a multipart upload moves
// the part files into place instead of concatenating them.
//
// In a bucket with versioning, the current version keeps the plain name, so reading it costs the same as in
// any other bucket; an overwrite or a delete marker renames the replaced bytes to the version's name.
//
// By default nothing is fsynced: ss33 is a test double and the OS page cache is the fast path. Durable
// makes every write reach the disk before it is acknowledged.
package store

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	ErrDeleteMarker     = errors.New("MethodNotAllowed") // the requested version is a delete marker
	ErrNoSuchVersion    = errors.New("NoSuchVersion")
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
	Public       bool              `json:"public,omitempty"`       // canned ACL public-read: anonymous GET allowed
	VersionID    string            `json:"versionId,omitempty"`    // "" is the null version
	DeleteMarker bool              `json:"deleteMarker,omitempty"` // a delete marker has no bytes
}

type BucketMeta struct {
	Name    string            `json:"name"`
	Created time.Time         `json:"created"`
	Policy  string            `json:"policy,omitempty"`
	Configs map[string]string `json:"configs,omitempty"` // subresource (cors, lifecycle, ...) -> XML as the client sent it
}

type Store struct {
	// Durable fsyncs object data, the journal and directory entries before a write is acknowledged.
	Durable bool

	root    string
	mu      sync.RWMutex
	buckets map[string]*bucket
}

type bucket struct {
	meta       BucketMeta
	versioning string                  // "", "Enabled" or "Suspended", from the stored versioning configuration
	objects    map[string]ObjectMeta   // the current version of each key, delete markers included
	versions   map[string][]ObjectMeta // noncurrent versions, newest first; only keys that have any
	noncurrent int                     // total entries in versions
	log        *os.File                // .meta.log, opened for append
	records    int                     // lines in the journal; compaction starts when this far outgrows the live versions
}

// record is one journal line, a change replayed by apply: put sets a key's current version, del removes a
// key with every version, retire makes the current version noncurrent, drop removes one version (the
// newest noncurrent one becomes current when the current one goes), upd rewrites one version's metadata,
// and vers sets a key's whole version list (written only by compaction). Each change is one short line, so
// a key with many versions costs no more to write than any other.
type record struct {
	Put    *ObjectMeta  `json:"put,omitempty"`
	Del    string       `json:"del,omitempty"`
	Retire string       `json:"retire,omitempty"`
	Drop   *versionRef  `json:"drop,omitempty"`
	Upd    *ObjectMeta  `json:"upd,omitempty"`
	Vers   *keyVersions `json:"vers,omitempty"`
}

type versionRef struct {
	Key     string `json:"key"`
	Version string `json:"version"` // as in matchesVersion: "null" is the null version
}

type keyVersions struct {
	Key  string       `json:"key"`
	List []ObjectMeta `json:"list"`
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
	b := &bucket{objects: map[string]ObjectMeta{}, versions: map[string][]ObjectMeta{}}
	if readJSON(filepath.Join(dir, ".bucket.json"), &b.meta) != nil {
		return nil, nil // not a bucket directory
	}
	b.versioning = versioningStatus(b.meta.Configs["versioning"])
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
	if err := replay(filepath.Join(dir, ".meta.log"), b); err != nil && !errors.Is(err, os.ErrNotExist) {
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

// replay applies journal records in order. A torn last line (crash mid-append) ends the replay.
func replay(path string, b *bucket) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := json.NewDecoder(bufio.NewReader(f)) // no line-length limit: a record holds a whole version list
	for {
		var rec record
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			var syntax *json.SyntaxError
			if errors.As(err, &syntax) {
				return nil // a torn tail; compaction rewrites the journal right after
			}
			return err
		}
		b.apply(rec)
	}
}

// apply makes one journal record's change in memory. Live writes and replay both go through it.
func (b *bucket) apply(rec record) {
	switch {
	case rec.Put != nil:
		b.objects[rec.Put.Key] = *rec.Put
	case rec.Del != "":
		delete(b.objects, rec.Del)
		b.noncurrent -= len(b.versions[rec.Del])
		delete(b.versions, rec.Del)
	case rec.Retire != "":
		if cur, ok := b.objects[rec.Retire]; ok {
			delete(b.objects, rec.Retire)
			b.versions[rec.Retire] = append([]ObjectMeta{cur}, b.versions[rec.Retire]...)
			b.noncurrent++
		}
	case rec.Drop != nil:
		k := rec.Drop.Key
		if cur, ok := b.objects[k]; ok && matchesVersion(cur, rec.Drop.Version) {
			delete(b.objects, k)
			if list := b.versions[k]; len(list) > 0 {
				b.objects[k] = list[0]
				b.setNoncurrent(k, list[1:])
			}
			return
		}
		for i, v := range b.versions[k] {
			if matchesVersion(v, rec.Drop.Version) {
				b.setNoncurrent(k, slices.Delete(slices.Clone(b.versions[k]), i, i+1))
				return
			}
		}
	case rec.Upd != nil:
		k := rec.Upd.Key
		if cur, ok := b.objects[k]; ok && cur.VersionID == rec.Upd.VersionID {
			b.objects[k] = *rec.Upd
			return
		}
		for i, v := range b.versions[k] {
			if v.VersionID == rec.Upd.VersionID {
				b.versions[k][i] = *rec.Upd
			}
		}
	case rec.Vers != nil:
		k := rec.Vers.Key
		delete(b.objects, k)
		b.setNoncurrent(k, nil)
		if len(rec.Vers.List) > 0 {
			b.objects[k] = rec.Vers.List[0]
			b.setNoncurrent(k, rec.Vers.List[1:])
		}
	}
}

func (b *bucket) setNoncurrent(key string, list []ObjectMeta) {
	b.noncurrent += len(list) - len(b.versions[key])
	if len(list) == 0 {
		delete(b.versions, key)
		return
	}
	b.versions[key] = list
}

// compact rewrites the journal with one record per live key and reopens it for append.
// Callers hold s.mu (or own b exclusively during Open).
func (s *Store) compact(name string, b *bucket) error {
	path := filepath.Join(s.bucketDir(name), ".meta.log")
	var buf bytes.Buffer
	for k, m := range b.objects {
		rec := record{Put: &m}
		if len(b.versions[k]) > 0 {
			rec = record{Vers: &keyVersions{Key: k, List: append([]ObjectMeta{m}, b.versions[k]...)}}
		}
		line, err := json.Marshal(rec)
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

// appendRecord journals lines (one record each) and returns the journal to sync once the lock is released,
// so concurrent durable writers share fsyncs instead of queueing behind each other. Callers hold s.mu.
func (s *Store) appendRecord(name string, b *bucket, lines []byte) (*os.File, error) {
	if _, err := b.log.Write(lines); err != nil {
		return nil, err
	}
	b.records += bytes.Count(lines, []byte{'\n'})
	if b.records > 2*(len(b.objects)+b.noncurrent)+1024 {
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

// versioningStatus reads Enabled or Suspended from a stored VersioningConfiguration.
func versioningStatus(config string) string {
	var v struct{ Status string }
	if config == "" || xml.Unmarshal([]byte(config), &v) != nil {
		return ""
	}
	if v.Status == "Enabled" || v.Status == "Suspended" {
		return v.Status
	}
	return ""
}

// matchesVersion reports whether m is the version a request names: "null" is the null version.
func matchesVersion(m ObjectMeta, versionID string) bool {
	return m.VersionID == versionID || versionID == "null" && m.VersionID == ""
}

// currentPath and versionPath are where a version's bytes live while it is current or noncurrent.
func currentPath(base string, m ObjectMeta) string {
	if m.Parts != nil {
		return base + ".parts"
	}
	return base + ".bin"
}

func versionPath(base string, m ObjectMeta) string {
	id := cmp.Or(m.VersionID, "null")
	if m.Parts != nil {
		return base + ".v-" + id + ".parts"
	}
	return base + ".v-" + id + ".bin"
}

// change collects the journal records of one write, applying each in memory as it is added, and the
// paths to delete once the lock is released.
type change struct {
	recs    []record
	garbage []string
}

func (b *bucket) do(c *change, rec record) {
	b.apply(rec)
	c.recs = append(c.recs, rec)
}

// retireCurrent makes the current version of key noncurrent: its bytes move to the version's name.
// Callers hold s.mu.
func (b *bucket) retireCurrent(c *change, base, key string) error {
	old, ok := b.objects[key]
	if !ok {
		return nil
	}
	if !old.DeleteMarker {
		if err := os.Rename(currentPath(base, old), versionPath(base, old)); err != nil {
			return err
		}
	}
	b.do(c, record{Retire: key})
	return nil
}

// dropVersion removes one version of key ("null" for the null version) and its bytes. When it is the
// current one, the newest noncurrent version takes its place. Callers hold s.mu.
func (b *bucket) dropVersion(c *change, base, key, versionID string, noncurrentOnly bool) (removed ObjectMeta, found bool, err error) {
	if cur, ok := b.objects[key]; ok && !noncurrentOnly && matchesVersion(cur, versionID) {
		if next := b.versions[key]; len(next) > 0 && !next[0].DeleteMarker {
			if err := os.Rename(versionPath(base, next[0]), currentPath(base, next[0])+".promote"); err != nil {
				return cur, true, err
			}
		}
		if !cur.DeleteMarker {
			c.garbage = append(c.garbage, discard(currentPath(base, cur), cur.Parts != nil))
		}
		if next := b.versions[key]; len(next) > 0 && !next[0].DeleteMarker {
			if err := os.Rename(currentPath(base, next[0])+".promote", currentPath(base, next[0])); err != nil {
				return cur, true, err
			}
		}
		b.do(c, record{Drop: &versionRef{key, versionID}})
		return cur, true, nil
	}
	for _, v := range b.versions[key] {
		if matchesVersion(v, versionID) {
			if !v.DeleteMarker {
				c.garbage = append(c.garbage, discard(versionPath(base, v), v.Parts != nil))
			}
			b.do(c, record{Drop: &versionRef{key, versionID}})
			return v, true, nil
		}
	}
	return ObjectMeta{}, false, nil
}

// discard unlinks a version's bytes: a file at once, a parts directory by renaming it aside and returning
// the new name for the caller to remove after releasing the lock.
func discard(path string, parts bool) (garbage string) {
	if !parts {
		os.Remove(path)
		return ""
	}
	garbage = path + ".trash-" + randomID()
	if os.Rename(path, garbage) != nil {
		return ""
	}
	return garbage
}

// journal appends a change's records, releases s.mu (which the caller holds), makes the change durable and
// removes its garbage.
func (s *Store) journal(bucketName string, b *bucket, c *change, err error) error {
	var log *os.File
	if err == nil && len(c.recs) > 0 {
		var lines []byte
		for _, rec := range c.recs {
			line, merr := journalLine(rec)
			if merr != nil {
				err = merr
				break
			}
			lines = append(lines, line...)
		}
		if err == nil {
			log, err = s.appendRecord(bucketName, b, lines)
		}
	}
	s.mu.Unlock()
	if err == nil {
		err = s.commit(log, s.bucketDir(bucketName))
	}
	for _, g := range c.garbage {
		if g != "" {
			os.RemoveAll(g)
		}
	}
	return err
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
	b := &bucket{meta: BucketMeta{Name: name, Created: time.Now().UTC()}, objects: map[string]ObjectMeta{}, versions: map[string][]ObjectMeta{}}
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
	// Copy on write: Bucket hands out b.meta, and its readers must never see the map change under them.
	b.meta.Configs = maps.Clone(b.meta.Configs)
	update(&b.meta)
	b.versioning = versioningStatus(b.meta.Configs["versioning"])
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
// With versioning enabled the replaced version is kept under its version name instead; suspended
// versioning replaces only the null version.
func (s *Store) publish(bucketName string, meta ObjectMeta, data string, cond Precondition) (ObjectMeta, error) {
	meta.LastModified = time.Now().UTC().Truncate(time.Millisecond)
	meta.VersionID, meta.DeleteMarker = "", false
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
	if err := cond.check(old, exists && !old.DeleteMarker); err != nil {
		s.mu.Unlock()
		return ObjectMeta{}, err
	}
	base := objectBase(s.bucketDir(bucketName), meta.Key)
	c := &change{}
	switch {
	case b.versioning == "Enabled":
		meta.VersionID = randomID()
		err = b.retireCurrent(c, base, meta.Key)
	case b.versioning == "Suspended" && exists && old.VersionID != "":
		err = b.retireCurrent(c, base, meta.Key)
	case exists && !old.DeleteMarker && (old.Parts != nil || meta.Parts != nil):
		c.garbage = append(c.garbage, s.unlinkData(base, old)) // a file-over-file rename replaces atomically on its own
	}
	if err == nil && b.versioning == "Suspended" {
		_, _, err = b.dropVersion(c, base, meta.Key, "null", true) // the null version is replaced, wherever it is
	}
	if err == nil {
		err = os.Rename(data, currentPath(base, meta))
	}
	if err != nil {
		return ObjectMeta{}, s.journal(bucketName, b, c, err)
	}
	if b.versioning == "" {
		// The common case: one put record, marshalled before the lock was taken.
		b.apply(record{Put: &meta})
		log, err := s.appendRecord(bucketName, b, line)
		s.mu.Unlock()
		if err == nil {
			err = s.commit(log, s.bucketDir(bucketName))
		}
		for _, g := range c.garbage {
			if g != "" {
				os.RemoveAll(g)
			}
		}
		return meta, err
	}
	b.do(c, record{Put: &meta})
	return meta, s.journal(bucketName, b, c, nil)
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

// HeadObject returns the current version of key. A current delete marker is ErrNoSuchKey, returned with
// the marker's metadata.
func (s *Store) HeadObject(bucketName, key string) (ObjectMeta, error) {
	return s.Version(bucketName, key, "")
}

// Version returns one version of key: "" for the current one, "null" for the null version. A delete
// marker named by its version ID is ErrDeleteMarker, returned with the marker's metadata.
func (s *Store) Version(bucketName, key, versionID string) (ObjectMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, _, err := s.find(bucketName, key, versionID)
	return m, err
}

// find locates a version and the path of its bytes. Callers hold s.mu.
func (s *Store) find(bucketName, key, versionID string) (ObjectMeta, string, error) {
	b, ok := s.buckets[bucketName]
	if !ok {
		return ObjectMeta{}, "", ErrNoSuchBucket
	}
	base := objectBase(s.bucketDir(bucketName), key)
	m, ok := b.objects[key]
	switch {
	case versionID == "" && ok && m.DeleteMarker:
		return m, "", ErrNoSuchKey
	case versionID == "" && ok, ok && matchesVersion(m, versionID):
		if m.DeleteMarker {
			return m, "", ErrDeleteMarker
		}
		return m, currentPath(base, m), nil
	case versionID == "":
		return ObjectMeta{}, "", ErrNoSuchKey
	}
	for _, v := range b.versions[key] {
		if matchesVersion(v, versionID) {
			if v.DeleteMarker {
				return v, "", ErrDeleteMarker
			}
			return v, versionPath(base, v), nil
		}
	}
	return ObjectMeta{}, "", ErrNoSuchVersion
}

// Object is an open object body. Single-file objects are an *os.File, so GET keeps the sendfile path.
type Object interface {
	io.ReadSeeker
	io.Closer
}

// OpenObject opens the current version of key; see OpenVersion.
func (s *Store) OpenObject(bucketName, key string) (ObjectMeta, Object, error) {
	return s.OpenVersion(bucketName, key, "")
}

// OpenVersion returns a version's metadata and its opened body; the caller closes it. Files are opened under
// the read lock, so a concurrent overwrite or delete cannot pull them away mid-read. Errors are Version's.
func (s *Store) OpenVersion(bucketName, key, versionID string) (ObjectMeta, Object, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, path, err := s.find(bucketName, key, versionID)
	if err != nil {
		return m, nil, err
	}
	if m.Parts == nil {
		f, err := os.Open(path)
		return m, f, err
	}
	pr, err := openParts(path, m.Parts)
	return m, pr, err
}

// UpdateObject rewrites a version's metadata (tags, ACL) without touching its bytes; versionID as in
// Version.
func (s *Store) UpdateObject(bucketName, key, versionID string, update func(*ObjectMeta)) error {
	s.mu.Lock()
	m, _, err := s.find(bucketName, key, versionID)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	b := s.buckets[bucketName]
	update(&m)
	c := &change{}
	b.do(c, record{Upd: &m})
	return s.journal(bucketName, b, c, nil)
}

// Deleted describes what a delete did: the version it removed or the delete marker it created.
type Deleted struct {
	VersionID    string // "" when versioning was never enabled; "null" for the null version
	DeleteMarker bool
}

// DeleteObject deletes key as S3 does without a version ID: idempotently, and with versioning by adding a
// delete marker (a null one when versioning is suspended) instead of removing bytes.
func (s *Store) DeleteObject(bucketName, key string) (Deleted, error) {
	s.mu.Lock()
	b, ok := s.buckets[bucketName]
	if !ok {
		s.mu.Unlock()
		return Deleted{}, ErrNoSuchBucket
	}
	base := objectBase(s.bucketDir(bucketName), key)
	old, exists := b.objects[key]
	c := &change{}
	if b.versioning == "" {
		if !exists {
			s.mu.Unlock()
			return Deleted{}, nil
		}
		c.garbage = append(c.garbage, s.unlinkData(base, old))
		b.do(c, record{Del: key})
		return Deleted{}, s.journal(bucketName, b, c, nil)
	}
	marker := ObjectMeta{Key: key, DeleteMarker: true, LastModified: time.Now().UTC().Truncate(time.Millisecond)}
	if b.versioning == "Enabled" {
		marker.VersionID = randomID()
	}
	var err error
	switch {
	case exists && marker.VersionID == "" && old.VersionID == "": // a null marker replaces the null version
		if !old.DeleteMarker {
			c.garbage = append(c.garbage, discard(currentPath(base, old), old.Parts != nil))
		}
	case exists:
		err = b.retireCurrent(c, base, key)
	}
	if err == nil && marker.VersionID == "" {
		_, _, err = b.dropVersion(c, base, key, "null", true)
	}
	if err == nil {
		b.do(c, record{Put: &marker})
	}
	return Deleted{VersionID: cmp.Or(marker.VersionID, "null"), DeleteMarker: true}, s.journal(bucketName, b, c, err)
}

// DeleteVersion permanently removes one version of key ("null" for the null version). Removing the current
// version makes the newest remaining one current. A missing version is not an error.
func (s *Store) DeleteVersion(bucketName, key, versionID string) (Deleted, error) {
	s.mu.Lock()
	b, ok := s.buckets[bucketName]
	if !ok {
		s.mu.Unlock()
		return Deleted{}, ErrNoSuchBucket
	}
	c := &change{}
	removed, _, err := b.dropVersion(c, objectBase(s.bucketDir(bucketName), key), key, versionID, false)
	return Deleted{VersionID: versionID, DeleteMarker: removed.DeleteMarker}, s.journal(bucketName, b, c, err)
}

// CopyObject copies a version of an object (versionID as in Version) server-side. When replace is nil the
// source metadata is kept (COPY directive). A single-file source is hard-linked, so the copy costs no data
// I/O.
func (s *Store) CopyObject(srcBucket, srcKey, srcVersionID, dstBucket, dstKey string, replace *ObjectMeta) (ObjectMeta, error) {
	if _, err := s.Bucket(dstBucket); err != nil {
		return ObjectMeta{}, err
	}
	// The metadata and the link are taken under one read lock, so an overwrite in between cannot pair
	// the old size and ETag with the new bytes.
	tmp := filepath.Join(s.bucketDir(dstBucket), ".put-"+randomID())
	s.mu.RLock()
	src, path, err := s.find(srcBucket, srcKey, srcVersionID)
	linked := err == nil && src.Parts == nil && os.Link(path, tmp) == nil
	s.mu.RUnlock()
	if err != nil {
		return ObjectMeta{}, err
	}
	meta := src
	if replace != nil {
		meta = *replace
	}
	meta.Key = dstKey
	if linked {
		meta.Size, meta.ETag, meta.Parts = src.Size, src.ETag, nil
		out, err := s.publish(dstBucket, meta, tmp, Precondition{})
		// Renaming a hard link onto the file it already names (a copy onto itself) succeeds without moving
		// anything, so the link can still be here.
		os.Remove(tmp)
		return out, err
	}
	_, f, err := s.OpenVersion(srcBucket, srcKey, srcVersionID)
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
		if strings.HasPrefix(k, prefix) && k > after && !m.DeleteMarker {
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

// VersionsResult is one page of ListObjectVersions: every version and delete marker in key order, newest
// first within a key.
type VersionsResult struct {
	Versions            []ObjectVersion
	CommonPrefixes      []string
	Truncated           bool
	NextKeyMarker       string
	NextVersionIDMarker string
}

type ObjectVersion struct {
	ObjectMeta
	IsLatest bool
}

// ListVersions pages through versions like List pages through keys. A page resumes after keyMarker, or
// within it after versionIDMarker ("null" for the null version).
func (s *Store) ListVersions(bucketName, prefix, delimiter, keyMarker, versionIDMarker string, max int) (VersionsResult, error) {
	s.mu.RLock()
	b, ok := s.buckets[bucketName]
	if !ok {
		s.mu.RUnlock()
		return VersionsResult{}, ErrNoSuchBucket
	}
	var keys []string
	for k := range b.objects {
		if strings.HasPrefix(k, prefix) && k >= keyMarker {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var res VersionsResult
	seen := map[string]bool{}
	full := func() bool {
		if len(res.Versions)+len(res.CommonPrefixes) < max {
			return false
		}
		res.Truncated = true
		return true
	}
	for _, k := range keys {
		if delimiter != "" {
			if i := strings.Index(k[len(prefix):], delimiter); i >= 0 {
				cp := k[:len(prefix)+i+len(delimiter)]
				if seen[cp] || cp <= keyMarker {
					continue
				}
				if full() {
					break
				}
				seen[cp] = true
				res.CommonPrefixes = append(res.CommonPrefixes, cp)
				res.NextKeyMarker, res.NextVersionIDMarker = cp, "" // resuming at cp skips every key under it
				continue
			}
		}
		list := append([]ObjectMeta{b.objects[k]}, b.versions[k]...)
		skip := k == keyMarker // a page that ended inside this key resumes after versionIDMarker
		for i, v := range list {
			if skip {
				skip = versionIDMarker == "" || !matchesVersion(v, versionIDMarker)
				if versionIDMarker == "" {
					break
				}
				continue
			}
			if full() {
				break
			}
			res.Versions = append(res.Versions, ObjectVersion{v, i == 0})
			res.NextKeyMarker, res.NextVersionIDMarker = k, cmp.Or(v.VersionID, "null")
		}
		if res.Truncated {
			break
		}
	}
	s.mu.RUnlock()
	return res, nil
}

// --- multipart ---

type upload struct {
	Bucket  string     `json:"bucket"`
	Meta    ObjectMeta `json:"meta"`
	Created time.Time  `json:"created"`
}

type Part struct {
	Number       int
	ETag         string
	Size         int64
	LastModified time.Time
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
	// Replacing a part is a glob, removes and a rename; the lock keeps two uploads of the same part
	// number (an SDK retry racing the original) from both surviving.
	s.mu.Lock()
	old, _ := filepath.Glob(filepath.Join(dir, prefix+"*"))
	for _, p := range old {
		os.Remove(p)
	}
	err = os.Rename(f.Name(), filepath.Join(dir, prefix+sum))
	s.mu.Unlock()
	if err != nil {
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
		out[n] = storedPart{Part{Number: n, ETag: `"` + sum + `"`, Size: info.Size(), LastModified: info.ModTime().UTC()}, filepath.Join(s.uploadDir(id), e.Name())}
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
func (s *Store) CompleteUpload(bucketName, key, id string, requested []Part, cond Precondition) (ObjectMeta, error) {
	u, err := s.loadUpload(bucketName, key, id)
	if err != nil {
		return ObjectMeta{}, err
	}
	// Checked here so a failed condition leaves the upload intact for a retry; publish checks again under
	// the lock that makes it atomic.
	if old, err := s.HeadObject(bucketName, key); cond != (Precondition{}) {
		if err := cond.check(old, err == nil); err != nil {
			return ObjectMeta{}, err
		}
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
	out, err := s.publish(bucketName, meta, dir, cond)
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
