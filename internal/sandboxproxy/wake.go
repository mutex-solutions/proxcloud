package sandboxproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// EnvdTokenHeader carries the envd access token on requests to envd (E2B SDK).
const EnvdTokenHeader = "X-Access-Token"

// errNoWake: the API answered that the sandbox is not paused for auto-resume. It may have just
// been resumed through another replica, so the caller looks the pod up again (uncached).
var errNoWake = fmt.Errorf("not resumable: %w", ErrNotFound)

// WakeError is a wake the API refused or could not finish: the status to answer the client with.
type WakeError struct {
	Status     int
	Message    string
	RetryAfter string
}

func (e *WakeError) Error() string { return fmt.Sprintf("wake: %d %s", e.Status, e.Message) }

// Waker asks the portal API to resume a paused sandbox (E2B autoResume) when traffic finds no
// pod. One wake per sandbox and token set runs at a time (later requests join it); it runs
// detached from the request, so a client that gives up does not abort it.
type Waker struct {
	URL, Token string // POST {URL}/{id}/wake with "Authorization: Bearer {Token}"
	Client     *http.Client
	Wait       time.Duration // how long one request waits for a wake (then 503 + Retry-After)
	Limit      time.Duration // how long one wake may take

	sf       singleflight.Group
	sem      chan struct{}
	once     sync.Once
	mu       sync.Mutex
	notFound map[string]time.Time // id -> until: the API said "not resumable"
	now      func() time.Time
}

// NewWaker returns a Waker with the production limits (nil when url or token is empty: wake off).
func NewWaker(url, token string) *Waker {
	if url == "" || token == "" {
		return nil
	}
	// A portal the proxy cannot reach answers in seconds (503, retry) rather than holding every miss for Wait.
	tr := &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 10 * time.Second}
	return &Waker{URL: strings.TrimSuffix(url, "/"), Token: token, Client: &http.Client{Timeout: 130 * time.Second, Transport: tr},
		Wait: 50 * time.Second, Limit: 120 * time.Second}
}

const (
	wakeNotFoundTTL = 10 * time.Second
	wakeConcurrency = 32
)

// Wake resumes sandbox id: nil once it runs, ErrNotFound when it is not resumable, *WakeError
// otherwise.
func (w *Waker) Wake(ctx context.Context, id string, port int, envdToken, trafficToken string) error {
	w.once.Do(func() {
		w.sem = make(chan struct{}, wakeConcurrency)
		w.notFound = map[string]time.Time{}
		if w.now == nil {
			w.now = time.Now
		}
	})
	w.mu.Lock()
	until, miss := w.notFound[id]
	w.mu.Unlock()
	if miss && w.now().Before(until) {
		return ErrNotFound
	}
	ch := w.sf.DoChan(id+"\x00"+envdToken+"\x00"+trafficToken, func() (any, error) {
		select {
		case w.sem <- struct{}{}:
			defer func() { <-w.sem }()
		default:
			return nil, &WakeError{Status: http.StatusServiceUnavailable, Message: "Too many sandboxes are waking", RetryAfter: "2"}
		}
		wctx, cancel := context.WithTimeout(context.Background(), w.Limit)
		defer cancel()
		err := w.call(wctx, id, port, envdToken, trafficToken)
		if err == errNoWake {
			w.mu.Lock()
			if len(w.notFound) > 4096 { // ponytail: wholesale reset bounds memory
				w.notFound = map[string]time.Time{}
			}
			w.notFound[id] = w.now().Add(wakeNotFoundTTL)
			w.mu.Unlock()
		}
		return nil, err
	})
	t := time.NewTimer(w.Wait)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.Err
	case <-t.C:
		return &WakeError{Status: http.StatusServiceUnavailable, Message: "The sandbox is resuming", RetryAfter: "2"}
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Waker) call(ctx context.Context, id string, port int, envdToken, trafficToken string) error {
	body, _ := json.Marshal(map[string]any{"port": port, "envdAccessToken": envdToken, "trafficAccessToken": trafficToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL+"/"+id+"/wake", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+w.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.Client.Do(req)
	if err != nil {
		return &WakeError{Status: http.StatusServiceUnavailable, Message: "The sandbox could not be resumed", RetryAfter: "2"}
	}
	defer resp.Body.Close()
	var e struct { // the portal's envelope: {"errors": {"code", "message"}}
		Errors struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
	msg := e.Errors.Message
	switch {
	case resp.StatusCode == http.StatusOK:
		return nil
	case resp.StatusCode == http.StatusNotFound:
		return errNoWake
	case resp.StatusCode == http.StatusForbidden:
		return &WakeError{Status: http.StatusForbidden, Message: msg}
	default: // 409 no GPU, 429 quota, 5xx, or our bearer refused: try again later
		return &WakeError{Status: http.StatusServiceUnavailable, Message: "The sandbox could not be resumed yet: " + msg, RetryAfter: "2"}
	}
}
