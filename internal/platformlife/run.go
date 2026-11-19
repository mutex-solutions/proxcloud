package platformlife

// run.go — a platformRun (design §3.4): one apply or remove of an installation, with the BOM frozen at its creation
// and one entry per stage. runner.go executes it.

import (
	"strings"
	"time"
)

// Run kinds.
const (
	RunApply  = "apply"
	RunRemove = "remove"
)

// Run states (stored). "cancelling" is derived from CancelRequested on a run that is not terminal yet.
const (
	RunQueued    = "queued"
	RunRunning   = "running"
	RunWaiting   = "waiting"
	RunSucceeded = "succeeded"
	RunFailed    = "failed"
	RunCancelled = "cancelled"
)

// Stage states.
const (
	StagePending = "pending"
	StageRunning = "running"
	StageWaiting = "waiting"
	StageDone    = "done"
	StageSkipped = "skipped"
	StageFailed  = "failed"
)

// Failure codes (design §4.7): a run or a stage stores one of these with a fixed message plus clipped PVE, CAPI or
// ArgoCD condition text, never a kubeconfig, a token or user-data. "<stage>_timeout" is built per stage.
const (
	CodePreflightFailed      = "preflight_failed"
	CodeIsolationNotEnforced = "isolation_not_enforced"
	CodeImageImportFailed    = "image_import_failed"
	CodeImageBlocked         = "image_blocked"
	CodeImageImportStalled   = "image_import_stalled"
	CodeSeedCloneFailed      = "seed_clone_failed"
	CodeSeedConfigRefused    = "seed_config_refused"
	CodeSeedNotBooted        = "seed_not_booted"
	CodeSeedAPIUnreachable   = "seed_api_unreachable"
	CodeSeedTLSMismatch      = "seed_tls_mismatch"
	CodeSeedIdentityLost     = "seed_identity_lost"
	CodeSeedCDNotDeleted     = "seed_cd_not_deleted"
	CodeComponentUnhealthy   = "component_unhealthy"
	CodeCAPIInitFailed       = "capi_init_failed"
	CodeMachineFailed        = "machine_failed"
	CodeCAPMOXAuthFailed     = "capmox_auth_failed"
	CodeControlPlaneTimeout  = "control_plane_timeout"
	CodeWorkloadUnreachable  = "workload_api_unreachable"
	CodeNodesTimeout         = "nodes_timeout"
	CodeProviderConflict     = "provider_conflict"
	CodeMachineDeleteStuck   = "machine_delete_stuck"
	CodeBOMUnsupported       = "bom_unsupported"
	CodePortalChanged        = "portal_changed"
	CodeJobDead              = "job_dead"
	CodePlatformDisabled     = "platform_disabled"
	// CodeStageUnavailable ends a run that reaches something this build does not do: a stage another release recorded,
	// or a seed of mode existing (Validate refuses that mode, so stage 1's spec check ends such a run first).
	CodeStageUnavailable = "stage_unavailable"
	// CodeTenantsExist refuses a Remove while tenant clusters or databases still run on the workload cluster.
	CodeTenantsExist = "tenants_exist"
	// CodeVolumeDeleteFailed: a recorded volume of the platform could not be deleted at Remove.
	CodeVolumeDeleteFailed = "volume_delete_failed"
	CodeCredentialsMissing = "credentials_missing"
	// CodeSeedLost: a seed that became ready is gone or no longer ours. It is never rebuilt automatically, because a
	// new, empty seed would orphan the workload's Cluster API state.
	CodeSeedLost = "seed_lost"
	// CodeUpgradeRefused: an apply names a target BOM this release does not move the platform to (the 409 of a run).
	CodeUpgradeRefused = "upgrade_refused"
	// CodeUnexpectedRollout: on an upgrade run the cluster chart of the target BOM would replace machines.
	CodeUnexpectedRollout = "unexpected_rollout"
	// CodeTenantsUnhealthy: on an upgrade run a tenant that was healthy before a wave is not healthy after it.
	CodeTenantsUnhealthy = "tenants_unhealthy"
	// CodeSeedK3sFailed: the k3s upgrade Job on the seed failed (before k3s was replaced: its script stops at the first
	// failure). CodeSeedK3sRefused: the seed's node reports a k3s the run does not move to the BOM's from.
	CodeSeedK3sFailed  = "seed_k3s_failed"
	CodeSeedK3sRefused = "seed_k3s_refused"
	// CodeSeedBackupMissing: an apply would start an upgrade that replaces the seed's k3s, and neither the portal saw a
	// backup of the seed VM nor did the operator say one was taken (the 409 of a run; a line of the plan).
	CodeSeedBackupMissing = "seed_backup_missing"
	// The roll of the platform cluster's machines (roll.go). The first five refuse the platform part of an upgrade before
	// anything is changed (a line of the plan, the 409 of a run, stage 1 of the run); capacity_short also ends a step
	// whose node lost its room since the plan was read, with nothing of that step applied.
	CodeNodeImageMissing  = "node_image_missing" // a node the cluster uses holds no template of the target node image
	CodeCapacityShort     = "capacity_short"     // no room for the one more machine a unit's replacement makes first
	CodeDrainBlocked      = "drain_blocked"      // a PodDisruptionBudget allows no disruption: Cluster API's drain would wait for ever
	CodePlatformUnhealthy = "platform_unhealthy" // a Machine not Running or a platform component not Healthy before the start
	CodeSeedNotUpgraded   = "seed_not_upgraded"  // the seed's k3s is not at the target yet: "Seed and node image" comes first
	// CodeRolloutStalled: no machine of the unit a roll stands at moved for rollStall.
	CodeRolloutStalled = "rollout_stalled"
	// CodeArgoCDAddressUnavailable: the Service of the platform cluster's ArgoCD asked MetalLB for the address of
	// spec.access.argocd and was not given it before stage 9's deadline.
	CodeArgoCDAddressUnavailable = "argocd_address_unavailable"
	// CodeBadPart: POST …/runs names a part that is neither "seed" nor empty (400).
	CodeBadPart = "bad_part"
)

// PartSeed is the part of an upgrade that imports the node image, moves the seed's k3s and the seed's own charts and
// touches no tenant (Run.Part). "" is the whole apply: for a move that replaces machines, the platform part.
const PartSeed = "seed"

// Stage ids of an apply (design §4.2) and of a remove (§4.6).
const (
	StageValidate         = "validate"
	StageImages           = "images"
	StageSeedVM           = "seed-vm"
	StageSeedAPI          = "seed-api"
	StageSeedIdentity     = "seed-identity"
	StageRemoveCluster    = "remove-cluster"
	StageRemoveSeed       = "remove-seed"
	StageRemoveSecrets    = "remove-secrets"
	stageSeedK3s          = "seed-k3s"
	stageSeedPlatform     = "seed-platform"
	stageCluster          = "cluster"
	stageWorkloadAccess   = "workload-access"
	stageWorkloadPlatform = "workload-platform"
	stageRegister         = "register"
	stageDone             = "done"
)

// TimeoutCode is the failure code of a stage whose deadline passed, in the [a-z_] form job codes take.
func TimeoutCode(stage string) string { return strings.ReplaceAll(stage, "-", "_") + "_timeout" }

// Run is one document of collection platformRun.
type Run struct {
	ID             string `json:"id"`
	InstallationID string `json:"installationId"`
	Kind           string `json:"kind"`
	BOM            BOMRef `json:"bom"` // frozen at creation
	// FromBOM is set on an upgrade run: the installed BOM it moves the platform from. BOM is then the target.
	FromBOM         *BOMRef `json:"fromBom,omitempty"`
	JobID           string  `json:"jobId,omitempty"`
	RequestedBy     string  `json:"requestedBy"`
	CancelRequested bool    `json:"cancelRequested,omitempty"`
	// Confirmed: the operator typed "remove <name>" when starting a Remove. It lets the run go on where it cannot
	// observe: the seed VM is gone, or the workload cluster does not answer whether tenants still run on it. On an apply
	// it records the typed "upgrade <name> to <release>" of the run that started an upgrade.
	Confirmed bool `json:"confirmed,omitempty"`
	// SeedBackupTaken: the apply that started an upgrade which replaces the seed's k3s said a backup of the seed VM was
	// taken that the portal could not see (seedBackupTaken).
	SeedBackupTaken bool `json:"seedBackupTaken,omitempty"`
	// Part is "seed" on a run that stops after the seed's own charts (stageIDs); "" on every other run.
	Part string `json:"part,omitempty"`
	// DeleteFirst are the units ("group/node") the plan this run was confirmed with replaces by deleting the machine
	// first, because their node had no room for one more. Stage 7 freezes it in status.clusters[0].roll when it
	// begins the roll; a run that continues a begun roll carries none.
	DeleteFirst []string   `json:"deleteFirst,omitempty"`
	State       string     `json:"state"`
	Code        string     `json:"code,omitempty"`
	Message     string     `json:"message,omitempty"`
	Stages      []Stage    `json:"stages,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
}

// Stage is one stage of a run. Deadline is absolute, so a restart does not reset it.
type Stage struct {
	ID             string        `json:"id"`
	State          string        `json:"state"`
	Retries        int           `json:"retries"`
	Refused        int           `json:"refused,omitempty"` // looks on end a cluster answered 401 or 403 (see startingUp)
	StartedAt      *time.Time    `json:"startedAt,omitempty"`
	Deadline       *time.Time    `json:"deadline,omitempty"`
	FinishedAt     *time.Time    `json:"finishedAt,omitempty"`
	LastProgressAt *time.Time    `json:"lastProgressAt,omitempty"`
	Progress       string        `json:"progress,omitempty"`
	Items          []StageItem   `json:"items,omitempty"`
	Code           string        `json:"code,omitempty"`
	Message        string        `json:"message,omitempty"`
	Receipt        *StageReceipt `json:"receipt,omitempty"`
}

// StageItem is one check, machine or Application a stage reports on.
type StageItem struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

// StageReceipt journals a stage's effects that are not idempotent, so a resumed stage finds them. It has no field for
// a secret.
type StageReceipt struct {
	VMID           int      `json:"vmid,omitempty"`
	Name           string   `json:"name,omitempty"`
	CloneUPID      string   `json:"cloneUpid,omitempty"`
	CDVolume       string   `json:"cdVolume,omitempty"`
	Generation     int      `json:"generation,omitempty"`
	ImportReceipts []string `json:"importReceipts,omitempty"`
	// Templates is the template VMID stage 2 found or imported per image and node: "seed:<node>", "node:<node>".
	Templates                 map[string]int `json:"templates,omitempty"`
	MachineDeploymentsDeleted []string       `json:"machineDeploymentsDeleted,omitempty"`
	// Resynced are the Applications whose failed sync this run started again (once each).
	Resynced []string `json:"resynced,omitempty"`
	// CAPMOXWokenAt is when this run made CAPMOX look at a ProxmoxCluster an earlier run had left without a condition.
	CAPMOXWokenAt *time.Time `json:"capmoxWokenAt,omitempty"`
	// RolloutChecked: this upgrade run compared the cluster chart of the target BOM with the live cluster before its
	// sync and found no machine to replace (rolloutGate).
	RolloutChecked bool `json:"rolloutChecked,omitempty"`
	// RollChecked is the highest step of a roll whose comparison this run read before the step's sync and found
	// nothing out of sync beyond the step's own unit (rolloutGate with a step).
	RollChecked int `json:"rollChecked,omitempty"`
}

// Terminal reports a run state that will not change any more.
func Terminal(state string) bool {
	return state == RunSucceeded || state == RunFailed || state == RunCancelled
}
