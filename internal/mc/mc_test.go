package mc_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/midagedev/ss33/internal/mc"
	"github.com/midagedev/ss33/internal/server"
	"github.com/midagedev/ss33/internal/sigv4"
	"github.com/midagedev/ss33/internal/store"
)

// Runs the command sequence a typical compose bootstrap uses: alias, mb, anonymous download, seed a bucket
// from a directory, clone it into a per-run bucket, count objects, drop the clone.
func TestBootstrapScript(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(&server.Server{Store: st, Creds: sigv4.Credentials{AccessKey: "admin", SecretKey: "admin-secret"}, Region: "us-east-1"})
	defer ts.Close()
	t.Setenv("MC_CONFIG_DIR", t.TempDir())

	seed := t.TempDir()
	for name, body := range map[string]string{"a.json": "{}", "cases/1/mesh.stl": "solid", "cases/1/meta.txt": "m"} {
		p := filepath.Join(seed, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}

	run := func(args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := mc.Main(args, &out, &errOut); code != 0 {
			t.Fatalf("mc %v: exit %d: %s", args, code, errOut.String())
		}
		return out.String()
	}
	run("alias", "set", "local", ts.URL, "admin", "admin-secret")
	run("mb", "--ignore-existing", "local/seed")
	run("mb", "--ignore-existing", "local/seed") // idempotent
	run("anonymous", "set", "download", "local/seed")
	run("cp", "--recursive", "--quiet", seed+"/", "local/seed/")
	run("mb", "--ignore-existing", "local/run-1")
	run("cp", "--recursive", "--quiet", "local/seed/", "local/run-1/")

	if buckets := run("ls", "local"); !strings.HasSuffix(strings.Split(buckets, "\n")[0], " run-1/") || !strings.Contains(buckets, "0B seed/\n") {
		t.Fatalf("ls on a bare alias should list buckets as `... 0B <name>/`:\n%s", buckets)
	}
	listing := run("ls", "--recursive", "local/run-1")
	if n := strings.Count(strings.TrimSpace(listing), "\n") + 1; n != 3 || !strings.Contains(listing, "cases/1/mesh.stl") {
		t.Fatalf("ls --recursive: %d lines\n%s", n, listing)
	}

	resp, err := http.Get(ts.URL + "/seed/cases/1/mesh.stl")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") == "" {
		t.Fatalf("anonymous download after `anonymous set download`: %s", resp.Status)
	}

	run("rm", "local/run-1/a.json")
	if out := run("rm", "--recursive", "--force", "local/run-1/cases/"); strings.Count(out, "Removed `local/run-1/cases/1/") != 2 {
		t.Fatalf("rm --recursive --force:\n%s", out)
	}
	if listing := run("ls", "--recursive", "local/run-1"); listing != "" {
		t.Fatalf("objects left after rm:\n%s", listing)
	}
	run("version", "enable", "local/run-1")
	if m, _ := st.Bucket("run-1"); !strings.Contains(m.Configs["versioning"], "Enabled") {
		t.Fatalf("version enable: %+v", m.Configs)
	}
	run("rb", "--force", "local/run-1")
	if _, err := st.Bucket("run-1"); err == nil {
		t.Fatal("bucket survived rb --force")
	}

	var errOut bytes.Buffer
	if code := mc.Main([]string{"mb", "local/seed"}, &bytes.Buffer{}, &errOut); code == 0 {
		t.Fatal("mb without --ignore-existing on an existing bucket should fail")
	}
}

// Aliases also come from MC_HOST_<alias> and the pre-2021 `mc config host add`.
func TestAliasFromEnvAndConfigHost(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	ts := httptest.NewServer(&server.Server{Store: st, Creds: sigv4.Credentials{AccessKey: "admin", SecretKey: "admin-secret"}, Region: "us-east-1"})
	defer ts.Close()
	t.Setenv("MC_CONFIG_DIR", t.TempDir())
	t.Setenv("MC_HOST_fromenv", strings.Replace(ts.URL, "http://", "http://admin:admin-secret@", 1))

	for _, args := range [][]string{
		{"mb", "fromenv/one"},
		{"config", "host", "add", "legacy", ts.URL, "admin", "admin-secret", "--api", "S3v4"},
		{"mb", "legacy/two"},
	} {
		var errOut bytes.Buffer
		if code := mc.Main(args, &bytes.Buffer{}, &errOut); code != 0 {
			t.Fatalf("mc %v: %s", args, errOut.String())
		}
	}
	if b := st.ListBuckets(); len(b) != 2 {
		t.Fatalf("buckets: %+v", b)
	}
}

// A key such as "../x" must not let a recursive download write outside the destination directory.
func TestRecursiveDownloadStaysInDestination(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	ts := httptest.NewServer(&server.Server{Store: st, Creds: sigv4.Credentials{AccessKey: "admin", SecretKey: "admin-secret"}, Region: "us-east-1"})
	defer ts.Close()
	t.Setenv("MC_CONFIG_DIR", t.TempDir())
	st.CreateBucket("bkt")
	st.PutObject("bkt", store.ObjectMeta{Key: "../../escaped.txt"}, strings.NewReader("x"), store.Precondition{})

	root := t.TempDir()
	dst := filepath.Join(root, "a", "dst")
	mc.Main([]string{"alias", "set", "local", ts.URL, "admin", "admin-secret"}, &bytes.Buffer{}, &bytes.Buffer{})
	var errOut bytes.Buffer
	if code := mc.Main([]string{"cp", "--recursive", "local/bkt", dst}, &bytes.Buffer{}, &errOut); code == 0 {
		t.Fatal("cp --recursive accepted a key that leaves the destination")
	}
	if _, err := os.Stat(filepath.Join(root, "escaped.txt")); err == nil {
		t.Fatal("file written outside the destination")
	}
}

// Versioned buckets from the CLI: version enable, ls --versions, rm --version-id, and rb --force removing
// every version; event add/ls/rm writing the bucket's notification configuration.
func TestVersionsAndEvents(t *testing.T) {
	st, _ := store.Open(t.TempDir())
	srv := &server.Server{Store: st, Creds: sigv4.Credentials{AccessKey: "admin", SecretKey: "admin-secret"}, Region: "us-east-1",
		Webhooks: map[string]server.Webhook{"PRIMARY": {Endpoint: "http://127.0.0.1:1"}}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	t.Setenv("MC_CONFIG_DIR", t.TempDir())
	run := func(args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := mc.Main(args, &out, &errOut); code != 0 {
			t.Fatalf("mc %v: exit %d: %s", args, code, errOut.String())
		}
		return out.String()
	}
	run("alias", "set", "local", ts.URL, "admin", "admin-secret")
	run("mb", "local/ver")
	run("version", "enable", "local/ver")
	src := filepath.Join(t.TempDir(), "f.txt")
	for _, body := range []string{"one", "two"} {
		os.WriteFile(src, []byte(body), 0o644)
		run("cp", src, "local/ver/f.txt")
	}
	run("rm", "local/ver/f.txt")
	listing := run("ls", "--versions", "local/ver")
	if strings.Count(listing, "\n") != 3 || !strings.Contains(listing, " v3 DEL f.txt") || !strings.Contains(listing, " v1 PUT f.txt") {
		t.Fatalf("ls --versions:\n%s", listing)
	}
	marker := strings.Fields(strings.Split(listing, "\n")[0])[5] // [date time zone] size class versionId ...
	run("rm", "--version-id", marker, "local/ver/f.txt")
	if got := run("ls", "local/ver"); !strings.Contains(got, "f.txt") {
		t.Fatalf("removing the delete marker should bring f.txt back:\n%s", got)
	}

	run("event", "add", "local/ver", "arn:minio:sqs::PRIMARY:webhook", "--event", "put,delete", "--prefix", "in/")
	if got := run("event", "ls", "local/ver"); !strings.Contains(got, "arn:minio:sqs::PRIMARY:webhook   s3:ObjectCreated:*,s3:ObjectRemoved:*   Filter: prefix=\"in/\"") {
		t.Fatalf("event ls:\n%s", got)
	}
	var errOut bytes.Buffer
	if code := mc.Main([]string{"event", "add", "local/ver", "arn:minio:sqs::MISSING:webhook"}, &bytes.Buffer{}, &errOut); code == 0 {
		t.Fatal("event add accepted a webhook target the server does not have")
	}
	run("event", "rm", "local/ver", "arn:minio:sqs::PRIMARY:webhook")
	if got := run("event", "ls", "local/ver"); got != "" {
		t.Fatalf("event ls after rm:\n%s", got)
	}

	run("rb", "--force", "local/ver")
	if _, err := st.Bucket("ver"); err == nil {
		t.Fatal("rb --force left a versioned bucket behind")
	}
}

const etagABC = "900150983cd24fb0d6963f7d28e17f72"

type mcEnv struct {
	t  *testing.T
	ts *httptest.Server
	st *store.Store
}

func newMC(t *testing.T) *mcEnv {
	return newMCHooks(t, nil)
}

func newMCHooks(t *testing.T, hooks map[string]server.Webhook) *mcEnv {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(&server.Server{
		Store:    st,
		Creds:    sigv4.Credentials{AccessKey: "admin", SecretKey: "admin-secret"},
		Region:   "us-east-1",
		Webhooks: hooks,
	})
	t.Cleanup(ts.Close)
	t.Setenv("MC_CONFIG_DIR", t.TempDir())
	e := &mcEnv{t: t, ts: ts, st: st}
	e.ok("alias", "set", "local", ts.URL, "admin", "admin-secret")
	return e
}

func (e *mcEnv) run(args ...string) (int, string, string) {
	e.t.Helper()
	var out, errb bytes.Buffer
	code := mc.Main(args, &out, &errb)
	return code, out.String(), errb.String()
}

func (e *mcEnv) ok(args ...string) string {
	e.t.Helper()
	code, out, errb := e.run(args...)
	if code != 0 {
		e.t.Fatalf("mc %v: exit %d\nstderr: %s\nstdout: %s", args, code, errb, out)
	}
	return out
}

func (e *mcEnv) jsonOK(args ...string) []string {
	e.t.Helper()
	code, out, errb := e.run(append([]string{"--json"}, args...)...)
	if code != 0 || errb != "" {
		e.t.Fatalf("mc --json %v: exit %d\nstderr: %s\nstdout: %s", args, code, errb, out)
	}
	lines := splitLines(out)
	if len(lines) == 0 {
		e.t.Fatalf("mc --json %v: empty stdout", args)
	}
	return lines
}

func (e *mcEnv) jsonFail(args ...string) []string {
	e.t.Helper()
	code, out, errb := e.run(append([]string{"--json"}, args...)...)
	if code == 0 || errb != "" {
		e.t.Fatalf("mc --json %v: exit %d\nstderr: %q\nstdout: %s", args, code, errb, out)
	}
	return splitLines(out)
}

func (e *mcEnv) put(remote, body string) {
	e.t.Helper()
	p := filepath.Join(e.t.TempDir(), strings.ReplaceAll(remote, "/", "_"))
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.ok("cp", p, "local/"+remote)
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// jsonKeyOrder walks one JSON value with a decoder and returns object keys in document order.
// It does not decode into a map, so key order is the order on the wire.
func jsonKeyOrder(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	var keys []string
	var walk func()
	walk = func() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("token: %v\n%s", err, line)
		}
		d, ok := tok.(json.Delim)
		if !ok {
			return
		}
		switch d {
		case '{':
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					t.Fatal(err)
				}
				s, ok := k.(string)
				if !ok {
					t.Fatalf("key %T in %s", k, line)
				}
				keys = append(keys, s)
				walk()
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim('}') {
				t.Fatalf("object end %v %v", end, err)
			}
		case '[':
			for dec.More() {
				walk()
			}
			end, err := dec.Token()
			if err != nil || end != json.Delim(']') {
				t.Fatalf("array end %v %v", end, err)
			}
		default:
			t.Fatalf("delim %q", d)
		}
	}
	walk()
	return keys
}

func assertKeys(t *testing.T, line string, want ...string) {
	t.Helper()
	got := jsonKeyOrder(t, line)
	if !slices.Equal(got, want) {
		t.Fatalf("key order\n got %q\nwant %q\n%s", got, want, line)
	}
}

func decodeLine(t *testing.T, line string, dest any) {
	t.Helper()
	if err := json.Unmarshal([]byte(line), dest); err != nil {
		t.Fatalf("decode: %v\n%s", err, line)
	}
}

func assertLocalTime(t *testing.T, stamp string, recent bool) {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Fatalf("time %q: %v", stamp, err)
	}
	_, off := ts.Zone()
	_, want := time.Now().Zone()
	if off != want {
		t.Fatalf("offset %d want local %d (%s)", off, want, stamp)
	}
	if recent && (time.Since(ts) > time.Minute || time.Until(ts) > time.Second) {
		t.Fatalf("not recent: %s", stamp)
	}
}

func withStdin(t *testing.T, body string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = old
		r.Close()
	})
	go func() {
		io.WriteString(w, body)
		w.Close()
	}()
	fn()
}

// Unknown flags fail closed, including under --json, and never reach the command.
func TestUnknownFlag(t *testing.T) {
	e := newMC(t)
	e.ok("mb", "local/bkt")
	e.put("bkt/a.txt", "abc")
	for _, args := range [][]string{
		{"ls", "--bogus", "local/bkt"},
		{"mb", "--force", "local/other"},
		{"--json", "ls", "--bogus", "local/bkt"},
	} {
		code, out, errb := e.run(args...)
		if code != 1 || out != "" || !strings.Contains(errb, "Invalid command usage, flag provided but not defined: -") {
			t.Fatalf("mc %v: exit %d\nstdout: %q\nstderr: %q", args, code, out, errb)
		}
	}
	_, _, errb := e.run("ls", "--bogusflag", "local/bkt")
	if !strings.Contains(errb, "flag provided but not defined: -bogusflag") {
		t.Fatalf("stderr: %s", errb)
	}
	code, out, errb := e.run("ls", "--help", "--bogus", "local/bkt")
	if code != 1 || out != "" || !strings.Contains(errb, "flag provided but not defined: -bogus") {
		t.Fatalf("help did not lose to the unknown flag: exit %d %q %q", code, out, errb)
	}
}

// ls --recursive --json emits one object per line and size appears before key.
func TestLsRecursiveJSON(t *testing.T) {
	e := newMC(t)
	e.ok("mb", "local/bkt")
	e.put("bkt/tree/a.txt", "a")
	e.put("bkt/tree/sub/b.txt", "bb")
	code, out, errb := e.run("ls", "--recursive", "--json", "local/bkt/tree")
	if code != 0 || errb != "" {
		t.Fatalf("exit %d stderr %s stdout %s", code, errb, out)
	}
	lines := splitLines(out)
	if len(lines) != 2 {
		t.Fatalf("lines: %q", lines)
	}
	for _, line := range lines {
		keys := jsonKeyOrder(t, line)
		if si, ki := slices.Index(keys, "size"), slices.Index(keys, "key"); si < 0 || ki < 0 || si > ki {
			t.Fatalf("size does not precede key: %v", keys)
		}
		var row struct {
			Status string `json:"status"`
			Type   string `json:"type"`
			Size   int64  `json:"size"`
			Key    string `json:"key"`
			ETag   string `json:"etag"`
		}
		decodeLine(t, line, &row)
		if row.Status != "success" || row.Type != "file" || row.ETag == "" || row.Size == 0 || row.Key == "" {
			t.Fatalf("%+v", row)
		}
	}
	var sub struct {
		Key  string `json:"key"`
		Size int64  `json:"size"`
	}
	decodeLine(t, lines[1], &sub)
	if sub.Key != "sub/b.txt" || sub.Size != 2 {
		t.Fatalf("relative key: %+v", sub)
	}
}

// stat exits 0 for an object that exists and non-zero when it does not.
func TestStatPresentMissing(t *testing.T) {
	e := newMC(t)
	e.ok("mb", "local/bkt")
	e.put("bkt/a.txt", "abc")
	if code, _, errb := e.run("stat", "local/bkt/a.txt"); code != 0 {
		t.Fatalf("present: exit %d %s", code, errb)
	}
	code, out, errb := e.run("stat", "local/bkt/nope.txt")
	if code == 0 || out != "" || !strings.Contains(errb, "Object does not exist") {
		t.Fatalf("missing: exit %d stdout %q stderr %q", code, out, errb)
	}
}

func TestJSONLineOrder(t *testing.T) {
	e := newMC(t)
	e.ok("mb", "local/plain")
	e.put("plain/a.txt", "abc")
	e.put("plain/b.txt", "abc")
	e.put("plain/a/x.txt", "z")
	e.put("plain/dir/p.txt", "abc")
	meta := filepath.Join(t.TempDir(), "meta.txt")
	if err := os.WriteFile(meta, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("cp", "--attr", "X-Amz-Meta-Owner=kim;Cache-Control=no-cache", meta, "local/plain/meta.txt")

	var sawFolder bool
	for _, line := range e.jsonOK("ls", "local/plain") {
		var row struct {
			Type string `json:"type"`
			Key  string `json:"key"`
			ETag string `json:"etag"`
			URL  string `json:"url"`
			Last string `json:"lastModified"`
			Size int64  `json:"size"`
		}
		decodeLine(t, line, &row)
		if !strings.HasSuffix(row.URL, "/plain/") {
			t.Fatalf("url %s", row.URL)
		}
		if row.Type == "folder" {
			sawFolder = true
			assertKeys(t, line, "status", "type", "lastModified", "size", "key", "etag", "url", "versionOrdinal")
			assertLocalTime(t, row.Last, true)
			if row.ETag != "" || row.Size != 0 {
				t.Fatalf("folder %+v", row)
			}
			continue
		}
		if sawFolder {
			t.Fatal("folder listed before an object")
		}
		assertKeys(t, line, "status", "type", "lastModified", "size", "key", "etag", "url", "versionOrdinal", "storageClass")
		assertLocalTime(t, row.Last, false)
		if row.Type != "file" {
			t.Fatalf("%+v", row)
		}
		if row.Key == "a.txt" && (row.Size != 3 || row.ETag != etagABC) {
			t.Fatalf("a.txt %+v", row)
		}
	}
	if !sawFolder {
		t.Fatal("no folder")
	}

	dirLines := e.jsonOK("ls", "local/plain/dir")
	if len(dirLines) != 1 {
		t.Fatalf("%v", dirLines)
	}
	var dirRow struct{ Key, URL, ETag string }
	decodeLine(t, dirLines[0], &dirRow)
	if dirRow.Key != "p.txt" || dirRow.ETag != etagABC || !strings.HasSuffix(dirRow.URL, "/plain/dir/") {
		t.Fatalf("%+v", dirRow)
	}
	if code, out, errb := e.run("--json", "ls", "local/plain/nope/"); code != 0 || out != "" || errb != "" {
		t.Fatalf("empty prefix: exit %d %q %q", code, out, errb)
	}

	miss := e.jsonFail("ls", "local/nobucket")
	assertKeys(t, miss[0], "status", "error", "message", "cause", "message", "error", "Bucket", "type")
	var env errEnv
	decodeLine(t, miss[0], &env)
	if env.Error.Message != "Unable to list folder." || env.Error.Type != "error" || env.Error.Cause.Message != "Bucket `nobucket` does not exist." {
		t.Fatalf("%+v", env)
	}

	e.ok("mb", "local/aaa")
	buckets := e.jsonOK("ls", "local")
	assertKeys(t, buckets[0], "status", "type", "lastModified", "size", "key", "etag", "url", "versionOrdinal")
	var bkt struct{ Type, Key, ETag, URL string }
	decodeLine(t, buckets[0], &bkt)
	if bkt.Type != "folder" || bkt.Key != "aaa/" || bkt.ETag != "" || bkt.URL != e.ts.URL+"/" {
		t.Fatalf("%+v", bkt)
	}

	st := e.jsonOK("stat", "local/plain/a.txt")
	assertKeys(t, st[0], "status", "name", "lastModified", "size", "etag", "type", "metadata", "Content-Type")
	var one statRow
	decodeLine(t, st[0], &one)
	assertLocalTime(t, one.LastModified, false)
	if one.Name != "a.txt" || one.Size != 3 || one.ETag != etagABC || one.Type != "file" {
		t.Fatalf("%+v", one)
	}

	metaLines := e.jsonOK("stat", "local/plain/meta.txt")
	assertKeys(t, metaLines[0], "status", "name", "lastModified", "size", "etag", "type", "metadata", "Cache-Control", "Content-Type", "X-Amz-Meta-Owner")
	var metaRow statRow
	decodeLine(t, metaLines[0], &metaRow)
	if metaRow.Metadata["X-Amz-Meta-Owner"] != "kim" || metaRow.Metadata["Cache-Control"] != "no-cache" || !strings.Contains(metaRow.Metadata["Content-Type"], "text/plain") {
		t.Fatalf("%v", metaRow.Metadata)
	}
	plainStat := e.ok("stat", "local/plain/meta.txt")
	iCache := strings.Index(plainStat, "Cache-Control")
	iType := strings.Index(plainStat, "Content-Type")
	iOwner := strings.Index(plainStat, "X-Amz-Meta-Owner")
	if iCache < 0 || iType < 0 || iOwner < 0 || !(iCache < iType && iType < iOwner) || !strings.Contains(plainStat, "Name") {
		t.Fatalf("plain stat\n%s", plainStat)
	}

	gone := e.jsonFail("stat", "local/plain/nope.txt")
	assertKeys(t, gone[0], "status", "error", "message", "cause", "message", "error", "type")
	decodeLine(t, gone[0], &env)
	if env.Error.Type != "fatal" || env.Error.Cause.Message != "Object does not exist" || env.Error.Message != "Unable to stat `local/plain/nope.txt`." {
		t.Fatalf("%+v", env)
	}

	bucketStat := e.jsonOK("stat", "local/plain")
	assertKeys(t, bucketStat[0], "status", "name", "lastModified", "size")
	var sb struct {
		Name, LastModified string
		Size               int64
	}
	decodeLine(t, bucketStat[0], &sb)
	if sb.Name != "plain/" || sb.LastModified != "" || sb.Size != 0 {
		t.Fatalf("%+v", sb)
	}
	pref := e.jsonOK("stat", "local/plain/dir/")
	var prefRow statRow
	decodeLine(t, pref[0], &prefRow)
	if prefRow.Name != "p.txt" || prefRow.Size != 3 {
		t.Fatalf("%+v", prefRow)
	}

	made := e.jsonOK("mb", "local/made")
	assertKeys(t, made[0], "status", "bucket", "region")
	var mb struct{ Status, Bucket, Region string }
	decodeLine(t, made[0], &mb)
	if mb.Status != "success" || mb.Bucket != "local/made" || mb.Region != "" {
		t.Fatalf("%+v", mb)
	}
	again := e.jsonOK("mb", "--ignore-existing", "local/made")
	decodeLine(t, again[0], &mb)
	if mb.Status != "success" {
		t.Fatalf("%+v", mb)
	}
	conflict := e.jsonFail("mb", "local/made")
	assertKeys(t, conflict[0], "status", "error", "message", "cause", "message", "error", "Code", "Message", "BucketName", "Key", "Resource", "RequestID", "HostID", "Region", "Server", "type")
	decodeLine(t, conflict[0], &env)
	if env.Error.Type != "error" || !strings.Contains(string(env.Error.Cause.Error), "BucketAlreadyOwnedByYou") {
		t.Fatalf("%s", conflict[0])
	}

	rbMiss := e.jsonFail("rb", "local/no-such-rb")
	assertKeys(t, rbMiss[0], "status", "error", "message", "cause", "message", "error", "Bucket", "type")
	decodeLine(t, rbMiss[0], &env)
	if env.Error.Message != "Unable to validate target `local/no-such-rb`." || !strings.Contains(env.Error.Cause.Message, "no-such-rb") {
		t.Fatalf("%+v", env)
	}
	removed := e.jsonOK("rb", "local/made")
	assertKeys(t, removed[0], "status", "bucket")

	src := filepath.Join(t.TempDir(), "p.txt")
	if err := os.WriteFile(src, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ok("mb", "local/cpb")
	cp := e.jsonOK("cp", src, "local/cpb/c.txt")
	if len(cp) != 2 {
		t.Fatalf("%v", cp)
	}
	assertKeys(t, cp[0], "status", "source", "target", "size", "totalCount", "totalSize")
	assertKeys(t, cp[1], "status", "total", "transferred", "duration", "speed")
	var cpl struct {
		Status, Source, Target string
		Size                   int64 `json:"size"`
		TotalCount             int   `json:"totalCount"`
		TotalSize              int64 `json:"totalSize"`
	}
	decodeLine(t, cp[0], &cpl)
	if cpl.Status != "success" || cpl.Size != 3 || cpl.TotalCount != 1 || cpl.TotalSize != 0 || cpl.Target != "local/cpb/c.txt" || !strings.Contains(cpl.Source, "p.txt") {
		t.Fatalf("%+v", cpl)
	}
	var sum struct {
		Status             string
		Total, Transferred int64
	}
	decodeLine(t, cp[1], &sum)
	if sum.Status != "success" || sum.Total != 3 || sum.Transferred != 3 {
		t.Fatalf("%+v", sum)
	}
	if code, out, errb := e.run("cp", "--quiet", src, "local/cpb/q.txt"); code != 0 || out != "" {
		t.Fatalf("plain cp: exit %d %q %s", code, out, errb)
	}

	tree := t.TempDir()
	os.MkdirAll(filepath.Join(tree, "sub"), 0o755)
	os.WriteFile(filepath.Join(tree, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(tree, "sub", "b.txt"), []byte("bb"), 0o644)
	rec := e.jsonOK("cp", "--recursive", tree+"/", "local/cpb/tree/")
	if len(rec) != 3 {
		t.Fatalf("%v", rec)
	}
	var recRow struct {
		Target     string
		TotalCount int
	}
	decodeLine(t, rec[0], &recRow)
	if recRow.TotalCount != 2 || !strings.HasSuffix(recRow.Target, "/tree/a.txt") {
		t.Fatalf("%+v", recRow)
	}

	mv := e.jsonOK("mv", "local/cpb/c.txt", "local/cpb/moved.txt")
	assertKeys(t, mv[0], "status", "source", "target", "size", "totalCount", "totalSize")
	if code, _, _ := e.run("stat", "local/cpb/c.txt"); code == 0 {
		t.Fatal("mv left the source")
	}
	if code, _, errb := e.run("stat", "local/cpb/moved.txt"); code != 0 {
		t.Fatal(errb)
	}
	local := filepath.Join(t.TempDir(), "local.txt")
	os.WriteFile(local, []byte("abc"), 0o644)
	e.jsonOK("mv", local, "local/cpb/fromlocal.txt")
	if _, err := os.Stat(local); !os.IsNotExist(err) {
		t.Fatal("local mv source remains")
	}
	if code, _, _ := e.run("mv", t.TempDir(), "local/cpb/nope"); code == 0 {
		t.Fatal("mv of a local directory was accepted")
	}
	dl := filepath.Join(t.TempDir(), "dl.txt")
	down := e.jsonOK("cp", "local/plain/a.txt", dl)
	var downRow struct{ Source, Target string }
	decodeLine(t, down[0], &downRow)
	if downRow.Source != "local/plain/a.txt" || downRow.Target != dl {
		t.Fatalf("%+v", downRow)
	}
	if b, _ := os.ReadFile(dl); string(b) != "abc" {
		t.Fatalf("download %q", b)
	}

	rm := e.jsonOK("rm", "local/cpb/moved.txt")
	assertKeys(t, rm[0], "status", "key", "deleteMarker", "versionID", "modTime", "dryRun")
	var rmRow struct {
		Status, Key, VersionID string
		DeleteMarker, DryRun   bool
		ModTime                *time.Time
	}
	decodeLine(t, rm[0], &rmRow)
	if rmRow.Status != "success" || rmRow.Key != "local/cpb/moved.txt" || rmRow.DeleteMarker || rmRow.VersionID != "" || rmRow.DryRun || rmRow.ModTime != nil {
		t.Fatalf("%+v", rmRow)
	}
	rmGone := e.jsonFail("rm", "local/cpb/moved.txt")
	assertKeys(t, rmGone[0], "status", "error", "message", "cause", "message", "error", "type")
	decodeLine(t, rmGone[0], &env)
	if env.Error.Type != "error" || env.Error.Message != "Failed to remove `local/cpb/moved.txt`." || env.Error.Cause.Message != "Object does not exist" {
		t.Fatalf("%+v", env)
	}

	alias := e.jsonOK("alias", "set", "loc2", e.ts.URL, "admin", "admin-secret")
	assertKeys(t, alias[0], "status", "alias", "URL", "accessKey", "secretKey", "api", "path")
	var al struct{ Alias, URL, API, Path string }
	decodeLine(t, alias[0], &al)
	if al.Alias != "loc2" || al.URL != e.ts.URL || al.API != "s3v4" || al.Path != "auto" {
		t.Fatalf("%+v", al)
	}
	if out := e.ok("alias", "set", "loc3", e.ts.URL, "admin", "admin-secret"); out != "Added `loc3` successfully.\n" {
		t.Fatalf("%q", out)
	}

	priv := e.jsonOK("anonymous", "get", "local/plain")
	assertKeys(t, priv[0], "operation", "status", "bucket", "permission")
	var an struct{ Operation, Status, Bucket, Permission string }
	decodeLine(t, priv[0], &an)
	if an.Operation != "get" || an.Permission != "private" || an.Bucket != "local/plain" {
		t.Fatalf("%+v", an)
	}
	if out := e.ok("anonymous", "get", "local/plain"); out != "Access permission for `local/plain` is `private`\n" {
		t.Fatalf("%q", out)
	}
	if out := e.ok("anonymous", "get-json", "local/plain"); out != "{}\n" {
		t.Fatalf("%q", out)
	}
	if out := e.ok("anonymous", "set", "download", "local/plain"); out != "Access permission for `local/plain` is set to `download`\n" {
		t.Fatalf("%q", out)
	}
	got := e.jsonOK("anonymous", "get", "local/plain")
	assertKeys(t, got[0], "operation", "status", "bucket", "permission", "anonymous", "Version", "Statement",
		"Effect", "Principal", "AWS", "Action", "Resource", "Effect", "Principal", "AWS", "Action", "Resource")
	decodeLine(t, got[0], &an)
	if an.Permission != "download" {
		t.Fatalf("%+v", an)
	}
	list := e.jsonOK("anonymous", "list", "local/plain")
	assertKeys(t, list[0], "resource", "allow")
	var lr struct{ Resource, Allow string }
	decodeLine(t, list[0], &lr)
	if lr.Resource != "plain/*" || lr.Allow != "readonly" {
		t.Fatalf("%+v", lr)
	}
	if out := e.ok("anonymous", "list", "local/plain"); out != "plain/* => readonly\n" {
		t.Fatalf("%q", out)
	}
	e.ok("mb", "local/modes")
	e.ok("anonymous", "set", "public", "local/modes")
	if out := e.ok("anonymous", "get", "local/modes"); !strings.Contains(out, "`public`") {
		t.Fatalf("%q", out)
	}
	e.ok("anonymous", "set", "upload", "local/modes")
	if out := e.ok("anonymous", "get", "local/modes"); !strings.Contains(out, "`upload`") {
		t.Fatalf("%q", out)
	}
	pol := filepath.Join(t.TempDir(), "pol.json")
	os.WriteFile(pol, []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::plain/only"]}]}`), 0o644)
	setj := e.jsonOK("anonymous", "set-json", pol, "local/plain")
	assertKeys(t, setj[0], "operation", "status", "bucket", "permission")
	var sj struct{ Operation, Permission string }
	decodeLine(t, setj[0], &sj)
	if sj.Operation != "set-json" || sj.Permission != pol {
		t.Fatalf("%+v", sj)
	}
	if out := e.ok("anonymous", "get", "local/plain"); !strings.Contains(out, "`custom`") {
		t.Fatalf("%q", out)
	}
	custom := e.jsonOK("anonymous", "list", "local/plain")
	decodeLine(t, custom[0], &lr)
	if lr.Allow != "custom" || lr.Resource != "plain/only" {
		t.Fatalf("%+v", lr)
	}
	code, out, errb := e.run("anonymous", "set-json", filepath.Join(t.TempDir(), "missing.json"), "local/plain")
	if code != 1 || out != "" || !strings.Contains(errb, "Invalid access file:") {
		t.Fatalf("missing file: exit %d %q %q", code, out, errb)
	}
	withStdin(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetBucketLocation","s3:ListBucket"],"Resource":["arn:aws:s3:::plain"]},{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::plain/*"]}]}`, func() {
		in := e.jsonOK("anonymous", "set-json", "-", "local/plain")
		decodeLine(t, in[0], &sj)
		if sj.Permission != "-" {
			t.Fatalf("%+v", sj)
		}
	})
	if out := e.ok("anonymous", "get", "local/plain"); !strings.Contains(out, "`download`") {
		t.Fatalf("stdin policy: %q", out)
	}
	if out := e.ok("policy", "set", "none", "local/plain"); !strings.Contains(out, "`none`") {
		t.Fatalf("%q", out)
	}

	info := e.jsonOK("version", "info", "local/plain")
	assertKeys(t, info[0], "Op", "status", "url", "versioning", "status", "MFADelete")
	var ver struct {
		Op         string
		URL        string
		Versioning struct{ Status, MFADelete string }
	}
	decodeLine(t, info[0], &ver)
	if ver.Op != "info" || ver.URL != "local/plain" || ver.Versioning.Status != "" || ver.Versioning.MFADelete != "" {
		t.Fatalf("%+v", ver)
	}
	if out := e.ok("version", "info", "local/plain"); out != "local/plain is un-versioned\n" {
		t.Fatalf("%q", out)
	}
	if out := e.ok("version", "enable", "local/plain"); out != "local/plain versioning is enabled\n" {
		t.Fatalf("%q", out)
	}
	sus := e.jsonOK("version", "suspend", "local/plain")
	assertKeys(t, sus[0], "Op", "status", "url", "versioning", "status", "MFADelete")
	decodeLine(t, sus[0], &ver)
	if ver.Op != "suspend" || ver.Versioning.Status != "" {
		t.Fatalf("%+v", ver)
	}
	if out := e.ok("version", "info", "local/plain"); out != "local/plain versioning is suspended\n" {
		t.Fatalf("%q", out)
	}

	ready := e.jsonOK("ready", "local")
	assertKeys(t, ready[0], "status", "alias", "healthy", "maintenanceMode", "writeQuorum", "healingDrives", "error")
	var rd struct {
		Status, Alias string
		Healthy       bool
		Error         *string
	}
	decodeLine(t, ready[0], &rd)
	if rd.Status != "success" || rd.Alias != "local" || !rd.Healthy || rd.Error != nil {
		t.Fatalf("%+v", rd)
	}
	if out := e.ok("ready", "local"); out != "The cluster 'local' is ready\n" {
		t.Fatalf("%q", out)
	}

	if out := e.ok("--json", "cat", "local/plain/a.txt"); out != "abc" {
		t.Fatalf("cat --json wrapped the body: %q", out)
	}
	catMiss := e.jsonFail("cat", "local/plain/nope.txt")
	decodeLine(t, catMiss[0], &env)
	if env.Error.Type != "fatal" || env.Error.Message != "Unable to read from `local/plain/nope.txt`." {
		t.Fatalf("%+v", env)
	}
	e.put("plain/lines.txt", "l1\nl2\nl3\n")
	if out := e.ok("head", "-n", "2", "local/plain/lines.txt"); out != "l1\nl2\n" {
		t.Fatalf("%q", out)
	}
	if out := e.ok("head", "--lines=1", "local/plain/lines.txt"); out != "l1\n" {
		t.Fatalf("%q", out)
	}
	if out := e.ok("head", "-n", "0", "local/plain/lines.txt"); out != "" {
		t.Fatalf("head -n 0 printed the body: %q", out)
	}
	if out := e.ok("--json", "head", "local/plain/lines.txt"); out != "l1\nl2\nl3\n" {
		t.Fatalf("head --json: %q", out)
	}

	withStdin(t, "piped", func() {
		pipe := e.jsonOK("pipe", "local/plain/piped.txt")
		assertKeys(t, pipe[0], "status", "target", "size")
		var pr struct {
			Status, Target string
			Size           int64
		}
		decodeLine(t, pipe[0], &pr)
		if pr.Status != "success" || pr.Target != "local/plain/piped.txt" || pr.Size != 5 {
			t.Fatalf("%+v", pr)
		}
	})
	if out := e.ok("cat", "local/plain/piped.txt"); out != "piped" {
		t.Fatalf("%q", out)
	}

	found := e.jsonOK("find", "local/plain", "--name", "a.txt")
	assertKeys(t, found[0], "status", "type", "lastModified", "size", "key", "etag")
	var fr struct {
		Status, Type, Key, ETag string
		Last                    string `json:"lastModified"`
		Size                    int64
	}
	decodeLine(t, found[0], &fr)
	assertLocalTime(t, fr.Last, false)
	if fr.Status != "success" || fr.Type != "" || fr.ETag != "" || fr.Key != "local/plain/a.txt" || fr.Size != 3 {
		t.Fatalf("%+v", fr)
	}
	if out := e.ok("find", "local/plain", "--name", "x.txt"); out != "local/plain/a/x.txt\n" {
		t.Fatalf("%q", out)
	}
	du := e.jsonOK("du", "local/plain/dir")
	assertKeys(t, du[0], "prefix", "size", "objects", "status", "isVersions")
	var dur struct {
		Prefix, Status string
		Size           int64
		Objects        int
		IsVersions     bool
	}
	decodeLine(t, du[0], &dur)
	if dur.Prefix != "plain/dir" || dur.Size != 3 || dur.Objects != 1 || dur.Status != "success" || dur.IsVersions {
		t.Fatalf("%+v", dur)
	}
	if out := e.ok("du", "local/plain/dir"); out != "3B\t1 objects\tplain/dir\n" {
		t.Fatalf("%q", out)
	}

	tag := e.jsonOK("tag", "set", "local/plain/a.txt", "k1=v1&k2=v2")
	assertKeys(t, tag[0], "status", "name", "versionID")
	var tg struct{ Status, Name, VersionID string }
	decodeLine(t, tag[0], &tg)
	wantURL := e.ts.URL + "/plain/a.txt"
	if tg.Status != "success" || tg.Name != wantURL || tg.VersionID != "" {
		t.Fatalf("%+v", tg)
	}
	if out := e.ok("tag", "set", "local/plain/a.txt", "k1=v1&k2=v2"); out != "Tags set for "+wantURL+".\n" {
		t.Fatalf("%q", out)
	}
	listed := e.jsonOK("tag", "list", "local/plain/a.txt")
	assertKeys(t, listed[0], "tagset", "k1", "k2", "status", "url", "versionID")
	var tl struct {
		Status, URL, VersionID string
		Tagset                 map[string]string
	}
	decodeLine(t, listed[0], &tl)
	if tl.URL != wantURL || tl.VersionID != "" || tl.Tagset["k1"] != "v1" || tl.Tagset["k2"] != "v2" {
		t.Fatalf("%+v", tl)
	}
	off := e.jsonOK("tag", "remove", "local/plain/a.txt")
	assertKeys(t, off[0], "status", "name", "versionID")
	if out := e.ok("tag", "remove", "local/plain/b.txt"); !strings.Contains(out, "Tags removed for ") {
		t.Fatalf("%q", out)
	}
}

type errEnv struct {
	Status string
	Error  struct {
		Message string
		Cause   struct {
			Message string
			Error   json.RawMessage
		}
		Type string
	}
}

type statRow struct {
	Status, Name, LastModified, ETag, Type string
	Size                                   int64
	Metadata                               map[string]string
}

func TestEventJSON(t *testing.T) {
	e := newMCHooks(t, map[string]server.Webhook{"PRIMARY": {Endpoint: "http://127.0.0.1:1"}})
	e.ok("mb", "local/evt")
	arn := "arn:minio:sqs::PRIMARY:webhook"
	add := e.jsonOK("event", "add", "local/evt", arn, "--prefix", "in/", "--suffix", ".txt")
	assertKeys(t, add[0], "status", "bucket", "arn")
	ls := e.jsonOK("event", "ls", "local/evt")
	assertKeys(t, ls[0], "status", "arn", "event", "prefix", "suffix")
	var row struct {
		ARN, Prefix, Suffix string
		Event               []string
	}
	decodeLine(t, ls[0], &row)
	if row.ARN != arn || row.Prefix != "in/" || row.Suffix != ".txt" || !slices.Contains(row.Event, "s3:ObjectCreated:*") {
		t.Fatalf("%+v", row)
	}
	plain := e.ok("event", "ls", "local/evt")
	if !strings.Contains(plain, arn+"   s3:ObjectCreated:*,s3:ObjectRemoved:*,s3:ObjectAccessed:*   Filter: prefix=\"in/\"   Filter: suffix=\".txt\"") {
		t.Fatalf("%s", plain)
	}
	rm := e.jsonOK("event", "rm", "local/evt", arn)
	assertKeys(t, rm[0], "status", "bucket", "arn")
	miss := e.jsonFail("event", "ls", "local/nobucket")
	assertKeys(t, miss[0], "status", "error", "message", "cause", "message", "error", "Code", "Message", "BucketName", "Key", "Resource", "RequestID", "HostID", "Region", "Server", "type")
	var env errEnv
	decodeLine(t, miss[0], &env)
	if env.Error.Type != "fatal" || env.Error.Message != "Unable to list notifications on the specified bucket." || !strings.Contains(string(env.Error.Cause.Error), "NoSuchBucket") {
		t.Fatalf("%s", miss[0])
	}
}

// Flags before or after the command, --name=value, `--`, MC_JSON, and --config-dir.
func TestParserFlags(t *testing.T) {
	e := newMC(t)
	e.ok("mb", "local/bkt")
	e.put("bkt/a.txt", "abc")
	if out := e.ok("ls", "--json", "local/bkt"); !strings.Contains(out, `"key":"a.txt"`) {
		t.Fatalf("%s", out)
	}
	if out := e.ok("--json", "ls", "local/bkt"); !strings.Contains(out, `"key":"a.txt"`) {
		t.Fatalf("%s", out)
	}
	e.ok("mb", "local/term")
	if code, _, _ := e.run("mb", "--", "--ignore-existing", "local/term"); code == 0 {
		t.Fatal("`--` did not end flag parsing")
	}
	if code, _, errb := e.run("config", "host", "add", "eq", e.ts.URL, "admin", "admin-secret", "--api=S3v4"); code != 0 {
		t.Fatal(errb)
	}
	e.ok("mb", "eq/one")
	if code, out, errb := e.run("head", "--lines"); code != 1 || out != "" || !strings.Contains(errb, "flag needs an argument: -lines") {
		t.Fatalf("missing value: exit %d %q %q", code, out, errb)
	}
	if code, _, errb := e.run("--json", "head", "--lines"); code != 1 || !strings.Contains(errb, "flag needs an argument") || strings.HasPrefix(errb, "{") {
		t.Fatalf("missing value under --json: exit %d %q", code, errb)
	}
	e.ok("ls", "--debug", "--no-color", "--insecure", "--disable-pager", "--dp", "-q", "local/bkt")
	e.ok("ls", "--limit-upload", "10MiB", "--limit-download=1MiB", "-H", "X-A: b", "--resolve", "h:9000=127.0.0.1", "local/bkt")

	dir := t.TempDir()
	if code, _, errb := e.run("--config-dir="+dir, "alias", "set", "other", e.ts.URL, "admin", "admin-secret"); code != 0 {
		t.Fatal(errb)
	}
	if _, err := os.Stat(filepath.Join(dir, "ss33-mc.json")); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := e.run("mb", "other/x"); code == 0 {
		t.Fatal("--config-dir wrote into MC_CONFIG_DIR")
	}
	if code, _, errb := e.run("-C", dir, "mb", "other/zzz"); code != 0 {
		t.Fatal(errb)
	}

	t.Setenv("MC_JSON", "On")
	if out := e.ok("ls", "local/bkt"); !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("MC_JSON did not enable JSON: %s", out)
	}
}

// rm of a missing key fails, a delete-marker version is still removable, and objects sort before folders.
func TestExistenceAndOrder(t *testing.T) {
	e := newMC(t)
	e.ok("mb", "local/ord")
	e.put("ord/b.txt", "abc")
	e.put("ord/a/x.txt", "z")
	plain := splitLines(e.ok("ls", "local/ord"))
	if len(plain) != 2 || !strings.Contains(plain[0], "b.txt") || !strings.Contains(plain[1], "a/") {
		t.Fatalf("objects should precede folders:\n%s", strings.Join(plain, "\n"))
	}
	js := e.jsonOK("ls", "local/ord")
	var first, second struct{ Type, Key string }
	decodeLine(t, js[0], &first)
	decodeLine(t, js[1], &second)
	if first.Type != "file" || first.Key != "b.txt" || second.Type != "folder" || second.Key != "a/" {
		t.Fatalf("%+v %+v", first, second)
	}

	if code, out, _ := e.run("--json", "rm", "local/ord/missing.txt"); code != 1 || !strings.Contains(out, "Object does not exist") {
		t.Fatalf("rm missing: exit %d %s", code, out)
	}
	// As the official client: --force does not excuse a missing object, but an empty prefix is not an error.
	if code, _, _ := e.run("rm", "--force", "local/ord/missing.txt"); code != 1 {
		t.Fatalf("rm --force missing: exit %d", code)
	}
	if out := e.ok("rm", "--recursive", "--force", "--quiet", "local/ord/none/"); out != "" {
		t.Fatalf("rm --recursive --force on an empty prefix: %q", out)
	}

	e.ok("mb", "local/ver")
	e.ok("version", "enable", "local/ver")
	src := filepath.Join(t.TempDir(), "f.txt")
	for _, body := range []string{"one", "two"} {
		os.WriteFile(src, []byte(body), 0o644)
		e.ok("cp", src, "local/ver/f.txt")
	}
	if out := e.ok("rm", "local/ver/f.txt"); !strings.Contains(out, "Created delete marker `local/ver/f.txt`") {
		t.Fatalf("%q", out)
	}
	listing := e.ok("ls", "--versions", "local/ver")
	lines := splitLines(listing)
	if len(lines) != 3 || !strings.Contains(lines[0], " v3 DEL f.txt") {
		t.Fatalf("%s", listing)
	}
	marker := strings.Fields(lines[0])[5]
	vlines := splitLines(e.ok("ls", "--json", "--versions", "local/ver"))
	assertKeys(t, vlines[0], "status", "type", "lastModified", "size", "key", "etag", "url", "versionId", "versionOrdinal", "isDeleteMarker")
	var del struct {
		Key, ETag, VersionID string
		VersionOrdinal       int
		IsDeleteMarker       bool
	}
	decodeLine(t, vlines[0], &del)
	if del.Key != "f.txt" || del.ETag != "" || !del.IsDeleteMarker || del.VersionOrdinal != 3 || del.VersionID != marker {
		t.Fatalf("%+v marker %s", del, marker)
	}
	assertKeys(t, vlines[2], "status", "type", "lastModified", "size", "key", "etag", "url", "versionId", "versionOrdinal", "storageClass")
	var oldest struct{ VersionOrdinal int }
	decodeLine(t, vlines[2], &oldest)
	if oldest.VersionOrdinal != 1 {
		t.Fatalf("ordinal %d", oldest.VersionOrdinal)
	}
	if code, _, errb := e.run("rm", "--version-id", marker, "local/ver/f.txt"); code != 0 {
		t.Fatalf("delete-marker version: %s", errb)
	}
	if got := e.ok("ls", "local/ver"); !strings.Contains(got, "f.txt") {
		t.Fatalf("object did not return:\n%s", got)
	}
	e.ok("rb", "--force", "local/ver")
}

// cat/head stay raw under --json, flag errors stay on stderr, API errors are JSON on stdout.
func TestStreamsAndMetadata(t *testing.T) {
	e := newMC(t)
	e.ok("mb", "local/bkt")
	e.put("bkt/a.txt", "abc")
	code, out, errb := e.run("--json", "ls", "--bogus", "local/bkt")
	if code != 1 || out != "" || !strings.Contains(errb, "flag provided but not defined: -bogus") {
		t.Fatalf("flag error: exit %d stdout %q stderr %q", code, out, errb)
	}
	code, out, errb = e.run("--json", "stat", "local/bkt/nope.txt")
	if code != 1 || errb != "" || !strings.HasPrefix(out, "{") || !strings.Contains(out, `"type":"fatal"`) {
		t.Fatalf("api error: exit %d stdout %q stderr %q", code, out, errb)
	}
	if got := e.ok("--json", "cat", "local/bkt/a.txt"); got != "abc" {
		t.Fatalf("%q", got)
	}
	// The missing bucket is the destination's, not a piece of the local source path.
	src := filepath.Join(t.TempDir(), "f.txt")
	os.WriteFile(src, []byte("x"), 0o644)
	if code, _, errb := e.run("cp", src, "local/nobkt/f.txt"); code != 1 || !strings.Contains(errb, "Failed to copy") || !strings.Contains(errb, "Bucket `nobkt` does not exist.") {
		t.Fatalf("cp to a missing bucket: exit %d %q", code, errb)
	}
	withStdin(t, "xyz", func() {
		if code, out, errb := e.run("pipe", "local/bkt/p.txt"); code != 0 || out != "" {
			t.Fatalf("pipe: exit %d %q %s", code, out, errb)
		}
	})
	if got := e.ok("cat", "local/bkt/p.txt"); got != "xyz" {
		t.Fatalf("%q", got)
	}
	withStdin(t, `{"Version":"2012-10-17","Statement":[]}`, func() {
		code, out, errb := e.run("anonymous", "set-json", "/dev/stdin", "local/bkt")
		if code != 0 || !strings.Contains(out, "set from `/dev/stdin`") {
			t.Fatalf("set-json /dev/stdin: exit %d %q %s", code, out, errb)
		}
	})

	code, out, errb = e.run("mirror", "--bogus")
	if code != 1 || out != "" || !strings.Contains(errb, "`mirror` is not a recognized command") || !strings.Contains(errb, "ss33 implements:") || strings.Contains(errb, "flag provided but not defined") {
		t.Fatalf("unknown command: exit %d %q %q", code, out, errb)
	}
	code, out, errb = e.run("--json", "admin")
	if code != 1 || errb != "" || !strings.Contains(out, "not a recognized command") || !strings.Contains(out, `"type":"fatal"`) {
		t.Fatalf("json unknown: exit %d %q %q", code, out, errb)
	}
}

func TestVersionHelpUnknown(t *testing.T) {
	t.Setenv("MC_CONFIG_DIR", t.TempDir())
	code, out, errb := runRaw("--version")
	if code != 0 || errb != "" {
		t.Fatalf("exit %d %q %q", code, out, errb)
	}
	lines := splitLines(out)
	if len(lines) != 2 || lines[0] != "mc version ss33-dev (commit-id=ss33-dev)" || !strings.HasPrefix(lines[1], "Runtime: ") || strings.Contains(out, "Copyright") {
		t.Fatalf("%q", out)
	}
	code, out, errb = runRaw("-v")
	if code != 0 || splitLines(out)[0] != lines[0] {
		t.Fatalf("-v: %d %q", code, out)
	}
	code, out, errb = runRaw("--help")
	if code != 0 || out != "" || !strings.Contains(errb, "usage:") {
		t.Fatalf("help: %d %q %q", code, out, errb)
	}
	code, out, errb = runRaw()
	if code != 1 || out != "" || !strings.Contains(errb, "usage:") {
		t.Fatalf("no args: %d %q %q", code, out, errb)
	}
	code, out, errb = runRaw("diff")
	if code != 1 || out != "" || !strings.Contains(errb, "`diff` is not a recognized command") || !strings.Contains(errb, "ss33 implements:") {
		t.Fatalf("diff: %d %q %q", code, out, errb)
	}
}

func runRaw(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := mc.Main(args, &out, &errb)
	return code, out.String(), errb.String()
}
