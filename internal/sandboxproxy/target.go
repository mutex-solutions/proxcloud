// Package sandboxproxy routes E2B-style sandbox URLs (<port>-<sandboxID>.<domain>) to the
// pod that backs the sandbox, mirroring the semantics of E2B's client-proxy.
package sandboxproxy

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	// EnvdPort is where envd listens inside every sandbox pod.
	EnvdPort = 49983

	headerSandboxID   = "E2b-Sandbox-Id"
	headerSandboxPort = "E2b-Sandbox-Port"
	// TrafficTokenHeader carries the per-sandbox traffic access token.
	TrafficTokenHeader = "e2b-traffic-access-token"
)

var (
	sandboxIDRe = regexp.MustCompile(`^sbx-[0-9a-f]{8}([0-9a-f]{8})?$`)
	// The first label of a function host: <function name>-<6 hex>. A sandbox host label starts
	// with its port, a digit, and the header-routing hosts (sandbox.<domain>, localhost) have no
	// such suffix: none of them matches.
	functionHostRe = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?-[0-9a-f]{6}$`)

	errInvalidHost      = errors.New("invalid host")
	errInvalidSandboxID = errors.New("invalid sandbox id")
	errInvalidPort      = errors.New("invalid sandbox port")
	errMissingHeader    = errors.New("missing routing header")

	// envd paths reserved for the control plane (envd.yaml x-internal: true).
	internalPaths = map[string]bool{
		"/init": true, "/freeze": true, "/unfreeze": true,
		"/collapse": true, "/fsfreeze": true, "/fsthaw": true,
	}
)

func parsePort(s string) (int, error) {
	p, err := strconv.ParseUint(s, 10, 16)
	if err != nil || p == 0 {
		return 0, errInvalidPort
	}
	return int(p), nil
}

// parseTarget extracts (sandboxID, port) from the request. Routing headers are honoured only
// when Host is sandbox.<domain>, localhost or an IP; otherwise the Host must be
// <port>-<sandboxID>.<domain>.
func parseTarget(r *http.Request, domain string) (string, int, error) {
	hostname := (&url.URL{Host: r.Host}).Hostname()
	headerHost := hostname == "sandbox."+domain || hostname == "localhost" || net.ParseIP(hostname) != nil
	if headerHost && (r.Header.Get(headerSandboxID) != "" || r.Header.Get(headerSandboxPort) != "") {
		id, ps := r.Header.Get(headerSandboxID), r.Header.Get(headerSandboxPort)
		if id == "" || ps == "" {
			return "", 0, errMissingHeader
		}
		port, err := parsePort(ps)
		if err != nil {
			return "", 0, err
		}
		if !sandboxIDRe.MatchString(id) {
			return "", 0, errInvalidSandboxID
		}
		return id, port, nil
	}

	label, rest, ok := strings.Cut(hostname, ".")
	if !ok || rest != domain {
		return "", 0, errInvalidHost
	}
	ps, id, ok := strings.Cut(label, "-")
	if !ok {
		return "", 0, errInvalidHost
	}
	port, err := parsePort(ps)
	if err != nil {
		return "", 0, err
	}
	if !sandboxIDRe.MatchString(id) {
		return "", 0, errInvalidSandboxID
	}
	return id, port, nil
}

// functionHost returns the first label of a function host, <name>-<6 hex>.<domain>, whatever
// its case or port: false for every other host, which takes the sandbox path. It is the only
// thing of a Host header function mode ever looks anything up by.
func functionHost(host, domain string) (string, bool) {
	label, rest, _ := strings.Cut(strings.ToLower((&url.URL{Host: host}).Hostname()), ".")
	return label, rest == domain && functionHostRe.MatchString(label)
}
