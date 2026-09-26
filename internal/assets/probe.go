package assets

import (
	"io/fs"

	caboose "github.com/bfreis/caboose"
)

// ProbeScriptPath is where the image probe sits, both in the embedded FS and
// in a checkout. It is embedded but deliberately in neither build context: it runs
// in a throwaway container of the image being checked, never inside the
// image caboose builds.
const ProbeScriptPath = "imagecheck.sh"

// ProbeScript returns the embedded copy of imagecheck.sh.
func ProbeScript() ([]byte, error) {
	return fs.ReadFile(caboose.Files, ProbeScriptPath)
}
