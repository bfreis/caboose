package config

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/bfreis/caboose/internal/sandboxcfg"
)

// checkpointsKey is the [checkpoints] table's paths key, by its dotted name.
const checkpointsKey = "checkpoints.paths"

// MaxCheckpointPaths is how many paths [checkpoints] paths may list.
const MaxCheckpointPaths = 32

// maxCheckpointPathLen bounds one entry, in bytes.
const maxCheckpointPathLen = 1024

// CheckCheckpoints checks [checkpoints] paths as written and returns them
// normalised: each a container path, absolute and clean (one under the
// home is written ~/REL, whether it was written so or /home/agent/REL).
// The home itself, a relative path, an empty, unclean or unprintable one,
// more than MaxCheckpointPaths, one listed twice and one at or inside
// another (its files would be copied twice) are refused. Whether a path is
// a writable part of a root or a kept path of the home is decided when a
// checkpoint is taken, from the roots and the sandbox config of the time.
func CheckCheckpoints(list []string) ([]string, error) {
	if len(list) > MaxCheckpointPaths {
		return nil, fmt.Errorf("paths lists %d paths, at most %d", len(list), MaxCheckpointPaths)
	}
	out := make([]string, 0, len(list))
	full := make([]string, 0, len(list)) // each as a container path, for the nesting check
	for _, p := range list {
		n, ctr, err := checkpointPath(p)
		if err != nil {
			return nil, err
		}
		for i, q := range full {
			switch {
			case q == ctr:
				return nil, fmt.Errorf("paths lists %q twice (as %q and %q)", n, list[i], p)
			case Within(ctr, q):
				return nil, fmt.Errorf("path %q is inside %q, whose checkpoints already copy it", p, list[i])
			case Within(q, ctr):
				return nil, fmt.Errorf("path %q is inside %q, whose checkpoints already copy it", list[i], p)
			}
		}
		out = append(out, n)
		full = append(full, ctr)
	}
	return out, nil
}

// checkpointPath checks one entry of [checkpoints] paths: n is it
// normalised, ctr the container path it names.
func checkpointPath(p string) (n, ctr string, err error) {
	switch {
	case p == "":
		return "", "", errors.New("paths has an empty path")
	case len(p) > maxCheckpointPathLen:
		return "", "", fmt.Errorf("path of %d bytes is too long", len(p))
	case !printableText(p):
		return "", "", fmt.Errorf("path %q has a control or invisible character", p)
	}
	home := strings.TrimSuffix(p, "/")
	if home == "~" || home == ContainerHome {
		return "", "", fmt.Errorf("path %q is the whole home: name the kept paths in it, such as ~/.claude", p)
	}
	if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, ContainerHome+"/") {
		rel, err := sandboxcfg.HomeRel(p)
		if err != nil || strings.HasSuffix(p, "/") {
			return "", "", fmt.Errorf("path %q is not a clean path under the home (~/...)", p)
		}
		return "~/" + rel, ContainerHome + "/" + rel, nil
	}
	if strings.HasPrefix(p, "~") {
		return "", "", fmt.Errorf("path %q: only ~/ (the sandbox's home) may start a path", p)
	}
	if !path.IsAbs(p) || path.Clean(p) != p {
		return "", "", fmt.Errorf("path %q is not an absolute, clean path in the sandbox (or one starting with ~/)", p)
	}
	if p == "/" {
		return "", "", errors.New(`path "/" is the whole sandbox: name a writable part of a root`)
	}
	if Within(p, "/home") || Within("/home", p) {
		return "", "", fmt.Errorf("path %q is at or around the home, but not inside it", p)
	}
	return p, p, nil
}

// NormalizeCheckpointPath is p, a path the sandbox names as one of
// [checkpoints] paths, normalised as CheckCheckpoints normalises an entry
// (~/REL for one under the home), or why it can name none: empty, too
// long, unprintable, relative, unclean, the home or "/".
func NormalizeCheckpointPath(p string) (string, error) {
	n, _, err := checkpointPath(p)
	return n, err
}
