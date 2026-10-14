// Package mc is the subset of the MinIO client CLI that bootstrap scripts use, talking to any S3
// endpoint. Output mirrors mc closely enough that `mc ls --recursive ... | wc -l` style scripts keep
// working, and `--json` prints one JSON object per line.
package mc

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/midagedev/ss33/internal/sigv4"
)

// Version is the mc build version, set with -ldflags -X. The default is dev.
var Version = "dev"

const usageText = "usage: mc [--json] <alias|anonymous|cat|config|cp|du|event|find|head|ls|mb|mv|pipe|policy|rb|ready|rm|stat|tag|version> ..."

// implemented commands, also the keys validated by the flag table below.
var commandFlags = map[string]map[string]bool{
	"alias":     {"api": true, "path": true},
	"config":    {"api": true, "path": true},
	"mb":        {"ignore-existing": true, "p": true},
	"rb":        {"force": true},
	"ls":        {"recursive": true, "versions": true},
	"cp":        {"recursive": true, "attr": true},
	"mv":        {"recursive": true},
	"rm":        {"recursive": true, "force": true, "version-id": true},
	"stat":      {"recursive": true, "version-id": true},
	"event":     {"event": true, "prefix": true, "suffix": true, "force": true},
	"head":      {"lines": true},
	"find":      {"name": true},
	"anonymous": {},
	"policy":    {},
	"version":   {},
	"ready":     {},
	"cat":       {},
	"pipe":      {},
	"du":        {},
	"tag":       {},
}

var globalFlags = map[string]bool{
	"json": true, "quiet": true, "no-color": true, "debug": true, "insecure": true,
	"disable-pager": true, "config-dir": true, "help": true,
}

var valueFlags = map[string]bool{
	"api": true, "path": true, "attr": true, "version-id": true,
	"event": true, "prefix": true, "suffix": true, "lines": true,
	"name": true, "config-dir": true,
}

// shortFlags maps each short or alternate spelling to the name the tables above use.
var shortFlags = map[string]string{
	"q": "quiet", "r": "recursive", "dp": "disable-pager", "C": "config-dir", "vid": "version-id", "n": "lines", "h": "help",
}

func implementedList() string {
	return strings.Join(slices.Sorted(maps.Keys(commandFlags)), ", ")
}

type alias struct {
	URL       string `json:"url"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

type config struct {
	Aliases map[string]alias `json:"aliases"`
}

type cli struct {
	stdout, stderr io.Writer
	json           bool
	configDir      string
	flags          map[string]bool
	values         map[string]string
	cfg            config
}

// errPrinted means the error message is already on stdout (JSON) or stderr (plain).
var errPrinted = errors.New("printed")

// Main runs the CLI and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	c := &cli{stdout: stdout, stderr: stderr}
	if err := c.execute(args); err != nil {
		if !errors.Is(err, errPrinted) {
			fmt.Fprintf(stderr, "mc: <ERROR> %v\n", err)
		}
		return 1
	}
	return 0
}

func (c *cli) execute(args []string) error {
	if len(args) == 0 {
		return errors.New(usageText)
	}
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		fmt.Fprintf(c.stdout, "mc version ss33-%s (commit-id=ss33-%s)\nRuntime: %s %s/%s\n",
			Version, Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return nil
	}
	flags, values, pos, given, perr := parseCLI(args)
	if perr != "" {
		fmt.Fprintf(c.stderr, "mc: <ERROR> %s\n", perr)
		return errPrinted
	}
	c.flags, c.values = flags, values
	if dir := values["config-dir"]; dir != "" {
		c.configDir = dir
	}
	if flags["json"] || envJSON() {
		c.json = true
	}
	cmd := ""
	if len(pos) > 0 {
		cmd = pos[0]
	}
	_, known := commandFlags[cmd]
	for _, raw := range given {
		if name := cmp.Or(shortFlags[raw], raw); globalFlags[name] || (known && commandFlags[cmd][name]) {
			continue
		}
		if known || cmd == "" {
			fmt.Fprintf(c.stderr, "mc: <ERROR> Invalid command usage, flag provided but not defined: -%s\n", raw)
			return errPrinted
		}
		break
	}
	if flags["help"] {
		fmt.Fprintln(c.stderr, usageText)
		return nil
	}
	if cmd == "" {
		return errors.New(usageText)
	}
	if !known {
		return c.unknown(cmd)
	}
	c.cfg = c.loadConfig()
	return c.finish(c.dispatch(pos))
}

func (c *cli) finish(err error) error {
	if err == nil || errors.Is(err, errPrinted) {
		return err
	}
	if c.json {
		return c.fail(err.Error(), "", "error", struct{}{})
	}
	return err
}

func envJSON() bool {
	switch strings.ToLower(os.Getenv("MC_JSON")) {
	case "1", "true", "on":
		return true
	}
	return false
}

// parseCLI splits flags from positionals. Flags may appear before or after the command.
// A bare `--` ends flag parsing. `--name=value` and `--name value` both work for value flags.
func parseCLI(args []string) (flags map[string]bool, values map[string]string, pos, given []string, flagErr string) {
	flags, values = map[string]bool{}, map[string]string{}
	end := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !end && a == "--" {
			end = true
			continue
		}
		if !end && len(a) > 1 && strings.HasPrefix(a, "-") {
			raw, val, has := strings.Cut(strings.TrimLeft(a, "-"), "=")
			if raw == "" {
				return nil, nil, nil, nil, "Invalid command usage, flag provided but not defined: -"
			}
			name := cmp.Or(shortFlags[raw], raw)
			if valueFlags[name] {
				if !has {
					if i+1 >= len(args) {
						return nil, nil, nil, nil, "Invalid command usage, flag needs an argument: -" + raw
					}
					i++
					val = args[i]
				}
				values[name] = val
			}
			flags[name] = true
			given = append(given, raw)
			continue
		}
		pos = append(pos, a)
	}
	return flags, values, pos, given, ""
}

func (c *cli) configPath() string {
	dir := c.configDir
	if dir == "" {
		dir = os.Getenv("MC_CONFIG_DIR")
	}
	if dir != "" {
		return filepath.Join(dir, "ss33-mc.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mc", "ss33-mc.json")
}

func (c *cli) loadConfig() config {
	cfg := config{Aliases: map[string]alias{}}
	if data, err := os.ReadFile(c.configPath()); err == nil {
		json.Unmarshal(data, &cfg)
	}
	if cfg.Aliases == nil {
		cfg.Aliases = map[string]alias{}
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
		cfg.Aliases[strings.TrimPrefix(name, "MC_HOST_")] = alias{URL: u.Scheme + "://" + u.Host, AccessKey: u.User.Username(), SecretKey: secret}
	}
	return cfg
}

func (c *cli) saveConfig() error {
	p := c.configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(c.cfg, "", "  ")
	return os.WriteFile(p, data, 0o600)
}

func (c *cli) emit(v any) error { return writeJSON(c.stdout, v) }

func (c *cli) fail(message, cause, typ string, body any) error {
	if body == nil {
		body = struct{}{}
	}
	if c.json {
		raw, err := marshalRaw(body)
		if err != nil {
			return err
		}
		if err := c.emit(errorLine{Status: "error", Error: errorBody{Message: message, Cause: errorCause{Message: cause, Error: raw}, Type: typ}}); err != nil {
			return err
		}
		return errPrinted
	}
	text := message
	if cause != "" {
		if !strings.HasSuffix(cause, ".") {
			cause += "."
		}
		text += " " + cause
	}
	fmt.Fprintf(c.stderr, "mc: <ERROR> %s\n", text)
	return errPrinted
}

func (c *cli) unknown(cmd string) error {
	msg := fmt.Sprintf("`%s` is not a recognized command. Get help using `--help` flag.", cmd)
	if c.json {
		return c.fail(msg, "", "fatal", struct{}{})
	}
	fmt.Fprintf(c.stderr, "mc: <ERROR> %s ss33 implements: %s\n", msg, implementedList())
	return errPrinted
}

func (c *cli) opErr(op, target string, err error) error {
	if err == nil || errors.Is(err, errPrinted) {
		return err
	}
	var se *s3err
	if !errors.As(err, &se) {
		return err
	}
	typ := "error"
	if op == "stat" || op == "cat" || op == "event" {
		typ = "fatal"
	}
	msg := opMessage(op, target, se)
	objectOp := op == "stat" || op == "cat" || op == "head" || op == "rm" || op == "tag" || op == "pipe" || op == "cp" || op == "mv"
	if se.Code == "NoSuchBucket" && op != "event" {
		b := se.Bucket
		if b == "" {
			b = bucketOf(target)
		}
		return c.fail(msg, fmt.Sprintf("Bucket `%s` does not exist.", b), typ, bucketMissing{Bucket: b})
	}
	if se.Code == "NoSuchKey" || (objectOp && se.Status == http.StatusNotFound) {
		return c.fail(msg, "Object does not exist", typ, struct{}{})
	}
	if se.Code != "" || se.Message != "" {
		cause := se.Message
		if cause == "" {
			cause = se.Error()
		}
		return c.fail(msg, cause, typ, se.payload())
	}
	return c.fail(msg, se.Error(), typ, struct{}{})
}

func opMessage(op, target string, se *s3err) string {
	switch op {
	case "ls":
		return "Unable to list folder."
	case "mb":
		return fmt.Sprintf("Unable to make bucket `%s`.", target)
	case "rb":
		if se != nil && (se.Code == "NoSuchBucket" || se.Status == http.StatusNotFound) {
			return fmt.Sprintf("Unable to validate target `%s`.", target)
		}
		return fmt.Sprintf("Unable to remove `%s`.", target)
	case "rm":
		return fmt.Sprintf("Failed to remove `%s`.", target)
	case "cat", "head":
		return fmt.Sprintf("Unable to read from `%s`.", target)
	case "stat":
		return fmt.Sprintf("Unable to stat `%s`.", target)
	case "cp", "mv":
		return fmt.Sprintf("Unable to copy `%s`.", target)
	case "find":
		return fmt.Sprintf("Unable to find `%s`.", target)
	case "du":
		return fmt.Sprintf("Unable to du `%s`.", target)
	case "tag":
		return fmt.Sprintf("Unable to tag `%s`.", target)
	case "pipe":
		return fmt.Sprintf("Unable to pipe `%s`.", target)
	case "anonymous":
		return fmt.Sprintf("Unable to set anonymous `%s`.", target)
	case "version":
		return fmt.Sprintf("Unable to set versioning `%s`.", target)
	default:
		return fmt.Sprintf("Unable to %s `%s`.", op, target)
	}
}

func bucketOf(ref string) string {
	_, rest, _ := strings.Cut(ref, "/")
	b, _, _ := strings.Cut(rest, "/")
	return b
}

func (c *cli) dispatch(pos []string) error {
	cmd, args := pos[0], pos[1:]
	if cmd == "config" && len(args) > 0 && args[0] == "host" { // mc config host add: the pre-2021 spelling
		cmd, args = "alias", args[1:]
		if len(args) > 0 && args[0] == "add" {
			args[0] = "set"
		}
	}
	switch cmd {
	case "alias":
		return c.aliasSet(args)
	case "mb":
		return c.mb(args)
	case "rb":
		return c.rb(args)
	case "ls":
		return c.ls(args)
	case "cp":
		return c.transfer(args, false)
	case "mv":
		return c.transfer(args, true)
	case "rm":
		return c.rm(args)
	case "stat":
		return c.stat(args)
	case "cat":
		return c.cat(args)
	case "head":
		return c.head(args)
	case "pipe":
		return c.pipe(args)
	case "find":
		return c.find(args)
	case "du":
		return c.du(args)
	case "tag":
		return c.tag(args)
	case "anonymous", "policy":
		return c.anonymous(args)
	case "version":
		return c.version(args)
	case "event":
		return c.event(args)
	case "ready":
		return c.ready(args)
	default:
		return c.unknown(cmd)
	}
}

func (c *cli) aliasSet(args []string) error {
	if len(args) != 5 || args[0] != "set" {
		return errors.New("usage: mc alias set ALIAS URL ACCESSKEY SECRETKEY")
	}
	c.cfg.Aliases[args[1]] = alias{URL: strings.TrimSuffix(args[2], "/"), AccessKey: args[3], SecretKey: args[4]}
	if err := c.saveConfig(); err != nil {
		return err
	}
	if c.json {
		return c.emit(aliasLine{Status: "success", Alias: args[1], URL: c.cfg.Aliases[args[1]].URL, AccessKey: args[3], SecretKey: args[4], API: "s3v4", Path: "auto"})
	}
	fmt.Fprintf(c.stdout, "Added `%s` successfully.\n", args[1])
	return nil
}

func (c *cli) mb(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mc mb [--ignore-existing] ALIAS/BUCKET")
	}
	ignore := c.flags["ignore-existing"] || c.flags["p"]
	for _, p := range args {
		t, err := c.cfg.target(p)
		if err != nil {
			return err
		}
		if err := t.makeBucket(ignore); err != nil {
			return c.opErr("mb", p, err)
		}
		if c.json {
			if err := c.emit(mbLine{Status: "success", Bucket: p, Region: ""}); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(c.stdout, "Bucket created successfully `%s`.\n", p)
		}
	}
	return nil
}

func (c *cli) rb(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mc rb [--force] ALIAS/BUCKET")
	}
	for _, p := range args {
		t, err := c.cfg.target(p)
		if err != nil {
			return err
		}
		if err := t.removeBucket(c.flags["force"]); err != nil {
			return c.opErr("rb", p, err)
		}
		if c.json {
			if err := c.emit(rbLine{Status: "success", Bucket: p}); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(c.stdout, "Removed `%s` successfully.\n", p)
		}
	}
	return nil
}

func (c *cli) ls(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: mc ls [--recursive] TARGET")
	}
	if a, ok := c.cfg.Aliases[strings.TrimSuffix(args[0], "/")]; ok && !strings.Contains(strings.TrimSuffix(args[0], "/"), "/") {
		return c.listBuckets(args[0], a)
	}
	t, err := c.cfg.target(args[0])
	if err != nil {
		return err
	}
	if c.flags["versions"] {
		return c.listVersions(t, args[0])
	}
	return c.list(t.asDir(), args[0], c.flags["recursive"])
}

func (c *cli) listBuckets(ref string, a alias) error {
	t := target{alias: a}
	resp, err := t.do(http.MethodGet, "", nil, nil, 0, nil)
	if err != nil {
		return c.opErr("ls", ref, err)
	}
	if resp.StatusCode != http.StatusOK {
		return c.opErr("ls", ref, checkResp(resp, nil, http.StatusOK))
	}
	defer resp.Body.Close()
	var out struct {
		Buckets []struct{ Name, CreationDate string } `xml:"Buckets>Bucket"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	base := strings.TrimRight(a.URL, "/") + "/"
	for _, b := range out.Buckets {
		if c.json {
			if err := c.emit(lsLine{Status: "success", Type: "folder", LastModified: formatLSTime(b.CreationDate), Size: 0, Key: b.Name + "/", ETag: "", URL: base, VersionOrdinal: 1}); err != nil {
				return err
			}
			continue
		}
		ts, _ := time.Parse("2006-01-02T15:04:05.000Z", b.CreationDate)
		fmt.Fprintf(c.stdout, "[%s] %7s %s/\n", ts.Local().Format("2006-01-02 15:04:05 MST"), "0B", b.Name)
	}
	return nil
}

func (c *cli) list(t target, ref string, recursive bool) error {
	u := t.listURL()
	err := t.forEachPage(recursive, func(objs, prefs []listedObject) error {
		for _, o := range objs {
			name := strings.TrimPrefix(o.Key, t.key)
			if c.json {
				sc := o.StorageClass
				if sc == "" {
					sc = "STANDARD"
				}
				if err := c.emit(lsLine{Status: "success", Type: "file", LastModified: formatLSTime(o.LastModified), Size: o.Size, Key: name, ETag: stripETag(o.ETag), URL: u, VersionOrdinal: 1, StorageClass: sc}); err != nil {
					return err
				}
				continue
			}
			ts, _ := time.Parse("2006-01-02T15:04:05.000Z", o.LastModified)
			fmt.Fprintf(c.stdout, "[%s] %7s STANDARD %s\n", ts.Local().Format("2006-01-02 15:04:05 MST"), humanSize(o.Size), name)
		}
		for _, p := range prefs {
			name := strings.TrimPrefix(p.Key, t.key)
			if c.json {
				if err := c.emit(lsLine{Status: "success", Type: "folder", LastModified: time.Now().Format(time.RFC3339Nano), Size: 0, Key: name, ETag: "", URL: u, VersionOrdinal: 1}); err != nil {
					return err
				}
				continue
			}
			fmt.Fprintf(c.stdout, "[%s] %7s %s\n", strings.Repeat(" ", 23), "0B", name)
		}
		return nil
	})
	if err != nil {
		return c.opErr("ls", ref, err)
	}
	return nil
}

func (c *cli) listVersions(t target, ref string) error {
	t = t.asDir()
	var all []listedVersion
	if err := t.walkVersions(func(v listedVersion) error { all = append(all, v); return nil }); err != nil {
		return c.opErr("ls", ref, err)
	}
	u := t.listURL()
	for i, v := range all {
		n := 1 // vN counts down to v1, the oldest version of the key
		for j := i + 1; j < len(all) && all[j].Key == v.Key; j++ {
			n++
		}
		name := strings.TrimPrefix(v.Key, t.key)
		del := v.XMLName.Local == "DeleteMarker"
		if c.json {
			line := lsLine{Status: "success", Type: "file", LastModified: formatLSTime(v.LastModified), Size: v.Size, Key: name, ETag: stripETag(v.ETag), URL: u, VersionID: v.VersionId, VersionOrdinal: n, IsDeleteMarker: del}
			if del {
				line.ETag = ""
			} else {
				line.StorageClass = "STANDARD"
			}
			if err := c.emit(line); err != nil {
				return err
			}
			continue
		}
		op := "PUT"
		if del {
			op = "DEL"
		}
		ts, _ := time.Parse("2006-01-02T15:04:05.000Z", v.LastModified)
		fmt.Fprintf(c.stdout, "[%s] %7s STANDARD %s v%d %s %s\n", ts.Local().Format("2006-01-02 15:04:05 MST"), humanSize(v.Size), v.VersionId, n, op, name)
	}
	return nil
}

func (c *cli) stat(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: mc stat TARGET")
	}
	ref := args[0]
	t, err := c.cfg.target(ref)
	if err != nil {
		return err
	}
	if c.flags["recursive"] || strings.HasSuffix(ref, "/") {
		dir := t.asDir()
		err := dir.walk(c.flags["recursive"], func(o listedObject, isPrefix bool) error {
			if isPrefix {
				return nil
			}
			obj := dir
			obj.key = o.Key
			return c.statObject(obj, dir.refKey(o.Key), strings.TrimPrefix(o.Key, dir.key))
		})
		if err != nil {
			return c.opErr("stat", ref, err)
		}
		return nil
	}
	if t.key == "" {
		return c.statBucket(t, ref)
	}
	return c.statObject(t, ref, path.Base(t.key))
}

func (c *cli) statBucket(t target, ref string) error {
	resp, err := t.do(http.MethodHead, "", nil, nil, 0, nil)
	if err != nil {
		return c.opErr("stat", ref, err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return c.fail(fmt.Sprintf("Unable to stat `%s`.", ref), fmt.Sprintf("Bucket `%s` does not exist.", t.bucket), "fatal", bucketMissing{Bucket: t.bucket})
	}
	if resp.StatusCode != http.StatusOK {
		return c.opErr("stat", ref, readS3Err(resp.StatusCode, nil))
	}
	if c.json {
		return c.emit(statBucketLine{Status: "success", Name: t.bucket + "/", LastModified: "", Size: 0})
	}
	fmt.Fprintf(c.stdout, "%-10s: %s/\n%-10s: %s\n", "Name", t.bucket, "Size", humanSize(0))
	return nil
}

func (c *cli) statObject(t target, ref, name string) error {
	q := url.Values{}
	if vid := c.values["version-id"]; vid != "" {
		q.Set("versionId", vid)
	}
	h, status, err := t.head(t.key, q)
	if status == http.StatusNotFound {
		return c.fail(fmt.Sprintf("Unable to stat `%s`.", ref), "Object does not exist", "fatal", struct{}{})
	}
	if err != nil {
		return c.opErr("stat", ref, err)
	}
	var when time.Time
	if lm := h.Get("Last-Modified"); lm != "" {
		when, _ = time.Parse(http.TimeFormat, lm)
	}
	size, _ := strconv.ParseInt(h.Get("Content-Length"), 10, 64)
	etag := stripETag(h.Get("ETag"))
	vid := h.Get("X-Amz-Version-Id")
	meta := statMeta(h)
	if c.json {
		mod := ""
		if !when.IsZero() {
			mod = when.Local().Format(time.RFC3339)
		}
		return c.emit(statLine{Status: "success", Name: name, LastModified: mod, Size: size, ETag: etag, Type: "file", Metadata: meta, VersionID: vid})
	}
	writePlainStat(c.stdout, name, when, size, etag, vid, meta)
	return nil
}

func (c *cli) cat(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mc cat TARGET...")
	}
	for _, ref := range args {
		if err := c.writeObject(ref, -1); err != nil {
			return err
		}
	}
	return nil
}

func (c *cli) head(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: mc head [-n N] TARGET")
	}
	n := 10
	if s := c.values["lines"]; s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			return fmt.Errorf("invalid line count %q", s)
		}
		n = v
	}
	return c.writeObject(args[0], n)
}

// writeObject copies an object to stdout: the first limit lines (head), or the whole body when limit < 0 (cat).
func (c *cli) writeObject(ref string, limit int) error {
	t, err := c.cfg.target(ref)
	if err != nil {
		return err
	}
	resp, err := t.do(http.MethodGet, t.key, nil, nil, 0, nil)
	if err != nil {
		return c.opErr("cat", ref, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		op := "cat"
		if limit >= 0 {
			op = "head"
		}
		return c.opErr(op, ref, readS3Err(resp.StatusCode, b))
	}
	if limit < 0 {
		_, err := io.Copy(c.stdout, resp.Body)
		return err
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 10<<20)
	for n := 0; n < limit && sc.Scan(); n++ {
		fmt.Fprintln(c.stdout, sc.Text())
	}
	return sc.Err()
}

func (c *cli) pipe(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: mc pipe TARGET")
	}
	t, err := c.cfg.target(args[0])
	if err != nil {
		return err
	}
	// ponytail: buffers the whole body in memory; upgrade path is a multipart upload.
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	h := http.Header{"Content-Type": {"application/octet-stream"}}
	if err := t.call(http.MethodPut, t.key, nil, bytes.NewReader(body), int64(len(body)), h, http.StatusOK); err != nil {
		return c.opErr("pipe", args[0], err)
	}
	if c.json {
		return c.emit(pipeLine{Status: "success", Target: args[0], Size: int64(len(body))})
	}
	return nil
}

func (c *cli) find(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: mc find TARGET [--name GLOB]")
	}
	t, err := c.cfg.target(args[0])
	if err != nil {
		return err
	}
	t = t.asDir()
	glob := c.values["name"]
	err = t.walk(true, func(o listedObject, isPrefix bool) error {
		if isPrefix {
			return nil
		}
		if glob != "" {
			ok, err := path.Match(glob, path.Base(o.Key))
			if err != nil || !ok {
				return err
			}
		}
		full := t.refKey(o.Key)
		if c.json {
			return c.emit(findLine{Status: "success", Type: "", LastModified: formatLSTime(o.LastModified), Size: o.Size, Key: full, ETag: ""})
		}
		fmt.Fprintln(c.stdout, full)
		return nil
	})
	if err != nil {
		return c.opErr("find", args[0], err)
	}
	return nil
}

func (c *cli) du(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: mc du TARGET")
	}
	t, err := c.cfg.target(args[0])
	if err != nil {
		return err
	}
	t = t.asDir()
	var n int
	var size int64
	if err := t.walk(true, func(o listedObject, isPrefix bool) error {
		if !isPrefix {
			n++
			size += o.Size
		}
		return nil
	}); err != nil {
		return c.opErr("du", args[0], err)
	}
	prefix := t.bucket
	if t.key != "" {
		prefix += "/" + strings.Trim(t.key, "/")
	}
	if c.json {
		return c.emit(duLine{Prefix: prefix, Size: size, Objects: n, Status: "success", IsVersions: false})
	}
	fmt.Fprintf(c.stdout, "%s\t%d objects\t%s\n", humanSize(size), n, prefix)
	return nil
}

func (c *cli) tag(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: mc tag set|list|remove TARGET [k=v&k2=v2]")
	}
	switch args[0] {
	case "set":
		if len(args) != 3 {
			return errors.New("usage: mc tag set TARGET k=v&k2=v2")
		}
		return c.tagSet(args[1], args[2])
	case "list":
		if len(args) != 2 {
			return errors.New("usage: mc tag list TARGET")
		}
		return c.tagList(args[1])
	case "remove":
		if len(args) != 2 {
			return errors.New("usage: mc tag remove TARGET")
		}
		return c.tagRemove(args[1])
	default:
		return fmt.Errorf("unsupported tag command %q (set, list, remove)", args[0])
	}
}

func (c *cli) tagURL(t target) string {
	return strings.TrimRight(t.alias.URL, "/") + "/" + t.bucket + "/" + t.key
}

func (c *cli) tagSet(ref, spec string) error {
	t, err := c.cfg.target(ref)
	if err != nil {
		return err
	}
	q, err := url.ParseQuery(spec)
	if err != nil {
		return err
	}
	body := taggingXML{NS: "http://s3.amazonaws.com/doc/2006-03-01/"}
	for _, k := range slices.Sorted(maps.Keys(q)) {
		body.Tags = append(body.Tags, tagPair{Key: k, Value: q.Get(k)})
	}
	raw, err := xml.Marshal(body)
	if err != nil {
		return err
	}
	h, _, err := t.exchange(http.MethodPut, t.key, url.Values{"tagging": {""}}, bytes.NewReader(raw), int64(len(raw)), nil, http.StatusOK)
	if err != nil {
		return c.opErr("tag", ref, err)
	}
	return c.tagWrote(t, h, "Tags set for %s.\n")
}

func (c *cli) tagRemove(ref string) error {
	t, err := c.cfg.target(ref)
	if err != nil {
		return err
	}
	h, _, err := t.exchange(http.MethodDelete, t.key, url.Values{"tagging": {""}}, nil, 0, nil, http.StatusNoContent, http.StatusOK)
	if err != nil {
		return c.opErr("tag", ref, err)
	}
	return c.tagWrote(t, h, "Tags removed for %s.\n")
}

func (c *cli) tagWrote(t target, h http.Header, plain string) error {
	u := c.tagURL(t)
	vid := ""
	if h != nil {
		vid = h.Get("X-Amz-Version-Id")
	}
	if c.json {
		return c.emit(tagChangeLine{Status: "success", Name: u, VersionID: vid})
	}
	fmt.Fprintf(c.stdout, plain, u)
	return nil
}

func (c *cli) tagList(ref string) error {
	t, err := c.cfg.target(ref)
	if err != nil {
		return err
	}
	h, body, err := t.exchange(http.MethodGet, t.key, url.Values{"tagging": {""}}, nil, 0, nil, http.StatusOK)
	if err != nil {
		return c.opErr("tag", ref, err)
	}
	var doc taggingXML
	xml.Unmarshal(body, &doc)
	tags := map[string]string{}
	for _, p := range doc.Tags {
		tags[p.Key] = p.Value
	}
	u := c.tagURL(t)
	vid := ""
	if h != nil {
		vid = h.Get("X-Amz-Version-Id")
	}
	if c.json {
		return c.emit(tagListLine{Tagset: tags, Status: "success", URL: u, VersionID: vid})
	}
	keys := slices.Sorted(maps.Keys(tags))
	width := len("Name")
	for _, k := range keys {
		if len(k) > width {
			width = len(k)
		}
	}
	fmt.Fprintf(c.stdout, "%-*s : %s\n", width, "Name", u)
	for _, k := range keys {
		fmt.Fprintf(c.stdout, "%-*s : %s\n", width, k, tags[k])
	}
	return nil
}

type taggingXML struct {
	XMLName xml.Name  `xml:"Tagging"`
	NS      string    `xml:"xmlns,attr,omitempty"`
	Tags    []tagPair `xml:"TagSet>Tag"`
}

type tagPair struct{ Key, Value string }

func (c *cli) anonymous(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mc anonymous set|get|get-json|set-json|list ...")
	}
	switch args[0] {
	case "set":
		if len(args) != 3 {
			return errors.New("usage: mc anonymous set <download|public|upload|none> TARGET")
		}
		t, err := c.cfg.target(args[2])
		if err != nil {
			return err
		}
		if err := t.setAnonymous(args[1]); err != nil {
			return c.opErr("anonymous", args[2], err)
		}
		if c.json {
			return c.emit(anonLine{Operation: "set", Status: "success", Bucket: args[2], Permission: args[1]})
		}
		fmt.Fprintf(c.stdout, "Access permission for `%s` is set to `%s`\n", args[2], args[1])
		return nil
	case "get", "get-json":
		if len(args) != 2 {
			return errors.New("usage: mc anonymous get TARGET")
		}
		return c.anonGet(args[0], args[1])
	case "set-json":
		if len(args) != 3 {
			return errors.New("usage: mc anonymous set-json FILE TARGET")
		}
		return c.anonSetJSON(args[1], args[2])
	case "list":
		if len(args) != 2 {
			return errors.New("usage: mc anonymous list TARGET")
		}
		return c.anonList(args[1])
	default:
		return fmt.Errorf("unsupported anonymous command %q", args[0])
	}
}

func (c *cli) fetchPolicy(t target, ref string) (perm string, raw []byte, err error) {
	resp, err := t.do(http.MethodGet, "", url.Values{"policy": {""}}, nil, 0, nil)
	if err != nil {
		return "", nil, c.opErr("anonymous", ref, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		return classifyPolicy(t.bucket, body), body, nil
	}
	se := readS3Err(resp.StatusCode, body)
	if se.Code == "NoSuchBucketPolicy" {
		return "private", nil, nil
	}
	return "", nil, c.opErr("anonymous", ref, se)
}

func (c *cli) anonGet(op, ref string) error {
	t, err := c.cfg.target(ref)
	if err != nil {
		return err
	}
	perm, raw, err := c.fetchPolicy(t, ref)
	if err != nil {
		return err
	}
	if c.json {
		line := anonLine{Operation: op, Status: "success", Bucket: ref, Permission: perm}
		if perm != "private" && len(raw) > 0 {
			line.Anonymous = json.RawMessage(bytes.TrimSpace(raw))
		}
		return c.emit(line)
	}
	if op == "get-json" {
		if perm == "private" || len(raw) == 0 {
			fmt.Fprintln(c.stdout, "{}")
			return nil
		}
		var buf bytes.Buffer
		if json.Compact(&buf, raw) == nil {
			fmt.Fprintln(c.stdout, buf.String())
		} else {
			fmt.Fprintf(c.stdout, "%s\n", bytes.TrimSpace(raw))
		}
		return nil
	}
	fmt.Fprintf(c.stdout, "Access permission for `%s` is `%s`\n", ref, perm)
	return nil
}

func (c *cli) anonSetJSON(file, ref string) error {
	t, err := c.cfg.target(ref)
	if err != nil {
		return err
	}
	data, err := readInput(file)
	if err != nil {
		return c.fail(fmt.Sprintf("Invalid access file: `%s`.", file), "", "error", struct{}{})
	}
	if err := t.call(http.MethodPut, "", url.Values{"policy": {""}}, bytes.NewReader(data), int64(len(data)), nil, http.StatusNoContent, http.StatusOK); err != nil {
		return c.opErr("anonymous", ref, err)
	}
	if c.json {
		return c.emit(anonLine{Operation: "set-json", Status: "success", Bucket: ref, Permission: file})
	}
	fmt.Fprintf(c.stdout, "Access permission for `%s` is set from `%s`\n", ref, file)
	return nil
}

func (c *cli) anonList(ref string) error {
	t, err := c.cfg.target(ref)
	if err != nil {
		return err
	}
	perm, raw, err := c.fetchPolicy(t, ref)
	if err != nil {
		return err
	}
	if perm == "private" {
		return nil
	}
	type row struct{ res, allow string }
	var rows []row
	if allow, ok := map[string]string{"download": "readonly", "public": "readwrite", "upload": "writeonly"}[perm]; ok {
		rows = []row{{t.bucket + "/*", allow}}
	} else {
		for _, res := range policyResources(raw) {
			rows = append(rows, row{strings.TrimPrefix(res, "arn:aws:s3:::"), "custom"})
		}
	}
	for _, r := range rows {
		if c.json {
			if err := c.emit(anonListLine{Resource: r.res, Allow: r.allow}); err != nil {
				return err
			}
			continue
		}
		fmt.Fprintf(c.stdout, "%s => %s\n", r.res, r.allow)
	}
	return nil
}

func readInput(name string) ([]byte, error) {
	if name == "-" || name == "/dev/stdin" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(name)
}

func (c *cli) version(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: mc version enable|suspend|info ALIAS/BUCKET")
	}
	t, err := c.cfg.target(args[1])
	if err != nil {
		return err
	}
	switch args[0] {
	case "enable", "suspend":
		status := map[string]string{"enable": "Enabled", "suspend": "Suspended"}[args[0]]
		body := `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>` + status + `</Status></VersioningConfiguration>`
		if err := t.call(http.MethodPut, "", url.Values{"versioning": {""}}, strings.NewReader(body), int64(len(body)), nil, http.StatusOK); err != nil {
			return c.opErr("version", args[1], err)
		}
		return c.versionOut(args[0], args[1], "", "")
	case "info":
		_, body, err := t.exchange(http.MethodGet, "", url.Values{"versioning": {""}}, nil, 0, nil, http.StatusOK)
		if err != nil {
			return c.opErr("version", args[1], err)
		}
		var v struct{ Status, MFADelete string }
		xml.Unmarshal(body, &v)
		return c.versionOut("info", args[1], v.Status, v.MFADelete)
	default:
		return errors.New("usage: mc version enable|suspend|info ALIAS/BUCKET")
	}
}

func (c *cli) versionOut(op, ref, status, mfa string) error {
	if c.json {
		line := versionLine{Op: op, Status: "success", URL: ref}
		if op == "info" {
			line.Versioning.Status = status
		}
		line.Versioning.MFADelete = mfa
		return c.emit(line)
	}
	if op == "info" && status == "" {
		fmt.Fprintf(c.stdout, "%s is un-versioned\n", ref)
		return nil
	}
	word := strings.ToLower(status) // Enabled, Suspended
	if op != "info" {
		word = op + "d" // enable, suspend
	}
	fmt.Fprintf(c.stdout, "%s versioning is %s\n", ref, word)
	return nil
}

func (c *cli) ready(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: mc ready ALIAS")
	}
	a, ok := c.cfg.Aliases[args[0]]
	if !ok {
		return fmt.Errorf("unknown alias %q", args[0])
	}
	resp, err := http.Get(a.URL + "/minio/health/ready")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("not ready: %s", resp.Status)
	}
	if c.json {
		return c.emit(readyLine{Status: "success", Alias: args[0], Healthy: true, MaintenanceMode: false, WriteQuorum: 0, HealingDrives: 0})
	}
	fmt.Fprintf(c.stdout, "The cluster '%s' is ready\n", args[0])
	return nil
}

func (c *cli) rm(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mc rm TARGET")
	}
	for _, p := range args {
		t, err := c.cfg.target(p)
		if err != nil {
			return err
		}
		vid := c.values["version-id"]
		switch {
		case vid != "":
			if err := c.removeOne(t, p, t.key, vid, false); err != nil {
				return err
			}
		case c.flags["recursive"]:
			if !c.flags["force"] {
				return errors.New("removal requires --force flag")
			}
			err := t.walk(true, func(o listedObject, isPrefix bool) error {
				if isPrefix {
					return nil
				}
				return c.removeOne(t, t.refKey(o.Key), o.Key, "", true)
			})
			if err != nil {
				return err
			}
		case t.key == "":
			return fmt.Errorf("%q is a bucket; use `rm --recursive --force` to empty it or `rb` to remove it", p)
		default:
			if err := c.removeOne(t, p, t.key, "", false); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeOne deletes one object or version. A key that did not come from a listing is checked first: an S3
// DELETE of a missing key succeeds, and the official client reports it as missing.
func (c *cli) removeOne(t target, shown, key, vid string, listed bool) error {
	q := url.Values{}
	if vid != "" {
		q.Set("versionId", vid)
	}
	if !listed {
		if _, status, err := t.head(key, q); status == http.StatusNotFound {
			return c.fail(fmt.Sprintf("Failed to remove `%s`.", shown), "Object does not exist", "error", struct{}{})
		} else if err != nil {
			return c.opErr("rm", shown, err)
		}
	}
	h, err := t.del(key, q)
	if err != nil {
		return c.opErr("rm", shown, err)
	}
	marker := h.Get("X-Amz-Delete-Marker") == "true"
	gotVid := h.Get("X-Amz-Version-Id")
	if c.json {
		return c.emit(rmLine{Status: "success", Key: shown, DeleteMarker: marker, VersionID: gotVid, DryRun: false})
	}
	if marker {
		fmt.Fprintf(c.stdout, "Created delete marker `%s` (versionId=%s).\n", shown, gotVid)
		return nil
	}
	if vid != "" {
		fmt.Fprintf(c.stdout, "Removed `%s` (versionId=%s).\n", shown, vid)
		return nil
	}
	fmt.Fprintf(c.stdout, "Removed `%s`.\n", shown)
	return nil
}

type copyJob struct {
	srcShow, dstShow string
	size             int64
	localSrc         string
	src              *target
	srcKey           string
	dst              target
	dstKey           string
}

func (c *cli) transfer(args []string, move bool) error {
	if len(args) != 2 {
		if move {
			return errors.New("usage: mc mv [--recursive] SOURCE TARGET")
		}
		return errors.New("usage: mc cp [--recursive] SOURCE TARGET")
	}
	src, dst := args[0], args[1]
	recursive := c.flags["recursive"]
	srcRemote, dstRemote := c.cfg.isRemote(src), c.cfg.isRemote(dst)
	if move && !srcRemote {
		info, err := os.Stat(src)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return fmt.Errorf("%q is a directory; move of a local directory is not supported", src)
		}
	}
	extra := parseAttr(c.values["attr"])
	var jobs []copyJob
	var err error
	switch {
	case !srcRemote && dstRemote:
		jobs, err = c.jobsUpload(src, dst, recursive)
	case srcRemote && dstRemote:
		jobs, err = c.jobsServerCopy(src, dst, recursive)
	case srcRemote && !dstRemote:
		jobs, err = c.jobsDownload(src, dst, recursive)
	default:
		return errors.New("local to local copy is not supported")
	}
	if err != nil {
		return err
	}
	start := time.Now()
	var total int64
	for _, job := range jobs {
		if err := c.runJob(job, extra); err != nil {
			return c.opErr("cp", job.srcShow, err)
		}
		total += job.size
		if c.json {
			if err := c.emit(cpLine{Status: "success", Source: job.srcShow, Target: job.dstShow, Size: job.size, TotalCount: len(jobs), TotalSize: 0}); err != nil {
				return err
			}
		}
		if move {
			if job.localSrc != "" {
				if err := os.Remove(job.localSrc); err != nil {
					return err
				}
			} else if job.src != nil {
				if _, err := job.src.del(job.srcKey, nil); err != nil {
					return c.opErr("mv", job.srcShow, err)
				}
			}
		}
	}
	if c.json {
		ns := time.Since(start).Nanoseconds()
		var speed float64
		if ns > 0 {
			speed = float64(total) / (float64(ns) / 1e9)
		}
		return c.emit(cpSummary{Status: "success", Total: total, Transferred: total, Duration: ns, Speed: speed})
	}
	return nil
}

func parseAttr(s string) http.Header {
	h := http.Header{}
	if s == "" {
		return h
	}
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(k) == "" {
			continue
		}
		h.Set(strings.TrimSpace(k), v)
	}
	return h
}

func (c *cli) runJob(job copyJob, extra http.Header) error {
	switch {
	case job.localSrc != "":
		return job.dst.upload(job.localSrc, job.dstKey, extra)
	case job.src != nil && job.dst.bucket != "":
		return job.dst.copyFrom(job.src.bucket, job.srcKey, job.dstKey, extra)
	case job.src != nil:
		_, err := job.src.download(job.srcKey, job.dstShow)
		return err
	default:
		return errors.New("empty copy job")
	}
}

func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func (c *cli) jobsUpload(src, dst string, recursive bool) ([]copyJob, error) {
	dt, err := c.cfg.target(dst)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(src)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		key := dt.key
		if key == "" || strings.HasSuffix(dst, "/") {
			key += filepath.Base(src)
		}
		return []copyJob{{srcShow: absPath(src), dstShow: dt.refKey(key), size: info.Size(), localSrc: src, dst: dt, dstKey: key}}, nil
	}
	if !recursive {
		return nil, fmt.Errorf("%q is a directory; use --recursive", src)
	}
	prefix := dt.key
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var jobs []copyJob
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		key := prefix + filepath.ToSlash(rel)
		st, err := d.Info()
		if err != nil {
			return err
		}
		jobs = append(jobs, copyJob{srcShow: absPath(p), dstShow: dt.refKey(key), size: st.Size(), localSrc: p, dst: dt, dstKey: key})
		return nil
	})
	return jobs, err
}

func (c *cli) jobsServerCopy(src, dst string, recursive bool) ([]copyJob, error) {
	st, err := c.cfg.target(src)
	if err != nil {
		return nil, err
	}
	dt, err := c.cfg.target(dst)
	if err != nil {
		return nil, err
	}
	if !recursive {
		key := dt.key
		if key == "" || strings.HasSuffix(dst, "/") {
			key += path.Base(st.key)
		}
		h, status, err := st.head(st.key, nil)
		if err != nil && status != http.StatusOK {
			return nil, err
		}
		size, _ := strconv.ParseInt(h.Get("Content-Length"), 10, 64)
		return []copyJob{{srcShow: src, dstShow: dt.refKey(key), size: size, src: &st, srcKey: st.key, dst: dt, dstKey: key}}, nil
	}
	var jobs []copyJob
	err = st.walk(true, func(o listedObject, isPrefix bool) error {
		if isPrefix {
			return nil
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(o.Key, st.key), "/")
		key := dt.key
		if key != "" && !strings.HasSuffix(key, "/") {
			key += "/"
		}
		key += rel
		srcT := st
		jobs = append(jobs, copyJob{srcShow: st.refKey(o.Key), dstShow: dt.refKey(key), size: o.Size, src: &srcT, srcKey: o.Key, dst: dt, dstKey: key})
		return nil
	})
	return jobs, err
}

func (c *cli) jobsDownload(src, dst string, recursive bool) ([]copyJob, error) {
	st, err := c.cfg.target(src)
	if err != nil {
		return nil, err
	}
	if !recursive {
		out := dst
		if info, err := os.Stat(dst); err == nil && info.IsDir() {
			out = filepath.Join(dst, path.Base(st.key))
		}
		h, status, err := st.head(st.key, nil)
		if err != nil && status != http.StatusOK {
			return nil, err
		}
		size, _ := strconv.ParseInt(h.Get("Content-Length"), 10, 64)
		return []copyJob{{srcShow: src, dstShow: out, size: size, src: &st, srcKey: st.key}}, nil
	}
	var jobs []copyJob
	err = st.walk(true, func(o listedObject, isPrefix bool) error {
		if isPrefix {
			return nil
		}
		rel := filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(o.Key, st.key), "/"))
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("refusing to write key %q outside %s", o.Key, dst)
		}
		srcT := st
		jobs = append(jobs, copyJob{srcShow: st.refKey(o.Key), dstShow: filepath.Join(dst, rel), size: o.Size, src: &srcT, srcKey: o.Key})
		return nil
	})
	return jobs, err
}

func (c *cli) event(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: mc event add|rm|ls ALIAS/BUCKET [ARN] [--event put,delete] [--prefix P] [--suffix S]")
	}
	t, err := c.cfg.target(args[1])
	if err != nil {
		return err
	}
	arn := ""
	if len(args) > 2 {
		arn = args[2]
	}
	resp, err := t.do(http.MethodGet, "", url.Values{"notification": {""}}, nil, 0, nil)
	if err != nil {
		return c.eventErr(args[0], args[1], err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return c.eventErr(args[0], args[1], readS3Err(resp.StatusCode, b))
	}
	var cfg notificationConfig
	err = xml.NewDecoder(resp.Body).Decode(&cfg)
	resp.Body.Close()
	if err != nil {
		return err
	}
	op := args[0]
	switch op {
	case "ls":
		for _, q := range cfg.Queues {
			if arn != "" && q.Queue != arn {
				continue
			}
			if c.json {
				ev := q.Events
				if ev == nil {
					ev = []string{}
				}
				prefix, suffix := "", ""
				for _, r := range q.Rules {
					switch strings.ToLower(r.Name) {
					case "prefix":
						prefix = r.Value
					case "suffix":
						suffix = r.Value
					}
				}
				if err := c.emit(eventLSLine{Status: "success", ARN: q.Queue, Event: ev, Prefix: prefix, Suffix: suffix}); err != nil {
					return err
				}
				continue
			}
			line := q.Queue + "   " + strings.Join(q.Events, ",")
			for _, r := range q.Rules {
				line += "   Filter: " + strings.ToLower(r.Name) + "=\"" + r.Value + "\""
			}
			fmt.Fprintln(c.stdout, line)
		}
		return nil
	case "add":
		if arn == "" {
			return errors.New("usage: mc event add ALIAS/BUCKET ARN [--event put,delete,get] [--prefix P] [--suffix S]")
		}
		q := queueConf{ID: fmt.Sprintf("ss33-%d", time.Now().UnixNano()), Queue: arn}
		for _, name := range strings.Split(cmp.Or(c.values["event"], "put,delete,get"), ",") {
			ev, ok := mcEvents[strings.TrimSpace(name)]
			if !ok {
				return fmt.Errorf("unsupported event %q (put, delete, get)", name)
			}
			q.Events = append(q.Events, ev)
		}
		if p := c.values["prefix"]; p != "" {
			q.Rules = append(q.Rules, filterRule{"prefix", p})
		}
		if s := c.values["suffix"]; s != "" {
			q.Rules = append(q.Rules, filterRule{"suffix", s})
		}
		cfg.Queues = append(cfg.Queues, q)
	case "rm":
		if arn == "" && !c.flags["force"] {
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
		return c.eventErr(op, args[1], err)
	}
	if c.json {
		return c.emit(eventChangeLine{Status: "success", Bucket: args[1], ARN: arn})
	}
	verb := map[string]string{"add": "added", "rm": "removed"}[op]
	fmt.Fprintf(c.stdout, "Successfully %s %s\n", verb, cmp.Or(arn, "all notifications"))
	return nil
}

func (c *cli) eventErr(op, ref string, err error) error {
	msg := "Unable to list notifications on the specified bucket."
	if op != "ls" {
		msg = fmt.Sprintf("Unable to configure notifications on `%s`.", ref)
	}
	var se *s3err
	if errors.As(err, &se) && (se.Code != "" || se.Message != "") {
		cause := se.Message
		if cause == "" {
			cause = se.Error()
		}
		return c.fail(msg, cause, "fatal", se.payload())
	}
	if err == nil {
		return c.fail(msg, "", "fatal", struct{}{})
	}
	return c.fail(msg, err.Error(), "fatal", struct{}{})
}

// target is an `alias/bucket/prefix` reference.
type target struct {
	aliasName string
	alias     alias
	bucket    string
	key       string
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
	return target{aliasName: name, alias: a, bucket: bucket, key: key}, nil
}

func (c config) isRemote(ref string) bool {
	name, _, _ := strings.Cut(ref, "/")
	_, ok := c.Aliases[name]
	return ok
}

// asDir treats a non-empty key without a trailing slash as a directory prefix.
func (t target) asDir() target {
	if t.key != "" && !strings.HasSuffix(t.key, "/") {
		t.key += "/"
	}
	return t
}

func (t target) refKey(key string) string { return t.aliasName + "/" + t.bucket + "/" + key }

func (t target) listURL() string {
	u := strings.TrimRight(t.alias.URL, "/") + "/" + t.bucket + "/"
	if t.key != "" {
		u += t.key
		if !strings.HasSuffix(u, "/") {
			u += "/"
		}
	}
	return u
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
	_, _, err := t.exchange(method, key, query, body, size, header, okCodes...)
	return err
}

func (t target) exchange(method, key string, query url.Values, body io.Reader, size int64, header http.Header, okCodes ...int) (http.Header, []byte, error) {
	resp, err := t.do(method, key, query, body, size, header)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	for _, code := range okCodes {
		if resp.StatusCode == code {
			return resp.Header, b, nil
		}
	}
	return resp.Header, b, readS3Err(resp.StatusCode, b)
}

func (t target) head(key string, q url.Values) (http.Header, int, error) {
	resp, err := t.do(http.MethodHead, key, q, nil, 0, nil)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusMethodNotAllowed {
		return resp.Header, resp.StatusCode, nil
	}
	return resp.Header, resp.StatusCode, readS3Err(resp.StatusCode, nil)
}

func (t target) del(key string, q url.Values) (http.Header, error) {
	h, _, err := t.exchange(http.MethodDelete, key, q, nil, 0, nil, http.StatusNoContent, http.StatusOK)
	return h, err
}

func checkResp(resp *http.Response, err error, okCodes ...int) error {
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	for _, code := range okCodes {
		if resp.StatusCode == code {
			io.Copy(io.Discard, resp.Body)
			return nil
		}
	}
	b, _ := io.ReadAll(resp.Body)
	return readS3Err(resp.StatusCode, b)
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
	case "download", "public", "upload":
		p := policyDocument(t.bucket, mode)
		return t.call(http.MethodPut, "", url.Values{"policy": {""}}, strings.NewReader(p), int64(len(p)), nil, http.StatusNoContent, http.StatusOK)
	}
	return fmt.Errorf("unsupported anonymous mode %q", mode)
}

func policyDocument(bucket, mode string) string {
	bucketActions, objectActions := `"s3:GetBucketLocation","s3:ListBucket"`, `"s3:GetObject"`
	switch mode {
	case "public":
		bucketActions += `,"s3:ListBucketMultipartUploads"`
		objectActions = `"s3:AbortMultipartUpload","s3:DeleteObject","s3:GetObject","s3:ListMultipartUploadParts","s3:PutObject"`
	case "upload":
		bucketActions = `"s3:GetBucketLocation","s3:ListBucketMultipartUploads"`
		objectActions = `"s3:AbortMultipartUpload","s3:DeleteObject","s3:ListMultipartUploadParts","s3:PutObject"`
	}
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[`+
		`{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":[%s],"Resource":["arn:aws:s3:::%s"]},`+
		`{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":[%s],"Resource":["arn:aws:s3:::%s/*"]}]}`,
		bucketActions, bucket, objectActions, bucket)
}

// classifyPolicy names a bucket policy the way `mc anonymous get` does: one of the canned modes when it
// grants the same actions on the same resources as policyDocument writes, else custom.
func classifyPolicy(bucket string, raw []byte) string {
	got, ok := policyActionSets(raw)
	if !ok {
		return "custom"
	}
	for _, mode := range []string{"download", "public", "upload"} {
		want, _ := policyActionSets([]byte(policyDocument(bucket, mode)))
		if maps.EqualFunc(got, want, maps.Equal) {
			return mode
		}
	}
	return "custom"
}

// policyActionSets unions Allow actions per resource. A Deny, a Condition or an unreadable statement is not comparable.
func policyActionSets(raw []byte) (map[string]map[string]struct{}, bool) {
	var doc struct {
		Statement json.RawMessage
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.Statement) == 0 {
		return nil, false
	}
	var stmts []policyStmt
	if json.Unmarshal(doc.Statement, &stmts) != nil {
		var one policyStmt
		if json.Unmarshal(doc.Statement, &one) != nil {
			return nil, false
		}
		stmts = []policyStmt{one}
	}
	out := map[string]map[string]struct{}{}
	for _, st := range stmts {
		if st.Effect != "Allow" || len(st.Condition) > 0 || len(st.NotAction) > 0 {
			return nil, false
		}
		actions, ok := jsonStrings(st.Action)
		if !ok {
			return nil, false
		}
		resources, ok := jsonStrings(st.Resource)
		if !ok {
			return nil, false
		}
		for _, res := range resources {
			if out[res] == nil {
				out[res] = map[string]struct{}{}
			}
			for _, act := range actions {
				out[res][act] = struct{}{}
			}
		}
	}
	return out, true
}

type policyStmt struct {
	Effect    string
	Action    json.RawMessage
	Resource  json.RawMessage
	Condition json.RawMessage
	NotAction json.RawMessage
}

func jsonStrings(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}, true
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many, true
	}
	return nil, false
}

func policyResources(raw []byte) []string {
	sets, ok := policyActionSets(raw)
	if !ok {
		return nil
	}
	var res []string
	for r := range sets {
		res = append(res, r)
	}
	slices.Sort(res)
	return res
}

type listedObject struct {
	Key          string
	Size         int64
	LastModified string
	ETag         string
	StorageClass string
}

// forEachPage lists one page at a time: objects and common prefixes, in server order within each group.
func (t target) forEachPage(recursive bool, page func(objs, prefs []listedObject) error) error {
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
		prefs := make([]listedObject, 0, len(out.CommonPrefixes))
		for _, p := range out.CommonPrefixes {
			prefs = append(prefs, listedObject{Key: p.Prefix})
		}
		if err := page(out.Contents, prefs); err != nil {
			return err
		}
		if !out.IsTruncated {
			return nil
		}
		token = out.NextContinuationToken
	}
}

// walk lists every object under t.key (recursive) or one level (delimiter "/"). Prefixes are visited
// before objects on each page, which is the order cp and rm have always used.
func (t target) walk(recursive bool, fn func(o listedObject, isPrefix bool) error) error {
	return t.forEachPage(recursive, func(objs, prefs []listedObject) error {
		for _, p := range prefs {
			if err := fn(p, true); err != nil {
				return err
			}
		}
		for _, o := range objs {
			if err := fn(o, false); err != nil {
				return err
			}
		}
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
	ETag         string
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

func (t target) upload(file, key string, extra http.Header) error {
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
	for k, vs := range extra {
		h[k] = append([]string(nil), vs...)
	}
	return t.call(http.MethodPut, key, nil, f, info.Size(), h, http.StatusOK)
}

func (t target) copyFrom(srcBucket, srcKey, key string, extra http.Header) error {
	h := http.Header{"X-Amz-Copy-Source": {"/" + srcBucket + "/" + sigv4.URIEncode(srcKey, false)}}
	if len(extra) > 0 {
		h.Set("X-Amz-Metadata-Directive", "REPLACE")
		for k, vs := range extra {
			cp := append([]string(nil), vs...)
			h[k] = cp
		}
	}
	return t.call(http.MethodPut, key, nil, nil, 0, h, http.StatusOK)
}

func (t target) download(key, out string) (int64, error) {
	resp, err := t.do(http.MethodGet, key, nil, nil, 0, nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, checkResp(resp, nil, http.StatusOK)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return 0, err
	}
	f, err := os.Create(out)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, resp.Body)
	if err != nil {
		f.Close()
		return n, err
	}
	return n, f.Close()
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
