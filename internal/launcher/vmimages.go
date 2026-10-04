package launcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bfreis/caboose/internal/vm"
	"github.com/bfreis/caboose/internal/vm/builder"
)

// imageStore is what the launcher asks of the images a sandbox is made
// from: docker's, or, under vm, the root disks the builder wrote.
type imageStore interface {
	ImageID(name string) string
	ImageLabels(name string) (map[string]string, bool, error)
}

// images is the image store of the configured isolation.
func (a *App) images() imageStore {
	if a.isVM() {
		return a.vmImages()
	}
	return a.Docker
}

// vmImages is the vm isolation's image store, in the data dir's
// vm/images/: for each image caboose build made, a record of what docker
// in the builder said of it (its ID, labels and config, which the guest
// cannot read) and the root disk it became, named by that ID. A base has
// a record with its ID only, for the staleness a build's labels are
// judged by. The host never reads a disk; the builder guest wrote it.
type vmImageStore struct{ dir string }

func (a *App) vmImages() *vmImageStore {
	return &vmImageStore{dir: filepath.Join(a.vmRoot(), "images")}
}

// vmImage is one record.
type vmImage struct {
	Name   string            `json:"name"`
	ID     string            `json:"id"`
	Labels map[string]string `json:"labels,omitempty"`
	Config builder.Config    `json:"config"`
	// Disk is the root disk's file in the store, "" for a base.
	Disk string `json:"disk,omitempty"`
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// recordPath is name's record: the name made a file name, and a hash, so
// two names never share one.
func (s *vmImageStore) recordPath(name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(s.dir, unsafeName.ReplaceAllString(name, "_")+"-"+hex.EncodeToString(sum[:4])+".json")
}

// disk is rec's root disk.
func (s *vmImageStore) disk(rec vmImage) string { return filepath.Join(s.dir, rec.Disk) }

// template is the empty ext4 the builder made, which scratch disks are
// cloned from.
func (s *vmImageStore) template() string { return filepath.Join(s.dir, "empty-ext4.img") }

// diskName is the root disk file for an image ID.
func diskName(id string) string {
	return "root-" + strings.TrimPrefix(id, "sha256:")[:min(16, len(strings.TrimPrefix(id, "sha256:")))] + ".img"
}

func (s *vmImageStore) get(name string) (vmImage, bool, error) {
	var rec vmImage
	b, err := os.ReadFile(s.recordPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return rec, false, nil
	}
	if err != nil {
		return rec, false, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return rec, false, fmt.Errorf("%s: %w", s.recordPath(name), err)
	}
	if rec.Disk != "" && !isFile(s.disk(rec)) {
		// A record whose disk is gone is an image that is gone.
		return rec, false, nil
	}
	return rec, true, nil
}

// ImageID is imageStore's: the ID docker in the builder gave it.
func (s *vmImageStore) ImageID(name string) string {
	rec, ok, err := s.get(name)
	if err != nil || !ok {
		return ""
	}
	return rec.ID
}

// ImageLabels is imageStore's.
func (s *vmImageStore) ImageLabels(name string) (map[string]string, bool, error) {
	rec, ok, err := s.get(name)
	if err != nil || !ok {
		return nil, false, err
	}
	return rec.Labels, true, nil
}

// put records rec, replacing name's record.
func (s *vmImageStore) put(rec vmImage) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	p := s.recordPath(rec.Name)
	if err := os.WriteFile(p+".new", append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(p+".new", p)
}

// prune removes the root disks no record names and no VM of vmRoot boots
// from: a running sandbox keeps its disk, however old, until it is
// recreated.
func (s *vmImageStore) prune(vmRoot string) {
	keep := map[string]bool{}
	recs, _ := filepath.Glob(filepath.Join(s.dir, "*.json"))
	for _, p := range recs {
		var rec vmImage
		if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &rec) == nil && rec.Disk != "" {
			keep[s.disk(rec)] = true
		}
	}
	states, _ := filepath.Glob(filepath.Join(vmRoot, "*", "state.json"))
	for _, p := range states {
		if st, err := vm.Dir(filepath.Dir(p)).ReadState(); err == nil {
			for _, d := range st.Machine.Disks {
				keep[d.Path] = true
			}
		}
	}
	disks, _ := filepath.Glob(filepath.Join(s.dir, "root-*.img"))
	for _, d := range disks {
		if !keep[d] {
			_ = os.Remove(d)
		}
	}
}
