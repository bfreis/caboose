package broker

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
)

// Reload reads the configuration and the writable set again and rebuilds
// the policy from them. The daemon calls it when config.toml changes or
// the sandboxes' mounts do; the broker calls it itself after writing an
// "always" answer, so the sandbox's next try is decided by it. A Source
// that fails keeps the policy as it was, and is the error.
func (b *Broker) Reload() error {
	if err := b.owns(); err != nil {
		return err
	}
	s, err := b.opts.Source()
	if err == nil && s.Config == nil {
		err = fmt.Errorf("no configuration")
	}
	if err != nil {
		b.logf("cannot read the configuration, so the policy stays as it was: %v", err)
		return fmt.Errorf("broker: reading the configuration: %w", err)
	}
	c := s.Config
	b.complete(&s)
	p := policy.Policy{
		Trust:              policy.Trust(c.Trust),
		HostExec:           c.HostExec,
		OpenURLs:           c.OpenURLs,
		OpenURLAllow:       slices.Clone(c.OpenURLsAllow),
		CheckpointRequests: c.CheckpointRequests,
		Aliases:            map[string]policy.Alias{},
		Egress:             b.egress(c),
	}
	if c.ForwardPorts != "" {
		fp, err := config.ParsePorts(c.ForwardPorts)
		if err != nil {
			b.logf("forward_ports cannot be read, so no port is forwarded without asking: %v", err)
		} else {
			p.ForwardPorts = fp
		}
	}
	p.Writable, p.WritableIDs = b.writable()
	for _, a := range c.Aliases {
		params := map[string][]string{}
		for k, v := range a.Params {
			params[k] = slices.Clone(v)
		}
		p.Aliases[a.Name] = policy.Alias{Name: a.Name, Description: a.Description, Command: slices.Clone(a.Command),
			Cwd: a.Cwd, Timeout: a.Timeout, Params: params, Policy: a.Policy}
	}
	b.mu.Lock()
	b.pol, b.settings = p, s
	b.writableAt = b.now()
	b.mu.Unlock()
	return nil
}

// complete fills in what s leaves out and its Config says: the vm
// profile in use and config.toml's path.
func (b *Broker) complete(s *Settings) {
	c := s.Config
	if s.VMProfile == "" && c.Isolation == config.KindVM {
		s.VMProfile = strings.TrimPrefix(c.Profile, config.KindVM+".")
	}
	if s.ConfigPath == "" {
		if c.File != nil && c.File.Path != "" {
			s.ConfigPath = c.File.Path
		} else {
			s.ConfigPath = b.envDir + "/" + config.FileName
		}
	}
}

// writableTTL is how old the writable set may be when an alias is
// decided: the sandboxes' mounts change only as one is created, so a
// second or two is "now", and repeated runs ask the backends no more
// often than this.
const writableTTL = 2 * time.Second

// refreshWritable reads the writable set again, when the one the policy
// holds is older than maxAge, and puts it in the policy. One read at a
// time: those that come meanwhile use what it read.
func (b *Broker) refreshWritable(maxAge time.Duration) {
	b.writableMu.Lock()
	defer b.writableMu.Unlock()
	b.mu.Lock()
	fresh := b.now().Sub(b.writableAt) < maxAge && !b.writableAt.IsZero()
	b.mu.Unlock()
	if fresh {
		return
	}
	w, ids := b.writable()
	b.mu.Lock()
	b.pol.Writable, b.pol.WritableIDs = w, ids
	b.writableAt = b.now()
	b.mu.Unlock()
}

// writable is what some sandbox can write now, as Options.Writable says,
// with the identity of each path that exists; everything when it cannot
// say.
func (b *Broker) writable() ([]string, []policy.FileID) {
	everything := []string{"/"}
	if b.opts.Writable == nil {
		b.logf("nothing says what the sandboxes can write: every alias command counts as writable, and is refused")
		return everything, identities(everything)
	}
	w, err := b.opts.Writable()
	if err != nil {
		b.logf("cannot tell what the sandboxes can write, so every alias command counts as writable, and is refused: %v", err)
		return everything, identities(everything)
	}
	w = slices.Clone(w)
	return w, identities(w)
}

// identities are the identities of the paths that exist, each once.
func identities(paths []string) []policy.FileID {
	var ids []policy.FileID
	for _, p := range paths {
		if p == "" {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if id, ok := policy.IDOf(fi); ok && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// egress is the outbound proxy's policy: nil unless the profile in use is
// a vm one with egress on, and nil when its settings do not parse.
func (b *Broker) egress(c *config.Config) *policy.Egress {
	if c.Isolation != config.KindVM || !c.Egress {
		return nil
	}
	ports, err := config.ParsePortsOf("egress_ports", c.EgressPorts)
	if err != nil {
		b.logf("the outbound proxy refuses everything: %v", err)
		return nil
	}
	allow, err := policy.ParseAllowList("egress_allow", c.EgressAllow)
	if err != nil {
		b.logf("the outbound proxy refuses everything: %v", err)
		return nil
	}
	deny, err := policy.ParseAllowList("egress_deny", c.EgressDeny)
	if err != nil {
		b.logf("the outbound proxy refuses everything: %v", err)
		return nil
	}
	return &policy.Egress{Ports: ports, Allow: allow, Deny: deny}
}

// snapshot is the policy with the grants last read, and the settings,
// as of now.
func (b *Broker) snapshot(now time.Time) (policy.Policy, Settings) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.pol
	p.Grants = policyGrants(b.st.grants.Grants, now)
	return p, b.settings
}

// AliasInfo is an alias as the sandbox may list it.
type AliasInfo struct {
	Name, Description string
	Params            map[string][]string
	// Asks is set when running it asks a person first.
	Asks bool
}

// Aliases lists the aliases, by name, at most MaxAliases.
func (b *Broker) Aliases() ([]AliasInfo, error) {
	if err := b.owns(); err != nil {
		return nil, err
	}
	p, _ := b.snapshot(b.now())
	var out []AliasInfo
	for _, name := range slices.Sorted(maps.Keys(p.Aliases)) {
		if len(out) == MaxAliases {
			break
		}
		a := p.Aliases[name]
		params := map[string][]string{}
		for k, v := range a.Params {
			params[k] = slices.Clone(v)
		}
		out = append(out, AliasInfo{Name: a.Name, Description: a.Description, Params: params, Asks: a.Policy == policy.AliasAsk})
	}
	return out, nil
}

// Alias is the alias name, as config.toml defines it.
func (b *Broker) Alias(name string) (policy.Alias, bool, error) {
	if err := b.owns(); err != nil {
		return policy.Alias{}, false, err
	}
	p, _ := b.snapshot(b.now())
	a, ok := p.Aliases[name]
	return a, ok, nil
}
