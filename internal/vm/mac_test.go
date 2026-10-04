package vm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMACIsStableAndLocalUnicast(t *testing.T) {
	data := t.TempDir()
	dirs := []Dir{
		Dir(filepath.Join(data, "envs", "a", "data", "vm", "caboose-a")),
		Dir(filepath.Join(data, "envs", "a", "data", "vm", "builder")),
		Dir(filepath.Join(data, "envs", "b", "data", "vm", "caboose-b")),
		Dir(filepath.Join(data, "envs", "b", "data", "vm", "builder")),
		Dir(filepath.Join(data, "caboose", "vm", "caboose")),
	}
	seen := map[string]Dir{}
	for _, d := range dirs {
		mac := d.MAC()
		if err := CheckMAC(mac); err != nil {
			t.Errorf("%s: %v", d, err)
		}
		if again := d.MAC(); again != mac {
			t.Errorf("%s: %s, then %s", d, mac, again)
		}
		if o, ok := seen[mac]; ok {
			t.Errorf("%s and %s share %s", d, o, mac)
		}
		seen[mac] = d
	}
	// A path given relative is the same VM as its absolute one.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if Dir("vm/box").MAC() != Dir(filepath.Join(wd, "vm", "box")).MAC() {
		t.Error("a relative dir has another MAC than its absolute path")
	}
	// Pinned: a change to the derivation hands every VM a new lease, so
	// it has to be a decision.
	if got := MACFor("/x/vm/box"); got != "32:d1:53:cb:22:23" {
		t.Errorf("MACFor = %q", got)
	}
}

func TestCheckMAC(t *testing.T) {
	for mac, ok := range map[string]bool{
		"02:00:00:00:00:01": true, "fe:12:34:56:78:9a": true,
		"00:11:22:33:44:55": false, // universally administered
		"03:00:00:00:00:01": false, // multicast
		"02:00:00:00:00":    false, "02-00-00-00-00-01": false,
		"02:00:00:00:00:00:00:01": false, "": false, "02:AB:00:00:00:01": false,
	} {
		if err := CheckMAC(mac); (err == nil) != ok {
			t.Errorf("CheckMAC(%q) = %v", mac, err)
		}
	}
}

func TestMachineMACRoundTrip(t *testing.T) {
	d := Dir(t.TempDir() + "/vm/box")
	m := Machine{Kernel: "/k", CPUs: 2, Network: "nat", MAC: d.MAC()}
	if err := d.WriteMachine(m); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(d.Machine())
	if err != nil {
		t.Fatal(err)
	}
	var got Machine
	if err := json.Unmarshal(b, &got); err != nil || !reflect.DeepEqual(got, m) {
		t.Fatalf("got %+v, %v", got, err)
	}
	// One from before the field: no MAC, which the runner leaves random.
	old := []byte(`{"kernel":"/k","initramfs":"/i","cpus":2,"memory_mib":512,"disks":[],"network":"nat","console":"/c"}`)
	got = Machine{}
	if err := json.Unmarshal(old, &got); err != nil || got.MAC != "" || got.Network != "nat" {
		t.Fatalf("old machine.json: %+v, %v", got, err)
	}
	// And none written when there is none.
	if err := d.WriteMachine(Machine{Network: "none"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(d.Machine()); json.Valid(b) && containsKey(b, "mac") {
		t.Errorf("an empty MAC is written: %s", b)
	}
}

func containsKey(b []byte, k string) bool {
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	_, ok := m[k]
	return ok
}
