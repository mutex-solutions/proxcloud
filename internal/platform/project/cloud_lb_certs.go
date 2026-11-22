package project

// cloud_lb_certs.go — the TLS certificates of a project, which its load balancers' tls and https listeners present:
//
//	GET    /api/v1/project/{id}/certificates           list (what each certificate says, and the balancers using it)
//	POST   /api/v1/project/{id}/certificates           upload {name, certificate, privateKey}: PEM chain and PEM key
//	GET    /api/v1/project/{id}/certificates/{certId}  one certificate
//	DELETE /api/v1/project/{id}/certificates/{certId}  409 while a listener names it
//
// A certificate does not change after its upload: to replace one, upload the new one, name it on the listeners and
// delete the old one. The appliances then get a new file and a new config line in one checked push (pve/lbtls.go).
//
// The chain and the key are stored as one blob sealed with the portal's encryption key (textcrypt.BlobKeys, AES-GCM),
// bound to the project and the certificate's id: a blob copied to another row does not open. Without a key of the
// portal nothing is stored and nothing is served: the upload answers 503. No route returns the key, and no log line
// or audit event holds it; the only place it is written to is the certificate directory of an appliance.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
	"github.com/mutex-solutions/proxcloud/internal/platform/rbac"
	"github.com/mutex-solutions/proxcloud/internal/platform/user"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
	"github.com/mutex-solutions/proxcloud/pkg/textcrypt"
)

const (
	// lbMaxProjectCerts is how many certificates a project holds.
	lbMaxProjectCerts = 50
	lbCertNoKeyMsg    = "TLS certificates are not available: the portal has no encryption key to protect them with. Ask the operator to set an encryption key of 32 characters or more"
)

// SetLBCertificates wires the lb_certificates collection and the key its blobs are sealed with (keys nil: no
// certificate is taken or served).
func (h *Handler) SetLBCertificates(s *pgdoc.Store, keys *textcrypt.BlobKeys) {
	h.lbCerts, h.lbCertKeys = s, keys
}

// lbCertificate is a row of lb_certificates. Sealed is never part of an answer (lbCertView).
type lbCertificate struct {
	ID        string     `json:"id"`
	ProjectID string     `json:"projectId"`
	Name      string     `json:"name"`
	CreatedAt *time.Time `json:"createdAt"`
	CreatedBy string     `json:"createdBy,omitempty"`
	pve.LBCertificate
	KeyID  string `json:"keyId"`  // the portal key the blob is sealed with (public)
	Sealed string `json:"sealed"` // base64 of the sealed chain and key
}

// lbCertView is a certificate as the API answers it: everything but the blob.
type lbCertView struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedAt *time.Time `json:"createdAt"`
	pve.LBCertificate
	// UsedBy are the load balancers with a listener that names the certificate.
	UsedBy   []string `json:"usedBy"`
	Warnings []string `json:"warnings,omitempty"`
}

func lbCertAAD(projectID, id string) []byte {
	return []byte("lb-certificate\x00" + projectID + "\x00" + id)
}

// lbCertOpen is the certificate's file for an appliance. It fails closed: without the key, or for a blob that is not
// this project's and this id's, there is no file.
func (h *Handler) lbCertOpen(c *lbCertificate) (pve.LBCertFile, error) {
	blob, err := base64.StdEncoding.DecodeString(c.Sealed)
	if err != nil {
		return pve.LBCertFile{}, textcrypt.ErrBlob
	}
	plain, err := h.lbCertKeys.Open(c.KeyID, blob, lbCertAAD(c.ProjectID, c.ID))
	if err != nil {
		return pve.LBCertFile{}, err
	}
	return pve.LBCertFile{Name: c.File, PEM: string(plain)}, nil
}

// lbCertsUsed maps a certificate id to the names of the project's balancers with a listener that names it.
func (h *Handler) lbCertsUsed(ctx context.Context, projectID string) (map[string][]string, error) {
	rows, err := h.cloud.FindByProjectAndType(ctx, projectID, cloud.TypeLoadBalancer)
	if err != nil {
		return nil, err
	}
	used := map[string][]string{}
	for _, lb := range lbsOfRows(rows) {
		for _, l := range lb.Listeners {
			for _, id := range l.Certificates {
				if !slices.Contains(used[id], lb.Name) {
					used[id] = append(used[id], lb.Name)
				}
			}
		}
	}
	return used, nil
}

// lbResolveCerts writes each tls and https listener's certificate files into the spec, from the project's
// certificates, and answers them. open false leaves the files' content out (a check of a request needs the names
// alone). why is a text for the customer when a listener names a certificate the project does not hold.
func (h *Handler) lbResolveCerts(ctx context.Context, projectID string, spec *pve.LBSpec, open bool) (files []pve.LBCertFile, why string, err error) {
	if !spec.HasTLS() {
		return nil, "", nil
	}
	if h.lbCerts == nil || h.lbCertKeys == nil {
		return nil, lbCertNoKeyMsg, nil
	}
	// The spec changes only when every certificate was found: a pass that fails leaves the row as it was.
	seen := map[string]*lbCertificate{}
	names := make([][]string, len(spec.Listeners))
	for i, l := range spec.Listeners {
		for _, id := range l.Certificates {
			c := seen[id]
			if c == nil {
				c = &lbCertificate{}
				found, err := h.lbCerts.FindOne(ctx, pgdoc.M{"_id": id, "projectId": projectID}, c)
				if err != nil {
					return nil, "", err
				}
				if !found {
					return nil, fmt.Sprintf("Listener on port %d names a certificate that is not a certificate of this project", l.Port), nil
				}
				seen[id] = c
				if open {
					f, err := h.lbCertOpen(c)
					if err != nil {
						return nil, "", fmt.Errorf("certificate %s cannot be opened: %w", id, err)
					}
					if !slices.ContainsFunc(files, func(have pve.LBCertFile) bool { return have.Name == f.Name }) {
						files = append(files, f)
					}
				}
			}
			names[i] = append(names[i], c.File)
		}
	}
	for i := range spec.Listeners {
		spec.Listeners[i].CertFiles = names[i]
	}
	return files, "", nil
}

// lbApplyCerts is lbResolveCerts for an apply. ok false: a certificate is missing or cannot be opened, the row says
// so, and the appliances keep the config they serve.
func (h *Handler) lbApplyCerts(ctx context.Context, proj *Project, lb *loadBalancer) (files []pve.LBCertFile, ok bool) {
	files, why, err := h.lbResolveCerts(ctx, proj.ID, &lb.LBSpec, true)
	switch {
	case err != nil:
		slog.Warn("load balancer: certificates", "project", proj.ID, "lb", lb.ID, "err", err)
		why = "A certificate of this load balancer cannot be read"
	case why == "":
		return files, true
	}
	lb.StatusMessage = why + ". The load balancer keeps the configuration it serves until its listeners name certificates of the project"
	if lb.AppliedHash == "" {
		lb.Status = lbStatusError
	}
	return nil, false
}

func (h *Handler) lbCertProject(w http.ResponseWriter, r *http.Request, perm string) (*Project, *user.User, bool) {
	u, ok := h.principal(w, r)
	if !ok {
		return nil, nil, false
	}
	p, ok := h.project(w, r, u, perm)
	if !ok {
		return nil, nil, false
	}
	if h.lbCerts == nil {
		httpx.Err(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable, lbCertNoKeyMsg)
		return nil, nil, false
	}
	return p, u, true
}

// lbCertAudit records a certificate's upload or removal by name and fingerprint, which are public.
func (h *Handler) lbCertAudit(u *user.User, p *Project, action string, c *lbCertificate) {
	ev := audit.ClientUserEvent(u.Sub, u.FullName())
	ev.EventContext = audit.ContextProject
	ev.Action = action
	ev.ResourceType = audit.ResourceProject
	ev.ResourceID = p.ID
	ev.ResourceDisplayName = p.Name
	ev.OrganizationID = p.OrganizationID
	ev.ProjectID = p.ID
	ev.ResourceMetadata = map[string]any{"field": "lbCertificate", "certificateId": c.ID, "name": c.Name, "fingerprint": c.Fingerprint}
	ev.Outcome = audit.OutcomeSuccess
	h.audit.LogAsync(ev)
}

func (h *Handler) lbCertList(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.lbCertProject(w, r, rbac.ProjectCloudResourceRead)
	if !ok {
		return
	}
	var certs []lbCertificate
	if err := h.lbCerts.Find(r.Context(), pgdoc.M{"projectId": p.ID}, &certs, pgdoc.Sort(pgdoc.DescK("createdAt", pgdoc.KTime))); err != nil {
		h.fail(w, err)
		return
	}
	used, err := h.lbCertsUsed(r.Context(), p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]lbCertView, len(certs))
	for i, c := range certs {
		out[i] = lbCertViewOf(&c, used[c.ID])
	}
	httpx.OK(w, out)
}

func lbCertViewOf(c *lbCertificate, usedBy []string) lbCertView {
	if usedBy == nil {
		usedBy = []string{}
	}
	return lbCertView{ID: c.ID, Name: c.Name, CreatedAt: c.CreatedAt, LBCertificate: c.LBCertificate, UsedBy: usedBy}
}

func (h *Handler) lbCertGet(w http.ResponseWriter, r *http.Request) {
	p, _, ok := h.lbCertProject(w, r, rbac.ProjectCloudResourceRead)
	if !ok {
		return
	}
	var c lbCertificate
	found, err := h.lbCerts.FindOne(r.Context(), pgdoc.M{"_id": chi.URLParam(r, "certId"), "projectId": p.ID}, &c)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !found {
		h.fail(w, httpx.NotFound("Certificate not found"))
		return
	}
	used, err := h.lbCertsUsed(r.Context(), p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpx.OK(w, lbCertViewOf(&c, used[c.ID]))
}

func (h *Handler) lbCertCreate(w http.ResponseWriter, r *http.Request) {
	p, u, ok := h.lbCertProject(w, r, rbac.ProjectCloudResourceManage)
	if !ok {
		return
	}
	var req struct {
		Name        string `json:"name"`
		Certificate string `json:"certificate"`
		PrivateKey  string `json:"privateKey"`
	}
	// A body that is not JSON may hold a key: its decode error is not passed on.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*pve.LBMaxCertBundle)).Decode(&req); err != nil {
		h.fail(w, httpx.BadRequest("Send a name, the certificate chain and the private key as JSON text fields"))
		return
	}
	c, warnings, err := h.lbCertStore(r.Context(), p.ID, u.Sub, req.Name, req.Certificate, req.PrivateKey)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.lbCertAudit(u, p, audit.ActionCreate, c)
	view := lbCertViewOf(c, nil)
	view.Warnings = warnings
	httpx.OK(w, view)
}

// lbCertStore checks an upload and stores it sealed. Its errors are for the customer and hold nothing of the key.
func (h *Handler) lbCertStore(ctx context.Context, projectID, sub, name, chainPEM, keyPEM string) (*lbCertificate, []string, error) {
	noKey := httpx.NewError(http.StatusServiceUnavailable, http.StatusServiceUnavailable, lbCertNoKeyMsg)
	if h.lbCerts == nil || h.lbCertKeys == nil {
		return nil, nil, noKey
	}
	if name = strings.TrimSpace(name); !pve.LBNameOK(name) {
		return nil, nil, httpx.BadRequest("Certificate names " + pve.LBNameRule)
	}
	info, bundle, warnings, refused := pve.CheckLBCertificate(chainPEM, keyPEM, time.Now())
	if refused != "" {
		return nil, nil, httpx.BadRequest(refused)
	}
	if n, err := h.lbCerts.Count(ctx, pgdoc.M{"projectId": projectID}); err != nil {
		return nil, nil, err
	} else if n >= lbMaxProjectCerts {
		return nil, nil, httpx.NewError(http.StatusConflict, http.StatusConflict, fmt.Sprintf("A project holds at most %d certificates. Delete one that no listener uses", lbMaxProjectCerts))
	}
	if taken, err := h.lbCerts.Exists(ctx, pgdoc.M{"projectId": projectID, "name": name}); err != nil {
		return nil, nil, err
	} else if taken {
		return nil, nil, httpx.NewError(http.StatusConflict, http.StatusConflict, "This project already has a certificate named "+name)
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	c := &lbCertificate{ID: hex.EncodeToString(raw), ProjectID: projectID, Name: name, CreatedAt: &now, CreatedBy: sub, LBCertificate: info}
	keyID, blob, err := h.lbCertKeys.Seal([]byte(bundle), lbCertAAD(projectID, c.ID))
	if errors.Is(err, textcrypt.ErrBlobKey) {
		return nil, nil, noKey
	} else if err != nil {
		return nil, nil, errors.New("the certificate could not be sealed")
	}
	c.KeyID, c.Sealed = keyID, base64.StdEncoding.EncodeToString(blob)
	if _, err := h.lbCerts.InsertOne(ctx, c); err != nil {
		return nil, nil, err
	}
	return c, warnings, nil
}

func (h *Handler) lbCertDelete(w http.ResponseWriter, r *http.Request) {
	p, u, ok := h.lbCertProject(w, r, rbac.ProjectCloudResourceManage)
	if !ok {
		return
	}
	ctx, id := r.Context(), chi.URLParam(r, "certId")
	var c lbCertificate
	found, err := h.lbCerts.FindOne(ctx, pgdoc.M{"_id": id, "projectId": p.ID}, &c)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !found {
		h.fail(w, httpx.NotFound("Certificate not found"))
		return
	}
	// ponytail: a listener given the certificate between this read and the delete keeps the config it serves, and
	// its row says which certificate is missing (lbApplyCerts). One lock over both, when that is seen to happen.
	used, err := h.lbCertsUsed(ctx, p.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if lbs := used[id]; len(lbs) > 0 {
		h.fail(w, httpx.NewError(http.StatusConflict, http.StatusConflict, "This certificate is used by a listener of load balancer "+strings.Join(lbs, ", ")+". Take it off the listener first"))
		return
	}
	if _, err := h.lbCerts.DeleteOne(ctx, pgdoc.M{"_id": id, "projectId": p.ID}); err != nil {
		h.fail(w, err)
		return
	}
	h.lbCertAudit(u, p, audit.ActionDelete, &c)
	httpx.OK(w, map[string]any{})
}
