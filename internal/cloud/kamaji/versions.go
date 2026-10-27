package kamaji

import (
	"fmt"
	"strconv"
	"strings"
)

// versions.go — the managed upgrade-path rule (plan §3.4). Control planes move at most ONE
// minor at a time (the kubeadm/Kamaji constraint), never backwards, same major only; node
// groups rotate with the CP off the same values change, so the kubelet skew rule
// (kubelet ≤ apiserver, ≤3 minors behind) can never be violated by an accepted upgrade.

// ParseVersion splits "1.35.4" (an optional leading "v" is tolerated) into numeric parts.
func ParseVersion(v string) (major, minor, patch int, err error) {
	parts := strings.SplitN(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".", 3)
	if len(parts) < 2 {
		return 0, 0, 0, fmt.Errorf("kubernetes version %q: want major.minor[.patch]", v)
	}
	nums := [3]int{}
	for i, p := range parts {
		n, perr := strconv.Atoi(p)
		if perr != nil || n < 0 {
			return 0, 0, 0, fmt.Errorf("kubernetes version %q: bad component %q", v, p)
		}
		nums[i] = n
	}
	return nums[0], nums[1], nums[2], nil
}

// ValidateUpgradePath accepts a patch bump within the current minor, or a jump of exactly one
// minor (any patch). Everything else — downgrades, same-version, multi-minor jumps, a major
// change — is refused with a client-correctable error.
func ValidateUpgradePath(current, target string) error {
	cMaj, cMin, cPat, err := ParseVersion(current)
	if err != nil {
		return err
	}
	tMaj, tMin, tPat, err := ParseVersion(target)
	if err != nil {
		return err
	}
	switch {
	case tMaj != cMaj:
		return fmt.Errorf("upgrade %s → %s: major version changes are not supported", current, target)
	case tMin < cMin, tMin == cMin && tPat <= cPat:
		return fmt.Errorf("upgrade %s → %s: target must be newer than the current version", current, target)
	case tMin > cMin+1:
		return fmt.Errorf("upgrade %s → %s: control planes upgrade one minor at a time — go through 1.%d first", current, target, cMin+1)
	}
	return nil
}
