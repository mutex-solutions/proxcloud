// Package registrypins provides dormant, cooperative PRIMARY-database fences.
// It performs no registry or cluster I/O and does not authorize real deletion.
package registrypins

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	ErrInvalid      = errors.New("registry pins: invalid identity or reference")
	ErrScope        = errors.New("registry pins: current own project scope required")
	ErrConflict     = errors.New("registry pins: identity, epoch or state conflict")
	ErrOperatorHold = errors.New("registry pins: operator hold")
	projectRE       = regexp.MustCompile(`^[a-z0-9]{1,40}$`)
	templateRE      = regexp.MustCompile(`^[0-9a-f]{20}$`)
	tagRE           = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRE        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	identityRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._/-]{0,127}$`)
)

// OperatorHoldError contains only a fixed reason code, never caller payloads.
type OperatorHoldError struct{ Reason string }

func (e *OperatorHoldError) Error() string { return ErrOperatorHold.Error() + ": " + e.Reason }
func (e *OperatorHoldError) Unwrap() error { return ErrOperatorHold }
func hold(reason string) error             { return &OperatorHoldError{Reason: reason} }

// RealDeleteHold is unconditional until writer integration, legacy adoption and
// a separately reviewed exact GC path exist. There is intentionally no override.
func RealDeleteHold() error { return hold("writer_coverage_and_legacy_adoption_required") }

// CanonicalHost validates a host[:port] taken ONLY from trusted provider Config.
// It accepts no URL, path, userinfo, query or fragment and never contacts a host.
// Host validation is not network authorization: callers must supply internal Config.
func CanonicalHost(raw string) (string, error) {
	if raw == "" || raw != strings.TrimSpace(raw) || strings.ContainsAny(raw, "/@?#%\\\t\r\n ") {
		return "", ErrInvalid
	}
	u, err := url.Parse("https://" + raw)
	if err != nil || u.Host != raw || u.User != nil || u.Hostname() == "" {
		return "", ErrInvalid
	}
	host := strings.ToLower(u.Hostname())
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	} else {
		if len(host) > 253 || strings.HasSuffix(host, ".") {
			return "", ErrInvalid
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", ErrInvalid
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return "", ErrInvalid
				}
			}
		}
	}
	port := u.Port()
	if strings.HasSuffix(raw, ":") {
		return "", ErrInvalid
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", ErrInvalid
		}
		port = strconv.Itoa(n)
	}
	if port == "443" {
		port = ""
	}
	if port != "" {
		return net.JoinHostPort(host, port), nil
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]", nil
	}
	return host, nil
}

// Ref is always an exact project template repository with either a digest or a
// pre-resolution tag alias. A bare repo, combined tag@digest, library or foreign
// host is rejected; nothing here discovers or resolves registry tags.
type Ref struct{ Host, Repo, Project, Template, Tag, Digest string }

func (r Ref) String() string {
	if r.Digest != "" {
		return r.Host + "/" + r.Repo + "@" + r.Digest
	}
	return r.Host + "/" + r.Repo + ":" + r.Tag
}
func ParseRef(trustedHost, raw string) (Ref, error) {
	host, err := CanonicalHost(trustedHost)
	if err != nil {
		return Ref{}, err
	}
	first, rest, ok := strings.Cut(raw, "/")
	if !ok {
		return Ref{}, ErrInvalid
	}
	got, err := CanonicalHost(first)
	if err != nil || got != host {
		return Ref{}, ErrInvalid
	}
	r := Ref{Host: host}
	if repo, digest, ok := strings.Cut(rest, "@"); ok {
		if !digestRE.MatchString(digest) {
			return Ref{}, ErrInvalid
		}
		r.Repo, r.Digest = repo, digest
	} else {
		i := strings.LastIndexByte(rest, ':')
		if i < 0 {
			return Ref{}, ErrInvalid
		}
		r.Repo, r.Tag = rest[:i], rest[i+1:]
		if !tagRE.MatchString(r.Tag) {
			return Ref{}, ErrInvalid
		}
	}
	parts := strings.Split(r.Repo, "/")
	if len(parts) != 3 || parts[0] != "t" || !projectRE.MatchString(parts[1]) || !templateRE.MatchString(parts[2]) {
		return Ref{}, ErrInvalid
	}
	r.Project, r.Template = parts[1], parts[2]
	return r, nil
}
