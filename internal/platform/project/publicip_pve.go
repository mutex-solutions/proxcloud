package project

// publicip_pve.go — public-IP IPAM for Proxmox public NICs. Each provider public
// network carries an IPv4 pool (config.network.publicNetworks[].cidr/gateway/
// ranges/exclude); attaching a public NIC takes one free address from it, the
// VM's ipfilter pins the NIC to that /32, and detach / server delete / project
// teardown give it back.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

// publicNIC is the NIC a public network plugs as (net0 sits on a project vnet).
const publicNIC = "net1"

// PublicIP is one allocated address (collection pvePublicIp). ServerID is the
// VM's cloud-resource ExternalID (the vmid). Rows on nic net0 are the fixed tenant
// addresses of vnets without DHCP (NetworkID = the vnet).
type PublicIP struct {
	ID        string `json:"id"`
	ServiceID string `json:"serviceId"`
	NetworkID string `json:"networkId"`
	IP        string `json:"ip"`
	Prefix    int    `json:"prefix"`
	ProjectID string `json:"projectId"`
	ServerID  string `json:"serverId"`
	NIC       string `json:"nic"`
	// BeforeRow: taken while its VM was still being built (a load balancer's), so a row younger than this
	// allocation says nothing about the vmid being reused.
	BeforeRow bool       `json:"beforeRow,omitempty"`
	CreatedAt *time.Time `json:"createdAt,omitempty"`
}

// PublicIPRepo persists the allocations. _id = serviceId/networkId/ip, so the
// primary key is the race guard: two attaches that pick the same free address
// collide on insert and the loser picks again.
type PublicIPRepo struct {
	col *pgdoc.Store
	// guard wraps every release of a public or cluster address: it may refuse to let the address go (file share exports).
	guard func(ctx context.Context, projectID string, rows []PublicIP, release func() error) error
}

func NewPublicIPRepo(db *pgdoc.DB) *PublicIPRepo { return &PublicIPRepo{col: db.C("pvePublicIp")} }

// SetPublicIPs wires the public-IP store (nil = public NICs cannot attach).
func (h *Handler) SetPublicIPs(r *PublicIPRepo) {
	h.publicIPs = r
	if r != nil {
		r.guard = h.guardAddressRelease
	}
}

func (r *PublicIPRepo) find(ctx context.Context, filter pgdoc.M) ([]PublicIP, error) {
	var out []PublicIP
	err := r.col.Find(ctx, filter, &out)
	return out, err
}

// ForServer lists the server's allocations on one service.
func (r *PublicIPRepo) ForServer(ctx context.Context, serviceID, serverID string) ([]PublicIP, error) {
	return r.find(ctx, pgdoc.M{"serviceId": serviceID, "serverId": serverID})
}

// InNetwork lists the allocations on one network (a vnet or a public network).
func (r *PublicIPRepo) InNetwork(ctx context.Context, serviceID, networkID string) ([]PublicIP, error) {
	return r.find(ctx, pgdoc.M{"serviceId": serviceID, "networkId": networkID})
}

// ForProject lists the project's allocations on one service, net0 included.
func (r *PublicIPRepo) ForProject(ctx context.Context, serviceID, projectID string) ([]PublicIP, error) {
	return r.find(ctx, pgdoc.M{"serviceId": serviceID, "projectId": projectID})
}

// IsPublic: a public NIC's address, the billed kind. A net0 row is an address on the project's own
// vnet (zones without DHCP) and costs nothing.
func (p PublicIP) IsPublic() bool { return p.NIC == publicNIC }

// CountForProject is the floatingIps quota usage: public NICs only, not the net0 addresses
// allocated on the project's own vnets.
func (r *PublicIPRepo) CountForProject(ctx context.Context, serviceID, projectID string) (int64, error) {
	return r.col.Count(ctx, pgdoc.M{"serviceId": serviceID, "projectId": projectID, "nic": publicNIC})
}

// ReleaseNIC frees the address the server holds on one NIC.
func (r *PublicIPRepo) ReleaseNIC(ctx context.Context, serviceID, serverID, nic string) error {
	return r.release(ctx, pgdoc.M{"serviceId": serviceID, "serverId": serverID, "nic": nic})
}

// ReleaseServer frees every address the server holds on the service.
func (r *PublicIPRepo) ReleaseServer(ctx context.Context, serviceID, serverID string) error {
	return r.release(ctx, pgdoc.M{"serviceId": serviceID, "serverId": serverID})
}

// purgeClusterRows drops the cluster addresses left on a vnet being deleted. The caller holds the share-access guard:
// going through release here would wait on that same lock.
func (r *PublicIPRepo) purgeClusterRows(ctx context.Context, serviceID, networkID string) error {
	_, err := r.col.DeleteMany(ctx, pgdoc.M{"serviceId": serviceID, "networkId": networkID, "nic": ReservedNIC})
	return err
}

// ReleaseProject frees every address the project holds on the service (teardown).
func (r *PublicIPRepo) ReleaseProject(ctx context.Context, serviceID, projectID string) error {
	return r.release(ctx, pgdoc.M{"serviceId": serviceID, "projectId": projectID})
}

// release frees the matching rows, the one way an address gets back to the pool. A public or cluster address goes
// through the guard of each project holding one first; a failed guard keeps the row, so the address cannot be
// reallocated. A net0 row has nothing to guard: the export lists the subnet, which network delete guards. Whether a
// cluster address can be on an export takes the project's providers to tell, so the guard decides that.
func (r *PublicIPRepo) release(ctx context.Context, filter pgdoc.M) error {
	del := func() error {
		_, err := r.col.DeleteMany(ctx, filter)
		return err
	}
	if r.guard == nil {
		return del()
	}
	rows, err := r.find(ctx, filter)
	if err != nil {
		return err
	}
	guarded := map[string][]PublicIP{}
	for _, row := range rows {
		if row.IsPublic() || row.NIC == ReservedNIC {
			guarded[row.ProjectID] = append(guarded[row.ProjectID], row)
		}
	}
	for projectID, held := range guarded {
		inner := del
		del = func() error { return r.guard(ctx, projectID, held, inner) }
	}
	return del()
}

// Allocate takes a free address from pn's pool for the server's nic.
func (r *PublicIPRepo) Allocate(ctx context.Context, serviceID, projectID, serverID, nic string, pn externalservice.PVEPublicNetwork) (*PublicIP, error) {
	return r.allocate(ctx, serviceID, projectID, serverID, nic, pn, false)
}

// AllocateBeforeRow is Allocate for a public NIC plugged during its VM's build.
func (r *PublicIPRepo) AllocateBeforeRow(ctx context.Context, serviceID, projectID, serverID, nic string, pn externalservice.PVEPublicNetwork) (*PublicIP, error) {
	return r.allocate(ctx, serviceID, projectID, serverID, nic, pn, true)
}

func (r *PublicIPRepo) allocate(ctx context.Context, serviceID, projectID, serverID, nic string, pn externalservice.PVEPublicNetwork, beforeRow bool) (*PublicIP, error) {
	used := func() ([]string, error) {
		rows, err := r.find(ctx, pgdoc.M{"serviceId": serviceID, "networkId": pn.ID})
		ips := make([]string, 0, len(rows))
		for _, row := range rows {
			ips = append(ips, row.IP)
		}
		return ips, err
	}
	now := time.Now().UTC()
	insert := func(ip string, prefix int) (*PublicIP, error) {
		a := &PublicIP{
			ID: serviceID + "/" + pn.ID + "/" + ip, ServiceID: serviceID, NetworkID: pn.ID,
			IP: ip, Prefix: prefix, ProjectID: projectID, ServerID: serverID, NIC: nic, BeforeRow: beforeRow, CreatedAt: &now,
		}
		_, err := r.col.InsertOne(ctx, a)
		return a, err
	}
	return allocatePublicIP(pn, used, insert)
}

// Reserve holds n free addresses of pn's pool for owner in one go, taken from the top of the range so
// servers, which take the lowest free one, reach them last. A range address, unlike pickPublicIP's
// result, is not tied to a NIC. Release is ReleaseServer(serviceID, owner).
func (r *PublicIPRepo) Reserve(ctx context.Context, serviceID, projectID, owner string, pn externalservice.PVEPublicNetwork, n int) ([]string, error) {
	rows, err := r.find(ctx, pgdoc.M{"serviceId": serviceID, "networkId": pn.ID})
	if err != nil {
		return nil, err
	}
	taken := make([]string, 0, len(rows))
	for _, row := range rows {
		taken = append(taken, row.IP)
	}
	free, prefix, herr := freeAddresses(pn, taken, n)
	if herr != nil {
		return nil, herr
	}
	now := time.Now().UTC()
	for _, ip := range free {
		a := &PublicIP{
			ID: serviceID + "/" + pn.ID + "/" + ip, ServiceID: serviceID, NetworkID: pn.ID,
			IP: ip, Prefix: prefix, ProjectID: projectID, ServerID: owner, NIC: ReservedNIC, CreatedAt: &now,
		}
		if _, err := r.col.InsertOne(ctx, a); err != nil {
			_ = r.ReleaseServer(context.WithoutCancel(ctx), serviceID, owner)
			if pgdoc.IsDup(err) {
				return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "Another address change is in progress — retry in a moment")
			}
			return nil, err
		}
	}
	return free, nil
}

// ReservedNIC marks an address held for a cluster: no NIC of any server, so no VM can orphan it.
const ReservedNIC = "reserved"

// freeAddresses returns the n highest free addresses of pn's pool, highest first.
func freeAddresses(pn externalservice.PVEPublicNetwork, used []string, n int) ([]string, int, *httpx.HTTPError) {
	// ponytail: repeated lowest-free picks then keep the top n, O(range*n) on a /24 at most.
	var all []string
	prefix := 0
	taken := append([]string(nil), used...)
	for {
		ip, p, herr := pickPublicIP(pn, taken)
		if herr != nil {
			break
		}
		all, prefix = append(all, ip), p
		taken = append(taken, ip)
	}
	if len(all) < n {
		return nil, 0, httpx.NewError(http.StatusConflict, http.StatusConflict,
			fmt.Sprintf("Network %s has %d free addresses, a cluster needs %d", pn.ID, len(all), n))
	}
	out := make([]string, 0, n)
	for i := len(all) - 1; i >= len(all)-n; i-- {
		out = append(out, all[i])
	}
	return out, prefix, nil
}

// publicIPAllocAttempts bounds the pick-insert loop under insert collisions.
const publicIPAllocAttempts = 5

// allocatePublicIP picks the first free address and inserts it; a duplicate-key
// insert means a concurrent attach took it first, so re-read and pick again.
func allocatePublicIP(pn externalservice.PVEPublicNetwork, used func() ([]string, error), insert func(ip string, prefix int) (*PublicIP, error)) (*PublicIP, error) {
	for i := 0; i < publicIPAllocAttempts; i++ {
		taken, err := used()
		if err != nil {
			return nil, err
		}
		ip, prefix, herr := pickPublicIP(pn, taken)
		if herr != nil {
			return nil, herr
		}
		a, err := insert(ip, prefix)
		if err == nil {
			return a, nil
		}
		if !pgdoc.IsDup(err) {
			return nil, err
		}
	}
	return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "Another public IP change is in progress — retry in a moment")
}

// pickPublicIP returns the lowest free address of pn's pool and the pool prefix:
// the ranges (or the whole CIDR) minus network/broadcast, gateway, exclude and
// used. IPv4 only.
func pickPublicIP(pn externalservice.PVEPublicNetwork, used []string) (string, int, *httpx.HTTPError) {
	if pn.CIDR == "" {
		return "", 0, httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable,
			fmt.Sprintf("Public network %s has no IP pool configured", pn.ID))
	}
	invalid := httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable,
		fmt.Sprintf("Public network %s has an invalid IP pool", pn.ID))
	_, pool, err := net.ParseCIDR(pn.CIDR)
	if err != nil || pool.IP.To4() == nil {
		return "", 0, invalid
	}
	ones, _ := pool.Mask.Size()
	if ones > 30 {
		return "", 0, invalid
	}
	u32 := func(ip net.IP) uint32 {
		b := ip.To4()
		return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	}
	first := u32(pool.IP)
	last := first | ^u32(net.IP(pool.Mask))
	skip := map[uint32]bool{first: true, last: true}
	for _, s := range append(append([]string{pn.Gateway}, pn.Exclude...), used...) {
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
			skip[u32(ip)] = true
		}
	}
	spans := [][2]uint32{{first, last}}
	if len(pn.Ranges) > 0 {
		spans = spans[:0]
		for _, rg := range pn.Ranges {
			lo, hi, _ := strings.Cut(rg, "-")
			if hi == "" {
				hi = lo
			}
			a, b := net.ParseIP(strings.TrimSpace(lo)), net.ParseIP(strings.TrimSpace(hi))
			if a == nil || b == nil || a.To4() == nil || b.To4() == nil || !pool.Contains(a) || !pool.Contains(b) || u32(a) > u32(b) {
				return "", 0, invalid
			}
			spans = append(spans, [2]uint32{u32(a), u32(b)})
		}
	}
	for _, sp := range spans {
		for n := sp[0]; ; n++ {
			if !skip[n] {
				return net.IPv4(byte(n>>24), byte(n>>16), byte(n>>8), byte(n)).String(), ones, nil
			}
			if n == sp[1] {
				break
			}
		}
	}
	return "", 0, httpx.NewError(http.StatusConflict, http.StatusConflict,
		fmt.Sprintf("Public network %s has no free IP address left", pn.ID))
}
