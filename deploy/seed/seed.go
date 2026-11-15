// Package seed holds the default documents of a new install as embedded files: the flavor catalog, the public rate
// card, and the platform and billing configuration. The API previews and applies them (GET and POST
// /admin/setup/defaults); seed-operator.py and the Compose seed read the same JSON, so there is one copy of the
// numbers.
package seed

import _ "embed"

var (
	//go:embed flavors.json
	Flavors []byte
	//go:embed price-plan-seed.json
	PricePlan []byte
	//go:embed platform-configuration.json
	PlatformConfiguration []byte
	//go:embed billing-configuration.json
	BillingConfiguration []byte
)

// DefaultQuota is the default project quota of a Proxmox provider (config.provisioning.quota), the DEFAULT_QUOTA of
// seed-operator.py. seed_test.go keeps the two equal.
var DefaultQuota = map[string]int{"servers": 20, "vcpus": 80, "ramMb": 102400, "diskGb": 2000, "networks": 10, "floatingIps": 5}
