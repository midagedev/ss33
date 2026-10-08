// Package mc is the subset of the MinIO client CLI that bootstrap scripts use, talking to any S3
// endpoint: alias set (and config host add, MC_HOST_*), mb, rb, ls (--versions), cp, rm (--version-id),
// anonymous/policy set, version enable/suspend, event add/ls/rm, ready. Output mirrors mc closely enough that
// `mc ls --recursive ... | wc -l` style scripts keep working.
package mc

import (
	"bytes"
	"cmp"
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
	// MC_HOST_<alias>=http://ACCESS:SECRET@host:port defines an alias without `mc alias set`.
	for _, kv := range os.Environ() {
		name, raw, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, "MC_HOST_") {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.User == nil {
			continue
		}
		secret, _ := u.User.Password()
		c.Aliases[strings.TrimPrefix(name, "MC_HOST_")] = alias{URL: u.Scheme + "://" + u.Host, AccessKey: u.User.Username(), SecretKey: secret}
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

// valueFlags take a value, as `--event put,delete` or `--event=put,delete`. --api and --path (from
// `config host add`) are read and ignored.
var valueFlags = map[string]bool{"api": true, "path": true, "event": true, "prefix": true, "suffix": true, "version-id": true}

// parseArgs splits flags (any --x / -x) and their values from positional arguments.
func parseArgs(args []string) (flags, map[string]string, []string) {
	f, values := flags{}, map[string]string{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
			f[name] = true
			if valueFlags[name] && !hasValue && i+1 < len(args) {
				i++
				value = args[i]
			}
			values[name] = value
			continue
		}
		pos = append(pos, a)
	}
	return f, values, pos
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mc <alias|mb|rb|ls|cp|rm|anonymous|version|ready> ...")
	}
	cfg := loadConfig()
	f, values, pos := parseArgs(args[1:])
	if args[0] == "config" && len(pos) > 0 && pos[0] == "host" { // mc config host add: the pre-2021 spelling
		args, pos = []string{"alias"}, pos[1:]
		if len(pos) > 0 && pos[0] == "add" {
			pos[0] = "set"
		}
	}
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
		if f["versions"] {
			return t.listVersions(stdout)
		}
		return t.list(stdout, f["recursive"] || f["r"])
	case "cp":
		if len(pos) != 2 {
			return errors.New("usage: mc cp [--recursive] SOURCE TARGET")
		}
		return cfg.copy(pos[0], pos[1], f["recursive"] || f["r"], stdout, f["quiet"] || f["q"])
	case "rm":
		for _, p := range pos {
			t, err := cfg.target(p)
			if err != nil {
				return err
			}
			name, _, _ := strings.Cut(p, "/")
			remove := func(key string) error {
				if err := t.call(http.MethodDelete, key, nil, nil, 0, nil, http.StatusNoContent, http.StatusOK); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "Removed `%s/%s/%s`.\n", name, t.bucket, key)
				return nil
			}
			switch {
			case values["version-id"] != "":
				if err := t.call(http.MethodDelete, t.key, url.Values{"versionId": {values["version-id"]}}, nil, 0, nil, http.StatusNoContent, http.StatusOK); err != nil {
					return err
				}
				fmt.Fprintf(stdout, "Removed `%s/%s/%s` (versionId=%s).\n", name, t.bucket, t.key, values["version-id"])
			case f["recursive"] || f["r"]:
				if !f["force"] {
					return errors.New("removal requires --force flag")
				}
				if err := t.walk(true, func(o listedObject, _ bool) error { return remove(o.Key) }); err != nil {
					return err
				}
			case t.key == "":
				return fmt.Errorf("%q is a bucket; use `rm --recursive --force` to empty it or `rb` to remove it", p)
			default:
				if err := remove(t.key); err != nil {
					return err
				}
			}
		}
		return nil
	case "version":
		status := map[string]string{"enable": "Enabled", "suspend": "Suspended"}
		if len(pos) != 2 || status[pos[0]] == "" {
			return errors.New("usage: mc version enable|suspend ALIAS/BUCKET")
		}
		t, err := cfg.target(pos[1])
		if err != nil {
			return err
		}
		body := `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>` + status[pos[0]] + `</Status></VersioningConfiguration>`
		if err := t.call(http.MethodPut, "", url.Values{"versioning": {""}}, strings.NewReader(body), int64(len(body)), nil, http.StatusOK); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s versioning is %sd\n", pos[1], pos[0])
		return nil
	case "event":
		if len(pos) < 2 {
			return errors.New("usage: mc event add|rm|ls ALIAS/BUCKET [ARN] [--event put,delete] [--prefix P] [--suffix S]")
		}
		t, err := cfg.target(pos[1])
		if err != nil {
			return err
		}
		arn := ""
		if len(pos) > 2 {
			arn = pos[2]
		}
		return t.event(pos[0], arn, values, f["force"], stdout)
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
	return fmt.Errorf("unsupported command %q (ss33 implements alias, mb, rb, ls, cp, rm, anonymous, version, event, ready)", args[0])
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
		// The statements the official mc writes for these modes.
		bucketActions, objectActions := `"s3:GetBucketLocation","s3:ListBucket"`, `"s3:GetObject"`
		if mode == "public" {
			bucketActions += `,"s3:ListBucketMultipartUploads"`
			objectActions = `"s3:AbortMultipartUpload","s3:DeleteObject","s3:GetObject","s3:ListMultipartUploadParts","s3:PutObject"`
		}
		policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
			`{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":[%s],"Resource":["arn:aws:s3:::%s"]},`+
			`{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":[%s],"Resource":["arn:aws:s3:::%s/*"]}]}`,
			bucketActions, t.bucket, objectActions, t.bucket)
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

// removeBucket deletes the bucket; with force it first removes every object version and delete marker, so
// a versioned bucket goes too.
func (t target) removeBucket(force bool) error {
	if force {
		err := t.walkVersions(func(v listedVersion) error {
			return t.call(http.MethodDelete, v.Key, url.Values{"versionId": {v.VersionId}}, nil, 0, nil, http.StatusNoContent, http.StatusOK)
		})
		if err != nil {
			return err
		}
	}
	return t.call(http.MethodDelete, "", nil, nil, 0, nil, http.StatusNoContent, http.StatusOK)
}

type listedVersion struct {
	XMLName      xml.Name
	Key          string
	VersionId    string
	IsLatest     bool
	LastModified string
	Size         int64
}

// walkVersions visits every version and delete marker under the target's prefix, newest first per key.
func (t target) walkVersions(fn func(v listedVersion) error) error {
	keyMarker, versionMarker := "", ""
	for {
		q := url.Values{"versions": {""}, "prefix": {t.key}}
		if keyMarker != "" {
			q.Set("key-marker", keyMarker)
			q.Set("version-id-marker", versionMarker)
		}
		resp, err := t.do(http.MethodGet, "", q, nil, 0, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return checkResp(resp, nil, http.StatusOK)
		}
		var out struct {
			Entries             []listedVersion `xml:",any"`
			IsTruncated         bool
			NextKeyMarker       string
			NextVersionIdMarker string
		}
		err = xml.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if err != nil {
			return err
		}
		for _, v := range out.Entries {
			if v.XMLName.Local != "Version" && v.XMLName.Local != "DeleteMarker" {
				continue
			}
			if err := fn(v); err != nil {
				return err
			}
		}
		if !out.IsTruncated {
			return nil
		}
		keyMarker, versionMarker = out.NextKeyMarker, out.NextVersionIdMarker
	}
}

// listVersions prints `mc ls --versions` lines: date, size, storage class, version ID, vN, PUT or DEL, key.
func (t target) listVersions(w io.Writer) error {
	var all []listedVersion
	if err := t.walkVersions(func(v listedVersion) error { all = append(all, v); return nil }); err != nil {
		return err
	}
	for i, v := range all {
		n := 1 // vN counts down to v1, the oldest version of the key
		for j := i + 1; j < len(all) && all[j].Key == v.Key; j++ {
			n++
		}
		op := "PUT"
		if v.XMLName.Local == "DeleteMarker" {
			op = "DEL"
		}
		ts, _ := time.Parse("2006-01-02T15:04:05.000Z", v.LastModified)
		fmt.Fprintf(w, "[%s] %7s STANDARD %s v%d %s %s\n", ts.Local().Format("2006-01-02 15:04:05 MST"), humanSize(v.Size), v.VersionId, n, op,
			strings.TrimPrefix(v.Key, t.key))
	}
	return nil
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
			rel := filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(o.Key, st.key), "/"))
			if !filepath.IsLocal(rel) {
				return fmt.Errorf("refusing to write key %q outside %s", o.Key, dst) // e.g. "../x"
			}
			return st.download(o.Key, filepath.Join(dst, rel))
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

type notificationConfig struct {
	XMLName xml.Name    `xml:"NotificationConfiguration"`
	NS      string      `xml:"xmlns,attr,omitempty"`
	Queues  []queueConf `xml:"QueueConfiguration"`
}

type queueConf struct {
	ID     string       `xml:"Id"`
	Queue  string       `xml:"Queue"`
	Events []string     `xml:"Event"`
	Rules  []filterRule `xml:"Filter>S3Key>FilterRule,omitempty"`
}

type filterRule struct{ Name, Value string }

// mcEvents maps `mc event --event` names to S3 event types.
var mcEvents = map[string]string{"put": "s3:ObjectCreated:*", "delete": "s3:ObjectRemoved:*", "get": "s3:ObjectAccessed:*"}

// event implements `mc event add|rm|ls`: read the bucket's notification configuration, change the queue
// entries for arn, and write it back.
func (t target) event(op, arn string, values map[string]string, force bool, w io.Writer) error {
	resp, err := t.do(http.MethodGet, "", url.Values{"notification": {""}}, nil, 0, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return checkResp(resp, nil, http.StatusOK)
	}
	var cfg notificationConfig
	err = xml.NewDecoder(resp.Body).Decode(&cfg)
	resp.Body.Close()
	if err != nil {
		return err
	}
	switch op {
	case "ls":
		for _, q := range cfg.Queues {
			if arn != "" && q.Queue != arn {
				continue
			}
			line := q.Queue + "   " + strings.Join(q.Events, ",")
			for _, r := range q.Rules {
				line += "   Filter: " + strings.ToLower(r.Name) + "=\"" + r.Value + "\""
			}
			fmt.Fprintln(w, line)
		}
		return nil
	case "add":
		if arn == "" {
			return errors.New("usage: mc event add ALIAS/BUCKET ARN [--event put,delete,get] [--prefix P] [--suffix S]")
		}
		q := queueConf{ID: fmt.Sprintf("ss33-%d", time.Now().UnixNano()), Queue: arn}
		for _, name := range strings.Split(cmp.Or(values["event"], "put,delete,get"), ",") {
			ev, ok := mcEvents[strings.TrimSpace(name)]
			if !ok {
				return fmt.Errorf("unsupported event %q (put, delete, get)", name)
			}
			q.Events = append(q.Events, ev)
		}
		if p := values["prefix"]; p != "" {
			q.Rules = append(q.Rules, filterRule{"prefix", p})
		}
		if s := values["suffix"]; s != "" {
			q.Rules = append(q.Rules, filterRule{"suffix", s})
		}
		cfg.Queues = append(cfg.Queues, q)
	case "rm":
		if arn == "" && !force {
			return errors.New("usage: mc event rm ALIAS/BUCKET ARN, or --force to remove every notification")
		}
		kept := cfg.Queues[:0]
		for _, q := range cfg.Queues {
			if arn != "" && q.Queue != arn {
				kept = append(kept, q)
			}
		}
		cfg.Queues = kept
	default:
		return fmt.Errorf("unsupported event command %q (add, rm, ls)", op)
	}
	cfg.NS = "http://s3.amazonaws.com/doc/2006-03-01/"
	body, err := xml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := t.call(http.MethodPut, "", url.Values{"notification": {""}}, bytes.NewReader(body), int64(len(body)), nil, http.StatusOK); err != nil {
		return err
	}
	verb := map[string]string{"add": "added", "rm": "removed"}[op]
	fmt.Fprintf(w, "Successfully %s %s\n", verb, cmp.Or(arn, "all notifications"))
	return nil
}
