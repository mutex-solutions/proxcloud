package audit

import (
	"strconv"
	"time"
)

// ParseLimit clamps the cursor page size to [1,200], default 50.
func ParseLimit(s string) int {
	if s == "" {
		return 50
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

// ParseInstant parses an RFC3339 timestamp query param, or nil if absent/invalid.
func ParseInstant(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}
