package project

// cloud_shares.go = the file share product on a TrueNAS provider: SHARE rows, cloud create/delete/actions, quota.
// The NAS sees clients by address, so an export lists the project's own addresses (shareNetworks). An address
// leaving the project goes off the exports BEFORE it can be reallocated to another tenant (withShareAccessRevoked).
// A provider's snatSources are added by truenas.Service on every export and are no tenant's to release.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/providers"
	"github.com/mutex-solutions/proxcloud/internal/cloud/truenas"
	"github.com/mutex-solutions/proxcloud/internal/platform/externalservice"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

func shareErr(err error) error {
	if errors.Is(err, truenas.ErrShare) {
		return httpx.NewError(http.StatusConflict, http.StatusConflict, strings.TrimPrefix(err.Error(), truenas.ErrShare.Error()+": "))
	}
	return httpx.NewError(http.StatusBadGateway, http.StatusBadGateway, err.Error())
}

// shareNetworks lists what a project's clients look like to the NAS: its public addresses and its subnets.
func (h *Handler) shareNetworks(ctx context.Context, p *Project) ([]string, error) {
	seen := map[string]bool{}
	for _, svcID := range p.ServiceIDs() {
		es, err := h.esSvc.Get(ctx, svcID)
		if errors.Is(err, externalservice.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !es.IsProxmox() {
			continue
		}
		if h.publicIPs != nil {
			ips, err := h.publicIPs.ForProject(ctx, svcID, p.ID)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if ip.IsPublic() || strings.HasPrefix(ip.ServerID, "k8s:") { // sharedWorkers: not on any project network
					seen[ip.IP+"/32"] = true
				}
			}
		}
	}
	rows, err := h.cloud.FindByProjectAndType(ctx, p.ID, cloud.TypeNetwork)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		for _, c := range networkCIDRs(&rows[i]) {
			seen[c] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// networkCIDRs lists every subnet of a NETWORK row.
func networkCIDRs(cr *cloud.CloudResource) []string {
	net, _ := cr.Data["network"].(map[string]any)
	var out []string
	if c, _ := net["cidr"].(string); c != "" {
		out = append(out, c)
	}
	if subs, ok := net["subnets"].([]any); ok {
		for _, v := range subs {
			if c, _ := v.(string); c != "" {
				out = append(out, c)
			}
		}
	}
	return out
}

// shareAccessLockMaxFor bounds a crashed holder. The longest holder is a share create (SMB user + a permission job).
const shareAccessLockMaxFor = 5 * time.Minute

// lockShareAccess serialises what reads the project's addresses onto an export against what takes one away: without
// it a refresh running during a release writes the address straight back. Busy for 10 s = 409.
func (h *Handler) lockShareAccess(ctx context.Context, projectID string) (func(), error) {
	if h.sdnLock == nil {
		return func() {}, nil
	}
	name := "share-access-" + projectID
	for range 20 {
		now := time.Now().UTC()
		ok, err := h.sdnLock.Lock(ctx, name, shareAccessLockMaxFor, now)
		if err != nil {
			return nil, err
		}
		if ok {
			return func() { _ = h.sdnLock.Unlock(context.WithoutCancel(ctx), name, 0, now, time.Now().UTC()) }, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "Another change to this project's file share access is in progress — retry in a moment")
}

// trueNASServices lists the project's file share providers.
func (h *Handler) trueNASServices(ctx context.Context, p *Project) ([]*externalservice.ExternalService, error) {
	var out []*externalservice.ExternalService
	for _, svcID := range p.ServiceIDs() {
		es, err := h.esSvc.Get(ctx, svcID)
		if errors.Is(err, externalservice.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if es.IsTrueNAS() {
			out = append(out, es)
		}
	}
	return out, nil
}

// reconcileShareExports brings every export of the project to its current addresses minus drop. Any failure is an
// error and the caller must not free the address. The caller holds the lock.
func (h *Handler) reconcileShareExports(ctx context.Context, p *Project, services []*externalservice.ExternalService, drop []string) error {
	networks, err := h.shareNetworks(ctx, p)
	if err != nil {
		return err
	}
	networks = slices.DeleteFunc(networks, func(n string) bool { return slices.Contains(drop, n) })
	for _, es := range services {
		if err := func() error {
			ts, err := es.TrueNASService()
			if err != nil {
				return err
			}
			defer ts.C.Close()
			list, err := ts.List(ctx, p.ID)
			if err != nil {
				return err
			}
			for i := range list {
				if !list[i].MapsRoot && ts.SameExport(list[i].Networks, networks) {
					continue
				}
				if err := ts.SetAccess(ctx, p.ID, list[i].Dataset, networks); err != nil {
					return err
				}
			}
			return nil
		}(); err != nil {
			return err
		}
	}
	return nil
}

// withShareAccessRevoked takes drop off every export of the project, then runs release, the step that lets those
// addresses go to another tenant. Failing closed: if an export cannot be updated, release never runs and the address
// stays held. The sync retries the release of a destroyed VM's address, and a detach or network delete can be re-sent.
func (h *Handler) withShareAccessRevoked(ctx context.Context, projectID string, drop []string, release func() error) error {
	p, err := h.svc.GetProjectByID(ctx, projectID)
	var he *httpx.HTTPError
	if errors.As(err, &he) && he.Status == http.StatusNotFound {
		return release() // no project left to hold a share
	}
	if err != nil {
		return err
	}
	services, err := h.trueNASServices(ctx, p)
	if err != nil {
		return err
	}
	if len(services) == 0 {
		return release()
	}
	unlock, err := h.lockShareAccess(ctx, projectID)
	if err != nil {
		return err
	}
	defer unlock()
	if err := h.reconcileShareExports(ctx, p, services, drop); err != nil {
		return fmt.Errorf("take the address off the file share exports: %w", err)
	}
	return release()
}

// ReconcileShareAccess is the sync's backstop: exports drifted by hand, or left stale by a version before the release
// guard, come back to the project's addresses. A busy lock skips this project until the next cycle.
func (h *Handler) ReconcileShareAccess(ctx context.Context, projectID string) error {
	p, err := h.svc.GetProjectByID(ctx, projectID)
	if err != nil {
		return err
	}
	services, err := h.trueNASServices(ctx, p)
	if err != nil || len(services) == 0 {
		return err
	}
	unlock, err := h.lockShareAccess(ctx, projectID)
	if err != nil {
		return err
	}
	defer unlock()
	return h.reconcileShareExports(ctx, p, services, nil)
}

// shareQuotaCheck enforces the provider's per-project limit on allocated space, over the cached rows.
func (h *Handler) shareQuotaCheck(ctx context.Context, p *Project, es *externalservice.ExternalService, exceptID string, addGiB int) error {
	limit := es.DefaultQuotaGiB()
	if limit <= 0 {
		return nil
	}
	rows, err := h.cloud.FindByProjectAndType(ctx, p.ID, cloud.TypeShare)
	if err != nil {
		return err
	}
	used := 0
	for i := range rows {
		if rows[i].ServiceID != es.ID || rows[i].ExternalID == exceptID {
			continue
		}
		if sh, ok := rows[i].Data["share"].(map[string]any); ok {
			used += intAny(sh["sizeBytes"]) >> 30
		}
	}
	if used+addGiB > limit {
		return httpx.NewError(http.StatusConflict, http.StatusConflict,
			fmt.Sprintf("Quota exceeded for file shares: %d GB allocated, %d GB requested, limit %d GB", used, addGiB, limit))
	}
	return nil
}

func (h *Handler) shareCreate(w http.ResponseWriter, r *http.Request, u *user.User, proj *Project, es *externalservice.ExternalService, req providers.CreateRequest) {
	ctx := r.Context()
	if req.Type != cloud.TypeShare {
		h.fail(w, httpx.BadRequest("This provider serves file shares only"))
		return
	}
	name, _ := req.Data["name"].(string)
	protocol, _ := req.Data["protocol"].(string)
	sizeGiB := intAny(req.Data["sizeGb"])
	if err := h.shareQuotaCheck(ctx, proj, es, "", sizeGiB); err != nil {
		h.fail(w, err)
		return
	}
	unlock, err := h.lockShareAccess(ctx, proj.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	defer unlock()
	networks, err := h.shareNetworks(ctx, proj)
	if err != nil {
		h.fail(w, err)
		return
	}
	if len(networks) == 0 {
		h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, "Create a network or attach a public IP first: the share is limited to this project's addresses"))
		return
	}
	ts, err := es.TrueNASService()
	if err != nil {
		h.fail(w, err)
		return
	}
	defer ts.C.Close()
	sh, err := ts.Create(ctx, proj.ID, name, protocol, sizeGiB, networks)
	if err != nil {
		h.fail(w, shareErr(err))
		return
	}
	now := time.Now().UTC()
	saved, err := h.cloud.Insert(ctx, &cloud.CloudResource{
		ServiceID: es.ID, ProjectID: proj.ID, Region: es.CephRegion(), Type: cloud.TypeShare,
		ExternalID: sh.Dataset, Data: ts.ShareData(proj.ID, sh), CreatedAt: &now, UpdatedAt: &now,
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	h.cloudResourceAudit(u, proj, "CLOUD_RESOURCE_CREATE", "", saved)
	out := *saved
	if sh.Password != "" {
		// Shown once: the row never holds it.
		data := map[string]any{"share": map[string]any{}}
		for k, v := range saved.Data["share"].(map[string]any) {
			data["share"].(map[string]any)[k] = v
		}
		data["share"].(map[string]any)["password"] = sh.Password
		out.Data = data
	}
	httpx.OK(w, out)
}

func (h *Handler) shareDelete(ctx context.Context, es *externalservice.ExternalService, proj *Project, cr *cloud.CloudResource) error {
	ts, err := es.TrueNASService()
	if err != nil {
		return err
	}
	defer ts.C.Close()
	if err := ts.Delete(ctx, proj.ID, cr.ExternalID); err != nil {
		return shareErr(err)
	}
	return nil
}

// shareAction: EXTEND {sizeGb} sets the allocated size (either way, above usage), REFRESH_ACCESS re-reads the
// project's addresses onto the export, RESET_PASSWORD issues a new SMB password.
func (h *Handler) shareAction(w http.ResponseWriter, r *http.Request, proj *Project, es *externalservice.ExternalService, cr *cloud.CloudResource, action string, data map[string]any) bool {
	ctx := r.Context()
	if action != "EXTEND" && action != "REFRESH_ACCESS" && action != "RESET_PASSWORD" {
		return false
	}
	ts, err := es.TrueNASService()
	if err != nil {
		h.fail(w, err)
		return true
	}
	defer ts.C.Close()
	var password string
	switch action {
	case "EXTEND":
		size := intAny(data["sizeGb"])
		if err = h.shareQuotaCheck(ctx, proj, es, cr.ExternalID, size); err != nil {
			h.fail(w, err)
			return true
		}
		err = ts.Resize(ctx, proj.ID, cr.ExternalID, size)
	case "REFRESH_ACCESS":
		var unlock func()
		if unlock, err = h.lockShareAccess(ctx, proj.ID); err != nil {
			h.fail(w, err)
			return true
		}
		defer unlock()
		var networks []string
		if networks, err = h.shareNetworks(ctx, proj); err != nil {
			h.fail(w, err)
			return true
		}
		err = ts.SetAccess(ctx, proj.ID, cr.ExternalID, networks)
	case "RESET_PASSWORD":
		password, err = ts.ResetPassword(ctx, proj.ID, cr.ExternalID)
	}
	if err != nil {
		h.fail(w, shareErr(err))
		return true
	}
	if list, err := ts.List(ctx, proj.ID); err == nil {
		for i := range list {
			if list[i].Dataset == cr.ExternalID {
				cr.Data = ts.ShareData(proj.ID, &list[i])
				if saved, err := h.cloud.Insert(ctx, cr); err == nil {
					*cr = *saved
				}
			}
		}
	}
	httpx.OK(w, map[string]any{"result": map[string]any{"resource": cr, "password": password}})
	return true
}
