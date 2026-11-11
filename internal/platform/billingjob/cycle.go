package billingjob

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/platform/pricing"
)

// Cycle is the immutable, versioned billing work identity. CycleAt is the
// ORIGINAL producer cutoff, never the time at which a worker happens to run.
type Cycle struct {
	Version   int       `json:"version"`
	ProfileID string    `json:"profileId"`
	TimeUnit  string    `json:"timeUnit"`
	CycleAt   time.Time `json:"cycleAt"`
}

func NewCycle(profileID, unit string, at time.Time) (Cycle, error) {
	c := Cycle{Version: 1, ProfileID: profileID, TimeUnit: unit, CycleAt: truncateForTimeUnit(at, unit)}
	return c, c.Validate()
}

func (c Cycle) Validate() error {
	if c.Version != 1 || c.ProfileID == "" || len(c.ProfileID) > 128 || strings.ContainsAny(c.ProfileID, "|\x00\r\n") {
		return errors.New("invalid billing cycle version or profile")
	}
	switch c.TimeUnit {
	case pricing.TimeUnitMinute, pricing.TimeUnitHour, pricing.TimeUnitMonth:
	default:
		return errors.New("invalid billing cycle time unit")
	}
	if c.CycleAt.IsZero() || !c.CycleAt.Equal(truncateForTimeUnit(c.CycleAt, c.TimeUnit)) {
		return errors.New("billing cycle cutoff must be a nonzero cadence boundary")
	}
	return nil
}

func (c Cycle) Key() string {
	return fmt.Sprintf("%s|%s|%s", c.ProfileID, c.TimeUnit, c.CycleAt.UTC().Format(time.RFC3339Nano))
}
