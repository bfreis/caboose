package hostlink

import "github.com/bfreis/caboose/internal/config"

// PortSet is a set of ports, as config.toml's forward_ports and
// egress_ports read (config.ParsePorts).
type PortSet = config.PortSet

// ParsePorts reads forward_ports: see config.ParsePorts.
func ParsePorts(s string) (PortSet, error) { return config.ParsePorts(s) }

// ParsePortsOf reads a setting of ParsePorts' form, key naming it in errors.
func ParsePortsOf(key, s string) (PortSet, error) { return config.ParsePortsOf(key, s) }
