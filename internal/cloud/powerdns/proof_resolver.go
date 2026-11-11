package powerdns

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// OwnershipResolvers accepts only operator-configured literal IP:port endpoints.
// No tenant value, URL, hostname resolution, search suffix or /etc/resolv.conf is
// used. A private resolver is a deliberate operator trust override for a lab or
// an independently operated validating recursor, NEVER a tenant-selected server.
func OwnershipResolvers(raw any) ([]string, error) {
	if raw == nil {
		return []string{"1.1.1.1:53", "8.8.8.8:53"}, nil
	}
	var values []string
	switch v := raw.(type) {
	case []string:
		values = v
	case []any:
		for _, x := range v {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("ownershipResolvers must contain literal IP:port endpoints")
			}
			values = append(values, s)
		}
	default:
		return nil, fmt.Errorf("ownershipResolvers must be a list of literal IP:port endpoints")
	}
	if len(values) < 1 || len(values) > 2 {
		return nil, fmt.Errorf("ownershipResolvers needs one or two literal IP:port endpoints (omit for public defaults)")
	}
	for i, s := range values {
		a, err := netip.ParseAddrPort(s)
		if err != nil || a.Port() == 0 || a.Addr().IsUnspecified() || a.Addr().IsMulticast() || a.Addr().Zone() != "" {
			return nil, fmt.Errorf("ownershipResolvers must contain literal unicast IP:port endpoints")
		}
		values[i] = a.String()
	}
	return values, nil
}

// LookupProofTXT has a four-second TOTAL budget, at most two fixed TCP queries,
// two seconds each. Any configured trusted resolver's exact-owner answer can
// prove control; cache disagreement does not expand the trust set. DNS is not
// DNSSEC-authenticated here: the operator trusts the resolver and network path.
// CNAME/DNAME indirection is intentionally not proof of this name's TXT control.
func LookupProofTXT(ctx context.Context, resolvers []string, name string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var out []string
	usable := false
	for _, endpoint := range resolvers {
		qctx, qcancel := context.WithTimeout(ctx, 2*time.Second)
		txt, err := queryProofTXT(qctx, endpoint, name)
		qcancel()
		if err == nil {
			usable = true
			out = append(out, txt...)
		}
	}
	if !usable {
		return nil, errors.New("ownership DNS lookup failed") // no endpoint/token in tenant errors or logs
	}
	return out, nil
}

func queryProofTXT(ctx context.Context, endpoint, name string) ([]string, error) {
	n, err := dnsmessage.NewName(name)
	if err != nil {
		return nil, err
	}
	var id [2]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: binary.BigEndian.Uint16(id[:]), RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: n, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}}}
	wire, err := q.Pack()
	if err != nil {
		return nil, err
	}
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	deadline, _ := ctx.Deadline()
	if err := c.SetDeadline(deadline); err != nil {
		return nil, err
	}
	frame := make([]byte, 2+len(wire))
	binary.BigEndian.PutUint16(frame, uint16(len(wire)))
	copy(frame[2:], wire)
	if _, err := io.Copy(c, strings.NewReader(string(frame))); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(c, frame[:2]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint16(frame[:2])
	if size < 12 || size > 8192 {
		return nil, errors.New("invalid DNS response length")
	}
	answer := make([]byte, size)
	if _, err := io.ReadFull(c, answer); err != nil {
		return nil, err
	}
	return parseProofTXT(answer, q.Header.ID, name)
}

func parseProofTXT(wire []byte, id uint16, name string) ([]string, error) {
	var m dnsmessage.Message
	if err := m.Unpack(wire); err != nil {
		return nil, err
	}
	if !m.Response || m.ID != id || m.OpCode != 0 || m.Truncated || len(m.Questions) != 1 || len(m.Answers) > 64 {
		return nil, errors.New("invalid DNS proof response")
	}
	q := m.Questions[0]
	if !strings.EqualFold(q.Name.String(), name) || q.Type != dnsmessage.TypeTXT || q.Class != dnsmessage.ClassINET {
		return nil, errors.New("DNS proof question mismatch")
	}
	if m.RCode != dnsmessage.RCodeSuccess && m.RCode != dnsmessage.RCodeNameError {
		return nil, errors.New("DNS proof resolver refused")
	}
	var txt []string
	for _, rr := range m.Answers {
		if rr.Header.Type == dnsmessage.TypeCNAME || rr.Header.Type == dnsmessage.Type(39) { // DNAME
			return nil, errors.New("DNS proof aliases are not accepted")
		}
		if body, ok := rr.Body.(*dnsmessage.TXTResource); ok && rr.Header.Class == dnsmessage.ClassINET &&
			strings.EqualFold(rr.Header.Name.String(), name) && m.RCode == dnsmessage.RCodeSuccess {
			txt = append(txt, strings.Join(body.TXT, "")) // chunks of ONE RR, never join separate RRs
		}
	}
	return txt, nil
}
