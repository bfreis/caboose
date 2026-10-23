package config

import (
	"strings"
	"testing"
)

// forward_ports may never hold one of the web UI's ports, alone or within
// a range, whether config.toml is read or caboose writes it.
func TestForwardPortsRefusesUIPorts(t *testing.T) {
	for _, tc := range []struct{ ports, port string }{
		{"[7171]", "7171"},
		{"[7172]", "7172"},
		{"[3000, 7172]", "7172"},
		{`["7000-7999"]`, "7171"},
		{`["7172-7200"]`, "7172"},
		{`["1-65535"]`, "7171"},
		{`["7171-7171"]`, "7171"},
	} {
		body := "[link]\nforward_ports = " + tc.ports + "\n"
		_, err := load(t, body)
		if err == nil || !strings.Contains(err.Error(), "forward_ports in [link] includes port "+tc.port) ||
			!strings.Contains(err.Error(), "web UI") {
			t.Errorf("load %s: err = %v", tc.ports, err)
		}
		if _, err := ParseFile(cfgPath, []byte(body)); err == nil {
			t.Errorf("ParseFile %s: not refused", tc.ports)
		}
		e := Edit{Set: map[string]any{"link.forward_ports": []string{"x"}}}
		if err := CheckEdit("config.toml", nil, []byte(body), e); err == nil || !strings.Contains(err.Error(), "includes port "+tc.port) {
			t.Errorf("CheckEdit %s: err = %v", tc.ports, err)
		}
	}
	for _, ok := range []string{"[7170]", "[7173]", `["7000-7170"]`, `["7173-7999"]`, "[]"} {
		if _, err := load(t, "[link]\nforward_ports = "+ok+"\n"); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	// The default holds neither.
	set, err := ParsePorts(DefaultForwardPorts)
	if err != nil || CheckForwardPorts("forward_ports", set) != nil {
		t.Errorf("default: %v", err)
	}
}
