package kamajik8s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// A watch is a trigger and nothing more: it says that something of a collection changed, never what. Who is told lists
// again and decides from the list, so nothing is ever decided from a stream that may have lost an event. The load
// balancer controller is the caller (cmd/lb-controller): a raw watch over HTTP on this client, no informer and no cache.

const (
	// watchSeconds is how long the API server is asked to keep one stream (timeoutSeconds); it ends it then, and the
	// next one starts at the resourceVersion the last one reached.
	watchSeconds = 300
	// watchSilence is how much longer than that a stream may live: a connection that died without a word is given up
	// then, since nothing else would end it.
	watchSilence = 30 * time.Second
	// watchMinWait is the wait between two streams, and the first wait after one that failed; watchMaxWait is the
	// longest, reached by doubling, so an API server that is down is asked twice a minute.
	watchMinWait = time.Second
	watchMaxWait = 30 * time.Second
)

// WatchServices calls changed for every change of a Service of any namespace, until ctx ends. See watch.
func (c *Client) WatchServices(ctx context.Context, changed func(error)) {
	c.watch(ctx, "/api/v1/services", "", changed, watchMinWait, watchMaxWait)
}

// WatchNodes calls changed for every change of a Node, until ctx ends. See watch.
func (c *Client) WatchNodes(ctx context.Context, changed func(error)) {
	c.watch(ctx, "/api/v1/nodes", "", changed, watchMinWait, watchMaxWait)
}

// WatchSecret calls changed for every change of the one Secret name of namespace ns, until ctx ends: the field
// selector metadata.name makes it a watch of that name and no other, which is also what a role that names the Secret
// (resourceNames) allows. There is no watch of a namespace's Secrets here. Nothing of the Secret is read off the
// stream: watch decodes an event's type and resourceVersion only, and GetSecret stays the one read.
func (c *Client) WatchSecret(ctx context.Context, ns, name string, changed func(error)) {
	c.watch(ctx, fmt.Sprintf(pathSecrets, url.PathEscape(ns)), "metadata.name="+name, changed, watchMinWait, watchMaxWait)
}

// watch follows the collection at path, stream after stream, until ctx ends. It calls changed(nil) for every event
// that is not a bookmark, and once more whenever a stream ends, whatever ended it (the server's timeout, an error, a
// resourceVersion that is gone): a caller that lists at every call has then seen what an event lost on the way would
// have said. An end that was an error is given to changed, for the log.
//
// A stream starts at the resourceVersion the last one reached (events and bookmarks carry it), so nothing happens
// unseen between two streams. Without one, at the start and after 410 Gone, the API server first sends every object
// as ADDED. After a stream the next one waits minWait, and twice as long, up to maxWait, after each one that brought
// nothing: an API server that is down, or answers and hangs up at once, is not asked in a loop.
func (c *Client) watch(ctx context.Context, path, fieldSelector string, changed func(error), minWait, maxWait time.Duration) {
	// A client of its own: the stream is the answer's body, so there is no time limit on the whole call as c.http
	// has. The limits are on reaching the server and on its first answer; stream ends a silent one.
	hc := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:       c.rc.tlsCfg,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer hc.CloseIdleConnections()
	rv, wait := "", time.Duration(0)
	for {
		began := time.Now()
		next, brought, err := c.stream(ctx, hc, path, fieldSelector, rv, changed)
		if ctx.Err() != nil {
			return
		}
		rv = next
		changed(err)
		if brought || time.Since(began) > maxWait {
			wait = minWait
		} else {
			wait = min(max(2*wait, minWait), maxWait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// stream is one watch request, read to its end. next is the resourceVersion the following stream starts at: the last
// one seen, or none after 410 Gone, which the API server says with the answer's status or with an ERROR event,
// depending on where it notices. brought says the stream delivered an event or a bookmark. err is nil when the server
// ended the stream itself.
func (c *Client) stream(ctx context.Context, hc *http.Client, path, fieldSelector, rv string, changed func(error)) (next string, brought bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, watchSeconds*time.Second+watchSilence)
	defer cancel()
	q := url.Values{"watch": {"1"}, "allowWatchBookmarks": {"true"}, "timeoutSeconds": {strconv.Itoa(watchSeconds)}}
	if fieldSelector != "" {
		q.Set("fieldSelector", fieldSelector)
	}
	if rv != "" {
		q.Set("resourceVersion", rv)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.rc.server, "/")+path+"?"+q.Encode(), nil)
	if err != nil {
		return rv, false, err
	}
	req.Header.Set("Accept", "application/json")
	if t := c.rc.bearer(); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	res, err := hc.Do(req)
	if err != nil {
		return rv, false, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusGone {
		return "", false, nil
	}
	if res.StatusCode != http.StatusOK {
		var status struct {
			Message string `json:"message"`
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		if json.Unmarshal(raw, &status) != nil || status.Message == "" {
			status.Message = "watch " + path
		}
		return rv, false, &APIError{Status: res.StatusCode, Message: status.Message}
	}
	// One JSON document per event. Of an event only this much is decoded: the object itself, a Secret's data
	// included, is skipped by the decoder and kept nowhere.
	dec := json.NewDecoder(res.Body)
	for {
		var ev struct {
			Type   string `json:"type"`
			Object struct {
				Code     int    `json:"code"`
				Message  string `json:"message"`
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
			} `json:"object"`
		}
		if err := dec.Decode(&ev); err != nil {
			if err == io.EOF { // the server's own end; a stream cut off ends in another error
				return rv, brought, nil
			}
			return rv, brought, fmt.Errorf("watch %s: %w", path, err)
		}
		if ev.Type == "ERROR" {
			if ev.Object.Code == http.StatusGone {
				return "", brought, nil
			}
			return rv, brought, &APIError{Status: ev.Object.Code, Message: ev.Object.Message}
		}
		brought = true
		if ev.Object.Metadata.ResourceVersion != "" {
			rv = ev.Object.Metadata.ResourceVersion
		}
		if ev.Type != "BOOKMARK" {
			changed(nil)
		}
	}
}
