package externalservice

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// TrueNASOwnerConflictError is a project-local configuration conflict. Both
// providers address the same dataset namespace, so neither may mutate exports.
type TrueNASOwnerConflictError struct {
	First, Second string
}

func (e *TrueNASOwnerConflictError) Error() string {
	return fmt.Sprintf("TrueNAS providers %s and %s share one backend and parent dataset: attach only one owner to this project before changing file shares", e.First, e.Second)
}

// ValidateTrueNASOwners checks attached providers, not the global provider catalog.
// Disabled bindings still own their existing datasets until explicitly detached.
// URL aliases for the same physical NAS are an operator responsibility; we never
// resolve DNS or compare credentials to guess whether two servers are identical.
func ValidateTrueNASOwners(services []*ExternalService) error {
	owners := map[string]string{}
	for _, es := range services {
		if es == nil || !es.IsTrueNAS() {
			continue
		}
		key, err := trueNASOwnerKey(es)
		if err != nil {
			return err
		}
		if owner, ok := owners[key]; ok && owner != es.ID {
			return &TrueNASOwnerConflictError{First: owner, Second: es.ID}
		}
		owners[key] = es.ID
	}
	return nil
}

// ValidateTrueNASOwner validates one target without making unrelated NAS parents
// unavailable when another pair of bindings conflicts on the same project.
func ValidateTrueNASOwner(target *ExternalService, attached []*ExternalService) error {
	key, err := trueNASOwnerKey(target)
	if err != nil {
		return err
	}
	for _, es := range attached {
		if es == nil || !es.IsTrueNAS() || es.ID == target.ID {
			continue
		}
		other, err := trueNASOwnerKey(es)
		if err != nil {
			continue // invalid unrelated binding owns no known target namespace
		}
		if key == other {
			return &TrueNASOwnerConflictError{First: target.ID, Second: es.ID}
		}
	}
	return nil
}

func trueNASOwnerKey(es *ExternalService) (string, error) {
	if es == nil {
		return "", fmt.Errorf("TrueNAS provider is missing")
	}
	u, err := url.Parse(strings.TrimSpace(str(es.Config["apiUrl"])))
	parent := strings.Trim(str(es.Config["parentDataset"]), "/ ")
	if err != nil || u.Hostname() == "" || parent == "" {
		return "", fmt.Errorf("TrueNAS provider %s has an invalid backend or parent dataset", es.ID)
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(strings.TrimSuffix(u.Hostname(), ".")), port) + "/" + parent, nil
}
