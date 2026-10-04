package linkdebug

import "testing"

func TestParse(t *testing.T) {
	k := Parse("sockbuf=0,guestbuf=0,window=262144,relaybuf=0,onewrite=0,other=1")
	want := Knobs{Raw: k.Raw, NoSockBuf: true, NoGuestBuf: true, NoRelayBuf: true, SplitFrames: true, Window: 262144}
	if k != want {
		t.Fatalf("got %+v", k)
	}
	if k := Parse("sockbuf=1"); k.NoSockBuf || k.Raw != "sockbuf=1" {
		t.Fatalf("sockbuf=1: %+v", k)
	}
	for _, v := range []string{"", "sockbuf=0 quiet", "window=1;x"} {
		if k := Parse(v); k != (Knobs{}) {
			t.Fatalf("%q: %+v", v, k)
		}
	}
}

func TestCmdline(t *testing.T) {
	t.Setenv(Var, "")
	if c := Cmdline(); c != "" {
		t.Fatalf("unset: %q", c)
	}
	t.Setenv(Var, "sockbuf=0,window=262144")
	c := Cmdline()
	if c != " caboose.debuglink=sockbuf=0,window=262144" {
		t.Fatalf("got %q", c)
	}
	if v := fromCmdline("console=hvc0 panic=-1" + c + " ip=dhcp"); v != "sockbuf=0,window=262144" {
		t.Fatalf("read back %q", v)
	}
}
