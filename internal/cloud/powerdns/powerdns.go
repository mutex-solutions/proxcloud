// Package powerdns serves tenant DNS zones from a PowerDNS Authoritative server over its HTTP API.
//
// A zone belongs to the project named in its account field. Every call that takes a zone name reads that field first:
// the zone namespace is global on the server, and a name in a request proves nothing.
package powerdns

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

// ErrZone is a request the server or the rules refuse; the message is safe to show.
var ErrZone = errors.New("zone")

func zonef(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrZone, fmt.Sprintf(format, a...))
}

const accountPrefix = "proxcloud:"

// recordTypes is what a tenant may write. SOA and the apex NS stay the operator's: they decide who serves the zone.
var recordTypes = []string{"A", "AAAA", "CAA", "CNAME", "MX", "NS", "PTR", "SRV", "TXT"}

var zoneNameRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]\.$`)

// Service is one PowerDNS server.
type Service struct {
	URL, ServerID, APIKey string
	Nameservers           []string // the NS records of a new zone, fully qualified
	HTTP                  *http.Client
}

// New builds a service; serverID defaults to localhost, PowerDNS's only server id in practice.
func New(url, serverID, apiKey string, nameservers []string) (*Service, error) {
	if url == "" || apiKey == "" {
		return nil, fmt.Errorf("powerdns: apiUrl and apiKey are required")
	}
	if len(nameservers) == 0 {
		return nil, fmt.Errorf("powerdns: nameservers are required")
	}
	if serverID == "" {
		serverID = "localhost"
	}
	ns := make([]string, 0, len(nameservers))
	for _, n := range nameservers {
		ns = append(ns, strings.TrimRight(n, ".")+".")
	}
	return &Service{URL: strings.TrimRight(url, "/"), ServerID: serverID, APIKey: apiKey, Nameservers: ns,
		HTTP: &http.Client{Timeout: 30 * time.Second}}, nil
}

// Zone is one tenant zone.
type Zone struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Kind    string  `json:"kind,omitempty"`
	Account string  `json:"account"`
	Serial  int64   `json:"serial,omitempty"`
	RRSets  []RRSet `json:"rrsets,omitempty"`
}

// RRSet is all records of one name and type.
type RRSet struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	TTL        int      `json:"ttl,omitempty"`
	ChangeType string   `json:"changetype,omitempty"`
	Records    []Record `json:"records"`
}

// Record is one record's content.
type Record struct {
	Content  string `json:"content"`
	Disabled bool   `json:"disabled"`
}

type apiError struct {
	Status int
	Msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("powerdns: %d: %s", e.Status, e.Msg) }

func (s *Service) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.URL+"/api/v1/servers/"+s.ServerID+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", s.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return &apiError{Status: resp.StatusCode, Msg: e.Error}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// NormalizeZone lowercases a zone name and adds the root dot.
func NormalizeZone(name string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(name)), ".") + "."
}

// List returns the project's zones. The API cannot filter by account.
func (s *Service) List(ctx context.Context, projectID string) ([]Zone, error) {
	var all []Zone
	if err := s.do(ctx, http.MethodGet, "/zones", nil, &all); err != nil {
		return nil, err
	}
	out := []Zone{}
	for _, z := range all {
		if z.Account == accountPrefix+projectID {
			out = append(out, z)
		}
	}
	return out, nil
}

// Create makes a native zone owned by the project. A name the server already holds is refused for any owner.
func (s *Service) Create(ctx context.Context, projectID, name string) (*Zone, error) {
	name = NormalizeZone(name)
	if !zoneNameRe.MatchString(name) {
		return nil, zonef("%s is not a valid zone name", name)
	}
	var z Zone
	err := s.do(ctx, http.MethodPost, "/zones", map[string]any{
		"name": name, "kind": "Native", "account": accountPrefix + projectID, "nameservers": s.Nameservers,
	}, &z)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusConflict {
		return nil, zonef("zone %s is already taken", name)
	}
	if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity {
		return nil, zonef("%s", ae.Msg)
	}
	if err != nil {
		return nil, err
	}
	return &z, nil
}

// get reads a zone and proves the project owns it. A zone of another project looks absent.
func (s *Service) get(ctx context.Context, projectID, name string) (*Zone, error) {
	var z Zone
	err := s.do(ctx, http.MethodGet, "/zones/"+NormalizeZone(name), nil, &z)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil, zonef("zone not found")
	}
	if err != nil {
		return nil, err
	}
	if z.Account != accountPrefix+projectID {
		return nil, zonef("zone not found")
	}
	return &z, nil
}

// Records lists a zone's record sets. The SOA is the server's business and stays out.
func (s *Service) Records(ctx context.Context, projectID, name string) ([]RRSet, error) {
	z, err := s.get(ctx, projectID, name)
	if err != nil {
		return nil, err
	}
	out := make([]RRSet, 0, len(z.RRSets))
	for _, r := range z.RRSets {
		if r.Type != "SOA" {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b RRSet) int { return strings.Compare(a.Name+a.Type, b.Name+b.Type) })
	return out, nil
}

// recordName turns what the user typed ("www", "@", a full name) into a name inside the zone.
func recordName(zone, name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	switch {
	case name == "" || name == "@":
		return zone, nil
	case strings.HasSuffix(name, "."):
	default:
		name += "." + zone
	}
	if name != zone && !strings.HasSuffix(name, "."+zone) {
		return "", zonef("%s is outside zone %s", name, zone)
	}
	return name, nil
}

func checkWritable(zone, name, typ string) error {
	if !slices.Contains(recordTypes, typ) {
		return zonef("record type %s is not available (use one of %s)", typ, strings.Join(recordTypes, ", "))
	}
	if typ == "NS" && name == zone {
		return zonef("the zone's own NS records are set by the operator")
	}
	return nil
}

// SetRecord replaces all records of one name and type. TXT content is quoted when the user did not.
func (s *Service) SetRecord(ctx context.Context, projectID, zone, name, typ string, ttl int, contents []string) error {
	z, err := s.get(ctx, projectID, zone)
	if err != nil {
		return err
	}
	typ = strings.ToUpper(typ)
	full, err := recordName(z.Name, name)
	if err != nil {
		return err
	}
	if err := checkWritable(z.Name, full, typ); err != nil {
		return err
	}
	if len(contents) == 0 {
		return zonef("a record needs content")
	}
	if ttl <= 0 {
		ttl = 3600
	}
	records := make([]Record, 0, len(contents))
	for _, c := range contents {
		c = strings.TrimSpace(c)
		if typ == "TXT" && !strings.HasPrefix(c, `"`) {
			c = `"` + strings.ReplaceAll(c, `"`, `\"`) + `"`
		}
		records = append(records, Record{Content: c})
	}
	return s.patch(ctx, z.Name, RRSet{Name: full, Type: typ, TTL: ttl, ChangeType: "REPLACE", Records: records})
}

// DeleteRecord removes all records of one name and type.
func (s *Service) DeleteRecord(ctx context.Context, projectID, zone, name, typ string) error {
	z, err := s.get(ctx, projectID, zone)
	if err != nil {
		return err
	}
	typ = strings.ToUpper(typ)
	full, err := recordName(z.Name, name)
	if err != nil {
		return err
	}
	if err := checkWritable(z.Name, full, typ); err != nil {
		return err
	}
	return s.patch(ctx, z.Name, RRSet{Name: full, Type: typ, ChangeType: "DELETE", Records: []Record{}})
}

func (s *Service) patch(ctx context.Context, zone string, rr RRSet) error {
	err := s.do(ctx, http.MethodPatch, "/zones/"+zone, map[string]any{"rrsets": []RRSet{rr}}, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusUnprocessableEntity {
		return zonef("%s", ae.Msg) // what the server says about bad content is what the user must fix
	}
	return err
}

// Delete removes a zone. One already gone is a no-op.
func (s *Service) Delete(ctx context.Context, projectID, name string) error {
	z, err := s.get(ctx, projectID, name)
	if errors.Is(err, ErrZone) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.do(ctx, http.MethodDelete, "/zones/"+z.Name, nil, nil)
}

// ZoneData is the cache row payload.
func (s *Service) ZoneData(z *Zone) map[string]any {
	return map[string]any{"zone": map[string]any{
		"name": z.Name, "status": "ACTIVE", "serial": z.Serial, "nameservers": anyStrings(s.Nameservers),
	}}
}

func anyStrings(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// ZoneSyncProvider lists a project's zones for the periodic sync (providers.Provider + ProjectScoped).
type ZoneSyncProvider struct {
	svc               *Service
	region, projectID string
}

// ZoneSyncProvider builds the sync provider of one project.
func (s *Service) ZoneSyncProvider(region, projectID string) *ZoneSyncProvider {
	return &ZoneSyncProvider{svc: s, region: region, projectID: projectID}
}

func (p *ZoneSyncProvider) Type() string          { return cloud.TypeDNSZone }
func (p *ZoneSyncProvider) ProjectID() string     { return p.projectID }
func (p *ZoneSyncProvider) CompareKeys() []string { return []string{} }

// List reads the server's zones of the project.
func (p *ZoneSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	list, err := p.svc.List(ctx, p.projectID)
	if err != nil {
		return nil, err
	}
	out := make([]cloud.CloudResource, 0, len(list))
	for i := range list {
		out = append(out, cloud.CloudResource{
			ExternalID: list[i].Name, ProjectID: p.projectID, Region: p.region, Type: cloud.TypeDNSZone,
			Data: p.svc.ZoneData(&list[i]),
		})
	}
	return out, nil
}
