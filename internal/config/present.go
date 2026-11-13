package config

import (
	"maps"
	"slices"
	"strings"
)

// Present reports, for each path, whether the configuration has a value
// there. A path is written as a Change names a setting:
// companies[3000].release_dual_limit for a setting, companies[3000] for
// a list item, payments for a section. A setting has a value when it is
// set to something other than the empty text; an item or a section has
// one when a setting below it has. Custom field definitions (extensions)
// are not looked at, as Diff compares them apart. A nil configuration
// has no value anywhere. It answers the control matrix, which shows
// whether the setting a control is linked to is still there (ADR-0078
// §8).
func (c *Config) Present(paths []string) map[string]bool {
	flat := flatten(c)
	maps.DeleteFunc(flat, func(_, v string) bool { return v == "" })
	keys := slices.Sorted(maps.Keys(flat))
	out := make(map[string]bool, len(paths))
	for _, p := range paths {
		out[p] = present(keys, p)
	}
	return out
}

// present reports whether the sorted paths hold path itself, or a path
// below it: one that goes on with a field (".") or a list item ("[").
// Each is looked up by binary search, so a matrix of thousands of links
// reads a large configuration once.
func present(keys []string, path string) bool {
	if path == "" {
		return false
	}
	if _, found := slices.BinarySearch(keys, path); found {
		return true
	}
	for _, below := range []string{path + ".", path + "["} {
		if i, _ := slices.BinarySearch(keys, below); i < len(keys) && strings.HasPrefix(keys[i], below) {
			return true
		}
	}
	return false
}
