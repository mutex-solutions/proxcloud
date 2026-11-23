package project

// cloud_lb_cluster_tls.go — the certificates of a Kubernetes cluster's Services: the Secrets its Services name in
// proxcloud.io/lb-tls-secret (internal/cloud/lbgw/tls.go has the three annotations and what crosses the gateway).
//
// The cluster's controller sends the hash of each Secret a Service names. A hash this portal holds no certificate
// for is asked for in the answer, and the controller sends that Secret once. What arrives is checked exactly as a
// certificate a tenant uploads (pve.CheckLBCertificate: the key fits the certificate, RSA of 2048 bits or more or
// ECDSA P-256 / P-384, a chain of five at most that signs in order, 16 KiB, no passphrase) and stored as a row of
// lb_certificates, sealed with the portal's key. The row belongs to the cluster's balancer and to no project
// (lbClusterCertOwner): its blob opens for that cluster and that row alone, no route of a project lists, names or
// deletes it, and it goes with the cluster. The cluster's state holds the hashes; nothing but the sealed blob and an
// appliance's certificate file holds a key, and no log line, audit event, Event or error text holds any of it.
//
// From the row on a certificate travels as one of the project's does: a file named by its chain's SHA-256, written
// before the config that names it, the standby first, checked and rolled back with the config (pve/lbtls.go). A
// Secret that changed is another hash: a new row, a new file, a new config line, one push; the file of before goes
// from the appliances after the new config was seen to be served, and its row when no state names it any more.
//
// A Service is never served without the TLS it asks for. While a Secret cannot be used (gone, of another type,
// refused by the check) or its certificate has not arrived yet, a Service the balancer serves stays exactly as the
// cluster's last accepted state had it, with the certificate of before, and one it never served gets no address.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/lbgw"
	"github.com/mutex-solutions/proxcloud/internal/cloud/pve"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
	"github.com/mutex-solutions/proxcloud/internal/platform/audit"
)

const (
	// lbClusterCertRows is how many certificate rows a cluster may leave behind before the ones no Service names
	// go at once, whatever their age.
	lbClusterCertRows = 4 * lbgw.MaxClusterCerts
	// lbClusterCertKeep is how long a row no state names is kept: a pass that started before the row's state was
	// stored must not take it for a leftover.
	lbClusterCertKeep = 5 * time.Minute
	lbClusterNoKeyMsg = "The portal takes no TLS certificate: it has no encryption key to protect one with. Ask your operator"
)

// lbClusterCertOwner is what a cluster's certificate rows carry where a project's carry the project id. No project
// has such an id, so no project's list, listener or teardown reaches them, and the blob is sealed to it.
func lbClusterCertOwner(clusterID string) string { return "lbc:" + clusterID }

// lbClusterCertID is the id of the row of the Secret content hash in cluster clusterID: the same content in two
// clusters is two rows, each sealed to its own.
func lbClusterCertID(clusterID, hash string) string {
	sum := sha256.Sum256([]byte(clusterID + "\x00" + hash))
	return hex.EncodeToString(sum[:])
}

// lbClusterCertRowsOf reads the cluster's certificate rows by the hash of the Secret each came from. Without a store
// there are none.
func (h *Handler) lbClusterCertRowsOf(ctx context.Context, clusterID string) (map[string]*lbCertificate, error) {
	out := map[string]*lbCertificate{}
	if h.lbCerts == nil {
		return out, nil
	}
	var rows []lbCertificate
	if err := h.lbCerts.Find(ctx, pgdoc.M{"projectId": lbClusterCertOwner(clusterID)}, &rows); err != nil {
		return nil, err
	}
	for i := range rows {
		out[rows[i].SecretHash] = &rows[i]
	}
	return out, nil
}

// lbClusterTLS is the certificate part of a controller's request (LBClusterState). It takes the certificates that
// were asked for, and answers: the Services to store, the hashes every row of which must stay (lbgw.State.CertHashes),
// what to answer for the Services whose TLS is not as the request has it (by UID), and the hashes to ask for.
//
// A request without a Service that asks for TLS reads nothing and changes nothing here.
func (h *Handler) lbClusterTLS(ctx context.Context, cred *lbgw.Credential, services []lbgw.Service, uploads []lbgw.CertificateData) (store []lbgw.Service, keep []string, held map[string]lbgw.ServiceStatus, need []string, err error) {
	if !slices.ContainsFunc(services, func(s lbgw.Service) bool { return lbgw.WantsTLS(&s) }) {
		return services, nil, nil, nil, nil
	}
	available := h.lbCerts != nil && h.lbCertKeys != nil
	rows, err := h.lbClusterCertRowsOf(ctx, cred.ID)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	named := map[string]string{} // hash -> the first Secret that holds it, for people
	for _, s := range services {
		for _, c := range s.Certificates {
			if _, ok := named[c.SHA256]; c.SHA256 != "" && !ok {
				named[c.SHA256] = s.Namespace + "/" + c.Secret
			}
		}
	}
	if available {
		if err := h.lbClusterTakeCerts(ctx, cred, rows, named, uploads); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	var last *lbgw.State // the state as it was accepted last, read when a Service has to stay as it was
	was := func(uid string) (*lbgw.Service, error) {
		if last == nil {
			if last, err = h.lbGW.State(ctx, cred.ID); err != nil {
				return nil, err
			}
			if last == nil {
				last = &lbgw.State{}
			}
		}
		if i := slices.IndexFunc(last.Services, func(s lbgw.Service) bool { return s.UID == uid }); i >= 0 {
			return &last.Services[i], nil
		}
		return nil, nil
	}
	held = map[string]lbgw.ServiceStatus{}
	now := time.Now()
	for _, s := range services {
		if !lbgw.WantsTLS(&s) {
			store = append(store, s)
			continue
		}
		reason, problem, waiting := lbgw.ReasonInvalidCertificate, lbgw.CertProblem(&s), false
		if problem == "" && !available {
			reason, problem = lbgw.ReasonTLSNotAvailable, lbClusterNoKeyMsg
		}
		var events []lbgw.Event
		if problem == "" {
			for _, c := range s.Certificates {
				row := rows[c.SHA256]
				switch {
				case row == nil:
					waiting = true
					if !slices.Contains(need, c.SHA256) {
						need = append(need, c.SHA256)
					}
				case row.Refused != "":
					problem = "Secret " + s.Namespace + "/" + c.Secret + ": " + row.Refused
				case now.After(row.NotAfter):
					events = append(events, lbgw.Event{Type: "Warning", Reason: lbgw.ReasonCertificateExpired,
						Message: "The certificate of Secret " + s.Namespace + "/" + c.Secret + " expired on " + row.NotAfter.Format("2 January 2006") + ": clients will refuse it"})
				}
			}
		}
		if problem == "" && !waiting {
			store = append(store, s)
			if len(events) > 0 {
				held[s.UID] = lbgw.ServiceStatus{Events: events}
			}
			continue
		}
		prev, err := was(s.UID)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		switch {
		case prev != nil && problem != "":
			store = append(store, *prev)
			held[s.UID] = lbgw.ServiceStatus{Events: []lbgw.Event{{Type: "Warning", Reason: reason,
				Message: problem + ". The load balancer keeps serving the Service as it did before this change, with the certificate it had"}}}
		case prev != nil: // its certificate is on the way
			store = append(store, *prev)
		case problem != "":
			held[s.UID] = lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateRefused, Reason: reason,
				Message: problem + ". The Service gets no address without its TLS certificate"}
		default:
			held[s.UID] = lbgw.ServiceStatus{UID: s.UID, State: lbgw.StateBuilding, Message: "The portal is taking the Service's TLS certificate"}
		}
	}
	for hash := range named {
		keep = append(keep, hash)
	}
	for _, s := range store {
		for _, c := range s.Certificates {
			if c.SHA256 != "" && !slices.Contains(keep, c.SHA256) {
				keep = append(keep, c.SHA256)
			}
		}
	}
	slices.Sort(keep)
	slices.Sort(need)
	if !available {
		need = nil
	}
	return store, keep, held, need, nil
}

// lbClusterTakeCerts checks and stores the certificates of a request. One is taken only when a Service of this very
// request names its hash and the cluster has no row for it: the portal computes the hash itself, so a controller
// cannot file a certificate under another's. A certificate the check refuses is remembered by its hash with the
// reason and without its content, so it is not asked for again while the Secret stays as it is.
func (h *Handler) lbClusterTakeCerts(ctx context.Context, cred *lbgw.Credential, rows map[string]*lbCertificate, named map[string]string, uploads []lbgw.CertificateData) error {
	owner := lbClusterCertOwner(cred.ID)
	for _, up := range uploads {
		hash := lbgw.SecretHash([]byte(up.Certificate), []byte(up.PrivateKey))
		secret, asked := named[hash]
		if !asked || rows[hash] != nil {
			continue
		}
		if len(rows) >= lbClusterCertRows {
			// Rows nothing names any more, of Secrets that changed faster than the passes cleared up after them.
			for have, row := range rows {
				if _, ok := named[have]; !ok {
					if _, err := h.lbCerts.DeleteOne(ctx, pgdoc.M{"_id": row.ID, "projectId": owner}); err != nil {
						return err
					}
					delete(rows, have)
				}
			}
		}
		now := time.Now().UTC()
		row := &lbCertificate{ID: lbClusterCertID(cred.ID, hash), ProjectID: owner, Name: secret, CreatedAt: &now, SecretHash: hash}
		info, bundle, _, refused := pve.CheckLBCertificate(up.Certificate, up.PrivateKey, now)
		if refused != "" {
			row.Refused = refused
		} else {
			keyID, blob, err := h.lbCertKeys.Seal([]byte(bundle), lbCertAAD(owner, row.ID))
			if err != nil {
				return errors.New("a certificate of the cluster could not be sealed")
			}
			row.LBCertificate, row.KeyID, row.Sealed = info, keyID, base64.StdEncoding.EncodeToString(blob)
		}
		if err := h.lbCerts.Upsert(ctx, row.ID, row); err != nil {
			return err
		}
		rows[hash] = row
		h.lbClusterCertAudit(cred, row)
	}
	return nil
}

// lbClusterCertAudit records a certificate taken from a cluster's Secret, or refused: the Secret's name and what
// the certificate says in public. Nothing of the key, and not the hash of the Secret's content.
func (h *Handler) lbClusterCertAudit(cred *lbgw.Credential, row *lbCertificate) {
	if h.audit == nil {
		return
	}
	ev := audit.SystemEvent()
	ev.RequestInterface, ev.EventContext = audit.InterfaceLBGateway, audit.ContextProject
	ev.Actor = &audit.AuditActor{Type: audit.ActorSystem, ID: lbgw.TokenID(cred.ID), DisplayName: "load balancer controller " + cred.ID}
	ev.Action, ev.Outcome = audit.ActionCreate, audit.OutcomeSuccess
	ev.ResourceType, ev.ResourceID, ev.ProjectID = audit.ResourceLoadBalancer, cred.ID, cred.ProjectID
	ev.ResourceMetadata = map[string]any{"field": "lbCertificate", "clusterId": cred.ID, "secret": row.Name, "certificateId": row.ID,
		"fingerprint": row.Fingerprint, "subject": row.Subject, "notAfter": row.NotAfter}
	if row.Refused != "" {
		ev.Action, ev.Outcome, ev.ErrorMessage = audit.ActionDeny, audit.OutcomeFailure, "certificate refused: "+row.Refused
		ev.ResourceMetadata = map[string]any{"field": "lbCertificate", "clusterId": cred.ID, "secret": row.Name}
	}
	h.audit.LogAsync(ev)
}

// lbClusterCertNames writes the certificates of the TLS listeners of Service w into them: the rows' ids, which the
// apply opens (lbResolveCerts), and their files, which the config names. A hash without a usable row is left out; a
// listener left with none gets no frontend (pve.renderHAProxy), never one without TLS.
func lbClusterCertNames(clusterID string, w lbgw.Service, rows map[string]*lbCertificate) (ids, files []string) {
	for _, c := range w.Certificates {
		row := rows[c.SHA256]
		if row == nil || row.Refused != "" || row.File == "" || slices.Contains(files, row.File) {
			continue
		}
		ids, files = append(ids, lbClusterCertID(clusterID, c.SHA256)), append(files, row.File)
	}
	return ids, files
}

// lbClusterPruneCerts deletes the cluster's certificate rows no state names any more, once they are lbClusterCertKeep
// old. It runs after a pass that left the balancer settled, under the cluster's converge lock: no apply reads a row
// then. An error is the next pass's to try again.
func (h *Handler) lbClusterPruneCerts(ctx context.Context, clusterID string) error {
	if h.lbCerts == nil {
		return nil
	}
	rows, err := h.lbClusterCertRowsOf(ctx, clusterID)
	if err != nil || len(rows) == 0 {
		return err
	}
	// The state as it is now, not as the pass read it: a request may have named another certificate since.
	st, err := h.lbGW.State(ctx, clusterID)
	if err != nil {
		return err
	}
	for hash, row := range rows {
		if st != nil && slices.Contains(st.CertHashes, hash) {
			continue
		}
		if row.CreatedAt != nil && time.Since(*row.CreatedAt) < lbClusterCertKeep {
			continue
		}
		if _, err := h.lbCerts.DeleteOne(ctx, pgdoc.M{"_id": row.ID, "projectId": lbClusterCertOwner(clusterID)}); err != nil {
			return err
		}
	}
	return nil
}
