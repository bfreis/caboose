package launcher

// Getting gVisor's runsc for an engine that caboose registers it with
// itself (OrbStack): the release binary, checked against the checksum
// published beside it, and the runtimes entry in the engine's daemon config
// that points docker at it (setup_isolation.go).
//
// The binary is the engine's to run, as root in its VM, so it lives where
// the sandbox cannot write: under CABOOSE_HOME, beside the environments,
// never in a data dir's mounted parts. It is shared by every environment,
// as the engine is.

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// runscName is the runtime's name in the daemon config, and so in docker info
// and at docker run --runtime.
const runscName = "runsc"

// runscArgs are the flags caboose's runsc runs with: the forwarded SSH agent's
// socket is a host Unix socket, which runsc refuses to connect to by
// default; Docker inside the sandbox needs raw sockets, and from Docker 28
// on, writes to packet sockets.
//
// --dcache=0 is for OrbStack's file sharing, which answers a directory
// read from the start again, through a handle already read, with what it
// said the first time: a directory listed while empty lists empty after
// files are added. runsc reads each directory through one host fd,
// rewound, kept for as long as it caches the directory; with no cache,
// it opens it anew each time something lists it. A directory some process
// holds open (a shell's cwd) is still cached, and can still list stale.
var runscArgs = []string{"--host-uds=open", "--net-raw", "--allow-packet-socket-write", "--dcache=0"}

// runscInstallArgs are the flags of runscArgs a runsc registered by
// anyone else needs too, for the sandbox to work as it does under
// caboose's: --dcache=0 is for OrbStack's file sharing alone.
var runscInstallArgs = []string{"--host-uds=open", "--net-raw", "--allow-packet-socket-write"}

// runscHostUDS reports whether runsc's flags let the sandbox connect to a
// host Unix socket (--host-uds=open or all), as the forwarded SSH agent's
// is: without, gVisor refuses the connection. A flag given twice counts
// as runsc reads it, the last.
func runscHostUDS(args []string) bool {
	val := ""
	for i := 0; i < len(args); i++ {
		name, v, hasV := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(args[i], "-"), "-"), "=")
		if name != "host-uds" || !strings.HasPrefix(args[i], "-") {
			continue
		}
		if !hasV && i+1 < len(args) {
			i++
			v = args[i]
		}
		val = v
	}
	return val == "open" || val == "all"
}

// hostUDSFix says how to give a runsc caboose did not register
// --host-uds=open: on Linux, the daemon config's runsc entry, which
// gVisor's own runsc install writes; elsewhere the engine's.
func hostUDSFix() string {
	if goos == "linux" {
		return fmt.Sprintf("add %q to runsc's runtimeArgs in /etc/docker/daemon.json "+
			"('sudo runsc install -- %s' writes them all), then 'sudo systemctl reload docker' and 'caboose restart'",
			"--host-uds=open", strings.Join(runscInstallArgs, " "))
	}
	return fmt.Sprintf("add %q to runsc's runtimeArgs in the Docker engine's daemon config, restart the engine, then 'caboose restart'",
		"--host-uds=open")
}

// Bounds on a release: the tarball is some 150MB, unpacked some 300MB.
const (
	runscMaxTarball  = 1 << 30
	runscMaxUnpacked = 2 << 30
)

// runscTarball is what gVisor publishes per architecture: runsc, its
// containerd shim and gvisor-bin/, which runsc may run from beside it.
const runscTarball = "gvisor.tar.bz2"

// runscArch is gVisor's name for the architecture docker info reports (its
// uname -m), or "" for one gVisor does not build.
func runscArch(engine string) string {
	switch engine {
	case "x86_64", "amd64":
		return "x86_64"
	case "aarch64", "arm64":
		return "aarch64"
	}
	return ""
}

// downloadRunsc fetches base/arch/gvisor.tar.bz2 and its .sha512, checks
// the one against the other, and unpacks the tarball into dir, as a whole:
// the new release goes into a directory beside it, which then replaces
// dir by rename, so a failure leaves dir as it was. Only plain files and
// directories are unpacked, each under dir. It returns dir/runsc.
//
// Which release it was is recorded in dir's runscRecord: its checksum,
// which tells it from a newer one, and when it was downloaded.
func downloadRunsc(ctx context.Context, client *http.Client, base, arch, dir string) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	url := runscURL(base, arch)
	want, err := latestRunscSum(ctx, client, base, arch)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(parent, ".gvisor-*.tar.bz2")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha512.New()
	if err := fetchTo(ctx, client, url, io.MultiWriter(f, h), runscMaxTarball); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("%s: checksum mismatch (sha512 %s, published %s)", url, got, want)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	fresh, err := os.MkdirTemp(parent, "."+filepath.Base(dir)+"-new-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(fresh)
	if err := untar(bzip2.NewReader(f), fresh); err != nil {
		return "", fmt.Errorf("%s: %v", url, err)
	}
	if fi, err := os.Lstat(filepath.Join(fresh, runscName)); err != nil || !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: no %s in it", url, runscName)
	}
	rec, err := json.Marshal(runscRelease{SHA512: want, URL: url, Downloaded: time.Now().UTC().Truncate(time.Second)})
	if err == nil {
		err = os.WriteFile(filepath.Join(fresh, runscRecord), append(rec, '\n'), 0o644)
	}
	if err != nil {
		return "", err
	}
	if err := os.Chmod(fresh, 0o755); err != nil {
		return "", err
	}
	// Swap: the old release aside, the new one in, the old one gone.
	old := ""
	if _, err := os.Lstat(dir); err == nil {
		old = fresh + "-old"
		if err := os.Rename(dir, old); err != nil {
			return "", err
		}
	}
	if err := os.Rename(fresh, dir); err != nil {
		if old != "" {
			os.Rename(old, dir)
		}
		return "", err
	}
	if old != "" {
		os.RemoveAll(old)
	}
	return filepath.Join(dir, runscName), nil
}

func runscURL(base, arch string) string {
	return strings.TrimSuffix(base, "/") + "/" + arch + "/" + runscTarball
}

// latestRunscSum is the sha512 gVisor publishes beside base's release for
// arch: what tells the release caboose downloaded from a newer one, for a
// fetch of a few bytes.
func latestRunscSum(ctx context.Context, client *http.Client, base, arch string) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	url := runscURL(base, arch) + ".sha512"
	var sums bytes.Buffer
	if err := fetchTo(ctx, client, url, &sums, 4096); err != nil {
		return "", err
	}
	sum, err := sha512Of(sums.String())
	if err != nil {
		return "", fmt.Errorf("%s: %v", url, err)
	}
	return sum, nil
}

// runscRecord is the file in a release's directory that says which
// release caboose downloaded there, and when.
const runscRecord = "caboose-release.json"

// runscRelease is runscRecord's content.
type runscRelease struct {
	SHA512     string    `json:"sha512"`
	URL        string    `json:"url"`
	Downloaded time.Time `json:"downloaded"`
}

// readRunscRelease is the record of the release in dir; ok is false when
// none reads, as for one downloaded before caboose kept a record. dir is
// under CABOOSE_HOME, which no container can write.
func readRunscRelease(dir string) (rec runscRelease, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, runscRecord))
	if err != nil || json.Unmarshal(b, &rec) != nil || rec.SHA512 == "" || rec.Downloaded.IsZero() {
		return runscRelease{}, false
	}
	return rec, true
}

// runscStale is how long after its download doctor says caboose's runsc
// is worth checking for a newer release: gVisor releases about weekly,
// security fixes among them.
const runscStale = 60 * 24 * time.Hour

// cabooseRunscDir is the release directory of the runsc at path when it is
// one caboose downloaded (CABOOSE_HOME/runsc/<arch>/runsc), else "".
func (a *App) cabooseRunscDir(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(path) != runscName || filepath.Dir(dir) != filepath.Join(a.Cfg.CabooseHome, "runsc") || runscArch(filepath.Base(dir)) == "" {
		return ""
	}
	return dir
}

// untar unpacks a tar stream into dir, which it may only add to: plain
// files and directories, each under dir, within runscMaxUnpacked in all.
// Anything else -- a link, a device, a path out of dir -- is refused.
func untar(r io.Reader, dir string) error {
	tr := tar.NewReader(r)
	budget := int64(runscMaxUnpacked)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(hdr.Name)
		if !filepath.IsLocal(name) {
			return fmt.Errorf("refusing %q: not under the release's directory", hdr.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if hdr.Size > budget {
				return errors.New("larger unpacked than a release can be")
			}
			budget -= hdr.Size
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if hdr.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(tr, hdr.Size))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("refusing %q: not a plain file or directory", hdr.Name)
		}
	}
}

func fetchTo(ctx context.Context, client *http.Client, url string, w io.Writer, limit int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("%s: %v", url, err)
	}
	if n > limit {
		return fmt.Errorf("%s: larger than %d bytes", url, limit)
	}
	return nil
}

// sha512Of is the hex sha512 in a sha512sum line ("<hex>  gvisor.tar.bz2").
func sha512Of(s string) (string, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return "", errors.New("empty")
	}
	sum := strings.ToLower(f[0])
	if b, err := hex.DecodeString(sum); err != nil || len(b) != sha512.Size {
		return "", fmt.Errorf("not a sha512: %.40q", f[0])
	}
	return sum, nil
}

// daemonRuntime is a runtimes entry in docker's daemon config.
type daemonRuntime struct {
	Path        string   `json:"path"`
	RuntimeArgs []string `json:"runtimeArgs,omitempty"`
}

// runscEntry is caboose's runtimes entry for the runsc at path.
func runscEntry(path string) daemonRuntime { return daemonRuntime{Path: path, RuntimeArgs: runscArgs} }

// withRuntime is the daemon config data (JSON; empty is {}) with
// runtimes.<name> set to rt, and whether that changed anything. Every
// other key stays, in its order and with its value; only the whitespace is
// docker's usual two-space indent. data that is not a JSON object is an
// error, and so is a runtimes that is not one.
func withRuntime(data []byte, name string, rt daemonRuntime) (out []byte, changed bool, err error) {
	top, err := jsonMembers(data)
	if err != nil {
		return nil, false, err
	}
	entry, err := json.Marshal(rt)
	if err != nil {
		return nil, false, err
	}
	i := top.find("runtimes")
	var rts jsonObject
	if i >= 0 {
		if rts, err = jsonMembers(top[i].value); err != nil {
			return nil, false, fmt.Errorf("runtimes: %v", err)
		}
	}
	if j := rts.find(name); j >= 0 {
		var cur daemonRuntime
		if json.Unmarshal(rts[j].value, &cur) == nil && cur.Path == rt.Path && slices.Equal(cur.RuntimeArgs, rt.RuntimeArgs) {
			return data, false, nil
		}
		rts[j].value = entry
	} else {
		rts = append(rts, jsonMember{name, entry})
	}
	if i >= 0 {
		top[i].value = rts.encode()
	} else {
		top = append(top, jsonMember{"runtimes", rts.encode()})
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, top.encode(), "", "  "); err != nil {
		return nil, false, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), true, nil
}

type jsonMember struct {
	key   string
	value json.RawMessage
}

// jsonObject is a JSON object's members in their order.
type jsonObject []jsonMember

// jsonMembers reads a JSON object, compacting each value; blank is {}.
func jsonMembers(data []byte) (jsonObject, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var o jsonObject
	seen := map[string]bool{}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key := t.(string)
		if seen[key] {
			return nil, fmt.Errorf("%q is there twice", key)
		}
		seen[key] = true
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		var c bytes.Buffer
		if err := json.Compact(&c, raw); err != nil {
			return nil, err
		}
		o = append(o, jsonMember{key, c.Bytes()})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("more after the JSON object")
	}
	return o, nil
}

func (o jsonObject) find(key string) int {
	for i, m := range o {
		if m.key == key {
			return i
		}
	}
	return -1
}

func (o jsonObject) encode() json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.value)
	}
	b.WriteByte('}')
	return b.Bytes()
}
