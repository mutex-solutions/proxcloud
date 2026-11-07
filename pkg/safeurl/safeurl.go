// Package safeurl guards tenant-supplied URLs that this platform, or a cluster node on its behalf, will fetch.
package safeurl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Blocked reports an address a tenant-supplied URL must not reach: everything that is not public.
func Blocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	return !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || cgnat.Contains(ip)
}

// Refusal is a URL refused for a reason safe to show to whoever supplied it.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

var errInternal = &Refusal{Msg: "The URL points to an internal address, which is not allowed"}

const (
	probeTimeout = 20 * time.Second
	maxRedirects = 5
)

// Target is what a probe found: the URL after redirects, and the size if the server says (-1 otherwise).
type Target struct {
	URL  string
	Size int64
}

// Check probes raw for a fetch by another host: http or https, no credentials, every address of every host on the
// redirect chain public and not refused by also (nil = none), and a 2xx answer. It returns the final URL, so the
// other host starts past the redirects this check has seen.
//
// ponytail: the other host resolves the name again and may follow new redirects, so DNS rebinding can still point it
// inward after this check. The caller must not let the fetched bytes reach the tenant unchecked; a fetch proxy
// pinned to the checked address would close it.
func Check(ctx context.Context, raw string, also func(netip.Addr) bool) (Target, error) {
	return probe(ctx, raw, func(ip netip.Addr) bool { return Blocked(ip) || (also != nil && also(ip)) })
}

func probe(ctx context.Context, raw string, blocked func(netip.Addr) bool) (Target, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	u, err := url.Parse(raw)
	if err != nil {
		return Target{}, &Refusal{Msg: "The URL is not valid"}
	}
	if err := checkHost(ctx, u, blocked); err != nil {
		return Target{}, err
	}
	// The connect-time check covers what the name resolved to when this process actually dialled.
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil || blocked(ap.Addr()) {
			return errInternal
		}
		return nil
	}}
	client := &http.Client{
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return &Refusal{Msg: "The URL redirects too many times"}
			}
			return checkHost(req.Context(), req.URL, blocked)
		},
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return Target{}, &Refusal{Msg: "The URL is not valid"}
	}
	res, err := client.Do(req)
	if err != nil {
		var r *Refusal
		if errors.As(err, &r) {
			return Target{}, r
		}
		return Target{}, &Refusal{Msg: "The URL could not be reached"}
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return Target{}, &Refusal{Msg: fmt.Sprintf("The URL answered with status %d", res.StatusCode)}
	}
	return Target{URL: res.Request.URL.String(), Size: res.ContentLength}, nil
}

func checkHost(ctx context.Context, u *url.URL, blocked func(netip.Addr) bool) error {
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return &Refusal{Msg: "The URL must be http or https, with a host and no credentials"}
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil || len(ips) == 0 {
		return &Refusal{Msg: "The URL's host does not resolve"}
	}
	for _, ip := range ips {
		if blocked(ip) {
			return errInternal
		}
	}
	return nil
}
