package templateupdates

// sandbox.go — the built-in sandbox template mirror (docs/template-updates.md §4): payload, receipts and the
// sandboxBuiltinPin record. The executor is sandboxjob.go, the runner sandboxrunner.go.
//
// A "mirror" job copies ghcr.io/mutex-solutions/sandbox-<name>@<digest> into the sandbox registry as
// <registry>/library/sandbox-<name>:<tag> (same digest, the trusted sandbox-import worker in copy mode), verifies the
// destination resolves to that digest with the registry's read/create-only mirror account, points the prepull
// DaemonSets at <registry>/library/sandbox-<name>:<tag>@<digest> and records the pin. A "rollback" job re-points the
// prepull DaemonSets at the previous pin (still in the registry: nothing deletes it). Templates switch through the
// existing adoption job once every prepull pod on a Ready node has pulled the new digest.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/durablejobs"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
)

// Sandbox job actions.
const (
	SandboxMirror   = "mirror"
	SandboxRollback = "rollback"
)

var (
	sandboxHostRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,252}(:[0-9]{1,5})?$`)
	sandboxServiceRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
)

// SandboxPayload is the frozen, secret-free job payload.
type SandboxPayload struct {
	Version   int    `json:"version"`
	Action    string `json:"action"` // mirror | rollback
	ServiceID string `json:"serviceId"`
	Name      string `json:"name"`     // sandbox-<name>
	Registry  string `json:"registry"` // the sandbox registry host[:port]
	Tag       string `json:"tag"`      // <YYYYMMDD>-<sha7>
	Digest    string `json:"digest"`
}

// Repo is <registry>/library/sandbox-<name>.
func (p SandboxPayload) Repo() string { return p.Registry + "/library/sandbox-" + p.Name }

// Ref is the prepull image: <registry>/library/sandbox-<name>:<tag>@<digest>.
func (p SandboxPayload) Ref() string { return p.Repo() + ":" + p.Tag + "@" + p.Digest }

// Source is the published image the mirror copies.
func (p SandboxPayload) Source() string {
	return sandboximport.MirrorSourceRepository(p.Name) + "@" + p.Digest
}

// Destination is the mirror's tag.
func (p SandboxPayload) Destination() string { return p.Repo() + ":" + p.Tag }

// Key is the durable job identity (one per provider/action/image/digest); a retry after a failure gets a suffix.
func (p SandboxPayload) Key() string {
	sum := sha256.Sum256([]byte(p.ServiceID + "\x00" + p.Action + "\x00" + p.Repo() + "\x00" + p.Digest))
	return "tu-sbx-" + hex.EncodeToString(sum[:])
}

// LockKey serializes enqueues per mirrored repository (dedup per template image).
func (p SandboxPayload) LockKey() string { return "templateupdate-sandbox:" + p.Repo() }

func (p SandboxPayload) Validate() error {
	switch {
	case p.Version != 1, p.Action != SandboxMirror && p.Action != SandboxRollback:
		return errors.New("payload action malformed")
	case !sandboxNameRE.MatchString(p.Name), !sandboximport.MirrorTagRE.MatchString(p.Tag), !digestRE.MatchString(p.Digest):
		return errors.New("payload image identity malformed")
	case !sandboxServiceRE.MatchString(p.ServiceID), !sandboxHostRE.MatchString(p.Registry), p.Registry == sandboximport.MirrorSourceHost:
		return errors.New("payload placement malformed")
	}
	if _, _, err := (sandboximport.MirrorPolicy{Registry: p.Registry}).Check(p.Source(), p.Destination()); err != nil {
		return errors.New("payload refs malformed")
	}
	return nil
}

// DecodeSandboxPayload strictly decodes a payload (unknown fields refused).
func DecodeSandboxPayload(raw []byte) (SandboxPayload, error) {
	var p SandboxPayload
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return p, errors.New("trailing payload")
	}
	return p, p.Validate()
}

// Sandbox receipt steps.
const (
	SbxQueued    Step = "queued"
	SbxMirroring Step = "mirroring" // intent: worker pod name recorded before it is created
	SbxMirrored  Step = "mirrored"  // the worker succeeded with a valid result for exactly this pair
	SbxVerified  Step = "verified"  // the portal read the destination back (mirror account): same digest
	SbxPrepull   Step = "prepull"   // intent: pin recorded, previous prepull images recorded, DaemonSet patch next
	SbxPinned    Step = "pinned"    // the prepull DaemonSets run Ref; adoption follows
	SbxSucceeded Step = "succeeded"
	SbxFailed    Step = "failed"
)

// PrepullImageRecord is one prepull init container image before the patch.
type PrepullImageRecord struct {
	DaemonSet string `json:"daemonSet"`
	Container string `json:"container"`
	Image     string `json:"image"`
}

// SandboxReceipt is the durable per-job record (collection templateUpdateSandbox, id = job id). No secret:
// Message is a fixed classification text.
type SandboxReceipt struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"` // "sandbox"
	Payload     SandboxPayload `json:"payload"`
	RequestedBy string         `json:"requestedBy"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
	Step        Step           `json:"step"`
	Epoch       int64          `json:"epoch"`
	Pod         string         `json:"pod,omitempty"`
	SizeBytes   int64          `json:"sizeBytes,omitempty"`
	// VerifiedDigest is what the destination manifest resolved to (must equal Payload.Digest).
	VerifiedDigest string               `json:"verifiedDigest,omitempty"`
	Previous       []PrepullImageRecord `json:"previous,omitempty"`
	// PinBefore is the pin record this job replaced (nil: none), restored if the patch never landed.
	PinBefore *SandboxPin `json:"pinBefore,omitempty"`
	HadPin    bool        `json:"hadPin,omitempty"`
	// PinRecorded: this job wrote the pin record (the prepull intent); Cleanup restores PinBefore when the
	// DaemonSet patch never landed.
	PinRecorded bool     `json:"pinRecorded,omitempty"`
	Code        string   `json:"code,omitempty"`
	Message     string   `json:"message,omitempty"`
	Warnings    []string `json:"warnings,omitempty"`
	Cleaned     bool     `json:"cleaned,omitempty"`
}

// SandboxPin is a sandboxBuiltinPin document: the digest the prepull DaemonSets were last pointed at from the portal
// for one built-in image of one registry (id = <registry>/library/sandbox-<name>). Installer stage 55 reads these
// (GET /admin/template-updates/sandbox/pins) so a make upgrade never renders an older image than the portal pinned.
type SandboxPin struct {
	ID        string    `json:"id"`
	Template  string    `json:"template"` // the image name (sandbox-<template>)
	Registry  string    `json:"registry"`
	Tag       string    `json:"tag"`
	Digest    string    `json:"digest"`
	Ref       string    `json:"ref"`
	Action    string    `json:"action"`
	Previous  string    `json:"previous,omitempty"` // the ref it replaced (the rollback target)
	ServiceID string    `json:"serviceId"`
	Actor     string    `json:"actor"`
	At        time.Time `json:"at"`
	MirrorJob string    `json:"mirrorJob"`
}

// Collections.
const (
	SandboxReceiptCollection = "templateUpdateSandbox"
	SandboxPinCollection     = "sandboxBuiltinPin"
)

// SandboxReceipts persists receipts; inside a Checkpoint the context carries the job transaction.
type SandboxReceipts interface {
	Load(ctx context.Context, id string) (*SandboxReceipt, error)
	Save(ctx context.Context, r *SandboxReceipt) error
}

// SandboxPins persists pin records (same transaction rule).
type SandboxPins interface {
	GetPin(ctx context.Context, id string) (*SandboxPin, error)
	SavePin(ctx context.Context, p *SandboxPin) error
	DeletePin(ctx context.Context, id string) error
}

// PGSandbox stores sandbox receipts and pins in pgdoc.
type PGSandbox struct{ DB *pgdoc.DB }

// Ensure creates both tables (enqueue reads them inside a transaction, where pgdoc cannot auto-create).
func (s PGSandbox) Ensure(ctx context.Context) error {
	if err := s.DB.C(SandboxReceiptCollection).Ensure(ctx); err != nil {
		return err
	}
	return s.DB.C(SandboxPinCollection).Ensure(ctx)
}

func (s PGSandbox) Load(ctx context.Context, id string) (*SandboxReceipt, error) {
	var r SandboxReceipt
	found, err := s.DB.C(SandboxReceiptCollection).Get(ctx, id, &r)
	if err != nil || !found {
		return nil, err
	}
	return &r, nil
}

func (s PGSandbox) Save(ctx context.Context, r *SandboxReceipt) error {
	return s.DB.C(SandboxReceiptCollection).Upsert(ctx, r.ID, r)
}

func (s PGSandbox) GetPin(ctx context.Context, id string) (*SandboxPin, error) {
	var p SandboxPin
	found, err := s.DB.C(SandboxPinCollection).Get(ctx, id, &p)
	if err != nil || !found {
		return nil, err
	}
	return &p, nil
}

func (s PGSandbox) SavePin(ctx context.Context, p *SandboxPin) error {
	return s.DB.C(SandboxPinCollection).Upsert(ctx, p.ID, p)
}

func (s PGSandbox) DeletePin(ctx context.Context, id string) error {
	_, err := s.DB.C(SandboxPinCollection).DeleteByID(ctx, id)
	return err
}

// Pins lists every pin record (optionally of one registry), sorted by id.
func (s PGSandbox) Pins(ctx context.Context, registry string) ([]SandboxPin, error) {
	out := []SandboxPin{}
	var filter pgdoc.M
	if registry != "" {
		filter = pgdoc.M{"registry": registry}
	}
	err := s.DB.C(SandboxPinCollection).Find(ctx, filter, &out, pgdoc.Sort(pgdoc.AscK("_id", pgdoc.KText)))
	return out, err
}

// Recent lists the newest sandbox receipts.
func (s PGSandbox) Recent(ctx context.Context, limit int64) ([]SandboxReceipt, error) {
	out := []SandboxReceipt{}
	err := s.DB.C(SandboxReceiptCollection).Find(ctx, nil, &out, pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime)), pgdoc.Limit(limit))
	return out, err
}

// ByRepo lists the receipts of one mirrored repository (dedup on enqueue).
func (s PGSandbox) ByRepo(ctx context.Context, registry, name string) ([]SandboxReceipt, error) {
	out := []SandboxReceipt{}
	err := s.DB.C(SandboxReceiptCollection).Find(ctx, pgdoc.M{"payload.registry": registry, "payload.name": name}, &out)
	return out, err
}

// DeadUncleaned lists dead sandbox jobs whose receipt is not marked cleaned (the dead-job cleanup pass).
func (s PGSandbox) DeadUncleaned(ctx context.Context) ([]string, error) {
	if err := s.DB.C(SandboxReceiptCollection).Ensure(ctx); err != nil {
		return nil, err
	}
	rows, err := s.DB.Pool.Query(ctx, `SELECT j.id::text FROM durable_job j JOIN "templateUpdateSandbox" t ON t.id=j.id::text
	 WHERE j.kind=$1 AND j.state='dead' AND j.last_error<>'cleaned_up' AND coalesce((t.doc->>'cleaned')::boolean,false)=false
	 AND (j.last_error<>'cleanup_pending' OR j.finished_at IS NULL OR j.finished_at < clock_timestamp()-$2*interval '1 microsecond')
	 ORDER BY j.finished_at NULLS FIRST LIMIT 20`, durablejobs.TemplateUpdateSandbox, cleanupRetryAfter.Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SandboxTerminal reports a receipt that holds nothing any more (finished or failed).
func SandboxTerminal(r *SandboxReceipt) bool {
	return r.Step == SbxSucceeded || r.Step == SbxFailed || r.Cleaned
}

// ReplayableSandboxReceipt: only a job that died mid-flight resumes usefully on replay.
func ReplayableSandboxReceipt(r *SandboxReceipt) error {
	switch {
	case r == nil:
		return errors.New("the sandbox template update receipt is missing")
	case r.Step == SbxFailed:
		return errors.New("this sandbox template update failed; start a new one from Template updates instead of replaying")
	case r.Step == SbxSucceeded || r.Step == SbxPinned:
		return errors.New("this sandbox template update already pinned its image")
	case r.Cleaned:
		return errors.New("this sandbox template update's leftovers were already cleaned up; start a new one")
	}
	return nil
}

// jobShort is the 16-hex label of a job id (its pod names and labels).
func jobShort(id string) string {
	s := strings.ReplaceAll(id, "-", "")
	if len(s) < 16 {
		return s
	}
	return s[:16]
}
