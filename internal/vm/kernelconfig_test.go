package vm

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bfreis/caboose/internal/shelltest"
)

// The guest kernel's config fragments, vm/kernel/config-*: merged onto
// allnoconfig by vm/kernel/Dockerfile, and checked against the result by
// vm/kernel/check-config. The build proves Kconfig took them; these tests
// hold the fragments to their form and to the decisions they carry.

const kernelDir = "../../vm/kernel"

// kernelArchs are the architectures with a fragment of their own.
var kernelArchs = []string{"arm64"}

var (
	optOn  = regexp.MustCompile(`^CONFIG_([A-Za-z0-9_]+)=(.+)$`)
	optOff = regexp.MustCompile(`^# CONFIG_([A-Za-z0-9_]+) is not set$`)
)

type kernelOpt struct {
	value string // "n" for is-not-set
	line  int
}

// readFragment parses a fragment, failing on any line that is neither a
// note (##), blank, nor an option, and on an option named twice.
func readFragment(t *testing.T, name string) map[string]kernelOpt {
	t.Helper()
	f, err := os.Open(filepath.Join(kernelDir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	opts := map[string]kernelOpt{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		var opt, val string
		switch {
		case strings.TrimSpace(line) == "" || strings.HasPrefix(line, "##"):
			continue
		case optOn.MatchString(line):
			m := optOn.FindStringSubmatch(line)
			opt, val = m[1], m[2]
			if val == "n" {
				t.Errorf("%s:%d: %s=n: write # CONFIG_%s is not set", name, n, opt, opt)
			}
		case optOff.MatchString(line):
			opt, val = optOff.FindStringSubmatch(line)[1], "n"
		default:
			t.Errorf("%s:%d: neither a ## note nor an option: %q", name, n, line)
			continue
		}
		if prev, ok := opts[opt]; ok {
			t.Errorf("%s:%d: %s already set at line %d", name, n, opt, prev.line)
		}
		opts[opt] = kernelOpt{val, n}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return opts
}

// kernelConfig is what the fragments ask for, common and arch together.
func kernelConfig(t *testing.T, arch string) map[string]string {
	t.Helper()
	all := map[string]string{}
	for opt, o := range readFragment(t, "config-common") {
		all[opt] = o.value
	}
	for opt, o := range readFragment(t, "config-"+arch) {
		if _, ok := all[opt]; ok {
			// A later fragment silently wins in merge_config: an option
			// belongs to one of them.
			t.Errorf("config-%s:%d: %s is config-common's too", arch, o.line, opt)
		}
		all[opt] = o.value
	}
	return all
}

func TestKernelFragments(t *testing.T) {
	entries, err := os.ReadDir(kernelDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if arch, ok := strings.CutPrefix(e.Name(), "config-"); ok && arch != "common" && !slices.Contains(kernelArchs, arch) {
			t.Errorf("%s has no entry in kernelArchs, so nothing checks it", e.Name())
		}
	}

	// What the guest cannot boot or run the sandbox without, and the
	// decisions the kernel was built to (vm/kernel's plan): each option's
	// value in every architecture's merged fragments.
	want := map[string]string{
		// boot: the launcher's uncompressed initramfs, hvc0, /dev
		"BLK_DEV_INITRD": "y", "DEVTMPFS": "y", "TTY": "y", "UNIX98_PTYS": "y",
		"PRINTK": "y", "BINFMT_ELF": "y", "BINFMT_SCRIPT": "y", "PCI": "y",
		"PCI_HOST_GENERIC": "y", "DEVTMPFS_MOUNT": "n",
		// the hypervisor's devices
		"VIRTIO_PCI": "y", "VIRTIO_BLK": "y", "VIRTIO_NET": "y", "VIRTIO_CONSOLE": "y",
		"HW_RANDOM_VIRTIO": "y", "VIRTIO_BALLOON": "y", "VSOCKETS": "y",
		"VIRTIO_VSOCKETS": "y", "VIRTIO_FS": "y", "FUSE_FS": "y",
		// the root disks, /work, docker's storage
		"EXT4_FS": "y", "OVERLAY_FS": "y", "TMPFS": "y", "PROC_FS": "y", "SYSFS": "y",
		// dockerd and runc
		"CGROUPS": "y", "MEMCG": "y", "CGROUP_PIDS": "y", "CPUSETS": "y",
		"CGROUP_BPF": "y", "BPF_SYSCALL": "y", "NAMESPACES": "y", "USER_NS": "y",
		"PID_NS": "y", "NET_NS": "y", "SECCOMP_FILTER": "y", "KEYS": "y",
		"VETH": "y", "BRIDGE": "y", "BRIDGE_NETFILTER": "y", "TUN": "y",
		"NF_NAT": "y", "NF_TABLES": "y", "IP_NF_NAT": "y", "IP_NF_TARGET_MASQUERADE": "y",
		"NETFILTER_XT_MATCH_ADDRTYPE": "y", "NETFILTER_XT_MATCH_CONNTRACK": "y",
		// ip=dhcp: the kernel's own client
		"IP_PNP": "y", "IP_PNP_DHCP": "y",
		// the wall clock from the hypervisor's RTC at boot, not 1970
		"RTC_CLASS": "y", "RTC_HCTOSYS": "y", "RTC_HCTOSYS_DEVICE": `"rtc0"`,
		// decisions: no modules, no io_uring, lockdown in integrity mode
		"MODULES": "n", "IO_URING": "n", "SECURITY_LOCKDOWN_LSM": "y",
		"SECURITY_LOCKDOWN_LSM_EARLY": "y", "LOCK_DOWN_KERNEL_FORCE_INTEGRITY": "y",
		// decisions: readable oopses; memory zeroed on allocation, not on free,
		// which would zero all of the VM's RAM at boot and make it resident
		"KALLSYMS": "y", "INIT_ON_FREE_DEFAULT_ON": "n",
		// decisions: swarm and overlay networks
		"IP_VS": "y", "IP_VS_NFCT": "y", "IP_VS_RR": "y", "NETFILTER_XT_MATCH_IPVS": "y",
		"VXLAN": "y",
		// hardening, and what the sandbox has no use for
		"INIT_ON_ALLOC_DEFAULT_ON": "y", "HARDENED_USERCOPY": "y", "RANDOMIZE_BASE": "y",
		"STACKPROTECTOR_STRONG": "y", "DEVMEM": "n", "DEVPORT": "n", "PROC_KCORE": "n",
		"KEXEC": "n", "DEBUG_FS": "n", "USERFAULTFD": "n", "LEGACY_TIOCSTI": "n",
		"IKCONFIG": "n", "SECURITY_SELINUX": "n", "AUDIT": "n",
		"LOCALVERSION": `"-caboose"`, "LOCALVERSION_AUTO": "n",
		// a random layout per build would make two builds differ
		"RANDSTRUCT_NONE": "y",
	}
	archWant := map[string]map[string]string{
		// 4K pages whatever the host's: jemalloc and others break on 16K.
		"arm64": {"ARM64_4K_PAGES": "y", "COMPAT": "n", "UNMAP_KERNEL_AT_EL0": "y", "ARM64_PTR_AUTH_KERNEL": "y",
			// the RTC every arm64 hypervisor caboose runs on gives
			"RTC_DRV_PL031": "y"},
	}
	for _, arch := range kernelArchs {
		got := kernelConfig(t, arch)
		check := func(opt, val string) {
			if g, ok := got[opt]; !ok {
				t.Errorf("%s: CONFIG_%s is in neither fragment; want %s", arch, opt, val)
			} else if g != val {
				t.Errorf("%s: CONFIG_%s is %s; want %s", arch, opt, g, val)
			}
		}
		for opt, val := range want {
			check(opt, val)
		}
		for opt, val := range archWant[arch] {
			check(opt, val)
		}
		// Nothing may come back as a module: there are none to load.
		for opt, val := range got {
			if val == "m" {
				t.Errorf("%s: CONFIG_%s=m, with modules off", arch, opt)
			}
		}
	}
}

// check-config passes a config that says what the fragments do, and names
// each option that differs in one that does not.
func TestCheckConfig(t *testing.T) {
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("no awk")
	}
	script, err := filepath.Abs(filepath.Join(kernelDir, "check-config"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	frag := write("frag", "## a note\n\nCONFIG_A=y\n# CONFIG_B is not set\n# CONFIG_C is not set\nCONFIG_S=\"x\"\n")
	// C absent: Kconfig leaves out an option whose menu is off.
	good := write("good", "CONFIG_A=y\n# CONFIG_B is not set\nCONFIG_S=\"x\"\nCONFIG_OTHER=y\n")
	bad := write("bad", "# CONFIG_A is not set\nCONFIG_B=y\nCONFIG_C=m\nCONFIG_S=\"y\"\n")
	badFrag := write("badfrag", "CONFIG_A=y\nnot an option\n")

	for _, sh := range shelltest.Shells(t) {
		run := func(args ...string) (string, error) {
			out, err := exec.Command(sh[0], append(append(sh[1:], script), args...)...).CombinedOutput()
			return string(out), err
		}
		if out, err := run(good, frag); err != nil {
			t.Errorf("%v: good config: %v\n%s", sh, err, out)
		} else if !strings.Contains(out, "all 4 options as asked") {
			t.Errorf("%v: good config said %q", sh, out)
		}
		out, err := run(bad, frag)
		if err == nil {
			t.Errorf("%v: bad config passed:\n%s", sh, out)
		}
		for _, w := range []string{
			"CONFIG_A=y, got it off", "CONFIG_B off, got CONFIG_B=y",
			"CONFIG_C off, got CONFIG_C=m", `CONFIG_S="x", got CONFIG_S="y"`, "4 of 4 options differ",
		} {
			if !strings.Contains(out, w) {
				t.Errorf("%v: bad config: no %q in\n%s", sh, w, out)
			}
		}
		if out, err := run(good, badFrag); err == nil || !strings.Contains(out, "not an option line") {
			t.Errorf("%v: a malformed fragment passed (%v):\n%s", sh, err, out)
		}
		if out, err := run(good); err == nil {
			t.Errorf("%v: no fragment passed:\n%s", sh, out)
		}
	}
}
