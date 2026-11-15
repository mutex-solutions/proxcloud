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
	CodePrivilegesMissing    = "privileges_missing"
	CodeNetworkProblem       = "network_problem"
	CodeISOStoreVisibility   = "iso_store_visibility"
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
	// CodeStageUnavailable ends a run that reaches a stage this build does not run (stages 6 to 11 of an apply, the
	// cluster part of a remove, a seed of mode existing).
	CodeStageUnavailable   = "stage_unavailable"
	CodeCredentialsMissing = "credentials_missing"
	// CodeSeedLost: a seed that became ready is gone or no longer ours. It is never rebuilt automatically, because a
	// new, empty seed would orphan the workload's Cluster API state.
	CodeSeedLost = "seed_lost"
)

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
	ID              string     `json:"id"`
	InstallationID  string     `json:"installationId"`
	Kind            string     `json:"kind"`
	BOM             BOMRef     `json:"bom"` // frozen at creation
	JobID           string     `json:"jobId,omitempty"`
	RequestedBy     string     `json:"requestedBy"`
	CancelRequested bool       `json:"cancelRequested,omitempty"`
	State           string     `json:"state"`
	Code            string     `json:"code,omitempty"`
	Message         string     `json:"message,omitempty"`
	Stages          []Stage    `json:"stages,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
}

// Stage is one stage of a run. Deadline is absolute, so a restart does not reset it.
type Stage struct {
	ID             string        `json:"id"`
	State          string        `json:"state"`
	Retries        int           `json:"retries"`
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
}

// Terminal reports a run state that will not change any more.
func Terminal(state string) bool {
	return state == RunSucceeded || state == RunFailed || state == RunCancelled
}
