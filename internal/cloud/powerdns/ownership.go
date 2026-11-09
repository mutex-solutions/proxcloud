package powerdns

import (
	"context"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// CanonicalZone is the single DNS-ownership identity: a strict IDNA A-label FQDN,
// beneath a known ICANN or private public suffix. Unknown/internal suffixes are
// not an alternate tenant namespace (controlled drills use e.g. lab.example.org).
func CanonicalZone(name string) (string, error) {
	bare := strings.TrimSuffix(strings.TrimSpace(name), ".")
	ascii, err := idna.Lookup.ToASCII(bare)
	ascii = strings.ToLower(ascii)
	if err != nil || len(ascii) > 253 || !zoneNameRe.MatchString(ascii+".") {
		return "", zonef("Enter a valid domain name, such as example.org")
	}
	suffix, icann := publicsuffix.PublicSuffix(ascii)
	if ascii == suffix || (!icann && !strings.Contains(suffix, ".")) {
		return "", zonef("Use a domain below a recognized public suffix, not a public suffix or internal name")
	}
	return ascii + ".", nil
}

// CheckAvailable performs a LIVE preflight before proof and creation. An exact
// zone is never proof of a new claim, even when its account is this project.
// Same-project managed parents/children remain permitted; foreign overlaps and
// operator zones retain the existing server-side isolation guard.
func (s *Service) CheckAvailable(ctx context.Context, projectID, name string) error {
	canonical, err := CanonicalZone(name)
	if err != nil {
		return err
	}
	if err := s.checkOverlap(ctx, projectID, canonical); err != nil {
		return err
	}
	own, err := s.List(ctx, projectID)
	if err != nil {
		return err
	}
	for _, z := range own {
		if NormalizeZone(z.Name) == canonical {
			return zonef("Zone already exists. Manage its records instead of creating it again")
		}
	}
	return nil
}
