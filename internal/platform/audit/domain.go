// Package audit is the audit pipeline: the auditEvent domain, an async writer
// (LogAsync), and cursor-paginated queries backing the client
// OrganizationAudit/AccountAudit endpoints.
// Events are emitted by the platform handlers after a successful mutation.
package audit

import "time"

// Enum string constants (stored as the enum name).
const (
	InterfaceClientArea  = "CLIENT_AREA"
	InterfaceUserAccount = "USER_ACCOUNT"
	InterfaceAdminArea   = "ADMIN_AREA"
	InterfaceSystemJob   = "SYSTEM_JOB"

	ContextOrganization = "ORGANIZATION"
	ContextProject      = "PROJECT"
	ContextUser         = "USER"
	ContextPlatform     = "PLATFORM"

	ActionCreate       = "CREATE"
	ActionUpdate       = "UPDATE"
	ActionDelete       = "DELETE"
	ActionAddMember    = "ADD_MEMBER"
	ActionRemoveMember = "REMOVE_MEMBER"
	ActionChangeRole   = "CHANGE_ROLE"
	ActionNotify       = "NOTIFY"
	ActionSuspend      = "SUSPEND"
	ActionUnsuspend    = "UNSUSPEND"
	ActionInvite       = "INVITE"
	ActionAcceptInvite = "ACCEPT_INVITE"

	ResourceUser             = "USER"
	ResourceOrganization     = "ORGANIZATION"
	ResourceProject          = "PROJECT"
	ResourceOrganizationRole = "ORGANIZATION_ROLE"
	ResourceSuspension       = "SUSPENSION"
	ResourceProjectInvite    = "PROJECT_INVITE"

	OutcomeSuccess = "SUCCESS"
	OutcomeFailure = "FAILURE"

	ActorUser   = "USER"
	ActorAdmin  = "ADMIN"
	ActorSystem = "SYSTEM"
)

// AuditActor is the event's actor.
type AuditActor struct {
	Type        string `json:"type,omitempty"`
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	IPAddress   string `json:"ipAddress,omitempty"`
	UserAgent   string `json:"userAgent,omitempty"`
}

// PropertyChange is one diff entry in changes.
type PropertyChange struct {
	Field    string `json:"field,omitempty"`
	OldValue any    `json:"oldValue,omitempty"`
	NewValue any    `json:"newValue,omitempty"`
}

// AuditEvent is a document in the "auditEvent" collection. The HTTP mapper omits
// null fields; `archived` is a primitive bool and is ALWAYS serialized (json tag
// has no omitempty).
type AuditEvent struct {
	ID                  string           `json:"id,omitempty"`
	Timestamp           *time.Time       `json:"timestamp,omitempty"`
	RequestInterface    string           `json:"requestInterface,omitempty"`
	EventContext        string           `json:"eventContext,omitempty"`
	Action              string           `json:"action,omitempty"`
	ResourceType        string           `json:"resourceType,omitempty"`
	ResourceID          string           `json:"resourceId,omitempty"`
	ResourceDisplayName string           `json:"resourceDisplayName,omitempty"`
	ResourceMetadata    map[string]any   `json:"resourceMetadata,omitempty"`
	Changes             []PropertyChange `json:"changes,omitempty"`
	Actor               *AuditActor      `json:"actor,omitempty"`
	OrganizationID      string           `json:"organizationId,omitempty"`
	ProjectID           string           `json:"projectId,omitempty"`
	Outcome             string           `json:"outcome,omitempty"`
	ErrorMessage        string           `json:"errorMessage,omitempty"`
	Archived            bool             `json:"archived"`
}

// ClientUserEvent presets a CLIENT_AREA event with a USER actor. The caller fills
// action/resource/etc.
func ClientUserEvent(sub, displayName string) AuditEvent {
	return AuditEvent{RequestInterface: InterfaceClientArea, Actor: &AuditActor{Type: ActorUser, ID: sub, DisplayName: displayName}}
}

// UserEvent presets a USER_ACCOUNT / USER-context event.
func UserEvent(sub, displayName string) AuditEvent {
	return AuditEvent{RequestInterface: InterfaceUserAccount, EventContext: ContextUser, Actor: &AuditActor{Type: ActorUser, ID: sub, DisplayName: displayName}}
}

// AdminEvent presets an ADMIN_AREA event with an ADMIN actor — used by the project
// admin member operations.
func AdminEvent(sub, displayName string) AuditEvent {
	return AuditEvent{RequestInterface: InterfaceAdminArea, Actor: &AuditActor{Type: ActorAdmin, ID: sub, DisplayName: displayName}}
}

// SystemEvent presets a SYSTEM_JOB event (SYSTEM actor id "system"; callers override
// eventContext) — the automatic suspension/dunning trail.
func SystemEvent() AuditEvent {
	return AuditEvent{RequestInterface: InterfaceSystemJob, EventContext: ContextPlatform,
		Actor: &AuditActor{Type: ActorSystem, ID: "system", DisplayName: "System"}}
}
