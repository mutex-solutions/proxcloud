package csigw

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
)

// memReg is the registry in memory.
type memReg struct {
	mu    sync.Mutex
	creds map[string]*Credential
	vols  map[string]*Volume
	tasks map[string]*Task
}

func (m *memReg) Credential(_ context.Context, id string) (*Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.creds[id]; c != nil {
		cp := *c
		return &cp, nil
	}
	return nil, nil
}

func (m *memReg) InsertVolume(_ context.Context, v *Volume) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vols[v.ID] != nil {
		return ErrExists
	}
	cp := *v
	m.vols[v.ID] = &cp
	return nil
}

func (m *memReg) Volume(_ context.Context, id string) (*Volume, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := m.vols[id]; v != nil {
		cp := *v
		return &cp, nil
	}
	return nil, nil
}

func (m *memReg) UpdateVolume(_ context.Context, id string, set pgdoc.M) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	v := m.vols[id]
	if v == nil {
		return nil
	}
	for k, val := range set {
		switch k {
		case "state":
			v.State = val.(string)
		case "volid":
			v.VolID = val.(string)
		case "sizeBytes":
			v.SizeBytes = val.(int64)
		case "deletedAt":
			t := val.(time.Time)
			v.DeletedAt = &t
		}
	}
	return nil
}

func (m *memReg) DeleteVolume(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.vols, id)
	return nil
}

func (m *memReg) ClusterVolumes(_ context.Context, cluster, svc, storage string) ([]Volume, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Volume
	for _, v := range m.vols {
		if v.ClusterID == cluster && v.ComputeServiceID == svc && v.Storage == storage && v.State != StateDeleted {
			out = append(out, *v)
		}
	}
	return out, nil
}

func (m *memReg) ProjectBytes(_ context.Context, project, svc string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for _, v := range m.vols {
		if v.ProjectID == project && v.ComputeServiceID == svc && v.State != StateDeleted {
			n += v.SizeBytes
		}
	}
	return n, nil
}

func (m *memReg) PutTask(_ context.Context, t *Task) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *t
	m.tasks[t.ID] = &cp
	return nil
}

func (m *memReg) Task(_ context.Context, id string) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t := m.tasks[id]; t != nil {
		cp := *t
		return &cp, nil
	}
	return nil, nil
}

func (m *memReg) FinishTask(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tasks[id]
	if t == nil || t.Done {
		return false, nil
	}
	t.Done = true
	return true, nil
}

// fakePVE answers the calls the gateway forwards and records the mutations.
type fakePVE struct {
	mu      sync.Mutex
	writes  []string // "METHOD path form"
	config  map[string]map[string]any
	content map[string][]map[string]any // storage → entries
	failNew bool
}

const (
	u1  = "11111111-1111-4111-8111-111111111111" // c1, attached to 101 on scsi1
	u1b = "1111bbbb-1111-4111-8111-111111111111" // c1, not attached
	u2  = "22222222-2222-4222-8222-222222222222" // c2, attached to 102 on scsi1
	u3  = "33333333-3333-4333-8333-333333333333" // nobody's
)

func newFake() *fakePVE {
	return &fakePVE{
		config: map[string]map[string]any{
			"101": {"scsi0": "local-zfs:vm-101-disk-0,size=10G", "scsi1": "local-zfs:vm-9999-pvc-" + u1 + ",backup=0,size=1G,wwn=0x5000000000000001"},
			"102": {"scsi0": "local-zfs:vm-102-disk-0,size=10G", "scsi1": "local-zfs:vm-9999-pvc-" + u2 + ",size=1G"},
			"103": {"scsi0": "local-zfs:vm-103-disk-0,size=10G"},
			"104": {"scsi0": "local-zfs:vm-104-disk-0,size=10G"},
		},
		content: map[string][]map[string]any{
			"local-zfs": {
				{"volid": "local-zfs:vm-101-disk-0", "size": 10 << 30},
				{"volid": "local-zfs:vm-103-disk-0", "size": 10 << 30},
				{"volid": "local-zfs:vm-9999-pvc-" + u1, "size": 1 << 30},
				{"volid": "local-zfs:vm-9999-pvc-" + u1b, "size": 1 << 30},
				{"volid": "local-zfs:vm-9999-pvc-" + u2, "size": 1 << 30},
			},
		},
	}
}

func (f *fakePVE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = r.ParseForm()
	p := strings.TrimPrefix(r.URL.Path, "/api2/json")
	seg := strings.Split(strings.Trim(p, "/"), "/")
	data := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+p+" "+r.Form.Encode())
	}
	upid := "UPID:pve1:0000ABCD:00000001:6743A1B2:task:" + seg[len(seg)-1] + ":csi@pve!gateway:"
	switch {
	case p == "/cluster/resources":
		switch r.Form.Get("type") {
		case "node":
			data([]map[string]any{{"type": "node", "node": "pve1", "status": "online"}, {"type": "node", "node": "pve2", "status": "online"}})
		case "storage":
			var out []map[string]any
			for _, n := range []string{"pve1", "pve2"} {
				out = append(out, map[string]any{"type": "storage", "storage": "local-zfs", "node": n, "status": "available", "shared": 0},
					map[string]any{"type": "storage", "storage": "nas", "node": n, "status": "available", "shared": 1},
					map[string]any{"type": "storage", "storage": "other-store", "node": n, "status": "available", "shared": 0})
			}
			data(out)
		case "vm":
			data([]map[string]any{
				{"type": "qemu", "vmid": 101, "node": "pve1", "pool": "pc-p1", "tags": "pc-k8s;pc-k8s-c1;proxcloud", "template": 0},
				{"type": "qemu", "vmid": 102, "node": "pve1", "pool": "pc-p2", "tags": "pc-k8s-c2;proxcloud", "template": 0},
				{"type": "qemu", "vmid": 103, "node": "pve1", "pool": "pc-p1", "tags": "proxcloud", "template": 0},
				{"type": "qemu", "vmid": 104, "node": "pve2", "pool": "pc-p2", "tags": "pc-k8s-c1", "template": 0},
				{"type": "qemu", "vmid": 105, "node": "pve1", "pool": "pc-p1", "tags": "pc-k8s-c1", "template": 1},
				{"type": "lxc", "vmid": 106, "node": "pve1", "pool": "pc-p1", "tags": "pc-k8s-c1", "template": 0},
			})
		}
	case len(seg) == 2 && seg[0] == "storage":
		data(map[string]any{"storage": seg[1], "type": "zfspool"})
	case len(seg) == 5 && seg[4] == "status" && seg[2] == "storage":
		data(map[string]any{"avail": 1 << 40})
	case len(seg) == 5 && seg[4] == "content" && r.Method == http.MethodGet:
		data(f.content[seg[3]])
	case len(seg) == 5 && seg[4] == "content":
		if f.failNew {
			w.WriteHeader(500)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "message": "zfs error"})
			return
		}
		volid := seg[3] + ":" + r.Form.Get("filename")
		f.content[seg[3]] = append(f.content[seg[3]], map[string]any{"volid": volid, "size": 1})
		data(volid)
	case len(seg) >= 6 && seg[4] == "content":
		data(upid)
	case len(seg) == 5 && seg[2] == "qemu" && seg[4] == "config":
		cfg := f.config[seg[3]]
		if r.Method == http.MethodPost {
			for k := range r.PostForm {
				cfg[k] = r.PostForm.Get(k)
			}
			data(upid)
			return
		}
		if r.Method == http.MethodPut {
			data(nil)
			return
		}
		data(cfg)
	case len(seg) == 5 && seg[4] == "unlink":
		delete(f.config[seg[3]], r.Form.Get("idlist"))
		data(nil)
	case len(seg) == 5 && seg[4] == "resize":
		data(upid)
	case len(seg) == 5 && seg[2] == "tasks":
		data(map[string]any{"status": "stopped", "exitstatus": "OK"})
	default:
		w.WriteHeader(501)
	}
}

func (f *fakePVE) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

type rig struct {
	h      *Handler
	reg    *memReg
	pve    *fakePVE
	audits []audit.AuditEvent
}

func newRig(t *testing.T) *rig {
	t.Helper()
	f := newFake()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	client := func(context.Context, string) (*pve.Client, error) {
		return pve.New(pve.Config{APIURL: srv.URL, TokenID: "csi@pve!gateway", Secret: "x"})
	}
	reg := &memReg{creds: map[string]*Credential{}, vols: map[string]*Volume{}, tasks: map[string]*Task{}}
	reg.creds["c1"] = &Credential{ID: "c1", ProjectID: "p1", ComputeServiceID: "svc", Pool: "pc-p1",
		Storages: []string{"local-zfs", "nas"}, ControllerVMID: 9999, SecretSHA256: hashSecret("s1")}
	reg.creds["c2"] = &Credential{ID: "c2", ProjectID: "p2", ComputeServiceID: "svc", Pool: "pc-p2",
		Storages: []string{"local-zfs"}, ControllerVMID: 9999, SecretSHA256: hashSecret("s2")}
	now := time.Now()
	reg.creds["gone"] = &Credential{ID: "gone", ComputeServiceID: "svc", Pool: "pc-p1", ControllerVMID: 9999,
		SecretSHA256: hashSecret("s3"), RevokedAt: &now}
	for _, v := range []struct{ cluster, project, uuid string }{{"c1", "p1", u1}, {"c1", "p1", u1b}, {"c2", "p2", u2}} {
		name := "vm-9999-pvc-" + v.uuid
		reg.vols[VolumeID("svc", "local-zfs", name)] = &Volume{ID: VolumeID("svc", "local-zfs", name), ClusterID: v.cluster,
			ProjectID: v.project, ComputeServiceID: "svc", Storage: "local-zfs", Node: "pve1",
			VolID: "local-zfs:" + name, SizeBytes: 1 << 30, State: StateReady}
	}
	reg.tasks["UPID:pve1:c2task:"] = &Task{ID: "UPID:pve1:c2task:", ClusterID: "c2", ExpiresAt: now.Add(time.Hour)}
	reg.tasks["UPID:pve1:old:"] = &Task{ID: "UPID:pve1:old:", ClusterID: "c1", ExpiresAt: now.Add(-time.Minute)}
	r := &rig{reg: reg, pve: f}
	r.h = &Handler{Reg: reg, PVE: client, Portal: client, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Audit: func(ev audit.AuditEvent) { r.audits = append(r.audits, ev) }}
	return r
}

func (r *rig) do(auth, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	return rec
}

const (
	c1 = "PVEAPIToken=csi@proxcloud!c1=s1"
	c2 = "PVEAPIToken=csi@proxcloud!c2=s2"
	j  = "/api2/json"
)

var (
	disk1  = "local-zfs:vm-9999-pvc-" + u1
	disk1b = "local-zfs:vm-9999-pvc-" + u1b
	disk2  = "local-zfs:vm-9999-pvc-" + u2
)

func q(kv ...string) string {
	v := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v.Encode()
}

func TestAuthentication(t *testing.T) {
	r := newRig(t)
	for name, auth := range map[string]string{
		"none":          "",
		"bearer":        "Bearer s1",
		"wrong secret":  "PVEAPIToken=csi@proxcloud!c1=s2",
		"other realm":   "PVEAPIToken=csi@pve!c1=s1",
		"unknown":       "PVEAPIToken=csi@proxcloud!c9=s1",
		"revoked":       "PVEAPIToken=csi@proxcloud!gone=s3",
		"no secret":     "PVEAPIToken=csi@proxcloud!c1",
		"bad cluster":   "PVEAPIToken=csi@proxcloud!C1=s1",
		"ticket cookie": "PVEAuthCookie=abc",
	} {
		if rec := r.do(auth, "GET", j+"/cluster/resources?type=node", ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, rec.Code)
		}
	}
	if len(r.audits) != 0 {
		t.Errorf("unauthenticated refusals are not audited: %d", len(r.audits))
	}
	// The plugin's client (resty) puts a space after the scheme; PVE takes both forms.
	for _, auth := range []string{c1, "PVEAPIToken csi@proxcloud!c1=s1"} {
		if rec := r.do(auth, "GET", j+"/cluster/resources?type=node", ""); rec.Code != http.StatusOK {
			t.Errorf("%q: %d", auth, rec.Code)
		}
	}
}

// TestRoutes runs every allowed call once and the refusals of each.
func TestRoutes(t *testing.T) {
	cases := []struct {
		name, auth, method, target, body string
		want                             int
		check                            func(t *testing.T, r *rig, body string)
	}{
		// #1
		{name: "nodes", target: j + "/cluster/resources?type=node", want: 200,
			check: func(t *testing.T, r *rig, b string) { mustContain(t, b, `"pve2"`) }},
		{name: "stores filtered", target: j + "/cluster/resources?type=storage", want: 200,
			check: func(t *testing.T, r *rig, b string) { mustContain(t, b, `"nas"`); mustLack(t, b, "other-store") }},
		{name: "workers only", target: j + "/cluster/resources?type=vm", want: 200,
			check: func(t *testing.T, r *rig, b string) {
				mustContain(t, b, `"vmid":101`)
				for _, id := range []string{"102", "103", "104", "105", "106"} {
					mustLack(t, b, `"vmid":`+id)
				}
			}},
		{name: "resources without type", target: j + "/cluster/resources", want: 403},
		{name: "resources of pools", target: j + "/cluster/resources?type=pool", want: 403},
		{name: "resources extra param", target: j + "/cluster/resources?type=vm&full=1", want: 403},
		// #2
		{name: "storage config", target: j + "/storage/local-zfs", want: 200},
		{name: "storage config of another store", target: j + "/storage/other-store", want: 403},
		// #3
		{name: "storage status", target: j + "/nodes/pve1/storage/local-zfs/status", want: 200},
		{name: "status of another store", target: j + "/nodes/pve1/storage/other-store/status", want: 403},
		{name: "status on a node without the store", target: j + "/nodes/pve9/storage/local-zfs/status", want: 403},
		// #4
		{name: "content filtered", target: j + "/nodes/pve1/storage/local-zfs/content", want: 200,
			check: func(t *testing.T, r *rig, b string) {
				mustContain(t, b, disk1)
				mustContain(t, b, disk1b)
				mustLack(t, b, u2)
				mustLack(t, b, "vm-101-disk-0")
				mustLack(t, b, "vm-103-disk-0")
			}},
		{name: "content with a filter param", target: j + "/nodes/pve1/storage/local-zfs/content?vmid=102", want: 403},
		// #5
		{name: "create", method: "POST", target: j + "/nodes/pve1/storage/local-zfs/content?" + q("filename", "vm-9999-pvc-"+u3, "vmid", "9999", "size", "1048576"), want: 200,
			check: func(t *testing.T, r *rig, b string) {
				v, _ := r.reg.Volume(context.Background(), VolumeID("svc", "local-zfs", "vm-9999-pvc-"+u3))
				if v == nil || v.State != StateReady || v.ClusterID != "c1" || v.SizeBytes != 1<<30 || v.VolID != "local-zfs:vm-9999-pvc-"+u3 {
					t.Fatalf("row %+v", v)
				}
				if w := r.pve.writes; len(w) != 1 || !strings.Contains(w[0], "filename=vm-9999-pvc-"+u3+"&size=1048576&vmid=9999") {
					t.Fatalf("forwarded %v", w)
				}
			}},
		{name: "create in the body", method: "POST", target: j + "/nodes/pve1/storage/local-zfs/content", body: q("filename", "vm-9999-pvc-"+u3, "vmid", "9999", "size", "1G"), want: 200},
		{name: "create retried after a lost answer", method: "POST", target: j + "/nodes/pve1/storage/local-zfs/content?" + q("filename", "vm-9999-pvc-"+u1b, "vmid", "9999", "size", "1G"), want: 200,
			check: func(t *testing.T, r *rig, b string) { mustContain(t, b, disk1b); noWrites(t, r) }},
		{name: "create a name another cluster holds", method: "POST", target: j + "/nodes/pve1/storage/local-zfs/content?" + q("filename", "vm-9999-pvc-"+u2, "vmid", "9999", "size", "1G"), want: 409,
			check: func(t *testing.T, r *rig, b string) { noWrites(t, r) }},
		{name: "create on a worker vmid", method: "POST", target: j + "/nodes/pve1/storage/local-zfs/content?" + q("filename", "vm-101-pvc-"+u3, "vmid", "101", "size", "1G"), want: 403},
		{name: "create with a foreign name", method: "POST", target: j + "/nodes/pve1/storage/local-zfs/content?" + q("filename", "vm-9999-disk-0", "vmid", "9999", "size", "1G"), want: 403},
		{name: "create with format", method: "POST", target: j + "/nodes/pve1/storage/local-zfs/content?" + q("filename", "vm-9999-pvc-"+u3, "vmid", "9999", "size", "1G", "format", "qcow2"), want: 403},
		{name: "create without size", method: "POST", target: j + "/nodes/pve1/storage/local-zfs/content?" + q("filename", "vm-9999-pvc-"+u3, "vmid", "9999"), want: 403},
		{name: "create with a relative size", method: "POST", target: j + "/nodes/pve1/storage/local-zfs/content?" + q("filename", "vm-9999-pvc-"+u3, "vmid", "9999", "size", "-1"), want: 403},
		{name: "StorageClass on another store", method: "POST", target: j + "/nodes/pve1/storage/other-store/content?" + q("filename", "vm-9999-pvc-"+u3, "vmid", "9999", "size", "1G"), want: 403},
		{name: "replicate StorageClass asks for a vmid", target: j + "/cluster/nextid?vmid=10000", want: 403},
		// #6
		{name: "snapshot copy", method: "POST", target: j + "/nodes/pve1/storage/local-zfs/content/vm-9999-pvc-" + u1 + "?" + q("target", "vm-9999-snapshot-"+u3), want: 200,
			check: func(t *testing.T, r *rig, b string) {
				v, _ := r.reg.Volume(context.Background(), VolumeID("svc", "local-zfs", "vm-9999-snapshot-"+u3))
				if v == nil || v.State != StateCreating || v.SourceID != VolumeID("svc", "local-zfs", "vm-9999-pvc-"+u1) {
					t.Fatalf("row %+v", v)
				}
				upid := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(b), `{"data":"`), `"}`)
				if rec := r.do(c1, "GET", j+"/nodes/pve1/tasks/"+upid+"/status", ""); rec.Code != 200 {
					t.Fatalf("status %d", rec.Code)
				}
				if v, _ = r.reg.Volume(context.Background(), v.ID); v.State != StateReady {
					t.Fatalf("after the task: %s", v.State)
				}
			}},
		{name: "copy of another cluster's disk", method: "POST", target: j + "/nodes/pve1/storage/local-zfs/content/vm-9999-pvc-" + u2 + "?" + q("target", "vm-9999-snapshot-"+u3), want: 403},
		{name: "copy to a foreign name", method: "POST", target: j + "/nodes/pve1/storage/local-zfs/content/vm-9999-pvc-" + u1 + "?" + q("target", "vm-103-disk-1"), want: 403},
		{name: "copy onto another node's missing store", method: "POST", target: j + "/nodes/pve1/storage/local-zfs/content/vm-9999-pvc-" + u1 + "?" + q("target", "vm-9999-pvc-"+u3, "target_node", "pve9"), want: 403},
		{name: "copy from the wrong node", method: "POST", target: j + "/nodes/pve2/storage/local-zfs/content/vm-9999-pvc-" + u1 + "?" + q("target", "vm-9999-pvc-"+u3), want: 403},
		// #7
		{name: "delete", method: "DELETE", target: j + "/nodes/pve1/storage/local-zfs/content/vm-9999-pvc-" + u1b, want: 200,
			check: func(t *testing.T, r *rig, b string) {
				id := VolumeID("svc", "local-zfs", "vm-9999-pvc-"+u1b)
				if v, _ := r.reg.Volume(context.Background(), id); v.State != StateDeleting {
					t.Fatalf("before the task: %s", v.State)
				}
				upid := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(b), `{"data":"`), `"}`)
				r.do(c1, "GET", j+"/nodes/pve1/tasks/"+upid+"/status", "")
				if v, _ := r.reg.Volume(context.Background(), id); v.State != StateDeleted || v.DeletedAt == nil {
					t.Fatalf("after the task: %+v", v)
				}
			}},
		{name: "crafted volumeHandle of another cluster", method: "DELETE", target: j + "/nodes/pve1/storage/local-zfs/content/vm-9999-pvc-" + u2, want: 403},
		{name: "delete a boot disk", method: "DELETE", target: j + "/nodes/pve1/storage/local-zfs/content/vm-101-disk-0", want: 403},
		{name: "delete on the wrong node", method: "DELETE", target: j + "/nodes/pve2/storage/local-zfs/content/vm-9999-pvc-" + u1b, want: 403},
		{name: "delete with a long delay", method: "DELETE", target: j + "/nodes/pve1/storage/local-zfs/content/vm-9999-pvc-" + u1b + "?delay=99", want: 403},
		{name: "delete under another vmid directory", method: "DELETE", target: j + "/nodes/pve1/storage/local-zfs/content/9998/vm-9999-pvc-" + u1b, want: 403},
		// #8
		{name: "worker config", target: j + "/nodes/pve1/qemu/101/config?current=1", want: 200},
		{name: "forged NodeID: another cluster's worker", target: j + "/nodes/pve1/qemu/102/config", want: 403},
		{name: "server of the same pool", target: j + "/nodes/pve1/qemu/103/config", want: 403},
		{name: "tag outside the pool", target: j + "/nodes/pve2/qemu/104/config", want: 403},
		{name: "template", target: j + "/nodes/pve1/qemu/105/config", want: 403},
		{name: "container", target: j + "/nodes/pve1/qemu/106/config", want: 403},
		{name: "worker on the wrong node", target: j + "/nodes/pve2/qemu/101/config", want: 403},
		{name: "vmid with a leading zero", target: j + "/nodes/pve1/qemu/0101/config", want: 403},
		{name: "config of a snapshot", target: j + "/nodes/pve1/qemu/101/config?snapshot=s", want: 403},
		// #9
		{name: "attach", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi2", disk1b+",backup=0,iothread=1,replicate=0,ssd=1,wwn=0x5000000000000002"), want: 200},
		{name: "rewrite the disk on its slot", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi1", disk1+",backup=0,size=1G,wwn=0x5000000000000001"), want: 200},
		{name: "attach another cluster's disk", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi2", disk2), want: 403},
		{name: "attach nobody's disk", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi2", "local-zfs:vm-103-disk-0"), want: 403},
		{name: "attach to another cluster's worker", method: "POST", target: j + "/nodes/pve1/qemu/102/config?" + q("scsi2", disk1b), want: 403},
		{name: "attach onto a taken slot", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi1", disk1b), want: 403},
		{name: "attach on scsi0", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi0", disk1b), want: 403},
		{name: "attach on scsi30", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi30", disk1b), want: 403},
		{name: "attach on ide", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("ide2", disk1b), want: 403},
		{name: "attach on virtio", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("virtio1", disk1b), want: 403},
		{name: "args", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("args", "-drive file=/dev/sda"), want: 403},
		{name: "hostpci", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("hostpci0", "0000:01:00"), want: 403},
		{name: "two keys", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi2", disk1b, "scsi3", disk1b), want: 403},
		{name: "replicated drive", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi2", disk1b+",replicate=1"), want: 403},
		{name: "drive importing", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi2", disk1b+",import-from=local-zfs:vm-103-disk-0"), want: 403},
		{name: "drive with two files", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi2", disk1b+",file="+disk2), want: 403},
		{name: "drive on another store", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi2", "other-store:vm-9999-pvc-"+u1b), want: 403},
		// #10
		{name: "revert", method: "PUT", target: j + "/nodes/pve1/qemu/101/config?" + q("revert", "scsi2,scsi3"), want: 200},
		{name: "revert scsi0", method: "PUT", target: j + "/nodes/pve1/qemu/101/config?" + q("revert", "scsi0"), want: 403},
		{name: "revert with a change", method: "PUT", target: j + "/nodes/pve1/qemu/101/config?" + q("revert", "scsi2", "memory", "99999"), want: 403},
		{name: "config change", method: "PUT", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi2", disk1b), want: 403},
		// #11
		{name: "unlink", method: "PUT", target: j + "/nodes/pve1/qemu/101/unlink?idlist=scsi1", want: 200},
		{name: "unlink the boot disk", method: "PUT", target: j + "/nodes/pve1/qemu/101/unlink?idlist=scsi0", want: 403},
		{name: "unlink an empty slot", method: "PUT", target: j + "/nodes/pve1/qemu/101/unlink?idlist=scsi5", want: 403},
		{name: "unlink with force", method: "PUT", target: j + "/nodes/pve1/qemu/101/unlink?idlist=scsi1&force=1", want: 403},
		{name: "unlink two", method: "PUT", target: j + "/nodes/pve1/qemu/101/unlink?idlist=scsi1,scsi0", want: 403},
		{name: "unlink on another cluster's worker", method: "PUT", target: j + "/nodes/pve1/qemu/102/unlink?idlist=scsi1", want: 403},
		// #12
		{name: "resize", method: "PUT", target: j + "/nodes/pve1/qemu/101/resize?" + q("disk", "scsi1", "size", "2048M"), want: 200,
			check: func(t *testing.T, r *rig, b string) {
				upid := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(b), `{"data":"`), `"}`)
				r.do(c1, "GET", j+"/nodes/pve1/tasks/"+upid+"/status", "")
				if v, _ := r.reg.Volume(context.Background(), VolumeID("svc", "local-zfs", "vm-9999-pvc-"+u1)); v.SizeBytes != 2<<30 {
					t.Fatalf("size %d", v.SizeBytes)
				}
			}},
		{name: "resize the boot disk", method: "PUT", target: j + "/nodes/pve1/qemu/101/resize?" + q("disk", "scsi0", "size", "20G"), want: 403},
		{name: "resize by a delta", method: "PUT", target: j + "/nodes/pve1/qemu/101/resize?" + q("disk", "scsi1", "size", "+1G"), want: 403},
		{name: "resize on another cluster's worker", method: "PUT", target: j + "/nodes/pve1/qemu/102/resize?" + q("disk", "scsi1", "size", "2G"), want: 403},
		// #13
		{name: "task of another cluster", target: j + "/nodes/pve1/tasks/UPID:pve1:c2task:/status", want: 403},
		{name: "task from elsewhere", target: j + "/nodes/pve1/tasks/UPID:pve1:0000ABCD:00000001:6743A1B2:qmstart:101:root@pam:/status", want: 403},
		{name: "expired task", target: j + "/nodes/pve1/tasks/UPID:pve1:old:/status", want: 403},
		{name: "task log", target: j + "/nodes/pve1/tasks/UPID:pve1:c2task:/log", want: 403},
		// never
		{name: "nextid", target: j + "/cluster/nextid", want: 403},
		{name: "VM create", method: "POST", target: j + "/nodes/pve1/qemu?" + q("vmid", "10000"), want: 403},
		{name: "replication", method: "POST", target: j + "/cluster/replication?" + q("id", "101-0", "target", "pve2", "type", "local"), want: 403},
		{name: "migrate", method: "POST", target: j + "/nodes/pve1/qemu/101/migrate?target=pve2", want: 403},
		{name: "ticket", method: "POST", target: j + "/access/ticket?" + q("username", "root@pam", "password", "x"), want: 403},
		{name: "version", target: j + "/version", want: 403},
		{name: "outside the api", target: "/api2/extjs/cluster/resources?type=vm", want: 403},
		// path tricks
		{name: "encoded slash", target: j + "/nodes/pve1/storage/local-zfs/content/9999%2Fvm-9999-pvc-" + u2, method: "DELETE", want: 403},
		{name: "encoded letter", target: j + "/cluster/resource%73?type=vm", want: 403},
		{name: "double slash", target: j + "/nodes//pve1/qemu/101/config", want: 403},
		{name: "leading double slash", target: "/api2/json//cluster/resources?type=vm", want: 403},
		{name: "case", target: j + "/Cluster/Resources?type=vm", want: 403},
		{name: "trailing slash", target: j + "/cluster/resources/?type=vm", want: 403},
		{name: "trailing segment", target: j + "/nodes/pve1/qemu/101/config/x", want: 403},
		{name: "dot segments", method: "DELETE", target: j + "/nodes/pve1/storage/local-zfs/content/../../../qemu/102", want: 403},
		{name: "dot", target: j + "/nodes/pve1/./qemu/101/config", want: 403},
		{name: "repeated parameter", target: j + "/cluster/resources?type=vm&type=node", want: 403},
		{name: "parameter in query and body", method: "POST", target: j + "/nodes/pve1/qemu/101/config?" + q("scsi2", disk1b), body: q("scsi2", disk2), want: 403},
		{name: "json body", method: "POST", target: j + "/nodes/pve1/qemu/101/config", body: `{"scsi2":"x"}`, want: 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			if tc.method == "" {
				tc.method = "GET"
			}
			if tc.auth == "" {
				tc.auth = c1
			}
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			if tc.body != "" {
				ct := "application/x-www-form-urlencoded"
				if strings.HasPrefix(tc.body, "{") {
					ct = "application/json"
				}
				req.Header.Set("Content-Type", ct)
			}
			req.Header.Set("Authorization", tc.auth)
			rec := httptest.NewRecorder()
			r.h.ServeHTTP(rec, req)
			body, _ := io.ReadAll(rec.Body)
			if rec.Code != tc.want {
				t.Fatalf("%s %s: %d %s, want %d", tc.method, tc.target, rec.Code, body, tc.want)
			}
			if tc.want == 403 || tc.want == 409 {
				noWrites(t, r)
				if len(r.audits) != 1 || r.audits[0].Action != audit.ActionDeny || r.audits[0].ProjectID != "p1" {
					t.Fatalf("audit %+v", r.audits)
				}
			}
			if tc.check != nil {
				tc.check(t, r, string(body))
			}
		})
	}
}

func TestCreateRefusedByPVELeavesNoRow(t *testing.T) {
	r := newRig(t)
	r.pve.failNew = true
	rec := r.do(c1, "POST", j+"/nodes/pve1/storage/local-zfs/content?"+q("filename", "vm-9999-pvc-"+u3, "vmid", "9999", "size", "1G"), "")
	if rec.Code != 500 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if v, _ := r.reg.Volume(context.Background(), VolumeID("svc", "local-zfs", "vm-9999-pvc-"+u3)); v != nil {
		t.Fatalf("row left: %+v", v)
	}
}

func TestQuotaRefusesCreate(t *testing.T) {
	r := newRig(t)
	var used int
	r.h.Quota = func(_ context.Context, project, svc string, add int, csiUsed func() (int, error)) error {
		used, _ = csiUsed()
		if project != "p1" || add != 5 {
			t.Errorf("quota asked for %s %d", project, add)
		}
		return conflict("Quota exceeded for diskGb")
	}
	rec := r.do(c1, "POST", j+"/nodes/pve1/storage/local-zfs/content?"+q("filename", "vm-9999-pvc-"+u3, "vmid", "9999", "size", "5G"), "")
	if rec.Code != 409 || used != 2 {
		t.Fatalf("%d %s, csi used %d", rec.Code, rec.Body, used)
	}
	noWrites(t, r)
}

func TestTaskResultAppliedOnce(t *testing.T) {
	r := newRig(t)
	rec := r.do(c1, "DELETE", j+"/nodes/pve1/storage/local-zfs/content/vm-9999-pvc-"+u1b, "")
	upid := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(rec.Body.String()), `{"data":"`), `"}`)
	r.do(c1, "GET", j+"/nodes/pve1/tasks/"+upid+"/status", "")
	id := VolumeID("svc", "local-zfs", "vm-9999-pvc-"+u1b)
	_ = r.reg.UpdateVolume(context.Background(), id, pgdoc.M{"state": StateReady})
	r.do(c1, "GET", j+"/nodes/pve1/tasks/"+upid+"/status", "")
	if v, _ := r.reg.Volume(context.Background(), id); v.State != StateReady {
		t.Fatalf("a second poll applied the result again: %s", v.State)
	}
	if rec := r.do(c2, "GET", j+"/nodes/pve1/tasks/"+upid+"/status", ""); rec.Code != 403 {
		t.Fatalf("another cluster polled it: %d", rec.Code)
	}
}

func TestSizeAndNames(t *testing.T) {
	for in, want := range map[string]int64{"1048576": 1 << 30, "1024M": 1 << 30, "2G": 2 << 30, "1T": 1 << 40, "4K": 4 << 10} {
		if got, ok := sizeBytes(in); !ok || got != want {
			t.Errorf("%s: %d %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "0", "+1G", "1.5G", "1P", "-1", "1g", "99999999999999999999"} {
		if _, ok := sizeBytes(in); ok {
			t.Errorf("%s accepted", in)
		}
	}
	for disk, want := range map[string]bool{"vm-9999-pvc-x": true, "9999/vm-9999-pvc-x.raw": true, "9998/vm-9999-pvc-x.raw": false, "9999/a/b": false, "9999/": false} {
		if _, ok := baseName(9999, disk); ok != want {
			t.Errorf("%s: %v", disk, ok)
		}
	}
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Fatalf("missing %q in %s", sub, s)
	}
}

func mustLack(t *testing.T, s, sub string) {
	t.Helper()
	if strings.Contains(s, sub) {
		t.Fatalf("%q leaked in %s", sub, s)
	}
}

func noWrites(t *testing.T, r *rig) {
	t.Helper()
	if n := r.pve.writeCount(); n != 0 {
		t.Fatalf("PVE got %d writes: %v", n, r.pve.writes)
	}
}
