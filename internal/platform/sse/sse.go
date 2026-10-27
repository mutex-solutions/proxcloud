// Package sse is the real-time event stream: clients open an SSE connection per project and
// receive cloud-resource (and other) events. Architecture: an in-memory subscriber Pool + a
// per-connection heartbeat + Notify that pushes an event to every subscriber matching its
// projectId or userId. Cross-pod fan-out (a rabbit topic sse_topic_streaming) is a Publisher
// boundary: the in-process Publisher pushes locally; the real rabbit-topic source is connected
// later (os-notification → topic → per-pod listener → Notify).
package sse

import (
	"strconv"
	"sync"
	"sync/atomic"
)

// SseData is one event: an event type + the target (projectId/userId) + payload.
type SseData struct {
	Type      string `json:"type"`
	ProjectID string `json:"projectId,omitempty"`
	UserID    string `json:"userId,omitempty"`
	Data      any    `json:"data,omitempty"`
}

// subscriber is a live SSE connection (its events arrive on ch).
type subscriber struct {
	id        string
	projectID string
	userID    string
	ch        chan SseData
}

// Pool is the in-memory subscriber registry.
type Pool struct {
	mu    sync.RWMutex
	subs  map[string]*subscriber
	seqno atomic.Uint64
}

func NewPool() *Pool { return &Pool{subs: make(map[string]*subscriber)} }

// add registers a new subscriber for (projectID, userID) and returns it (with a unique id + a
// buffered channel). The caller streams from sub.ch until the client disconnects, then calls remove.
func (p *Pool) add(projectID, userID string) *subscriber {
	s := &subscriber{
		id:        projectID + "|" + userID + "|" + strconv.FormatUint(p.seqno.Add(1), 10),
		projectID: projectID,
		userID:    userID,
		ch:        make(chan SseData, 32),
	}
	p.mu.Lock()
	p.subs[s.id] = s
	p.mu.Unlock()
	return s
}

// remove deregisters a subscriber and closes its channel.
func (p *Pool) remove(id string) {
	p.mu.Lock()
	if s, ok := p.subs[id]; ok {
		delete(p.subs, id)
		close(s.ch)
	}
	p.mu.Unlock()
}

// Notify pushes the event to every subscriber whose projectId or userId matches the event's
// target. Non-blocking — a slow/full subscriber drops the event (never blocks the publisher),
// best-effort send.
func (p *Pool) Notify(d SseData) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, s := range p.subs {
		if (d.ProjectID != "" && s.projectID == d.ProjectID) || (d.UserID != "" && s.userID == d.UserID) {
			select {
			case s.ch <- d:
			default: // buffer full → drop (best-effort)
			}
		}
	}
}

// Count returns the number of live subscribers (for diagnostics/tests).
func (p *Pool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.subs)
}

// Publisher is the cross-pod fan-out boundary (project/user events → rabbit topic).
// The in-process impl pushes straight to the local Pool; the rabbit-topic impl lands later.
type Publisher interface {
	Publish(d SseData)
}

// LocalPublisher fans an event straight into the local Pool (single-pod / test / pre-fan-out).
type LocalPublisher struct{ Pool *Pool }

func (l LocalPublisher) Publish(d SseData) { l.Pool.Notify(d) }
