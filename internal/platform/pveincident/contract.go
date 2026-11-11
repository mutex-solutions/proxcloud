// Package pveincident records operator evidence, not cloud/billing policy or notifications.
// Its only input is an immutable, same-token, read-only snapshot supplied by the inventory adapter.
package pveincident

import "time"

const (
	CaptureTTL          = 2 * time.Minute
	ObservationInterval = time.Minute
	KindVisibility      = "VISIBILITY_WARNING"
	KindProvider        = "PROVIDER_UNREACHABLE"
	KindNode            = "NODE_OFFLINE"
	KindProtection      = "PROTECTION_DEGRADED"
	StatePending        = "PENDING"
	StateOpen           = "OPEN"
	StateRecovered      = "RECOVERED"
)

type Trust string

const (
	Verified    Trust = "VERIFIED"
	Untrusted   Trust = "VISIBILITY_UNTRUSTED"
	Unreachable Trust = "UNREACHABLE_UNKNOWN"
	Malformed   Trust = "MALFORMED"
)

type NodeStatus string

const (
	Online      NodeStatus = "ONLINE"
	Offline     NodeStatus = "OBSERVED_OFFLINE"
	NodeUnknown NodeStatus = "UNREACHABLE_UNKNOWN"
)

type SourceTrust struct{ Provider, Nodes, VMs, Protection Trust }
type NodeFact struct {
	NodeID string
	Status NodeStatus
}
type ProtectionFact struct {
	ServerID, InstanceID, Mode string // Mode is shared or replicated, never a cached display status.
	Enabled, HA                bool
	LastSync                   time.Time
	FailCount                  int
}

// Link must be a positively verified platform identity, never discovered from cached PVE claims.
// The repository preserves last-verified links when VM visibility is unknown.
type Link struct {
	ServerID   string `json:"serverId"`
	ProjectID  string `json:"projectId"`
	InstanceID string `json:"instanceId"`
	NodeID     string `json:"nodeId"`
}
type Capture struct {
	ProviderID string    `json:"providerId"`
	Generation string    `json:"generation"` // opaque cluster + credential generation, not credentials
	Region     string    `json:"region"`
	Epoch      int64     `json:"epoch"`
	StartedAt  time.Time `json:"startedAt"`
}
type SnapshotResult struct {
	Capture       Capture
	Trust         SourceTrust
	Nodes         []NodeFact
	Protection    []ProtectionFact
	Links         []Link
	LinksComplete bool
}

// Incident is the operator DTO. Internal links/confirmation counters never escape through it.
type Incident struct {
	ID               string     `json:"id"`
	ProviderID       string     `json:"providerId"`
	Generation       string     `json:"generation"`
	Region           string     `json:"region"`
	Kind             string     `json:"kind"`
	Subject          string     `json:"subject"`
	State            string     `json:"state"`
	Episode          int64      `json:"episode"`
	Version          int64      `json:"version"`
	Reason           string     `json:"reason"`
	EvidenceQuality  Trust      `json:"evidenceQuality"`
	AffectedComplete bool       `json:"affectedComplete"`
	FirstSeen        time.Time  `json:"firstSeen"`
	LastSeen         time.Time  `json:"lastSeen"`
	RecoveredAt      *time.Time `json:"recoveredAt,omitempty"`
}
type Event struct {
	ID               string    `json:"id"`
	IncidentID       string    `json:"incidentId"`
	ProviderID       string    `json:"providerId"`
	Episode          int64     `json:"episode"`
	Version          int64     `json:"version"`
	Action           string    `json:"action"`
	ObservedAt       time.Time `json:"observedAt"`
	Reason           string    `json:"reason"`
	Affected         []Link    `json:"affected"`
	EvidenceQuality  Trust     `json:"evidenceQuality"`
	AffectedComplete bool      `json:"affectedComplete"`
}
type Affected struct {
	ServerID  string `json:"serverId"`
	ProjectID string `json:"projectId"`
}
type Filter struct {
	State, Kind, Cursor string
	Limit               int
}
type Page[T any] struct {
	Data []T    `json:"data"`
	Next string `json:"next,omitempty"`
}
type ObserveResult struct {
	Accepted bool
	Events   int
}

type observation struct {
	kind, subject, reason string
	health                int
	quality               Trust
}
type state struct {
	Incident
	Bad, Good int
	Window    time.Time
	LastEpoch int64
	Affected  []Link
}

// advance is pure; now is always supplied by the repository's transaction DB clock.
func advance(s state, o observation, epoch int64, now time.Time) (state, string) {
	if epoch <= s.LastEpoch {
		return s, ""
	}
	if !s.LastSeen.IsZero() && now.Sub(s.LastSeen) > CaptureTTL {
		s.Bad, s.Good = 0, 0
	}
	s.LastEpoch, s.Version, s.LastSeen = epoch, s.Version+1, now
	s.Reason, s.EvidenceQuality = o.reason, o.quality
	if s.FirstSeen.IsZero() {
		s.FirstSeen = now
	}
	action := ""
	switch o.health {
	case -1:
		s.Good = 0
		if s.Bad == 0 {
			s.Window = now
		}
		s.Bad++
		if s.State != StateOpen && s.Bad >= 3 && now.Sub(s.Window) >= 2*time.Minute {
			s.State, s.Episode, s.RecoveredAt, action = StateOpen, s.Episode+1, nil, "OPEN"
		}
	case 1:
		s.Bad = 0
		if s.Good == 0 {
			s.Window = now
		}
		s.Good++
		if s.State == StateOpen && s.Good >= 2 && now.Sub(s.Window) >= time.Minute {
			s.State, s.RecoveredAt, action = StateRecovered, &now, "RECOVERED"
		}
	default:
		s.Bad, s.Good = 0, 0 // unknown breaks consecutive evidence, never closes an incident
		s.AffectedComplete = false
	}
	return s, action
}
