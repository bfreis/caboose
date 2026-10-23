package broker

import (
	"errors"
	"slices"
	"strings"

	"github.com/bfreis/caboose/internal/config"
	"github.com/bfreis/caboose/internal/policy"
)

// MaxEgressEditEntries is the most entries one egress edit adds and
// removes in all.
const MaxEgressEditEntries = 32

// maxEgressEntry is the longest entry an egress edit takes, in bytes: a
// DNS name, a pattern of one, or a CIDR.
const maxEgressEntry = 260

// EgressEdit is a person's change to the egress_allow and egress_deny of
// the vm profile in use.
type EgressEdit struct {
	// AddAllow and AddDeny are entries to add, each one egress_allow
	// takes (policy.ParseAllowList); RemoveAllow and RemoveDeny are
	// entries to remove, each in the list now.
	AddAllow, RemoveAllow, AddDeny, RemoveDeny []string
	// By is who edits: ByCLI, ByWeb or ByApp.
	By string
	// Confirm is the DiffHash of the diff the person was shown. Without
	// it nothing is written: the edit's diff comes back to be shown. With
	// it the edit is written when it still makes exactly that diff, and
	// is ErrStaleDiff otherwise.
	Confirm string
	// Shown, when set, reports whether a diff is shown as it is
	// written. An edit whose diff it refuses is neither offered nor
	// written: the hash confirms the diff as written, so what the
	// person saw must be that.
	Shown func(diff string) bool
}

// EgressEditResult is what an egress edit does, or did.
type EgressEditResult struct {
	// Diff is the change to config.toml, exactly as it is (or was)
	// written, and DiffHash the hash a confirmation names it by.
	Diff, DiffHash string
	// Effect says what the edit does, in the host's words.
	Effect string
	// Written is set once config.toml has the change and the policy
	// decides by it.
	Written bool
}

// EditEgress computes, and with a confirmation writes, a change to the
// egress_allow and egress_deny of the vm profile in use. The edit is
// computed from config.toml as it is now, never from what the policy last
// read, and it runs under the same lock as an answer's write, so two
// edits never interleave; once written, the policy is rebuilt at once, so
// the next connection is decided by it.
func (b *Broker) EditEgress(e EgressEdit) (EgressEditResult, error) {
	var res EgressEditResult
	if err := b.owns(); err != nil {
		return res, err
	}
	if err := e.check(); err != nil {
		return res, err
	}
	if b.opts.ConfigWriter == nil {
		return res, newError(KindUnavailable, "this host cannot write config.toml")
	}
	var (
		editErr    error
		written    bool
		configPath string
	)
	err := b.txn(func(*state) (bool, bool, error) {
		set, err := b.opts.Source()
		if err == nil && set.Config == nil {
			err = errors.New("no configuration")
		}
		if err != nil {
			editErr = newError(KindUnavailable, "cannot read the configuration: %v", err)
			return false, false, nil
		}
		b.complete(&set)
		configPath = set.ConfigPath
		if set.VMProfile == "" {
			editErr = newError(KindBadRequest, "egress rules are enforced only under vm today")
			return false, false, nil
		}
		edit, effect, err := b.egressEdit(e, set)
		if err != nil {
			editErr = err
			return false, false, nil
		}
		diff, err := b.opts.ConfigWriter.Diff(edit)
		if err != nil {
			editErr = newError(KindUnavailable, "cannot make the change to %s: %v", set.ConfigPath, err)
			return false, false, nil
		}
		if diff == "" {
			editErr = newError(KindBadRequest, "%s already says so: nothing to change", set.ConfigPath)
			return false, false, nil
		}
		if len(diff) > MaxOfferDiffBytes || strings.Count(strings.TrimSuffix(diff, "\n"), "\n") >= MaxOfferDiffLines {
			editErr = newError(KindUnavailable, "the change to %s is over %d lines or %d bytes: too long to show, so not made", set.ConfigPath, MaxOfferDiffLines, MaxOfferDiffBytes)
			return false, false, nil
		}
		if e.Shown != nil && !e.Shown(diff) {
			editErr = newError(KindUnavailable, "the change to %s cannot be shown as it would be written, so it is not made; edit the file itself", set.ConfigPath)
			return false, false, nil
		}
		res.Diff, res.DiffHash, res.Effect = diff, DiffHash(diff), effect
		if e.Confirm == "" {
			return false, false, nil
		}
		if e.Confirm != res.DiffHash {
			editErr = newError(KindStaleDiff, "%s would change differently than the diff confirmed; look at it again", set.ConfigPath)
			return false, false, nil
		}
		if err := b.opts.ConfigWriter.Write(edit, e.Confirm); err != nil {
			if errors.Is(err, ErrStaleDiff) {
				editErr = err
			} else {
				editErr = newError(KindUnavailable, "cannot write %s: %v", set.ConfigPath, err)
			}
			return false, false, nil
		}
		written = true
		return false, false, nil
	})
	if err == nil {
		err = editErr
	}
	if written {
		// The file has the change whatever comes next: decide by it now.
		if rerr := b.Reload(); rerr != nil {
			b.logf("after writing an egress edit: %v", rerr)
			if err == nil {
				err = newError(KindUnavailable, "%s has the change, but the policy could not be rebuilt from it: %v", configPath, rerr)
			}
		}
		b.dlog.egressEdit(b.now(), e.By, res.Effect)
	}
	if err != nil {
		return EgressEditResult{}, err
	}
	res.Written = written
	return res, nil
}

func (e *EgressEdit) check() error {
	switch e.By {
	case ByCLI, ByWeb, ByApp:
	default:
		return newError(KindBadRequest, "an egress edit comes from %q, %q or %q, not %q", ByCLI, ByWeb, ByApp, clip(e.By, 32))
	}
	n := len(e.AddAllow) + len(e.RemoveAllow) + len(e.AddDeny) + len(e.RemoveDeny)
	if n == 0 {
		return newError(KindBadRequest, "an egress edit adds or removes at least one entry")
	}
	if n > MaxEgressEditEntries {
		return newError(KindBadRequest, "an egress edit adds and removes at most %d entries, not %d", MaxEgressEditEntries, n)
	}
	for _, l := range []struct {
		key  string
		adds bool
		list []string
	}{{"egress_allow", true, e.AddAllow}, {"egress_allow", false, e.RemoveAllow}, {"egress_deny", true, e.AddDeny}, {"egress_deny", false, e.RemoveDeny}} {
		for i, v := range l.list {
			if len(v) > maxEgressEntry {
				return newError(KindBadRequest, "%s: an entry over %d bytes", l.key, maxEgressEntry)
			}
			if slices.Contains(l.list[:i], v) {
				return newError(KindBadRequest, "%s: %q is named twice", l.key, shown(v, 100))
			}
			if !l.adds {
				continue
			}
			if _, err := policy.ParseAllowList(l.key, []string{v}); err != nil {
				return newError(KindBadRequest, "%s", shown(err.Error(), 400))
			}
		}
	}
	for _, v := range e.AddAllow {
		if slices.Contains(e.RemoveAllow, v) {
			return newError(KindBadRequest, "egress_allow: %q is both added and removed", shown(v, 100))
		}
	}
	for _, v := range e.AddDeny {
		if slices.Contains(e.RemoveDeny, v) {
			return newError(KindBadRequest, "egress_deny: %q is both added and removed", shown(v, 100))
		}
	}
	return nil
}

// egressEdit is the config.toml edit e makes under set, and its effect.
func (b *Broker) egressEdit(e EgressEdit, set Settings) (config.Edit, string, error) {
	c := set.Config
	trust := policy.Trust(c.Trust)
	edit := config.Edit{Set: map[string]any{}}
	var effects []string
	for _, l := range []struct {
		field          string
		deny           bool
		cur            []string
		adds, removals []string
	}{
		{"egress_allow", false, c.EgressAllow, e.AddAllow, e.RemoveAllow},
		{"egress_deny", true, c.EgressDeny, e.AddDeny, e.RemoveDeny},
	} {
		if len(l.adds) == 0 && len(l.removals) == 0 {
			continue
		}
		key := "vm." + set.VMProfile + "." + l.field
		list := l.cur
		if c.File != nil {
			list = fileList(c, key)
		}
		list = slices.Clone(list)
		for _, v := range l.removals {
			i := slices.Index(list, v)
			if i < 0 {
				return config.Edit{}, "", newError(KindBadRequest, "%s in [vm.%s] has no entry %q to remove", l.field, set.VMProfile, shown(v, 100))
			}
			list = slices.Delete(list, i, i+1)
			effects = append(effects, policy.EgressRuleEffect(trust, l.deny, false, v, b.opts.Env))
		}
		for _, v := range l.adds {
			if slices.Contains(list, v) {
				continue
			}
			list = append(list, v)
			effects = append(effects, policy.EgressRuleEffect(trust, l.deny, true, v, b.opts.Env))
		}
		if list == nil {
			list = []string{}
		}
		edit.Set[key] = list
	}
	if len(effects) == 0 {
		return config.Edit{}, "", newError(KindBadRequest, "%s already says so: nothing to change", set.ConfigPath)
	}
	if err := b.keepsRequiredHosts(e, set, edit); err != nil {
		return config.Edit{}, "", err
	}
	effect := strings.Join(effects, "; ")
	if trust == policy.TrustFallible && !c.Egress {
		effect += "; egress is off in [vm." + set.VMProfile + "], so none of this applies until it is on"
	}
	return edit, effect, nil
}

// keepsRequiredHosts refuses, under any trust but fallible, an edit that
// would leave a host of Options.RequiredHosts unallowed or denied when
// the lists before it did not: a hostile launch refuses until
// egress_allow names every one, so the edit would stop the sandbox from
// starting. With no RequiredHosts it refuses any removal from
// egress_allow and any addition to egress_deny.
func (b *Broker) keepsRequiredHosts(e EgressEdit, set Settings, edit config.Edit) error {
	c := set.Config
	if c.Trust == config.TrustFallible {
		return nil
	}
	if len(e.RemoveAllow) == 0 && len(e.AddDeny) == 0 {
		return nil
	}
	if b.opts.RequiredHosts == nil {
		return newError(KindBadRequest, "under trust = %s this host cannot tell which hosts the sandbox needs to start, so it removes no egress_allow entry and adds no egress_deny entry: edit %s by hand", shown(c.Trust, 32), set.ConfigPath)
	}
	allowKey, denyKey := "vm."+set.VMProfile+".egress_allow", "vm."+set.VMProfile+".egress_deny"
	newAllow, newDeny := c.EgressAllow, c.EgressDeny
	if v, ok := edit.Set[allowKey].([]string); ok {
		newAllow = v
	}
	if v, ok := edit.Set[denyKey].([]string); ok {
		newDeny = v
	}
	reach := func(allowList, denyList []string) (func(string) bool, error) {
		allow, err := policy.ParseAllow(strings.Join(allowList, " "))
		if err != nil {
			return nil, err
		}
		deny, err := policy.ParseAllowOf("egress_deny", strings.Join(denyList, " "))
		if err != nil {
			return nil, err
		}
		return func(h string) bool { return allow.Name(h) && !deny.Name(h) }, nil
	}
	after, err := reach(newAllow, newDeny)
	if err != nil {
		return newError(KindBadRequest, "%v", err)
	}
	// What the file allows now; a list that does not parse counts as
	// reaching every host, so the edit must keep them all.
	before, err := reach(c.EgressAllow, c.EgressDeny)
	if err != nil {
		before = func(string) bool { return true }
	}
	var cut []string
	for _, h := range b.opts.RequiredHosts {
		if before(h) && !after(h) {
			cut = append(cut, shown(h, 100))
		}
	}
	if len(cut) > 0 {
		return newError(KindBadRequest, "under trust = %s a launch refuses unless egress_allow reaches %s, which this edit would cut off: the sandbox could not start", shown(c.Trust, 32), strings.Join(cut, ", "))
	}
	return nil
}
