package config

import (
	"fmt"
	"strconv"
	"strings"
)

// The sizes of a VM's disks. EmptyDiskSize is the empty ext4 every new
// disk is cloned from: a scratch disk is that file, made longer, and the
// guest grows the filesystem to fill it while it is mounted (TestScratch
// tries sizes up to MaxScratchSize), so no scratch disk is smaller than it.
const (
	EmptyDiskSize      = 4 << 30
	DefaultScratchSize = 64 << 30
	DefaultDockerSize  = 64 << 30
	MaxScratchSize     = 1024 << 30
)

// The bounds of a vm profile's volume_limit: the total of its volumes'
// sizes a sandbox's resize request may bring them to. It is 256G unless
// set, at least the smallest disk, and at most what the profile's
// volumes could total (MaxVMVolumes disks of MaxScratchSize).
const (
	DefaultVolumeLimit = 256 << 30
	MaxVolumeLimit     = MaxVMVolumes * MaxScratchSize
)

// ParseVolumeLimit is a volume_limit in bytes, written as a size is
// ("256G", "262144M"): from EmptyDiskSize to MaxVolumeLimit.
func ParseVolumeLimit(s string) (int64, error) {
	mib, err := parseMiB(s, "\"256G\" or \"262144M\"")
	if err != nil {
		return 0, err
	}
	n := int64(mib) << 20
	if n < EmptyDiskSize || n > MaxVolumeLimit {
		return 0, fmt.Errorf("%q is outside %dG to %dG, the totals a profile's volumes can have", s, EmptyDiskSize>>30, MaxVolumeLimit>>30)
	}
	return n, nil
}

// ParseScratchSize is a scratch disk's size in bytes, written as a
// memory is ("64G", "65536M", or a number of MiB): at least EmptyDiskSize
// and at most MaxScratchSize. It is what a VM profile's scratch_size is
// read as, and what a settings proposal's is checked with.
func ParseScratchSize(s string) (int64, error) {
	mib, err := parseMiB(s, "\"64G\" or \"65536M\"")
	if err != nil {
		return 0, err
	}
	n := int64(mib) << 20
	if n < EmptyDiskSize || n > MaxScratchSize {
		return 0, fmt.Errorf("%q is outside %dG to %dG, the sizes of disk a sandbox can have", s, EmptyDiskSize>>30, MaxScratchSize>>30)
	}
	return n, nil
}

// ParseMemory is an amount of memory in MiB: "8G", "8GiB", "4096M",
// "4096MiB", or a number of MiB. It is what a VM profile's memory is read
// as, and what a settings proposal's is checked with.
func ParseMemory(s string) (int, error) {
	n, err := parseMiB(s, "\"8G\" or \"4096M\"")
	if err != nil {
		return 0, err
	}
	if n < 512 {
		return 0, fmt.Errorf("%q is under 512M, too little for the sandbox", s)
	}
	return n, nil
}

// parseMiB reads an amount in MiB, as ParseMemory documents; like names
// the forms an error offers.
func parseMiB(s, like string) (int, error) {
	t := strings.TrimSpace(strings.ToUpper(s))
	mul := 1
	switch {
	case strings.HasSuffix(t, "GIB"), strings.HasSuffix(t, "GB"), strings.HasSuffix(t, "G"):
		mul, t = 1024, strings.TrimRight(t, "GIB")
	case strings.HasSuffix(t, "MIB"), strings.HasSuffix(t, "MB"), strings.HasSuffix(t, "M"):
		t = strings.TrimRight(t, "MIB")
	}
	n, err := strconv.Atoi(t)
	if err != nil || n < 1 || n > 1<<30 {
		return 0, fmt.Errorf("%q is not an amount like %s", s, like)
	}
	return n * mul, nil
}
