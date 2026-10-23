package daemonproto

import (
	"context"
	"path/filepath"
)

// MethodWritable answers which host paths the sandboxes under this
// CABOOSE_HOME reach now (no params, result WritableResult): what each
// can write, and the roots each mounts read-only. It is read off every
// sandbox when asked, never kept from an earlier answer, since a sandbox
// created meanwhile mounts other paths. A restore judges its target by
// it.
const MethodWritable = "writable"

// Bounds of a writable answer.
const (
	MaxWritablePaths = 1024 // paths in each list
	MaxWritableEnvs  = 256  // environments of one path
	maxWritablePath  = 4096
)

// WritableResult is the answer to writable.
type WritableResult struct {
	// Writable are the host paths, physical, some sandbox can write.
	Writable []WritablePath `json:"writable"`
	// ReadOnly are the roots sandboxes mount without writing them.
	ReadOnly []WritablePath `json:"read_only"`
}

// WritablePath is a host path and the environments whose sandboxes reach
// it.
type WritablePath struct {
	Path string   `json:"path"`
	Envs []string `json:"envs"`
}

// Validate checks an answer: bounded lists, absolute clean printable
// paths, valid environment names.
func (r *WritableResult) Validate() error {
	for _, l := range [][]WritablePath{r.Writable, r.ReadOnly} {
		if len(l) > MaxWritablePaths {
			return bad("more than %d writable paths", MaxWritablePaths)
		}
		for _, p := range l {
			if err := checkText("a writable path", p.Path, maxWritablePath); err != nil {
				return err
			}
			if !filepath.IsAbs(p.Path) || filepath.Clean(p.Path) != p.Path {
				return bad("a writable path is not absolute and clean")
			}
			if len(p.Envs) == 0 || len(p.Envs) > MaxWritableEnvs {
				return bad("a writable path with no environment, or more than %d", MaxWritableEnvs)
			}
			for _, e := range p.Envs {
				if !ValidEnv(e) {
					return bad("bad environment name in a writable path")
				}
			}
		}
	}
	return nil
}

// Writable asks which host paths the sandboxes reach now.
func (c *Client) Writable(ctx context.Context) (*WritableResult, error) {
	var r WritableResult
	if err := c.Call(ctx, MethodWritable, nil, &r); err != nil {
		return nil, err
	}
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return &r, nil
}
