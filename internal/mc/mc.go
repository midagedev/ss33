// Package mc is the subset of the MinIO client CLI that bootstrap scripts use, talking to any S3
// endpoint: alias set, mb, rb, ls, cp, anonymous set/get. Output mirrors mc closely enough that
// `mc ls --recursive ... | wc -l` style scripts keep working.
package mc

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/midagedev/ss33/internal/sigv4"
)

type alias struct {
	URL       string `json:"url"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

type config struct {
	Aliases map[string]alias `json:"aliases"`
}

func configPath() string {
	if dir := os.Getenv("MC_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "ss33-mc.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mc", "ss33-mc.json")
}

func loadConfig() config {
	c := config{Aliases: map[string]alias{}}
	if data, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(data, &c)
	}
	if c.Aliases == nil {
		c.Aliases = map[string]alias{}
	}
	return c
}

func (c config) save() error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, data, 0o600)
}

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if err := run(args, stdout); err != nil {
		fmt.Fprintf(stderr, "mc: <ERROR> %v\n", err)
		return 1
	}
	return 0
}

type flags map[string]bool

// parseArgs splits flags (any --x / -x) from positional arguments.
func parseArgs(args []string) (flags, []string) {
	f := flags{}
	var pos []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			f[strings.TrimLeft(a, "-")] = true
			continue
		}
		pos = append(pos, a)
	}
	return f, pos
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mc <alias|mb|rb|ls|cp|anonymous|ready> ...")
	}
	cfg := loadConfig()
	f, pos := parseArgs(args[1:])
	switch args[0] {
	case "alias":
		if len(pos) == 5 && pos[0] == "set" {
			cfg.Aliases[pos[1]] = alias{URL: strings.TrimSuffix(pos[2], "/"), AccessKey: pos[3], SecretKey: pos[4]}
			if err := cfg.save(); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Added `%s` successfully.\n", pos[1])
			return nil
		}
		return errors.New("usage: mc alias set ALIAS URL ACCESSKEY SECRETKEY")
	case "mb":
		for _, p := range pos {
			t, err := cfg.target(p)
			if err != nil {
				return err
			}
			if err := t.makeBucket(f["ignore-existing"] || f["p"]); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Bucket created successfully `%s`.\n", p)
		}
		return nil
	case "rb":
		for _, p := range pos {
			t, err := cfg.target(p)
			if err != nil {
				return err
			}
			if err := t.removeBucket(f["force"]); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "Removed `%s` successfully.\n", p)
		}
		return nil
	case "ls":
		if len(pos) != 1 {
			return errors.New("usage: mc ls [--recursive] TARGET")
		}
		if a, ok := cfg.Aliases[strings.TrimSuffix(pos[0], "/")]; ok {
			return target{alias: a}.listBuckets(stdout)
		}
		t, err := cfg.target(pos[0])
		if err != nil {
			return err
		}
		return t.list(stdout, f["recursive"] || f["r"])
	case "cp":
		if len(pos) != 2 {
			return errors.New("usage: mc cp [--recursive] SOURCE TARGET")
		}
		return cfg.copy(pos[0], pos[1], f["recursive"] || f["r"], stdout, f["quiet"] || f["q"])
	case "anonymous", "policy":
		if len(pos) == 3 && pos[0] == "set" {
			t, err := cfg.target(pos[2])
			if err != nil {
				return err
			}
			return t.setAnonymous(pos[1])
		}
		return errors.New("usage: mc anonymous set <download|public|none> TARGET")
	case "ready":
		if len(pos) != 1 {
			return errors.New("usage: mc ready ALIAS")
		}
		a, ok := cfg.Aliases[pos[0]]
		if !ok {
			return fmt.Errorf("unknown alias %q", pos[0])
		}
		resp, err := http.Get(a.URL + "/minio/health/ready")
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("not ready: %s", resp.Status)
		}
		return nil
	}
	return fmt.Errorf("unsupported command %q (ss33 implements alias, mb, rb, ls, cp, anonymous, ready)", args[0])
}

// target is an `alias/bucket/prefix` reference.
type target struct {
	alias  alias
	bucket string
	key    string
}

func (c config) target(ref string) (target, error) {
	name, rest, _ := strings.Cut(ref, "/")
	a, ok := c.Aliases[name]
	if !ok {
		return target{}, fmt.Errorf("unknown alias %q in %q", name, ref)
	}
	bucket, key, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return target{}, fmt.Errorf("no bucket in %q", ref)
	}
	return target{alias: a, bucket: bucket, key: key}, nil
}

func (c config) isRemote(ref string) bool {
	name, _, _ := strings.Cut(ref, "/")
	_, ok := c.Aliases[name]
	return ok
}

func (t target) do(method, key string, query url.Values, body io.Reader, size int64, header http.Header) (*http.Response, error) {
	u := t.alias.URL + "/" + t.bucket
	if key != "" {
		u += "/" + sigv4.URIEncode(key, false)
	}
	if len(query) > 0 {
		u += "?" + strings.ReplaceAll(query.Encode(), "+", "%20") // SigV4 canonical form encodes space as %20
	}
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	for k, v := range header {
		req.Header[k] = v
	}
	sigv4.Sign(req, sigv4.Credentials{AccessKey: t.alias.AccessKey, SecretKey: t.alias.SecretKey}, "us-east-1", time.Now())
	return http.DefaultClient.Do(req)
}

func (t target) call(method, key string, query url.Values, body io.Reader, size int64, header http.Header, okCodes ...int) error {
	resp, err := t.do(method, key, query, body, size, header)
	return checkResp(resp, err, okCodes...)
}

type s3Error struct {
	Code    string
	Message string
}

func checkResp(resp *http.Response, err error, okCodes ...int) error {
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for _, c := range okCodes {
		if resp.StatusCode == c {
			io.Copy(io.Discard, resp.Body)
			return nil
		}
	}
	var e s3Error
	xml.NewDecoder(resp.Body).Decode(&e)
	if e.Code == "" {
		e.Code = resp.Status
	}
	return fmt.Errorf("%s: %s", e.Code, e.Message)
}

func (t target) makeBucket(ignoreExisting bool) error {
	resp, err := t.do(http.MethodPut, "", nil, nil, 0, nil)
	if err == nil && ignoreExisting && resp.StatusCode == http.StatusConflict {
		resp.Body.Close()
		return nil
	}
	return checkResp(resp, err, http.StatusOK)
}

func (t target) setAnonymous(mode string) error {
	switch mode {
	case "none", "private":
		return t.call(http.MethodDelete, "", url.Values{"policy": {""}}, nil, 0, nil, http.StatusNoContent, http.StatusOK)
	case "download", "public":
		actions := `"s3:GetObject"`
		if mode == "public" {
			actions = `"s3:GetObject","s3:PutObject","s3:DeleteObject"`
		}
		policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":[%s],"Resource":["arn:aws:s3:::%s/*"]}]}`, actions, t.bucket)
		return t.call(http.MethodPut, "", url.Values{"policy": {""}}, strings.NewReader(policy), int64(len(policy)), nil, http.StatusNoContent, http.StatusOK)
	}
	return fmt.Errorf("unsupported anonymous mode %q", mode)
}

type listedObject struct {
	Key          string
	Size         int64
	LastModified string
}

// walk lists every object under t.key (recursive) or one level (delimiter "/").
func (t target) walk(recursive bool, fn func(o listedObject, isPrefix bool) error) error {
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {t.key}}
		if !recursive {
			q.Set("delimiter", "/")
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := t.do(http.MethodGet, "", q, nil, 0, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return checkResp(resp, nil, http.StatusOK)
		}
		var out struct {
			Contents              []listedObject
			CommonPrefixes        []struct{ Prefix string }
			IsTruncated           bool
			NextContinuationToken string
		}
		err = xml.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil {
			return err
		}
		for _, p := range out.CommonPrefixes {
			if err := fn(listedObject{Key: p.Prefix}, true); err != nil {
				return err
			}
		}
		for _, o := range out.Contents {
			if err := fn(o, false); err != nil {
				return err
			}
		}
		if !out.IsTruncated {
			return nil
		}
		token = out.NextContinuationToken
	}
}

// listBuckets prints one `[date]      0B <bucket>/` line per bucket, as `mc ls ALIAS` does.
func (t target) listBuckets(w io.Writer) error {
	resp, err := t.do(http.MethodGet, "", nil, nil, 0, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return checkResp(resp, nil, http.StatusOK)
	}
	defer resp.Body.Close()
	var out struct {
		Buckets []struct{ Name, CreationDate string } `xml:"Buckets>Bucket"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	for _, b := range out.Buckets {
		ts, _ := time.Parse("2006-01-02T15:04:05.000Z", b.CreationDate)
		fmt.Fprintf(w, "[%s] %7s %s/\n", ts.Local().Format("2006-01-02 15:04:05 MST"), "0B", b.Name)
	}
	return nil
}

func (t target) list(w io.Writer, recursive bool) error {
	return t.walk(recursive, func(o listedObject, isPrefix bool) error {
		name := strings.TrimPrefix(o.Key, t.key)
		if isPrefix {
			fmt.Fprintf(w, "[%s] %7s %s\n", strings.Repeat(" ", 23), "0B", name)
			return nil
		}
		ts, _ := time.Parse("2006-01-02T15:04:05.000Z", o.LastModified)
		fmt.Fprintf(w, "[%s] %7s STANDARD %s\n", ts.Local().Format("2006-01-02 15:04:05 MST"), humanSize(o.Size), name)
		return nil
	})
}

func (t target) removeBucket(force bool) error {
	if force {
		err := t.walk(true, func(o listedObject, _ bool) error {
			return t.call(http.MethodDelete, o.Key, nil, nil, 0, nil, http.StatusNoContent, http.StatusOK)
		})
		if err != nil {
			return err
		}
	}
	return t.call(http.MethodDelete, "", nil, nil, 0, nil, http.StatusNoContent, http.StatusOK)
}

func (c config) copy(src, dst string, recursive bool, stdout io.Writer, quiet bool) error {
	srcRemote, dstRemote := c.isRemote(src), c.isRemote(dst)
	switch {
	case !srcRemote && dstRemote:
		dt, err := c.target(dst)
		if err != nil {
			return err
		}
		info, err := os.Stat(src)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			key := dt.key
			if key == "" || strings.HasSuffix(dst, "/") {
				key += filepath.Base(src)
			}
			return dt.upload(src, key)
		}
		if !recursive {
			return fmt.Errorf("%q is a directory; use --recursive", src)
		}
		prefix := dt.key
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(src, p)
			return dt.upload(p, prefix+filepath.ToSlash(rel))
		})
	case srcRemote && dstRemote:
		st, err := c.target(src)
		if err != nil {
			return err
		}
		dt, err := c.target(dst)
		if err != nil {
			return err
		}
		if !recursive {
			key := dt.key
			if key == "" || strings.HasSuffix(dst, "/") {
				key += path.Base(st.key)
			}
			return dt.copyFrom(st.bucket, st.key, key)
		}
		return st.walk(true, func(o listedObject, _ bool) error {
			rel := strings.TrimPrefix(strings.TrimPrefix(o.Key, st.key), "/")
			key := dt.key
			if key != "" && !strings.HasSuffix(key, "/") {
				key += "/"
			}
			return dt.copyFrom(st.bucket, o.Key, key+rel)
		})
	case srcRemote && !dstRemote:
		st, err := c.target(src)
		if err != nil {
			return err
		}
		if !recursive {
			out := dst
			if info, err := os.Stat(dst); err == nil && info.IsDir() {
				out = filepath.Join(dst, path.Base(st.key))
			}
			return st.download(st.key, out)
		}
		return st.walk(true, func(o listedObject, _ bool) error {
			rel := strings.TrimPrefix(strings.TrimPrefix(o.Key, st.key), "/")
			return st.download(o.Key, filepath.Join(dst, filepath.FromSlash(rel)))
		})
	}
	return errors.New("local to local copy is not supported")
}

func (t target) upload(file, key string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	h := http.Header{}
	if ct := mime.TypeByExtension(filepath.Ext(file)); ct != "" {
		h.Set("Content-Type", ct)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	return t.call(http.MethodPut, key, nil, f, info.Size(), h, http.StatusOK)
}

func (t target) copyFrom(srcBucket, srcKey, key string) error {
	h := http.Header{"X-Amz-Copy-Source": {"/" + srcBucket + "/" + sigv4.URIEncode(srcKey, false)}}
	return t.call(http.MethodPut, key, nil, nil, 0, h, http.StatusOK)
}

func (t target) download(key, out string) error {
	resp, err := t.do(http.MethodGet, key, nil, nil, 0, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return checkResp(resp, nil, http.StatusOK)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
