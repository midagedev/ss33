package store

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
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
			if err := s.DeleteObject("bkt", "b"); err != nil {
				t.Fatal(err)
			}
			id, _ := s.CreateUpload("bkt", ObjectMeta{Key: "mp", ContentType: "model/stl"})
			e1, _ := s.PutPart("bkt", "mp", id, 1, strings.NewReader("hello "))
			e2, _ := s.PutPart("bkt", "mp", id, 2, strings.NewReader("world"))
			if _, err := s.CompleteUpload("bkt", "mp", id, []Part{{Number: 1, ETag: e1}, {Number: 2, ETag: e2}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.CopyObject("bkt", "a", "bkt", "copy-of-a", nil); err != nil {
				t.Fatal(err)
			}
			put("mp2", "x")
			id2, _ := s.CreateUpload("bkt", ObjectMeta{Key: "mp2"})
			e, _ := s.PutPart("bkt", "mp2", id2, 1, strings.NewReader("multipart over a file"))
			if _, err := s.CompleteUpload("bkt", "mp2", id2, []Part{{Number: 1, ETag: e}}); err != nil {
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
	if _, err := s.CompleteUpload("bkt", "k", id, parts); err != nil {
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
