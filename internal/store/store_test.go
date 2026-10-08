package store

import (
	"bytes"
	"cmp"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func read(t *testing.T, s *Store, bucket, key string) []byte {
	t.Helper()
	_, f, err := s.OpenObject(bucket, key)
	if err != nil {
		t.Fatalf("open %s: %v", key, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// Everything a restart must bring back: overwrites, deletes, multipart objects and hard-linked copies.
func TestReopenRestoresState(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "fast", true: "durable"}[durable], func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			s.Durable = durable
			if err := s.CreateBucket("bkt"); err != nil {
				t.Fatal(err)
			}
			put := func(key, body string) {
				if _, err := s.PutObject("bkt", ObjectMeta{Key: key}, strings.NewReader(body), Precondition{}); err != nil {
					t.Fatal(err)
				}
			}
			put("a", "first")
			put("b", "gone soon")
			put("a", "second") // overwrite
			if _, err := s.DeleteObject("bkt", "b"); err != nil {
				t.Fatal(err)
			}
			id, _ := s.CreateUpload("bkt", ObjectMeta{Key: "mp", ContentType: "model/stl"})
			e1, _ := s.PutPart("bkt", "mp", id, 1, strings.NewReader("hello "))
			e2, _ := s.PutPart("bkt", "mp", id, 2, strings.NewReader("world"))
			if _, err := s.CompleteUpload("bkt", "mp", id, []Part{{Number: 1, ETag: e1}, {Number: 2, ETag: e2}}, Precondition{}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CopyObject("bkt", "a", "", "bkt", "copy-of-a", nil); err != nil {
				t.Fatal(err)
			}
			put("mp2", "x")
			id2, _ := s.CreateUpload("bkt", ObjectMeta{Key: "mp2"})
			e, _ := s.PutPart("bkt", "mp2", id2, 1, strings.NewReader("multipart over a file"))
			if _, err := s.CompleteUpload("bkt", "mp2", id2, []Part{{Number: 1, ETag: e}}, Precondition{}); err != nil {
				t.Fatal(err)
			}
			s.Close()

			s, err = Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			want := map[string]string{"a": "second", "mp": "hello world", "copy-of-a": "second", "mp2": "multipart over a file"}
			res, _ := s.List("bkt", "", "", "", 1000)
			if len(res.Objects) != len(want) {
				t.Fatalf("after reopen: %d objects, want %d: %+v", len(res.Objects), len(want), res.Objects)
			}
			for key, body := range want {
				if got := string(read(t, s, "bkt", key)); got != body {
					t.Errorf("%s = %q, want %q", key, got, body)
				}
			}
			if m, _ := s.HeadObject("bkt", "mp"); m.ContentType != "model/stl" || !strings.HasSuffix(m.ETag, `-2"`) {
				t.Errorf("multipart meta after reopen: %+v", m)
			}
		})
	}
}

// Range reads that start inside one part and end inside another.
func TestPartsReaderSeeksAcrossParts(t *testing.T) {
	s, _ := Open(t.TempDir())
	defer s.Close()
	s.CreateBucket("bkt")
	id, _ := s.CreateUpload("bkt", ObjectMeta{Key: "k"})
	chunks := []string{"0123", "", "4567", "89"}
	var parts []Part
	for i, c := range chunks {
		etag, _ := s.PutPart("bkt", "k", id, i+1, strings.NewReader(c))
		parts = append(parts, Part{Number: i + 1, ETag: etag})
	}
	if _, err := s.CompleteUpload("bkt", "k", id, parts, Precondition{}); err != nil {
		t.Fatal(err)
	}
	_, f, _ := s.OpenObject("bkt", "k")
	defer f.Close()
	if end, _ := f.Seek(0, io.SeekEnd); end != 10 {
		t.Fatalf("size = %d", end)
	}
	for _, c := range []struct{ off, n int }{{0, 10}, {2, 5}, {3, 1}, {4, 4}, {9, 1}} {
		f.Seek(int64(c.off), io.SeekStart)
		buf := make([]byte, c.n)
		if _, err := io.ReadFull(f, buf); err != nil || string(buf) != "0123456789"[c.off:c.off+c.n] {
			t.Errorf("read %d+%d = %q, %v", c.off, c.n, buf, err)
		}
	}
}

// A data directory written by v0.1 (one <hash>.json per object) opens and is folded into the journal.
func TestOpensV01Layout(t *testing.T) {
	dir := t.TempDir()
	bdir := filepath.Join(dir, "old")
	os.MkdirAll(bdir, 0o755)
	os.WriteFile(filepath.Join(bdir, ".bucket.json"), []byte(`{"name":"old"}`), 0o644)
	base := objectBase(bdir, "dir/file.txt")
	os.WriteFile(base+".bin", []byte("legacy"), 0o644)
	sum := md5.Sum([]byte("legacy"))
	meta, _ := json.Marshal(ObjectMeta{Key: "dir/file.txt", Size: 6, ETag: `"` + hex.EncodeToString(sum[:]) + `"`})
	os.WriteFile(base+".json", meta, 0o644)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, "old", "dir/file.txt"); !bytes.Equal(got, []byte("legacy")) {
		t.Fatalf("legacy object = %q", got)
	}
	s.Close()
	if _, err := os.Stat(base + ".json"); !os.IsNotExist(err) {
		t.Fatal("legacy metadata file was not folded into the journal")
	}
	s, _ = Open(dir)
	defer s.Close()
	if _, err := s.HeadObject("old", "dir/file.txt"); err != nil {
		t.Fatalf("after migration: %v", err)
	}
}

// A copy reads the source's metadata and links its bytes under one lock: an overwrite racing the copy
// once left the copy with the old size and ETag over the new bytes.
func TestCopyRacingOverwriteStaysConsistent(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.CreateBucket("bkt")
	bodies := []string{strings.Repeat("a", 10), strings.Repeat("b", 20)}
	putSrc := func(i int) {
		if _, err := s.PutObject("bkt", ObjectMeta{Key: "src"}, strings.NewReader(bodies[i%2]), Precondition{}); err != nil {
			t.Error(err)
		}
	}
	putSrc(0)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2000; i++ {
			putSrc(i)
		}
	}()
	for i := 0; i < 2000; i++ {
		if _, err := s.CopyObject("bkt", "src", "", "bkt", "dst", nil); err != nil {
			t.Fatal(err)
		}
		meta, f, err := s.OpenObject("bkt", "dst")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(f)
		f.Close()
		if sum := md5.Sum(data); meta.Size != int64(len(data)) || meta.ETag != `"`+hex.EncodeToString(sum[:])+`"` {
			t.Fatalf("copy %d: metadata says %d bytes %s, file has %d bytes", i, meta.Size, meta.ETag, len(data))
		}
	}
	<-done
}

// Copying an object onto itself (to replace its metadata) once left a hard link behind on every call.
func TestCopyOntoItselfLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.CreateBucket("bkt")
	s.PutObject("bkt", ObjectMeta{Key: "k"}, strings.NewReader("v"), Precondition{})
	for i := 0; i < 3; i++ {
		if _, err := s.CopyObject("bkt", "k", "", "bkt", "k", &ObjectMeta{ContentType: "text/plain"}); err != nil {
			t.Fatal(err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "bkt", ".put-*")); len(left) > 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
	if got := string(read(t, s, "bkt", "k")); got != "v" {
		t.Fatalf("object after self-copy: %q", got)
	}
}

// An SDK retry racing the original upload of the same part must leave one part file, the last one written.
func TestConcurrentPutPartKeepsOneFile(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.CreateBucket("bkt")
	for round := 0; round < 50; round++ {
		id, err := s.CreateUpload("bkt", ObjectMeta{Key: "k"})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.PutPart("bkt", "k", id, 1, strings.NewReader(strings.Repeat("x", i+1))); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if files, _ := filepath.Glob(filepath.Join(dir, ".uploads", id, "p00001-*")); len(files) != 1 {
			t.Fatalf("round %d: %d files for part 1", round, len(files))
		}
	}
}

func setVersioning(t *testing.T, s *Store, bucket, status string) {
	t.Helper()
	err := s.UpdateBucket(bucket, func(m *BucketMeta) {
		if m.Configs == nil {
			m.Configs = map[string]string{}
		}
		m.Configs["versioning"] = `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>` + status + `</Status></VersioningConfiguration>`
	})
	if err != nil {
		t.Fatal(err)
	}
}

func readVersion(t *testing.T, s *Store, bucket, key, versionID string) string {
	t.Helper()
	_, f, err := s.OpenVersion(bucket, key, versionID)
	if err != nil {
		t.Fatalf("open %s@%s: %v", key, versionID, err)
	}
	defer f.Close()
	data, _ := io.ReadAll(f)
	return string(data)
}

func versionIDs(t *testing.T, s *Store, bucket string) []string {
	t.Helper()
	res, err := s.ListVersions(bucket, "", "", "", "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, v := range res.Versions {
		id := cmp.Or(v.VersionID, "null")
		if v.DeleteMarker {
			id = "marker:" + id
		}
		ids = append(ids, v.Key+"@"+id)
	}
	return ids
}

// Versioning end to end at the store level, including a restart in the middle.
func TestVersioning(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.CreateBucket("bkt")
	put := func(key, body string) ObjectMeta {
		t.Helper()
		m, err := s.PutObject("bkt", ObjectMeta{Key: key}, strings.NewReader(body), Precondition{})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	put("k", "before") // written before versioning: becomes the null version
	setVersioning(t, s, "bkt", "Enabled")
	v1, v2 := put("k", "one"), put("k", "two")
	if v1.VersionID == "" || v1.VersionID == v2.VersionID {
		t.Fatalf("version IDs %q %q", v1.VersionID, v2.VersionID)
	}
	if got := string(read(t, s, "bkt", "k")); got != "two" {
		t.Fatalf("current: %q", got)
	}
	for id, want := range map[string]string{v1.VersionID: "one", "null": "before", v2.VersionID: "two"} {
		if got := readVersion(t, s, "bkt", "k", id); got != want {
			t.Fatalf("version %s: %q, want %q", id, got, want)
		}
	}

	del, err := s.DeleteObject("bkt", "k")
	if err != nil || !del.DeleteMarker {
		t.Fatalf("delete: %+v %v", del, err)
	}
	if m, err := s.HeadObject("bkt", "k"); err != ErrNoSuchKey || !m.DeleteMarker {
		t.Fatalf("head after delete: %+v %v", m, err)
	}
	if _, err := s.Version("bkt", "k", del.VersionID); err != ErrDeleteMarker {
		t.Fatalf("GET of the marker's version: %v", err)
	}
	if res, _ := s.List("bkt", "", "", "", 100); len(res.Objects) != 0 {
		t.Fatalf("listed under a delete marker: %+v", res.Objects)
	}
	if err := s.DeleteBucket("bkt"); err != ErrBucketNotEmpty {
		t.Fatalf("delete bucket holding only versions: %v", err)
	}

	// Restart: every version and the marker come back from the journal.
	s.Close()
	if s, err = Open(dir); err != nil {
		t.Fatal(err)
	}
	want := []string{"k@marker:" + del.VersionID, "k@" + v2.VersionID, "k@" + v1.VersionID, "k@null"}
	if got := versionIDs(t, s, "bkt"); !slices.Equal(got, want) {
		t.Fatalf("versions after restart:\n got %v\nwant %v", got, want)
	}

	// Removing the marker brings v2 back; removing v2 promotes v1.
	if _, err := s.DeleteVersion("bkt", "k", del.VersionID); err != nil {
		t.Fatal(err)
	}
	if got := string(read(t, s, "bkt", "k")); got != "two" {
		t.Fatalf("after removing the marker: %q", got)
	}
	if _, err := s.DeleteVersion("bkt", "k", v2.VersionID); err != nil {
		t.Fatal(err)
	}
	if got := string(read(t, s, "bkt", "k")); got != "one" {
		t.Fatalf("after removing the current version: %q", got)
	}
	if _, err := s.Version("bkt", "k", v2.VersionID); err != ErrNoSuchVersion {
		t.Fatalf("removed version still readable: %v", err)
	}

	// Suspended: writes replace the null version, and other versions stay.
	setVersioning(t, s, "bkt", "Suspended")
	put("k", "null-1")
	put("k", "null-2")
	if got := versionIDs(t, s, "bkt"); !slices.Equal(got, []string{"k@null", "k@" + v1.VersionID}) {
		t.Fatalf("after suspended writes: %v", got)
	}
	if got := readVersion(t, s, "bkt", "k", "null"); got != "null-2" {
		t.Fatalf("null version: %q", got)
	}
	if del, _ := s.DeleteObject("bkt", "k"); del.VersionID != "null" || !del.DeleteMarker {
		t.Fatalf("suspended delete: %+v", del)
	}
	if got := versionIDs(t, s, "bkt"); !slices.Equal(got, []string{"k@marker:null", "k@" + v1.VersionID}) {
		t.Fatalf("after suspended delete: %v", got)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "bkt", "*.v-null.bin")); len(files) != 0 {
		t.Fatalf("replaced null version left bytes behind: %v", files)
	}
}

// Multipart objects are directories of parts; they move between current and noncurrent names too.
func TestVersioningMultipartObjects(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.CreateBucket("bkt")
	setVersioning(t, s, "bkt", "Enabled")
	complete := func(body string) ObjectMeta {
		t.Helper()
		id, _ := s.CreateUpload("bkt", ObjectMeta{Key: "mp"})
		etag, err := s.PutPart("bkt", "mp", id, 1, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		m, err := s.CompleteUpload("bkt", "mp", id, []Part{{Number: 1, ETag: etag}}, Precondition{})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	first := complete("first")
	complete("second")
	if got := readVersion(t, s, "bkt", "mp", first.VersionID); got != "first" {
		t.Fatalf("noncurrent multipart version: %q", got)
	}
	cur, _ := s.HeadObject("bkt", "mp")
	if _, err := s.DeleteVersion("bkt", "mp", cur.VersionID); err != nil {
		t.Fatal(err)
	}
	if got := string(read(t, s, "bkt", "mp")); got != "first" {
		t.Fatalf("promoted multipart version: %q", got)
	}
}

// ListVersions pages one entry at a time across keys and within a key.
func TestListVersionsPaging(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.CreateBucket("bkt")
	setVersioning(t, s, "bkt", "Enabled")
	for _, k := range []string{"a", "a", "b", "c", "c", "c"} {
		s.PutObject("bkt", ObjectMeta{Key: k}, strings.NewReader(k), Precondition{})
	}
	all := versionIDs(t, s, "bkt")
	var paged []string
	key, vid := "", ""
	for i := 0; i < 20; i++ {
		res, err := s.ListVersions("bkt", "", "", key, vid, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range res.Versions {
			paged = append(paged, v.Key+"@"+cmp.Or(v.VersionID, "null"))
		}
		if !res.Truncated {
			break
		}
		key, vid = res.NextKeyMarker, res.NextVersionIDMarker
	}
	if !slices.Equal(paged, all) || len(all) != 6 {
		t.Fatalf("paged %v\n  all %v", paged, all)
	}
}
