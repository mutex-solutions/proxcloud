package project

import (
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
	ActionDisable:       {StatusEnabled},
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

// StatusAfterCancel is the status a cancelled deletion returns to: the one held before scheduling. A row with no record
// (scheduled before the field existed) gets DISABLED: cancelling must never lift a suspension it knows nothing about.
func StatusAfterCancel(held string) string {
	if held == StatusEnabled {
		return StatusEnabled
	}
	return StatusDisabled
}

// ApplyBillingSuspension moves the project with its billing profile's suspend or resume, and reports a change. A project
// scheduled for deletion keeps its status and the held one moves, so a cancel cannot bring back what billing took away.
// What an operator disabled stays disabled: a resume can follow a member paying a bill.
func (p *Project) ApplyBillingSuspension(suspend bool) bool {
	from, to := StatusDisabled, StatusEnabled
	if suspend {
		from, to = StatusEnabled, StatusDisabled
	}
	if !suspend && p.DisabledByOperator {
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
