package version

import (
	"runtime/debug"
	"testing"
)

func buildInfo(mainVersion string, settings ...string) *debug.BuildInfo {
	bi := &debug.BuildInfo{Main: debug.Module{Path: "github.com/bfreis/caboose", Version: mainVersion}}
	for i := 0; i+1 < len(settings); i += 2 {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
	}
	return bi
}

func TestResolveLdflagsWin(t *testing.T) {
	bi := buildInfo("v9.9.9", "vcs.revision", "ffff", "vcs.modified", "true")
	got := resolve("v1.2.3", "abc123", "2026-09-20T00:00:00Z", bi)
	want := Info{Version: "v1.2.3", Commit: "abc123", Date: "2026-09-20T00:00:00Z"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveGoInstallModuleVersion(t *testing.T) {
	got := resolve("", "", "", buildInfo("v1.2.3"))
	if want := (Info{Version: "v1.2.3"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveDevelWithVCS(t *testing.T) {
	bi := buildInfo("(devel)",
		"vcs", "git",
		"vcs.revision", "3fea36d0123456789abcdef0123456789abcdef0",
		"vcs.time", "2026-09-19T12:00:00Z",
		"vcs.modified", "true")
	got := resolve("", "", "", bi)
	want := Info{Version: "dev", Commit: "3fea36d0123456789abcdef0123456789abcdef0",
		Date: "2026-09-19T12:00:00Z", Modified: true}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveNothing(t *testing.T) {
	for _, bi := range []*debug.BuildInfo{nil, buildInfo(""), buildInfo("(devel)")} {
		got := resolve("", "", "", bi)
		if got != (Info{Version: "dev"}) {
			t.Errorf("got %+v", got)
		}
	}
}

func TestGetNeverEmpty(t *testing.T) {
	if Get().Version == "" {
		t.Error("Get().Version is empty")
	}
}

// What `make launcher` stamps where git knows nothing (no repo, or no
// commits yet): Version=dev with empty Commit and Date. That is a stamp,
// and it wins as one; the empty fields stay unknown.
func TestResolveStampedWithoutCommit(t *testing.T) {
	got := resolve("dev", "", "", buildInfo("(devel)", "vcs.revision", "ffff"))
	if want := (Info{Version: "dev"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
