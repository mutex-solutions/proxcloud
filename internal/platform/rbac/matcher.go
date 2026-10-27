package rbac

import (
	"sort"
	"strings"
)

// MatchesPattern reports whether a granted pattern covers a permission key:
//
//	"*"            matches everything
//	exact equality
//	"resource:*"   matches keys starting "resource:"
func MatchesPattern(pattern, key string) bool {
	switch {
	case pattern == "" || key == "":
		return false
	case pattern == "*":
		return true
	case pattern == key:
		return true
	case strings.HasSuffix(pattern, ":*"):
		resourceType := pattern[:len(pattern)-2]
		return strings.HasPrefix(key, resourceType+":")
	default:
		return false
	}
}

// Matches reports whether any granted pattern covers the required key.
func Matches(grantedPatterns []string, key string) bool {
	for _, p := range grantedPatterns {
		if MatchesPattern(p, key) {
			return true
		}
	}
	return false
}

// ExpandPatterns returns the concrete permission keys covered by the patterns,
// sorted (stable output).
func ExpandPatterns(patterns []string) []string {
	if len(patterns) == 0 {
		return []string{}
	}
	var out []string
	for _, key := range AllPermissions {
		if Matches(patterns, key) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}
