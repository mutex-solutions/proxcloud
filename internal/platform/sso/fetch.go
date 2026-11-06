package sso

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// fetcher reads tenant-supplied URLs from inside the management network, so it refuses to reach
// anything but public addresses. The check runs on the resolved address at connect time (the
// Control hook), which also covers redirects and DNS rebinding.
type fetcher struct {
	allowInternal bool // development only: IdPs on http and private addresses
	client        *http.Client
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func blockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() || cgnat.Contains(ip)
}

func newFetcher(allowInternal bool) *fetcher {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	if !allowInternal {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || blockedIP(ap.Addr()) {
				return errors.New("address not allowed")
			}
			return nil
		}
	}
	f := &fetcher{allowInternal: allowInternal}
	f.client = &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 5 * time.Second},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			return f.checkScheme(req.URL)
		},
	}
	return f
}

func (f *fetcher) checkScheme(u *url.URL) error {
	switch {
	case u.Hostname() == "" || u.User != nil:
		return errors.New("URL must have a host and no credentials")
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && f.allowInternal:
		return nil
	}
	return errors.New("URL must use https")
}

// checkURL validates a URL Keycloak will call later, when this process is not in the path: the
// scheme, and that the host does not resolve to an internal address right now.
func (f *fetcher) checkURL(ctx context.Context, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("invalid URL")
	}
	if err := f.checkScheme(u); err != nil {
		return err
	}
	if f.allowInternal {
		return nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil {
		return errors.New("host does not resolve")
	}
	for _, ip := range ips {
		if blockedIP(ip) {
			return errors.New("host resolves to an internal address")
		}
	}
	return nil
}

const maxMetadataBytes = 1 << 20

// get fetches a discovery document or SAML metadata. Errors never echo the response body.
func (f *fetcher) get(ctx context.Context, raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid URL")
	}
	if err := f.checkScheme(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, errors.New("invalid URL")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, errors.New("could not reach the identity provider")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the identity provider answered with status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes+1))
	if err != nil {
		return nil, errors.New("could not read the document")
	}
	if len(body) > maxMetadataBytes {
		return nil, errors.New("the document is too large")
	}
	return body, nil
}
