//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/billing"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/org"
	"github.com/mutex-solutions/proxcloud/internal/platform/project"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

// sgPVE fakes one cluster firewall group, gaaa of project p1, the way PVE keeps it: a new rule lands at position 0,
// the list carries its digest, and a delete whose digest is not the list's is refused.
type sgPVE struct {
	mu      sync.Mutex
	rules   []url.Values // position = index
	added   []url.Values // every rule create, as sent
	deleted []string     // "<pos> <proto>/<dport>" of every rule delete PVE carried out
	// onDelete runs once before a delete is checked: a writer that gets in between the portal's read and its delete.
	onDelete func()
}

func (f *sgPVE) digest() string {
	parts := make([]string, len(f.rules))
	for i, r := range f.rules {
		parts[i] = r.Encode()
	}
	return fmt.Sprintf("%x", strings.Join(parts, "|"))
}

func (f *sgPVE) add(r url.Values) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append([]url.Values{r}, f.rules...)
}

func (f *sgPVE) service(t *testing.T) *pve.Service {
	t.Helper()
	write := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api2/json/cluster/firewall/groups", func(w http.ResponseWriter, _ *http.Request) {
		write(w, []any{map[string]any{"group": "gaaa", "comment": "st:p1:web"}})
	})
	mux.HandleFunc("GET /api2/json/cluster/firewall/groups/gaaa", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []any{}
		for i, r := range f.rules {
			row := map[string]any{"pos": i, "digest": f.digest(), "enable": 1}
			for k := range r {
				row[k] = r.Get(k)
			}
			out = append(out, row)
		}
		write(w, out)
	})
	mux.HandleFunc("POST /api2/json/cluster/firewall/groups/gaaa", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		rule := url.Values{}
		for k := range r.PostForm {
			if k != "enable" {
				rule.Set(k, r.PostForm.Get(k))
			}
		}
		f.add(rule)
		f.mu.Lock()
		f.added = append(f.added, rule)
		f.mu.Unlock()
		write(w, nil)
	})
	mux.HandleFunc("DELETE /api2/json/cluster/firewall/groups/gaaa/{pos}", func(w http.ResponseWriter, r *http.Request) {
		if hook := f.onDelete; hook != nil {
			f.onDelete = nil
			hook()
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		pos, _ := strconv.Atoi(r.PathValue("pos"))
		if r.URL.Query().Get("digest") != f.digest() {
			// PVE's refusal is the reason phrase of a 500 (assert_if_modified).
			conn, buf, _ := w.(http.Hijacker).Hijack()
			_, _ = buf.WriteString("HTTP/1.1 500 detected modified configuration - file changed by other user? Try again.\r\n" +
				"Content-Length: 13\r\nConnection: close\r\n\r\n{\"data\":null}")
			_ = buf.Flush()
			_ = conn.Close()
			return
		}
		f.deleted = append(f.deleted, fmt.Sprintf("%d %s/%s", pos, f.rules[pos].Get("proto"), f.rules[pos].Get("dport")))
		f.rules = append(f.rules[:pos], f.rules[pos+1:]...)
		write(w, nil)
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			t.Errorf("unexpected PVE call %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	ps, err := pve.NewService(pve.Config{APIURL: srv.URL, TokenID: "t", Secret: "s"}, "pve", "pc-")
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// sgRig is the project handler over a fresh database and the fake group, with the group's cache row in place.
type sgRig struct {
	t      *testing.T
	fake   *sgPVE
	router http.Handler
	rowID  string
}

func newSGRig(t *testing.T) *sgRig {
	t.Helper()
	db := freshPG(t)
	repo := project.NewRepo(db)
	orgRepo := org.NewRepo(db)
	billingRepo := billing.NewRepo(db)
	users := user.NewRepo(db)
	cloudRepo := cloud.NewRepo(db)
	esSvc := externalservice.NewService(externalservice.NewRepo(db), textcrypt.New("test-key"))
	h := project.NewHandler(project.NewService(repo, orgRepo, billingRepo, users, nil), project.NewPolicy(org.NewPolicy(orgRepo)),
		org.NewService(orgRepo, billingRepo, nil), users, audit.NewService(audit.NewRepo(db), slog.Default()), billingRepo,
		cloudRepo, esSvc, nil, nil, "r1")
	fake := &sgPVE{}
	ps := fake.service(t)
	h.SetProxmox(func(*externalservice.ExternalService) (*pve.Service, error) { return ps, nil })
	router := chi.NewRouter()
	h.Routes(router)
	for coll, doc := range map[string]pgdoc.M{
		"users":                {"sub": "owner-sub", "email": "o@example.org"},
		"organization":         {"_id": "o1", "name": "o"},
		"organization_members": {"organizationId": "o1", "sub": "owner-sub", "roles": []string{"OWNER"}},
		"externalService": {"_id": "pve", "name": "pve", "type": externalservice.TypeCloud, "status": externalservice.StatusPublic,
			"config": pgdoc.M{"provider": "proxmox"}},
		"project": {"_id": "p1", "name": "p", "status": project.StatusEnabled, "organizationId": "o1",
			"memberships": []pgdoc.M{{"sub": "owner-sub", "role": project.RoleOwner}},
			"services":    []any{pgdoc.M{"serviceId": "pve", "provider": "proxmox", "region": "r1", "pool": "pc-p1"}}},
	} {
		mustInsert(t, db, coll, doc)
	}
	row, err := cloudRepo.Insert(context.Background(), &cloud.CloudResource{ExternalID: "gaaa", ServiceID: "pve", ProjectID: "p1",
		Region: "r1", Type: cloud.TypeSecurityGroup, Data: map[string]any{"securityGroup": map[string]any{"id": "gaaa", "name": "web"}}})
	if err != nil {
		t.Fatal(err)
	}
	return &sgRig{t: t, fake: fake, router: router, rowID: row.ID}
}

// act posts one action on the group and returns the status and the body.
func (g *sgRig) act(action, data string) (int, string) {
	g.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/project/p1/cloud/"+g.rowID+"/action",
		strings.NewReader(`{"action":"`+action+`","data":`+data+`}`))
	req = req.WithContext(httpx.WithRC(req.Context(), &httpx.RequestContext{Sub: "owner-sub"}))
	w := httptest.NewRecorder()
	g.router.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

type sgListedRule struct {
	ID        string `json:"id"`
	Direction string `json:"direction"`
	Protocol  string `json:"protocol"`
	PortRange string `json:"port_range"`
	Remote    string `json:"remote_ip_prefix"`
}

func (g *sgRig) list() []sgListedRule {
	g.t.Helper()
	code, body := g.act("LIST_RULES", `{}`)
	var env struct {
		Data struct {
			Result []sgListedRule `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); code != http.StatusOK || err != nil {
		g.t.Fatalf("LIST_RULES: %d %s %v", code, body, err)
	}
	return env.Data.Result
}

// D5 and the validation: a rule the provider cannot store as asked is refused with a 400 that names what is wrong,
// and never reaches Proxmox. Before, the keys were dropped and a wider rule stored (a DROP as an ALLOW, a remote
// group as "anywhere", a port given as text as every port).
func TestSecurityGroupAddRuleRefusesWhatItCannotStore(t *testing.T) {
	g := newSGRig(t)
	for _, tt := range []struct{ name, data, want string }{
		{"remote group", `{"direction":"ingress","protocol":"tcp","portRangeMin":22,"remoteGroupId":"ge93pznd"}`, "Unsupported rule field: remoteGroupId"},
		{"deny, log, priority", `{"protocol":"tcp","portRangeMin":23,"action":"DROP","log":"info","priority":1}`, "Unsupported rule field: action, log, priority"},
		{"direction", `{"direction":"sideways"}`, "direction must be ingress or egress"},
		{"protocol", `{"protocol":"bogus"}`, "protocol must be one of"},
		{"protocol not text", `{"protocol":6}`, "protocol must be a string"},
		{"port too high", `{"protocol":"tcp","portRangeMin":70000}`, "portRangeMin must be a whole number from 1 to 65535"},
		{"port as text", `{"protocol":"tcp","portRangeMin":"22"}`, "portRangeMin must be a whole number"},
		{"port zero", `{"protocol":"tcp","portRangeMin":0}`, "portRangeMin must be a whole number"},
		{"port fraction", `{"protocol":"tcp","portRangeMin":22.5}`, "portRangeMin must be a whole number"},
		{"max without min", `{"protocol":"tcp","portRangeMax":80}`, "portRangeMax needs portRangeMin"},
		{"max below min", `{"protocol":"tcp","portRangeMin":90,"portRangeMax":80}`, "portRangeMax must not be below portRangeMin"},
		{"port without protocol", `{"portRangeMin":22}`, "A port range needs protocol tcp, udp or sctp"},
		{"port with icmp", `{"protocol":"icmp","portRangeMin":22}`, "A port range needs protocol tcp, udp or sctp"},
		{"not a CIDR", `{"remoteIpPrefix":"nope"}`, "remoteIpPrefix must be a CIDR"},
		{"an IP set, not a CIDR", `{"remoteIpPrefix":"+dc/management"}`, "remoteIpPrefix must be a CIDR"},
		{"host bits", `{"remoteIpPrefix":"10.0.0.5/24"}`, "use 10.0.0.0/24"},
	} {
		code, body := g.act("ADD_RULE", tt.data)
		if code != http.StatusBadRequest || !strings.Contains(body, tt.want) {
			t.Errorf("%s: %d %s, want 400 with %q", tt.name, code, body, tt.want)
		}
	}
	if len(g.fake.added) != 0 {
		t.Fatalf("refused rules reached Proxmox: %v", g.fake.added)
	}
}

// D6: the remote CIDR of an egress rule is its destination (it was written as the source), the answer says the rule
// restricts nothing, and the list tells the two directions apart.
func TestSecurityGroupRuleDirection(t *testing.T) {
	g := newSGRig(t)
	code, body := g.act("ADD_RULE", `{"direction":"ingress","protocol":"TCP","portRangeMin":8000,"portRangeMax":8100,"remoteIpPrefix":"203.0.113.0/24","description":"app"}`)
	if code != http.StatusOK || strings.Contains(body, "warning") {
		t.Fatalf("ingress: %d %s", code, body)
	}
	code, body = g.act("ADD_RULE", `{"direction":"egress","protocol":"tcp","portRangeMin":443,"remoteIpPrefix":"1.1.1.1/32"}`)
	if code != http.StatusOK || !strings.Contains(body, "restricts nothing") {
		t.Fatalf("egress: %d %s, want a warning that it restricts nothing", code, body)
	}
	in, out := g.fake.added[0], g.fake.added[1]
	if in.Get("type") != "in" || in.Get("action") != "ACCEPT" || in.Get("proto") != "tcp" || in.Get("dport") != "8000:8100" ||
		in.Get("source") != "203.0.113.0/24" || in.Has("dest") || in.Get("comment") != "app" {
		t.Errorf("ingress rule sent as %v", in)
	}
	if out.Get("type") != "out" || out.Get("dport") != "443" || out.Get("dest") != "1.1.1.1/32" || out.Has("source") {
		t.Errorf("egress rule sent as %v, want the CIDR in dest and no source", out)
	}
	rules := g.list() // the egress rule was added last, so it is at position 0
	if len(rules) != 2 || rules[0].Direction != "egress" || rules[0].Remote != "1.1.1.1/32" ||
		rules[1].Direction != "ingress" || rules[1].Remote != "203.0.113.0/24" || rules[1].PortRange != "8000:8100" {
		t.Fatalf("listed %+v", rules)
	}
}

// Gap 7: a delete names the rule, not a position. The id read from a list that is no longer current is refused with
// 409 and nothing is deleted (it removed whatever rule had moved to that position); a delete that names no rule is
// refused (it removed rule 0); after the 409 the list is current and the delete goes through.
func TestSecurityGroupDeleteRuleNamesTheRule(t *testing.T) {
	g := newSGRig(t)
	for _, port := range []string{"22", "80"} {
		if code, body := g.act("ADD_RULE", `{"protocol":"tcp","portRangeMin":`+port+`}`); code != http.StatusOK {
			t.Fatalf("add %s: %d %s", port, code, body)
		}
	}
	ssh := g.list()[1] // tcp/22, at position 1
	if ssh.PortRange != "22" {
		t.Fatalf("listed %+v", ssh)
	}

	for _, data := range []string{`{}`, `{"pos":0}`, `{"ruleId":""}`} {
		if code, body := g.act("DELETE_RULE", data); code != http.StatusBadRequest || !strings.Contains(body, "ruleId") {
			t.Errorf("DELETE_RULE %s: %d %s, want 400 naming ruleId", data, code, body)
		}
	}

	// Another session adds a rule: tcp/22 moves to position 2 and tcp/80 takes position 1.
	g.fake.add(url.Values{"type": {"in"}, "action": {"ACCEPT"}, "proto": {"udp"}, "dport": {"53"}})
	for _, id := range []string{ssh.ID, "gaaa-1"} { // the id of the stale page, and the id shape of v0.8.0
		if code, body := g.act("DELETE_RULE", `{"ruleId":"`+id+`"}`); code != http.StatusConflict || !strings.Contains(body, "Refresh") {
			t.Fatalf("stale delete %s: %d %s, want 409", id, code, body)
		}
	}
	if len(g.fake.deleted) != 0 {
		t.Fatalf("a refused delete removed %v", g.fake.deleted)
	}

	// The 409 refreshed the cache, so the list the client reloads has the rule under its current id.
	fresh := g.list()
	if len(fresh) != 3 || fresh[2].PortRange != "22" || fresh[2].ID == ssh.ID {
		t.Fatalf("list after the 409: %+v", fresh)
	}

	// A writer between the portal's read and its delete: PVE refuses on the digest, the tenant gets the same 409.
	g.fake.onDelete = func() { g.fake.add(url.Values{"type": {"in"}, "action": {"ACCEPT"}, "proto": {"icmp"}}) }
	if code, body := g.act("DELETE_RULE", `{"ruleId":"`+fresh[2].ID+`"}`); code != http.StatusConflict || !strings.Contains(body, "Refresh") {
		t.Fatalf("delete raced by a writer: %d %s, want 409", code, body)
	}
	if len(g.fake.deleted) != 0 {
		t.Fatalf("a refused delete removed %v", g.fake.deleted)
	}

	fresh = g.list()
	if code, body := g.act("DELETE_RULE", `{"ruleId":"`+fresh[3].ID+`"}`); code != http.StatusOK {
		t.Fatalf("delete with the current id: %d %s", code, body)
	}
	if len(g.fake.deleted) != 1 || g.fake.deleted[0] != "3 tcp/22" {
		t.Fatalf("deleted %v, want only tcp/22 at position 3", g.fake.deleted)
	}
	for _, r := range g.list() {
		if r.PortRange == "22" {
			t.Fatalf("tcp/22 is still listed: %+v", r)
		}
	}
}
