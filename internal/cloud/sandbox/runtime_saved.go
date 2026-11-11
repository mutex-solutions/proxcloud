package sandbox

import (
	"errors"
	"regexp"
	"strings"

	distref "github.com/distribution/reference"
	"github.com/mutex-solutions/proxcloud/internal/registrypins"
)

// ErrManagedSavedStartHeld is a fixed, non-secret capability refusal. No request,
// copied CREATE annotation or dormant SnapshotProof enables a saved lifecycle.
var ErrManagedSavedStartHeld = errors.New("Managed snapshot resume/fork is held at this location; retained state is unchanged. Contact the platform operator.")

var savedProjectRE = regexp.MustCompile(`^[a-z0-9]{1,40}$`)

// SavedLifecycleAvailable is deliberately false until trusted snapshot lineage
// and exclusion of old issued Apply/controller recreation have a reviewed protocol.
// Capture/export and pinned CREATE are separate capabilities. There is no override.
func SavedLifecycleAvailable(Config) bool { return false }

// CheckSavedRuntimeImage permits only low-level compatibility with an EXACT
// current operator-curated canonical external/library image. It grants no platform
// pause/resume/fork capability. Managed, missing and unknown representations HOLD,
// even when listed as a curated template. Saved annotations are never authority.
func CheckSavedRuntimeImage(cfg Config, projectID, image string) error {
	if !savedProjectRE.MatchString(projectID) || image == "" || len(image) > 512 || strings.ContainsAny(image, "?#[]%\\\t\r\n ") {
		return ErrManagedSavedStartHeld
	}
	reg := strings.TrimRight(cfg.TemplateRegistry, "/")
	host, path, ok := strings.Cut(reg, "/")
	canonical, err := registrypins.CanonicalHost(host)
	if !ok || path != "t" || err != nil || canonical != host || reg != cfg.TemplateRegistry {
		return ErrManagedSavedStartHeld
	}
	first, rest, ok := strings.Cut(image, "/")
	actualHost, err := registrypins.CanonicalHost(first)
	if !ok || err != nil || actualHost != first {
		return ErrManagedSavedStartHeld
	}
	parsed, err := distref.Parse(image)
	named, isNamed := parsed.(distref.Named)
	if err != nil || !isNamed || named.String() != image {
		return ErrManagedSavedStartHeld
	}
	_, tagged := parsed.(distref.Tagged)
	_, digested := parsed.(distref.Canonical)
	if tagged == digested { // explicit tag OR digest, never combined or unqualified
		return ErrManagedSavedStartHeld
	}
	if digested {
		d := parsed.(distref.Canonical).Digest()
		if d.Algorithm().String() != "sha256" || len(d.Encoded()) != 64 || strings.ToLower(d.Encoded()) != d.Encoded() || d.Validate() != nil {
			return ErrManagedSavedStartHeld
		}
	}
	if actualHost == host && (!strings.HasPrefix(rest, "library/") || strings.TrimPrefix(rest, "library/") == "") {
		return ErrManagedSavedStartHeld
	}
	for _, template := range cfg.Templates {
		if template.Image == image {
			return nil
		}
	}
	return ErrManagedSavedStartHeld
}
