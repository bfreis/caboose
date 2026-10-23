package launcher

import (
	"slices"
	"testing"

	"github.com/bfreis/caboose/internal/config"
)

// The link's broker knows the hosts a hostile launch needs, so an egress
// edit cannot cut them off; a broker given none would refuse every
// removal under hostile instead.
func TestLinkBrokerKnowsClaudeHosts(t *testing.T) {
	r := &linkRunner{a: &App{Cfg: &config.Config{Env: "e", CabooseHome: t.TempDir(), EnvDir: t.TempDir()}}}
	got := r.brokerOptions(nil, nil).RequiredHosts
	if !slices.Equal(got, claudeHosts) {
		t.Fatalf("RequiredHosts %q, want %q", got, claudeHosts)
	}
	got[0] = "changed"
	if claudeHosts[0] == "changed" {
		t.Fatal("RequiredHosts shares claudeHosts' array")
	}
}
