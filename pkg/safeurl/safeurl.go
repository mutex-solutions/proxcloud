// Package safeurl guards tenant-supplied URLs that this platform, or a cluster node on its behalf, will fetch.
package safeurl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"syscall"
	"time"
)

// special4 and special6 are the IANA special-purpose blocks that are not globally routable, plus the transition
// prefixes that lead to an IPv4 address: 6to4 and Teredo hide whichever one their bits name. IPv6 outside 2000::/3 is
// not allocated for global unicast, so it needs no list: NAT64 (64:ff9b::/96), the old compatible form (::/96), local and
// link-local addresses all fall there.
var (
	special4 = prefixes("0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24", "192.88.99.0/24", "192.168.0.0/16",
		"192.175.48.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4")
	global6  = netip.MustParsePrefix("2000::/3")
	special6 = prefixes("2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20")
)

func prefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// Blocked reports an address a tenant-supplied URL must not reach: everything that is not globally routable.
func Blocked(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("")
	if !ip.IsValid() {
		return true
	}
	list := special4
	if ip.Is6() {
		if !global6.Contains(ip) {
			return true
		}
		list = special6
	}
	return slices.ContainsFunc(list, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// Refusal is a URL refused for a reason safe to show to whoever supplied it.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

var errInternal = &Refusal{Msg: "The URL points to an internal address, which is not allowed"}

const (
	headerTimeout = 20 * time.Second
	maxRedirects  = 5
)

// Source is the open answer to a checked URL. The caller closes Body. Size is what the server says, -1 if it does not;
// a body that ends short of a stated size fails the read.
type Source struct {
	Body io.ReadCloser
	Size int64
}

// Open fetches raw for the caller to stream: http or https, no credentials, every address of every host on the redirect
// chain public and not refused by also (nil = none), and a 2xx answer. The check that counts runs on the address being
// dialled, for every connection, so it also covers a name that changes its answer and a redirect that points inward.
//
// That holds only for this process's own fetch. A URL handed to another host, once checked, can answer that host
// differently, so never pass one on: stream the bytes instead.
func Open(ctx context.Context, raw string, also func(netip.Addr) bool) (*Source, error) {
	return open(ctx, raw, func(ip netip.Addr) bool { return Blocked(ip) || (also != nil && also(ip)) })
}

func open(ctx context.Context, raw string, blocked func(netip.Addr) bool) (*Source, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, &Refusal{Msg: "The URL is not valid"}
	}
	if err := checkHost(ctx, u, blocked); err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		ap, err := netip.ParseAddrPort(address)
		if err != nil || blocked(ap.Addr()) {
			return errInternal
		}
		return nil
	}}
	client := &http.Client{
		// No proxy, or the check would be on the proxy's address. No keep-alive: one fetch, one transport. No
		// decompression: Size must be the size of what is read.
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: headerTimeout,
			DisableKeepAlives: true, DisableCompression: true},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return &Refusal{Msg: "The URL redirects too many times"}
			}
			return checkHost(req.Context(), req.URL, blocked)
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, &Refusal{Msg: "The URL is not valid"}
	}
	res, err := client.Do(req)
	if err != nil {
		var r *Refusal
		if errors.As(err, &r) {
			return nil, r
		}
		return nil, &Refusal{Msg: "The URL could not be reached"}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		res.Body.Close()
		return nil, &Refusal{Msg: fmt.Sprintf("The URL answered with status %d", res.StatusCode)}
	}
	return &Source{Body: res.Body, Size: res.ContentLength}, nil
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
