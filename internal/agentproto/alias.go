package agentproto

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The bounds of an alias run's argv (OpAliasRun): a name, then at most
// MaxAliasArgs KEY=VALUE arguments.
const MaxAliasArgs = 32

var (
	// aliasName is an alias's or a parameter's name, as config.toml's
	// [alias.NAME] and its params take them.
	aliasName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	// aliasValue is a parameter's value.
	aliasValue = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// ParseAliasArgv reads an alias run's argv, [NAME, KEY=VALUE...]: the
// alias's name and its arguments, each key once. Both sides read it so:
// the agent to say what is wrong before it asks, the host because what
// the agent sends is the sandbox's. Only the syntax is checked here; which
// keys and values an alias takes is the host's to say.
func ParseAliasArgv(argv []string) (name string, args map[string]string, err error) {
	if len(argv) == 0 || argv[0] == "" {
		return "", nil, errors.New("no alias named")
	}
	if !aliasName.MatchString(argv[0]) {
		return "", nil, fmt.Errorf("%q is not an alias name: lowercase letters, digits, - and _, starting with a letter or digit, at most 32", clipArg(argv[0]))
	}
	if len(argv)-1 > MaxAliasArgs {
		return "", nil, fmt.Errorf("%d arguments, at most %d", len(argv)-1, MaxAliasArgs)
	}
	args = map[string]string{}
	for _, a := range argv[1:] {
		k, v, ok := strings.Cut(a, "=")
		switch {
		case !ok:
			return "", nil, fmt.Errorf("%q is not KEY=VALUE: an alias takes only its parameters, each as KEY=VALUE", clipArg(a))
		case !aliasName.MatchString(k):
			return "", nil, fmt.Errorf("%q is not a parameter name: lowercase letters, digits, - and _, at most 32", clipArg(k))
		case !aliasValue.MatchString(v):
			return "", nil, fmt.Errorf("%q is not a parameter value: letters, digits, . _ and -, 1 to 64", clipArg(v))
		}
		if _, dup := args[k]; dup {
			return "", nil, fmt.Errorf("%s is given twice", k)
		}
		args[k] = v
	}
	return argv[0], args, nil
}

// clipArg is s cut short for an error, with what a terminal would not
// show as it is escaped by %q.
func clipArg(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.ToValidUTF8(s, "?")
}
