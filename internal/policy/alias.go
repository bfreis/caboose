package policy

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Substitute is the argv alias a runs with args: every element that is
// exactly {KEY}, for a key of a's params, replaced by args[KEY], or by
// the first of its values when args has none. Nothing else is replaced,
// not even {KEY} inside a longer element. args are checked as Decide
// checks them: at most MaxArgs, each key a parameter of a and each value
// one of its values.
func Substitute(a Alias, args map[string]string) ([]string, error) {
	if err := checkArgs(a, args); err != nil {
		return nil, err
	}
	if len(a.Command) == 0 {
		return nil, errors.New("the alias has no command")
	}
	argv := make([]string, len(a.Command))
	for i, e := range a.Command {
		argv[i] = e
		key, ok := strings.CutPrefix(e, "{")
		if !ok {
			continue
		}
		key, ok = strings.CutSuffix(key, "}")
		if !ok {
			continue
		}
		values, ok := a.Params[key]
		if !ok {
			continue
		}
		if v, ok := args[key]; ok {
			argv[i] = v
			continue
		}
		if len(values) == 0 {
			return nil, fmt.Errorf("parameter %s has no value to take", key)
		}
		argv[i] = values[0]
	}
	return argv, nil
}

// checkArgs checks a request's KEY=VALUE arguments against alias a.
func checkArgs(a Alias, args map[string]string) error {
	if len(args) > MaxArgs {
		return fmt.Errorf("%d arguments, at most %d", len(args), MaxArgs)
	}
	for _, k := range slices.Sorted(maps.Keys(args)) {
		values, ok := a.Params[k]
		if !ok {
			return fmt.Errorf("%q is not one of its parameters", clip(printable(k), 64))
		}
		if !slices.Contains(values, args[k]) {
			return fmt.Errorf("%q is not a value parameter %s takes", clip(printable(args[k]), 64), clip(printable(k), 64))
		}
	}
	return nil
}
