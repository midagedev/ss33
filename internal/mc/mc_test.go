package mc_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
