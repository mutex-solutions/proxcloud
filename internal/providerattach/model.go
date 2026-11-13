// Package providerattach attaches one cloud provider to many existing projects as a durable, fenced job
// (docs/cloud-integration.md "Attaching a provider to existing projects").
//
// An admin starts a run on a provider with a scope (every project, the projects of some organizations, or picked
// projects). The HTTP request only records the run and enqueues one durablejobs.ProviderAttach job; a Runner claims it,
// lists the scoped projects into per-project items once (the plan), then attaches them with bounded concurrency through
// the same code path as the per-project admin attach (project.Handler.AttachByOperator → BootstrapOnto). Every step is
// committed through durablejobs.Store.Checkpoint, so a runner that lost its lease writes nothing more, and the next
// acquisition resumes from the items: an item in flight is attached again, which the attach makes a no-op when it had
// already landed (no second binding, no provider call). Results are fixed codes, never a provider's error text.
package providerattach

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

// Version is the payload version.
const Version = 1

// Payload is the durable job payload: only identities. The scope and the results live in the run and item documents.
type Payload struct {
	Version   int    `json:"version"`
	RunID     string `json:"runId"`
	ServiceID string `json:"serviceId"`
}

var (
	idRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	errBadInput = errors.New("invalid provider attach payload")
)

// ValidID reports an id the run accepts for a provider, project or organization.
func ValidID(s string) bool { return idRE.MatchString(s) }

// Validate checks the payload shape.
func (p Payload) Validate() error {
	if p.Version != Version || !ValidID(p.RunID) || !ValidID(p.ServiceID) {
		return errBadInput
	}
	return nil
}

// DecodePayload decodes and validates a job payload (unknown fields refused).
func DecodePayload(raw []byte) (Payload, error) {
	var p Payload
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return Payload{}, errBadInput
	}
	return p, p.Validate()
}

// Scope modes.
const (
	ScopeAll           = "all"           // every project
	ScopeOrganizations = "organizations" // the projects of these organizations
	ScopeProjects      = "projects"      // these projects
	ScopeRetry         = "retry"         // the failed projects of an earlier run
)

// Limits of a scope given in a request.
const (
	MaxScopeIDs    = 2000
	MinConcurrency = 1
	MaxConcurrency = 4
	DefaultWorkers = 2
	// MaxItemAttempts bounds how often one project is tried across acquisitions (a crash on one project must not
	// loop the job): past it the item fails with CodeAttemptsExhausted.
	MaxItemAttempts = 3
	// JobAttempts is the durable job's max_attempts: a lost lease or a crash costs one, and the next acquisition resumes.
	JobAttempts = 5
	// MaxRunProjects caps the projects one run may attach (counted at the start, not counting those already attached).
	MaxRunProjects = 5000
	// PlanBatch is how many projects one planning step lists and records (one fenced transaction each): planning a run
	// of any size is a series of short transactions, resumed from Run.PlanCursor.
	PlanBatch = 500
	// ItemRetention is how long the per-project items of a finished run are kept; the run keeps its frozen counts.
	ItemRetention = 30 * 24 * time.Hour
)

// Scope selects the projects of a run.
type Scope struct {
	Mode            string   `json:"mode"`
	OrganizationIDs []string `json:"organizationIds,omitempty"`
	ProjectIDs      []string `json:"projectIds,omitempty"`
	SourceRunID     string   `json:"sourceRunId,omitempty"`
}

// Run states (stored). The API derives "failed" from a dead job and "cancelling" from a cancel not yet honoured.
const (
	RunQueued                = "queued"
	RunRunning               = "running"
	RunSucceeded             = "succeeded"
	RunCompletedWithFailures = "completed_with_failures"
	RunCancelled             = "cancelled"
	RunFailed                = "failed"
	RunCancelling            = "cancelling"
)

// Terminal reports a run state that will not change any more.
func Terminal(state string) bool {
	return state == RunSucceeded || state == RunCompletedWithFailures || state == RunCancelled || state == RunFailed
}

// Run is one bulk attach (collection providerAttachRun; id = the run id, also in the job payload).
type Run struct {
	ID          string `json:"id"`
	ServiceID   string `json:"serviceId"`
	ServiceName string `json:"serviceName,omitempty"`
	JobID       string `json:"jobId,omitempty"`
	Scope       Scope  `json:"scope"`
	Concurrency int    `json:"concurrency"`
	RequestedBy string `json:"requestedBy"`
	Status      string `json:"status"`
	Planned     bool   `json:"planned"`
	// PlanCursor is the last project id planned (planning goes in id order, PlanBatch at a time).
	PlanCursor string `json:"planCursor,omitempty"`
	// PlanSkipped counts the projects the plan found already attached: they get no item row, only this count.
	PlanSkipped map[string]int64 `json:"planSkipped,omitempty"`
	// ItemsPruned: the per-project items were removed ItemRetention after the run finished (the counts stay).
	ItemsPruned     bool       `json:"itemsPruned,omitempty"`
	CancelRequested bool       `json:"cancelRequested,omitempty"`
	CancelledBy     string     `json:"cancelledBy,omitempty"`
	Code            string     `json:"code,omitempty"` // why a whole run ended early (provider_missing, provider_disabled)
	Counts          Counts     `json:"counts"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
}

// Item states.
const (
	ItemPending   = "pending"
	ItemRunning   = "running"
	ItemAttached  = "attached"
	ItemSkipped   = "skipped"
	ItemFailed    = "failed"
	ItemCancelled = "cancelled"
)

// Item codes: the only reasons ever stored.
const (
	CodeAttached          = "attached"
	CodeAlreadyAttached   = "already_attached"
	CodeDeleting          = "deleting"
	CodeNotFound          = "not_found"
	CodeProviderError     = "provider_error"
	CodeTimeout           = "timeout"
	CodeProviderMissing   = "provider_missing"
	CodeProviderDisabled  = "provider_disabled"
	CodeProviderUnsupport = "provider_unsupported"
	CodeAttemptsExhausted = "attempts_exhausted"
	CodeCancelled         = "cancelled"
)

// Item is one project of a run (collection providerAttachItem; id = runId + ":" + projectId).
type Item struct {
	ID             string    `json:"id"`
	RunID          string    `json:"runId"`
	ProjectID      string    `json:"projectId"`
	ProjectName    string    `json:"projectName,omitempty"`
	OrganizationID string    `json:"organizationId,omitempty"`
	State          string    `json:"state"`
	Code           string    `json:"code,omitempty"`
	Attempts       int       `json:"attempts"`
	Epoch          int64     `json:"epoch"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// ItemID is the item id of a project in a run.
func ItemID(runID, projectID string) string { return runID + ":" + projectID }

// WithPlanSkipped adds the projects the plan skipped without an item row to counts from the items.
func (r *Run) WithPlanSkipped(c Counts) Counts {
	for code, n := range r.PlanSkipped {
		if c.ByCode == nil {
			c.ByCode = map[string]int64{}
		}
		c.Total += n
		c.Skipped += n
		c.ByCode[code] += n
	}
	return c
}

// Counts are the items per state, plus the skipped ones per code.
type Counts struct {
	Total     int64            `json:"total"`
	Pending   int64            `json:"pending"`
	Running   int64            `json:"running"`
	Attached  int64            `json:"attached"`
	Skipped   int64            `json:"skipped"`
	Failed    int64            `json:"failed"`
	Cancelled int64            `json:"cancelled"`
	ByCode    map[string]int64 `json:"byCode,omitempty"`
}
