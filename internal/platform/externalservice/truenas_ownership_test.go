package externalservice

import (
	"errors"
	"testing"
)

func TestTrueNASParentHasOneOwnerPerProject(t *testing.T) {
	nas := func(id, endpoint, parent string) *ExternalService {
		return &ExternalService{ID: id, Config: map[string]any{"provider": "truenas", "apiUrl": endpoint, "parentDataset": parent}}
	}
	for _, tc := range []struct {
		name     string
		other    *ExternalService
		conflict bool
	}{
		{"identical", nas("b", "https://nas.example", "tank/shares"), true},
		{"websocket canonical", nas("b", "wss://NAS.example.:443/api/current/", "/tank/shares/"), true},
		{"same provider binding twice", nas("a", "https://nas.example:443", "tank/shares"), false},
		{"different parent", nas("b", "https://nas.example", "tank/other"), false},
		{"different backend", nas("b", "https://other.example", "tank/shares"), false},
		{"different port", nas("b", "https://nas.example:8443", "tank/shares"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var conflict *TrueNASOwnerConflictError
			err := ValidateTrueNASOwners([]*ExternalService{nas("a", "https://nas.example", "tank/shares"), tc.other})
			if errors.As(err, &conflict) != tc.conflict {
				t.Fatalf("err = %v", err)
			}
		})
	}
}
