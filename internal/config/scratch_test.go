package config

import "testing"

func TestParseScratchSize(t *testing.T) {
	for in, want := range map[string]int64{
		"64G": 64 << 30, "64GiB": 64 << 30, "64gb": 64 << 30, "4G": 4 << 30,
		"4096M": 4 << 30, "4096": 4 << 30, "1024G": 1024 << 30, "131072M": 128 << 30,
	} {
		if got, err := ParseScratchSize(in); err != nil || got != want {
			t.Errorf("%q: %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "lots", "0", "-8G", "3G", "4095M", "1025G", "1T", "64.5G", "G"} {
		if _, err := ParseScratchSize(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	if EmptyDiskSize > DefaultScratchSize || DefaultScratchSize > MaxScratchSize {
		t.Error("the default scratch size is outside the sizes it may have")
	}
	if EmptyDiskSize > DefaultDockerSize || DefaultDockerSize > MaxScratchSize {
		t.Error("the default docker size is outside the sizes it may have")
	}
}

// Memory and scratch sizes are one format: where both accept a text, it
// stands for the same amount.
func TestSizeFormatIsMemorys(t *testing.T) {
	for _, in := range []string{"8G", "8GiB", "4096M", "4096MiB", "4096", "lots", "-1G", "8T", "", "0"} {
		mib, merr := ParseMemory(in)
		size, serr := ParseScratchSize(in)
		if merr == nil && serr == nil && int64(mib)<<20 != size {
			t.Errorf("%q: %d MiB, %d bytes", in, mib, size)
		}
	}
}
