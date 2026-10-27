// Package cloud is the cloud-resource cache domain (the `cloudResource` table
// that mirrors OpenStack state) + its history archive. This file defines the CloudResource
// document and the persistence/sync layer (the two-op optimistic findAndModify + the
// cloudResourceHistory soft-delete + the wasUserDeletedAfter recreation guard); the read
// side (counts) backs the project endpoints. Providers + the live OpenStack sync (via
// internal/cloud/client) land in later phases.
package cloud

import (
	"strings"
	"time"
)

// CloudResourceType values — the full set (40 values, in
// declaration order). `type` is the SOLE discriminator for the free-form `data` payload.
const (
	TypeServer               = "SERVER"
	TypeBaremetalServer      = "BAREMETAL_SERVER"
	TypeVolume               = "VOLUME"
	TypeVolumeBackup         = "VOLUME_BACKUP"
	TypeVolumeSnapshot       = "VOLUME_SNAPSHOT"
	TypeImage                = "IMAGE"
	TypeKeypair              = "KEYPAIR"
	TypeDNSZone              = "DNS_ZONE"
	TypeKubernetesCluster    = "KUBERNETES_CLUSTER"
	TypeDatabaseCluster      = "DATABASE_CLUSTER"
	TypeLoadBalancer         = "LOAD_BALANCER"
	TypeFloatingIP           = "FLOATING_IP"
	TypeNetwork              = "NETWORK"
	TypeRouter               = "ROUTER"
	TypeBucket               = "BUCKET"
	TypePort                 = "PORT"
	TypeSecurityGroup        = "SECURITY_GROUP"
	TypeServerGroup          = "SERVER_GROUP"
	TypeSubnet               = "SUBNET"
	TypeApplicationCred      = "APPLICATION_CREDENTIAL"
	TypeUser                 = "USER"
	TypeCredential           = "CREDENTIAL"
	TypeStack                = "STACK"
	TypeShare                = "SHARE"
	TypeShareSnapshot        = "SHARE_SNAPSHOT"
	TypeShareNetwork         = "SHARE_NETWORK"
	TypeShareGroup           = "SHARE_GROUP"
	TypeShareSnapshotGroup   = "SHARE_SNAPSHOT_GROUP"
	TypeShareSecurityService = "SHARE_SECURITY_SERVICE"
	TypeVHICredentials       = "VHI_CREDENTIALS"
	TypeVPNService           = "VPN_SERVICE"
	TypeVPNEndpointGroup     = "VPN_ENDPOINT_GROUP"
	TypeIKEPolicy            = "IKE_POLICY"
	TypeIPSecPolicy          = "IPSEC_POLICY"
	TypeIPSecSiteConnection  = "IPSEC_SITE_CONNECTION"
	TypeTrilioBackupTarget   = "TRILIO_BACKUP_TARGET"
	TypeTrilioWorkload       = "TRILIO_WORKLOAD"
	TypeTrilioSnapshot       = "TRILIO_SNAPSHOT"
	TypeTrilioRestore        = "TRILIO_RESTORE"
	TypeBarbicanSecret       = "BARBICAN_SECRET"
	TypeBarbicanContainer    = "BARBICAN_CONTAINER"
)

// Info is the nested CloudResource.info.
type Info struct {
	CreatedAt *time.Time `json:"createdAt,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

// CloudResource is the cloudResource collection document. `data` is a FREE-FORM
// sub-document (a bare, untyped object — no discriminated
// hierarchy; the shape is decided by `Type`, by convention, and decoded on demand), so it's
// modelled as map[string]any. `EphemeralData` is transient — the record builder drops it before
// persistence. There is NO optimistic-lock version field
// other than `UpdatedAt` (the OCC comparison key).
type CloudResource struct {
	ID               string         `json:"id,omitempty"`
	ProjectID        string         `json:"projectId,omitempty"`
	UserID           string         `json:"userId,omitempty"`
	ExternalID       string         `json:"externalId,omitempty"`
	ServiceID        string         `json:"serviceId,omitempty"`
	Type             string         `json:"type,omitempty"`
	PricePlan        any            `json:"pricePlan,omitempty"`
	Region           string         `json:"region,omitempty"`
	AvailabilityZone string         `json:"availabilityZone,omitempty"`
	Data             map[string]any `json:"data,omitempty"`
	// One-time secrets (keypair private key, generated password) returned to the caller exactly
	// once. Reaches the wire (json) but is never persisted: the storage record is built field-by-
	// field by toDbRecord, which does not copy this field (json:"-" would hide it from the wire too).
	EphemeralData map[string]any `json:"ephemeralData,omitempty"`
	CreatedAt     *time.Time     `json:"createdAt,omitempty"`
	UpdatedAt     *time.Time     `json:"updatedAt,omitempty"`
	Info          *Info          `json:"info,omitempty"`
}

// ServerIsVolumeBacked recognizes both the platform marker and Nova's live
// representation of a boot-from-volume server (an empty image reference).
// Deriving it from live data keeps billing and lifecycle guards correct when a
// notification or action replaces the free-form cache payload.
func ServerIsVolumeBacked(data map[string]any) bool {
	if data == nil {
		return false
	}
	if marked, _ := data["volumeBacked"].(bool); marked {
		return true
	}
	server, ok := data["server"].(map[string]any)
	if !ok || server == nil {
		return false
	}
	image, present := server["image"]
	if !present {
		return false
	}
	switch image := image.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(image) == ""
	case map[string]any:
		id, _ := image["id"].(string)
		return strings.TrimSpace(id) == ""
	default:
		return false
	}
}
