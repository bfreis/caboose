package selfupdate

import (
	"regexp"
	"strconv"
	"strings"
)

// release matches a release tag: v, three numbers, and an optional
// prerelease. What `make` stamps from `git describe` between tags
// (v1.2.3-4-gabcdef, -dirty) matches too, as a prerelease of v1.2.3 -- which
// is why a tag alone never makes an install updatable (see Layout.Managed).
var release = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)

// Valid reports whether v is a release tag, vMAJOR.MINOR.PATCH with an
// optional -PRERELEASE.
func Valid(v string) bool { return release.MatchString(v) }

// Compare orders two release tags as semver does: -1, 0 or 1. A prerelease
// comes before its release; prereleases compare field by field, numbers as
// numbers. Both must be Valid.
func Compare(a, b string) int {
	ma, mb := release.FindStringSubmatch(a), release.FindStringSubmatch(b)
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return sign(x - y)
		}
	}
	pa, pb := strings.TrimPrefix(ma[4], "-"), strings.TrimPrefix(mb[4], "-")
	switch {
	case pa == pb:
		return 0
	case pa == "":
		return 1
	case pb == "":
		return -1
	}
	fa, fb := strings.Split(pa, "."), strings.Split(pb, ".")
	for i := 0; i < len(fa) && i < len(fb); i++ {
		x, errx := strconv.Atoi(fa[i])
		y, erry := strconv.Atoi(fb[i])
		switch {
		case errx == nil && erry == nil:
			if x != y {
				return sign(x - y)
			}
		case errx == nil:
			return -1 // numbers before words
		case erry == nil:
			return 1
		case fa[i] != fb[i]:
			return strings.Compare(fa[i], fb[i])
		}
	}
	return sign(len(fa) - len(fb))
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
