package seed

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"testing"
)

// Every embedded file is the file on disk and parses; the build fails first when one is missing.
func TestEmbeddedFilesParse(t *testing.T) {
	for name, raw := range map[string][]byte{
		"flavors.json": Flavors, "price-plan-seed.json": PricePlan,
		"platform-configuration.json": PlatformConfiguration, "billing-configuration.json": BillingConfiguration,
	} {
		disk, err := os.ReadFile(name)
		if err != nil || string(disk) != string(raw) {
			t.Errorf("%s: embedded bytes differ from the file (%v)", name, err)
		}
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// seed-operator.py and the API write the same default quota.
func TestDefaultQuotaMatchesSeedOperator(t *testing.T) {
	src, err := os.ReadFile("seed-operator.py")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^DEFAULT_QUOTA = (\{.*\})$`).FindSubmatch(src)
	if m == nil {
		t.Fatal("DEFAULT_QUOTA not found in seed-operator.py")
	}
	var py map[string]int
	if err := json.Unmarshal(m[1], &py); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(py, DefaultQuota) {
		t.Fatalf("seed-operator.py %v, seed.go %v", py, DefaultQuota)
	}
}
