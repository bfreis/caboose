// Package apkobuild builds a base image from a list of Wolfi packages, with
// apko used as a library: the list is resolved to exact packages in a lock,
// and the lock is built into an image tarball docker can load. The same lock
// builds the same image, byte for byte.
package apkobuild

import "github.com/bfreis/caboose/internal/apkobuild/pkgset"

// The package groups, names and specs are pkgset's, which holds them apart
// from apko; they are repeated here so that a caller of this package needs
// no other.
type (
	// Group is a named set of packages the sandbox offers together.
	Group = pkgset.Group
	// Spec is what an environment asks for: its own packages, and whether
	// the default groups come with them.
	Spec = pkgset.Spec
)

// Groups returns the package groups in the order they are offered.
func Groups() []Group { return pkgset.Groups() }

// Required returns the packages of the required group.
func Required() []string { return pkgset.Required() }

// DefaultPackages returns the packages of the required group and of every
// default group.
func DefaultPackages() []string { return pkgset.DefaultPackages() }

// CheckName reports why s is not an apk package name.
func CheckName(s string) error { return pkgset.CheckName(s) }
