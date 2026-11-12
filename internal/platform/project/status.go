package project

import (
	"net/http"
	"slices"

	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// StatusAction names a change of a project's status. allowedFrom is the whole status machine: every path that changes a
// status checks it through CheckTransition, so DELETED and DELETE_IN_PROGRESS stay out of reach. A status written back
// over DELETE_IN_PROGRESS drops the project out of the deletion sweep mid-teardown.
type StatusAction string

const (
	ActionEnable        StatusAction = "enable it"  // operator or billing, never a member
	ActionDisable       StatusAction = "disable it" // operator
	ActionSchedule      StatusAction = "schedule its deletion"
	ActionCancel        StatusAction = "cancel deletion"
	ActionDeleteNow     StatusAction = "delete it"
	ActionChangeBilling StatusAction = "change its billing profile"
)

var allowedFrom = map[StatusAction][]string{
	ActionEnable:        {StatusDisabled},
	ActionDisable:       {StatusEnabled, StatusDisabled}, // on DISABLED it pins a billing disable, which a resume then leaves
	ActionSchedule:      {StatusEnabled, StatusDisabled},
	ActionCancel:        {StatusScheduledForDeletion},
	ActionDeleteNow:     {StatusEnabled, StatusDisabled, StatusScheduledForDeletion},
	ActionChangeBilling: {StatusEnabled, StatusDisabled},
}

// CheckTransition refuses an action the current status does not allow, with a 400 saying why.
func CheckTransition(a StatusAction, current string) error {
	if slices.Contains(allowedFrom[a], current) {
		return nil
	}
	switch {
	case a == ActionSchedule && current == StatusScheduledForDeletion:
		return httpx.BadRequest("Project is already scheduled for deletion")
	case a == ActionCancel && (current == StatusEnabled || current == StatusDisabled):
		return httpx.BadRequest("Project is not scheduled for deletion")
	case current == StatusDeleted:
		return httpx.BadRequest("Project is deleted. Cannot " + string(a))
	case current == StatusDeleteInProgress:
		return httpx.BadRequest("Project is deleting. Cannot " + string(a))
	case current == StatusScheduledForDeletion:
		return httpx.BadRequest("Project is scheduled for deletion. Cancel the deletion first")
	}
	return httpx.BadRequest("Project is " + current + ". Cannot " + string(a))
}

// Why a project that is not ENABLED stays that way when it is opened (POST /project/{id}/init answers 409 with one of these
// as errors.details.reason, next to the project's status).
const (
	ReasonDisabledByOperator      = "disabled_by_operator"      // an operator disabled it; only an operator enables it
	ReasonBillingProfileInactive  = "billing_profile_inactive"  // the paying profile is not ACTIVE yet (NEW: no billing details / payment method)
	ReasonBillingProfileSuspended = "billing_profile_suspended" // the paying profile is SUSPENDED (unpaid balance)
	ReasonBillingProfileMissing   = "billing_profile_missing"   // no paying profile can be found
	ReasonScheduledForDeletion    = "scheduled_for_deletion"
	ReasonDeletionInProgress      = "deletion_in_progress"
	ReasonDeleted                 = "deleted"
	ReasonNotEnabled              = "not_enabled" // any other status
)

// errNotOpened is the answer of an open (POST /project/{id}/init) that leaves the project as it is: a 409 whose details say
// the status and why, so a client can tell the user what to do instead of carrying on as if the project were usable.
// profileStatus (the paying billing profile's) is included when there is one.
func errNotOpened(p *Project, reason, profileStatus string) *httpx.HTTPError {
	var msg string
	switch reason {
	case ReasonDisabledByOperator:
		msg = "This project was suspended by your platform operator. Contact your operator to enable it again."
	case ReasonBillingProfileInactive:
		msg = "This project is not active yet: its billing profile is not active (" + profileStatus + "). Add your billing details or a payment method under Billing to activate it."
	case ReasonBillingProfileSuspended:
		msg = "This project is suspended for an unpaid balance. Add funds or pay the open bills under Billing and it resumes."
	case ReasonBillingProfileMissing:
		msg = "This project is not active: no billing profile pays for it. Contact your organization owner or your platform operator."
	case ReasonScheduledForDeletion:
		msg = "This project is scheduled for deletion. Cancel the deletion to use it again."
	case ReasonDeletionInProgress:
		msg = "This project is being deleted."
	case ReasonDeleted:
		msg = "This project is deleted."
	default:
		msg = "This project is " + p.Status + "."
	}
	details := map[string]string{"status": p.Status, "reason": reason}
	if profileStatus != "" {
		details["billingProfileStatus"] = profileStatus
	}
	return httpx.NewError(http.StatusConflict, http.StatusConflict, msg).WithDetails(details)
}

// notOpenedReason is the reason for a status an open never changes (any but ENABLED and DISABLED).
func notOpenedReason(status string) string {
	switch status {
	case StatusScheduledForDeletion:
		return ReasonScheduledForDeletion
	case StatusDeleteInProgress:
		return ReasonDeletionInProgress
	case StatusDeleted:
		return ReasonDeleted
	}
	return ReasonNotEnabled
}

// StatusAfterCancel is the status a cancelled deletion returns to: the one held before scheduling. A row with no record
// (scheduled before the field existed) gets DISABLED: cancelling must never lift a suspension it knows nothing about.
func StatusAfterCancel(held string) string {
	if held == StatusEnabled {
		return StatusEnabled
	}
	return StatusDisabled
}

// PaidBy reports whether this billing profile is the one that applies to the project: its own, or with none of its own the
// organization's, which the caller selected it through.
func (p *Project) PaidBy(billingProfileID string) bool {
	return p.BillingProfileID == "" || p.BillingProfileID == billingProfileID
}

// ApplyBillingSuspension moves the project with the suspend or resume of the billing profile that pays for it, and reports a
// change. Another profile's says nothing about it. A project scheduled for deletion keeps its status and the held one moves,
// so a cancel cannot bring back what billing took away. What an operator disabled stays disabled: a resume can follow a
// member paying a bill.
func (p *Project) ApplyBillingSuspension(billingProfileID string, suspend bool) bool {
	from, to := StatusDisabled, StatusEnabled
	if suspend {
		from, to = StatusEnabled, StatusDisabled
	}
	if !p.PaidBy(billingProfileID) || (!suspend && p.DisabledByOperator) {
		return false
	}
	switch {
	case p.Status == from:
		p.Status = to
	case p.Status == StatusScheduledForDeletion && p.StatusBeforeDeletion == from:
		p.StatusBeforeDeletion = to
	default:
		return false
	}
	return true
}
