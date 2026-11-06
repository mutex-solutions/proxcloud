package truenas

// share.go = the file share product: one dataset with a refquota plus one NFS or SMB share per tenant share.
// The dataset id is <parent>/<projectId>-<name>; the proxcloud:project property is what proves ownership.
//
// Access is per project: the export (NFS `networks`, SMB `hostsallow`) lists the project's addresses the caller
// passes in. An empty list would mean "everyone" to TrueNAS, so it becomes localhost.
//
// Service account: SHARING_ADMIN covers datasets and shares (lab-checked on 25.10.8, including
// pool.dataset.create). Creating the per-share SMB user also needs ACCOUNT_WRITE.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
)

const (
	ProtocolNFS = "NFS"
	ProtocolSMB = "SMB"

	projectProp = "proxcloud:project"
	protoProp   = "proxcloud:protocol"
)

// ErrShare is a request the NAS or the rules refuse; the message is safe to show.
var ErrShare = errors.New("share")

func sharef(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrShare, fmt.Sprintf(format, a...))
}

var shareNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$`)

// Service is the share surface of one NAS.
type Service struct {
	C      *Client
	Parent string // parent dataset, e.g. tank/shares
	Host   string // the address clients mount from
}

// Share is one tenant share. Password is set only by create and password reset.
type Share struct {
	Name, Protocol, Dataset string
	SizeBytes, UsedBytes    int64
	Networks                []string
	ShareID                 int
	Username, Password      string
}

func (s *Service) datasetFor(projectID, name string) string {
	return s.Parent + "/" + projectID + "-" + name
}

func mountPath(dataset string) string { return "/mnt/" + dataset }

// smbName is the share's SMB name: unique per NAS, and free of the characters SMB dislikes.
func smbName(projectID, name string) string {
	return "pc" + projectID[max(0, len(projectID)-8):] + "-" + name
}

func smbUser(projectID, name string) string {
	u := "pcs" + projectID[max(0, len(projectID)-8):] + "-" + name
	return u[:min(len(u), 32)]
}

func exportList(networks []string) []string {
	if len(networks) == 0 {
		return []string{"127.0.0.1/32"}
	}
	return networks
}

type dsValue struct {
	Parsed  any    `json:"parsed"`
	Raw     string `json:"rawvalue"`
	Source  string `json:"source"`
	Display string `json:"value"`
}

func (v dsValue) bytes() int64 {
	var n int64
	_, _ = fmt.Sscan(v.Raw, &n)
	return n
}

type dataset struct {
	ID        string             `json:"id"`
	RefQuota  dsValue            `json:"refquota"`
	Used      dsValue            `json:"used"`
	Available dsValue            `json:"available"`
	Props     map[string]dsValue `json:"user_properties"`
}

type nfsRow struct {
	ID       int      `json:"id"`
	Path     string   `json:"path"`
	Networks []string `json:"networks"`
}

type smbRow struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	Options struct {
		HostsAllow []string `json:"hostsallow"`
	} `json:"options"`
}

// Create makes the dataset, the share and, for SMB, a user with a generated password.
func (s *Service) Create(ctx context.Context, projectID, name, protocol string, sizeGiB int, networks []string) (*Share, error) {
	if !shareNameRe.MatchString(name) {
		return nil, sharef("name must be 3-32 lowercase letters, digits and hyphens, not starting or ending with a hyphen")
	}
	if protocol != ProtocolNFS && protocol != ProtocolSMB {
		return nil, sharef("protocol must be NFS or SMB")
	}
	if sizeGiB <= 0 {
		return nil, sharef("size must be at least 1 GB")
	}
	size := int64(sizeGiB) << 30
	var parent []dataset
	if err := s.C.Call(ctx, "pool.dataset.query", []any{[][]any{{"id", "=", s.Parent}}, map[string]any{"select": []string{"id", "available"}}}, &parent); err != nil {
		return nil, err
	}
	if len(parent) == 0 {
		return nil, fmt.Errorf("truenas: parent dataset %s not found", s.Parent)
	}
	if size > parent[0].Available.bytes() {
		return nil, sharef("the NAS has only %s free", parent[0].Available.Display)
	}
	id := s.datasetFor(projectID, name)
	err := s.C.Call(ctx, "pool.dataset.create", []any{map[string]any{
		"name": id, "refquota": size, "share_type": "GENERIC",
		"user_properties": []map[string]string{{"key": projectProp, "value": projectID}, {"key": protoProp, "value": protocol}},
	}}, nil)
	if err != nil {
		var re *RPCError
		if errors.As(err, &re) && strings.Contains(re.Error(), "already exists") {
			return nil, sharef("a share named %s already exists", name)
		}
		return nil, err
	}
	sh := &Share{Name: name, Protocol: protocol, Dataset: id, SizeBytes: size, Networks: exportList(networks)}
	if err := s.export(ctx, projectID, sh); err != nil {
		// A dataset nobody can reach is only a bill: remove what was made.
		_ = s.Delete(ctx, projectID, id)
		return nil, err
	}
	return sh, nil
}

func (s *Service) export(ctx context.Context, projectID string, sh *Share) error {
	path := mountPath(sh.Dataset)
	if sh.Protocol == ProtocolNFS {
		// Root on the client stays root: the tenant owns the whole dataset, and the export is limited to its addresses.
		var row nfsRow
		if err := s.C.Call(ctx, "sharing.nfs.create", []any{map[string]any{
			"path": path, "networks": sh.Networks, "maproot_user": "root", "maproot_group": "wheel",
		}}, &row); err != nil {
			return err
		}
		sh.ShareID = row.ID
		return nil
	}
	password, err := newPassword()
	if err != nil {
		return err
	}
	user := smbUser(projectID, sh.Name)
	var uid struct {
		ID  int `json:"id"`
		UID int `json:"uid"`
	}
	if err := s.C.Call(ctx, "user.create", []any{map[string]any{
		"username": user, "full_name": "ProxCloud share " + sh.Name, "group_create": true,
		"password": password, "smb": true, "home": "/var/empty", "shell": "/usr/sbin/nologin",
	}}, &uid); err != nil {
		return err
	}
	var job int
	if err := s.C.Call(ctx, "filesystem.setperm", []any{map[string]any{"path": path, "uid": uid.UID, "mode": "770"}}, &job); err != nil {
		return err
	}
	if err := s.waitJob(ctx, job); err != nil {
		return err
	}
	var row smbRow
	if err := s.C.Call(ctx, "sharing.smb.create", []any{map[string]any{
		"name": smbName(projectID, sh.Name), "path": path, "purpose": "DEFAULT_SHARE",
		"options": map[string]any{"hostsallow": sh.Networks},
	}}, &row); err != nil {
		return err
	}
	sh.ShareID, sh.Username, sh.Password = row.ID, user, password
	return nil
}

func newPassword() (string, error) {
	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func (s *Service) waitJob(ctx context.Context, id int) error {
	for range 60 {
		var jobs []struct {
			State string `json:"state"`
			Error string `json:"error"`
		}
		if err := s.C.Call(ctx, "core.get_jobs", []any{[][]any{{"id", "=", id}}}, &jobs); err != nil {
			return err
		}
		if len(jobs) == 1 {
			switch jobs[0].State {
			case "SUCCESS":
				return nil
			case "FAILED", "ABORTED":
				return fmt.Errorf("truenas: job %d: %s", id, jobs[0].Error)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("truenas: job %d still running", id)
}

// List returns the project's shares with their usage. An error is an error, never "no shares".
func (s *Service) List(ctx context.Context, projectID string) ([]Share, error) {
	var rows []dataset
	prefix := s.Parent + "/" + projectID + "-"
	if err := s.C.Call(ctx, "pool.dataset.query", []any{[][]any{{"id", "^", prefix}}, map[string]any{
		"select": []string{"id", "refquota", "used", "user_properties"},
	}}, &rows); err != nil {
		return nil, err
	}
	out := []Share{}
	if len(rows) == 0 {
		return out, nil
	}
	var nfs []nfsRow
	if err := s.C.Call(ctx, "sharing.nfs.query", []any{}, &nfs); err != nil {
		return nil, err
	}
	var smb []smbRow
	if err := s.C.Call(ctx, "sharing.smb.query", []any{}, &smb); err != nil {
		return nil, err
	}
	for _, d := range rows {
		if d.Props[projectProp].Raw != projectID {
			continue
		}
		sh := Share{
			Name: strings.TrimPrefix(d.ID, prefix), Dataset: d.ID, Protocol: d.Props[protoProp].Raw,
			SizeBytes: d.RefQuota.bytes(), UsedBytes: d.Used.bytes(),
		}
		for _, n := range nfs {
			if n.Path == mountPath(d.ID) {
				sh.ShareID, sh.Networks = n.ID, n.Networks
			}
		}
		for _, n := range smb {
			if n.Path == mountPath(d.ID) {
				sh.ShareID, sh.Networks = n.ID, n.Options.HostsAllow
				sh.Username = smbUser(projectID, sh.Name)
			}
		}
		out = append(out, sh)
	}
	return out, nil
}

// get reads one share, refusing a dataset this project does not own.
func (s *Service) get(ctx context.Context, projectID, id string) (*Share, error) {
	list, err := s.List(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Dataset == id {
			return &list[i], nil
		}
	}
	return nil, sharef("share not found")
}

// Resize sets the refquota. ZFS would accept a quota below usage and freeze the dataset, so that is refused here.
func (s *Service) Resize(ctx context.Context, projectID, id string, sizeGiB int) error {
	sh, err := s.get(ctx, projectID, id)
	if err != nil {
		return err
	}
	size := int64(sizeGiB) << 30
	if sizeGiB <= 0 || size < sh.UsedBytes {
		return sharef("the share uses %d bytes; choose a larger size", sh.UsedBytes)
	}
	return s.C.Call(ctx, "pool.dataset.update", []any{id, map[string]any{"refquota": size}}, nil)
}

// SetAccess replaces the addresses allowed on the share's export.
func (s *Service) SetAccess(ctx context.Context, projectID, id string, networks []string) error {
	sh, err := s.get(ctx, projectID, id)
	if err != nil {
		return err
	}
	if sh.ShareID == 0 {
		return sharef("the share has no export")
	}
	if sh.Protocol == ProtocolNFS {
		return s.C.Call(ctx, "sharing.nfs.update", []any{sh.ShareID, map[string]any{"networks": exportList(networks)}}, nil)
	}
	return s.C.Call(ctx, "sharing.smb.update", []any{sh.ShareID, map[string]any{"purpose": "DEFAULT_SHARE", "options": map[string]any{"hostsallow": exportList(networks)}}}, nil)
}

// ResetPassword issues a new password for an SMB share's user.
func (s *Service) ResetPassword(ctx context.Context, projectID, id string) (string, error) {
	sh, err := s.get(ctx, projectID, id)
	if err != nil {
		return "", err
	}
	if sh.Protocol != ProtocolSMB {
		return "", sharef("only SMB shares have a password")
	}
	userID, err := s.userID(ctx, sh.Username)
	if err != nil {
		return "", err
	}
	password, err := newPassword()
	if err != nil {
		return "", err
	}
	if err := s.C.Call(ctx, "user.update", []any{userID, map[string]any{"password": password}}, nil); err != nil {
		return "", err
	}
	return password, nil
}

func (s *Service) userID(ctx context.Context, username string) (int, error) {
	var users []struct {
		ID int `json:"id"`
	}
	if err := s.C.Call(ctx, "user.query", []any{[][]any{{"username", "=", username}}, map[string]any{"select": []string{"id"}}}, &users); err != nil {
		return 0, err
	}
	if len(users) == 0 {
		return 0, nil
	}
	return users[0].ID, nil
}

// Delete removes the share, its SMB user and the dataset with its data. A share already gone is a no-op.
func (s *Service) Delete(ctx context.Context, projectID, id string) error {
	if !strings.HasPrefix(id, s.Parent+"/"+projectID+"-") {
		return sharef("share not found")
	}
	sh, err := s.get(ctx, projectID, id)
	if errors.Is(err, ErrShare) {
		return nil
	}
	if err != nil {
		return err
	}
	if sh.ShareID != 0 {
		method := "sharing.nfs.delete"
		if sh.Protocol == ProtocolSMB {
			method = "sharing.smb.delete"
		}
		if err := s.C.Call(ctx, method, []any{sh.ShareID}, nil); err != nil {
			return err
		}
	}
	if sh.Protocol == ProtocolSMB {
		userID, err := s.userID(ctx, smbUser(projectID, sh.Name))
		if err != nil {
			return err
		}
		if userID != 0 {
			if err := s.C.Call(ctx, "user.delete", []any{userID, map[string]any{"delete_group": true}}, nil); err != nil {
				return err
			}
		}
	}
	return s.C.Call(ctx, "pool.dataset.delete", []any{id, map[string]any{"recursive": true, "force": true}}, nil)
}

// ShareData is the cache row payload.
func (s *Service) ShareData(projectID string, sh *Share) map[string]any {
	mount := fmt.Sprintf("mount -t nfs %s:%s /mnt/%s", s.Host, mountPath(sh.Dataset), sh.Name)
	if sh.Protocol == ProtocolSMB {
		mount = fmt.Sprintf(`mount -t cifs //%s/%s /mnt/%s -o username=%s`, s.Host, smbName(projectID, sh.Name), sh.Name, sh.Username)
	}
	return map[string]any{"share": map[string]any{
		"name": sh.Name, "protocol": sh.Protocol, "dataset": sh.Dataset, "status": "available",
		"sizeBytes": sh.SizeBytes, "usedBytes": sh.UsedBytes, "networks": sh.Networks,
		"username": sh.Username, "server": s.Host, "mount": mount,
	}}
}

// --- sync ---

// ShareSyncProvider lists a project's shares for the periodic sync (providers.Provider + ProjectScoped).
type ShareSyncProvider struct {
	svc               *Service
	region, projectID string
}

func (s *Service) ShareSyncProvider(region, projectID string) *ShareSyncProvider {
	return &ShareSyncProvider{svc: s, region: region, projectID: projectID}
}

func (p *ShareSyncProvider) Type() string          { return cloud.TypeShare }
func (p *ShareSyncProvider) ProjectID() string     { return p.projectID }
func (p *ShareSyncProvider) CompareKeys() []string { return []string{} }

func (p *ShareSyncProvider) List(ctx context.Context) ([]cloud.CloudResource, error) {
	list, err := p.svc.List(ctx, p.projectID)
	if err != nil {
		return nil, err
	}
	out := make([]cloud.CloudResource, 0, len(list))
	for i := range list {
		out = append(out, cloud.CloudResource{
			ExternalID: list[i].Dataset, ProjectID: p.projectID, Region: p.region, Type: cloud.TypeShare,
			Data: p.svc.ShareData(p.projectID, &list[i]),
		})
	}
	return out, nil
}
