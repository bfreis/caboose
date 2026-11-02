package proposal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestVolumeProposal(t *testing.T) {
	p, err := Parse("v.toml", []byte("title = \"A cache\"\n[volume]\nname = \"gocache\"\npath = \"~/.cache/go-build\"\nsize = \"40G\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Volume == nil || *p.Volume != (Volume{Name: "gocache", Path: "~/.cache/go-build", Size: "40G"}) || p.Settings != nil || p.Root != nil {
		t.Fatalf("%+v", p)
	}
	// No size: the host's default, and the only value outside vm.
	p, err = Parse("v.toml", []byte("title = \"Data\"\n[volume]\nname = \"data\"\npath = \"/data\"\n"))
	if err != nil || p.Volume == nil || p.Volume.Size != "" || p.Volume.Path != "/data" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestVolumeProposalRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"no name", "[volume]\npath = \"/data\"\n", "[volume] needs name"},
		{"no path", "[volume]\nname = \"data\"\n", "[volume] needs path"},
		{"bad name", "[volume]\nname = \"Data\"\npath = \"/data\"\n", "volume.name: 'Data' is not a volume name"},
		{"long name", "[volume]\nname = \"" + strings.Repeat("a", 21) + "\"\npath = \"/data\"\n", "is not a volume name"},
		{"docker", "[volume]\nname = \"docker\"\npath = \"/data\"\n", "dockerd storage"},
		{"relative", "[volume]\nname = \"data\"\npath = \"data\"\n", "volume.path: \"data\" is not an absolute, clean path"},
		{"unclean", "[volume]\nname = \"data\"\npath = \"/data/../etc\"\n", "not an absolute, clean path"},
		{"the home", "[volume]\nname = \"data\"\npath = \"~\"\n", "the whole home"},
		{"too deep", "[volume]\nname = \"data\"\npath = \"~/a/b\"\n", "too deep"},
		{"caboose's own", "[volume]\nname = \"data\"\npath = \"~/.caboose-proposals\"\n", "is caboose's own"},
		{"kept for caboose", "[volume]\nname = \"data\"\npath = \"~/.claude\"\n", "kept on this machine, which reads it"},
		{"a system dir", "[volume]\nname = \"data\"\npath = \"/etc\"\n", "volume.path"},
		{"a comma", "[volume]\nname = \"data\"\npath = \"/a,b\"\n", "volume.path"},
		{"escape", "[volume]\nname = \"data\"\npath = \"/data\\u001b[2J\"\n", "character U+001B"},
		{"bidi", "[volume]\nname = \"data\\u202e\"\npath = \"/data\"\n", "character U+202E"},
		{"size bad", "[volume]\nname = \"data\"\npath = \"/data\"\nsize = \"huge\"\n", "volume.size"},
		{"size small", "[volume]\nname = \"data\"\npath = \"/data\"\nsize = \"3G\"\n", "volume.size: \"3G\" is outside 4G to 1024G"},
		{"size big", "[volume]\nname = \"data\"\npath = \"/data\"\nsize = \"1025G\"\n", "outside 4G to 1024G"},
		{"size number", "[volume]\nname = \"data\"\npath = \"/data\"\nsize = 40\n", "not a proposal"},
		{"unknown key", "[volume]\nname = \"data\"\npath = \"/data\"\nprofile = \"vm.big\"\n", "unknown key volume.profile"},
		{"with settings", "[volume]\nname = \"data\"\npath = \"/data\"\n[settings.session]\ntmux = true\n", "[volume] with another change"},
		{"with a root", "[volume]\nname = \"data\"\npath = \"/data\"\n[roots]\nx = \"~/x\"\n", "[volume] with another change"},
		{"two volumes", "[volume]\nname = \"data\"\npath = \"/data\"\n[[volume]]\nname = \"x\"\n", "not a proposal"},
	} {
		_, err := Parse("v.toml", []byte("title = \"t\"\n"+tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// state.toml says which profile a volume goes in, the volumes it has and
// its volume_limit.
func TestWriteCurrentVolumes(t *testing.T) {
	data := t.TempDir()
	s := State{Image: "apko.default", VolumeProfile: "vm.default", VolumeLimit: "256G",
		Volumes: map[string]StateVolume{"cache": {Path: "/home/agent/.cache/go-build", Size: "64G"}}}
	if err := WriteCurrent(data, s); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(data, Dir, CurrentDir, "state.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		VolumeProfile string                 `toml:"volume_profile"`
		VolumeLimit   string                 `toml:"volume_limit"`
		Volumes       map[string]StateVolume `toml:"volumes"`
	}
	if _, err := toml.Decode(string(b), &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if got.VolumeProfile != "vm.default" || got.VolumeLimit != "256G" || got.Volumes["cache"] != s.Volumes["cache"] {
		t.Errorf("%+v\n%s", got, b)
	}
}
