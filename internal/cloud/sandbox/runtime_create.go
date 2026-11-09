package sandbox

// CREATE-only consumer fencing. Resume/fork and producers deliberately do not
// inherit this authority. Physical deletion never releases a runtime image pin.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/registrypins"
)

// RuntimeCreateSource is server-resolved candidate metadata, not proof or a
// caller-selectable bypass. Spec excludes it from public JSON decoding.
type RuntimeCreateSource struct {
	RequestedAlias, TemplateID, BuildID, Generation, CandidateHash string
}

const (
	annotationRuntimeOperation   = "proxcloud.io/runtime-operation"
	annotationRuntimeNonce       = "proxcloud.io/runtime-nonce"
	annotationRuntimeMode        = "proxcloud.io/runtime-mode"
	annotationRuntimeImage       = "proxcloud.io/runtime-image"
	annotationRuntimeFingerprint = "proxcloud.io/runtime-spec-sha256"
)

// NewSandboxID reserves a server identity before the standalone durable owner
// commit. CreateSandbox accepts it only with an exact opaque runtime proof.
func NewSandboxID() string { return newSandboxID() }

// CloneCreateSpec freezes mutable caller maps/pointers before validation, hashing
// and the owner CAS. It preserves opaque proof identity rather than copying its
// implementation or serializing credentials.
func CloneCreateSpec(in Spec) Spec {
	out := in
	clone := func(v, dst any) { b, _ := json.Marshal(v); _ = json.Unmarshal(b, dst) }
	if in.Env != nil {
		out.Env = nil
		clone(in.Env, &out.Env)
	}
	if in.Metadata != nil {
		out.Metadata = nil
		clone(in.Metadata, &out.Metadata)
	}
	if in.GPU != nil {
		v := *in.GPU
		out.GPU = &v
	}
	if in.Network != nil {
		out.Network = nil
		clone(in.Network, &out.Network)
	}
	if in.AllowPublicTraffic != nil {
		v := *in.AllowPublicTraffic
		out.AllowPublicTraffic = &v
	}
	if in.Custom != nil {
		out.Custom = nil
		clone(in.Custom, &out.Custom)
	}
	if in.Quota != nil {
		v := *in.Quota
		out.Quota = &v
	}
	if in.RuntimeSource != nil {
		v := *in.RuntimeSource
		out.RuntimeSource = &v
	}
	return out
}

// RuntimeCreateImage classifies the effective source using trusted provider
// configuration, never a request flag. Only operator-curated library/external
// images are outside the managed t/<project>/<template> GC namespace. Every
// same-host non-library spelling (including tags, URLs and foreign projects)
// fails closed rather than falling back to an unprotected builtin.
func RuntimeCreateImage(cfg Config, spec Spec) (string, bool, error) {
	t, ok := spec.template(cfg.WithDefaults())
	if !ok || t.Image == "" {
		return "", false, registrypins.ErrScope
	}
	image := t.Image
	reg := strings.TrimRight(cfg.TemplateRegistry, "/")
	if reg == "" {
		if spec.Custom != nil {
			return "", false, registrypins.ErrScope
		}
		return image, false, nil
	}
	host, path, ok := strings.Cut(reg, "/")
	canonical, err := registrypins.CanonicalHost(host)
	if err != nil || !ok || path != "t" || canonical != host {
		return "", false, registrypins.ErrScope
	}
	first, _, _ := strings.Cut(image, "/")
	got, hostErr := registrypins.CanonicalHost(first)
	internal := hostErr == nil && got == canonical
	// A malformed URL or host spelling must not disguise an internal managed ref.
	if !internal && strings.Contains(image, host) {
		return "", false, registrypins.ErrScope
	}
	if !internal {
		if spec.Custom != nil {
			return "", false, registrypins.ErrScope
		}
		return image, false, nil
	}
	if spec.Custom == nil && first == host && strings.HasPrefix(image, host+"/library/") && !strings.ContainsAny(image, " \\?#\t\r\n") {
		return image, false, nil
	}
	r, err := registrypins.ParseRef(host, image)
	if err != nil || r.Project != spec.ProjectID || r.Digest == "" || r.Tag != "" || r.String() != image {
		return "", false, registrypins.ErrScope
	}
	return image, true, nil
}

// RuntimeCreateSpec is fingerprint version 1. Both platform owner and low-level
// guard use this one encoder on a validated/defaulted frozen spec. Explicit fields
// include ID, project, Custom, Quota and source metadata omitted by Spec JSON.
// Environment values participate only in this aggregate hash; no raw spec or
// environment is persisted. Server-minted envd/traffic tokens are excluded and
// minted only after Consume. The provider connection is hashed, never stored.
func RuntimeCreateSpec(cfg Config, service string, in Spec) (registrypins.RuntimeSpec, error) {
	spec := CloneCreateSpec(in)
	cfg = cfg.WithDefaults()
	if err := spec.Validate(cfg); err != nil {
		return registrypins.RuntimeSpec{}, err
	}
	image, _, err := RuntimeCreateImage(cfg, spec)
	if err != nil || !ValidID(spec.ID) {
		return registrypins.RuntimeSpec{}, registrypins.ErrScope
	}
	t, _ := spec.template(cfg)
	mbps := cfg.EgressMbps
	if spec.Custom == nil && t.EgressMbps > 0 {
		mbps = t.EgressMbps
	}
	connection := sha256.Sum256([]byte(cfg.Kubeconfig))
	b, err := json.Marshal(struct {
		Version                                                        int
		Service, ID, Project                                           string
		Spec                                                           Spec
		Custom                                                         *Template
		Quota                                                          *ProjectQuota
		Source                                                         *RuntimeCreateSource
		Effective                                                      Template
		RuntimeClass, Region, Domain, TemplateRegistry, ConnectionHash string
		Envd                                                           bool
		EgressMbps                                                     int
		Limits                                                         Limits
		Startup                                                        StartupConfig
		GPUModels                                                      []string
	}{1, service, spec.ID, spec.ProjectID, spec, spec.Custom, spec.Quota, spec.RuntimeSource, t,
		cfg.RuntimeClass, cfg.Region, cfg.Domain, cfg.TemplateRegistry, hex.EncodeToString(connection[:]),
		cfg.Envd, mbps, cfg.Limits, cfg.Startup, cfg.GPUModels})
	if err != nil {
		return registrypins.RuntimeSpec{}, err
	}
	sum := sha256.Sum256(b)
	return registrypins.RuntimeSpec{ServiceID: service, SandboxID: spec.ID, Mode: registrypins.RuntimeCreate, Image: image, Fingerprint: hex.EncodeToString(sum[:])}, nil
}

// consumeRuntimeCreate runs BEFORE namespace creation, quota writes, list calls
// or any other cluster access. A supplied proof is always consumed, even in the
// fake/legacy constructor. Production requires it for every owned managed image,
// including a provider builtin pointing into the managed namespace.
func (s *Service) consumeRuntimeCreate(spec *Spec) (map[string]string, error) {
	_, protected, err := RuntimeCreateImage(s.cfg, *spec)
	if err != nil {
		return nil, err
	}
	if spec.RuntimePins == nil {
		if protected && s.requireRuntimeCreatePins {
			return nil, registrypins.ErrConflict
		}
		if spec.ID != "" {
			return nil, registrypins.ErrConflict
		}
		spec.ID = newSandboxID()
		return nil, nil
	}
	envelope, err := RuntimeCreateSpec(s.cfg, s.serviceID, *spec)
	if err != nil {
		return nil, err
	}
	op, nonce, err := spec.RuntimePins.Consume(spec.ProjectID, envelope)
	if err != nil {
		return nil, err
	}
	return map[string]string{annotationRuntimeOperation: op, annotationRuntimeNonce: nonce,
		annotationRuntimeMode: string(envelope.Mode), annotationRuntimeImage: envelope.Image,
		annotationRuntimeFingerprint: envelope.Fingerprint}, nil
}

func stampRuntimeCreate(cr map[string]any, ann map[string]string) {
	for k, v := range ann {
		dig(cr, "metadata", "annotations").(map[string]any)[k] = v
		dig(cr, "spec", "podTemplate", "metadata", "annotations").(map[string]any)[k] = v
	}
}

// RuntimeCreateEvidence independently reads the CURRENT CR and its actual Pod.
// ReadyCandidate/cache receipts alone are not image-binding evidence. Missing
// imageID, a different owner UID, nonce, main image or envelope is a HOLD.
func (s *Service) RuntimeCreateEvidence(ctx context.Context, project, id string) (registrypins.RuntimePodEvidence, error) {
	var out registrypins.RuntimePodEvidence
	cr, err := s.get(ctx, project, id)
	if err != nil {
		return out, err
	}
	pod, err := s.api.GetCore(ctx, "pods", NamespaceFor(project), id)
	if err != nil {
		return out, err
	}
	if pod == nil || !s.owns(pod, project) || digStr(cr, "metadata", "name") != id || digStr(pod, "metadata", "name") != id ||
		digStr(cr, "metadata", "namespace") != NamespaceFor(project) || digStr(pod, "metadata", "namespace") != NamespaceFor(project) ||
		digStr(pod, "metadata", "labels", LabelSandboxID) != id || dig(cr, "metadata", "deletionTimestamp") != nil || dig(pod, "metadata", "deletionTimestamp") != nil {
		return out, registrypins.ErrConflict
	}
	out.UID, out.CRUID = digStr(pod, "metadata", "uid"), digStr(cr, "metadata", "uid")
	out.Operation = digStr(cr, "metadata", "annotations", annotationRuntimeOperation)
	out.Nonce = digStr(cr, "metadata", "annotations", annotationRuntimeNonce)
	out.Spec = registrypins.RuntimeSpec{ServiceID: s.serviceID, SandboxID: id,
		Mode:  registrypins.RuntimeMode(digStr(cr, "metadata", "annotations", annotationRuntimeMode)),
		Image: digStr(cr, "metadata", "annotations", annotationRuntimeImage), Fingerprint: digStr(cr, "metadata", "annotations", annotationRuntimeFingerprint)}
	out.Phase = digStr(pod, "status", "phase")
	if out.UID == "" || out.CRUID == "" || out.Operation == "" || out.Nonce == "" || out.Spec.Mode != registrypins.RuntimeCreate || out.Phase != "Running" {
		return out, registrypins.ErrConflict
	}
	for _, key := range []string{annotationRuntimeOperation, annotationRuntimeNonce, annotationRuntimeMode, annotationRuntimeImage, annotationRuntimeFingerprint} {
		v := digStr(cr, "metadata", "annotations", key)
		if v == "" || digStr(cr, "spec", "podTemplate", "metadata", "annotations", key) != v || digStr(pod, "metadata", "annotations", key) != v {
			return out, registrypins.ErrConflict
		}
	}
	owned := false
	refs, _ := dig(pod, "metadata", "ownerReferences").([]any)
	for _, raw := range refs {
		r, _ := raw.(map[string]any)
		if str(r["uid"]) == out.CRUID && str(r["name"]) == id && str(r["kind"]) == "Sandbox" && str(r["apiVersion"]) == CRGroup+"/"+CRVersion && r["controller"] == true {
			owned = true
		}
	}
	mainImage := func(containers any) string {
		cs, _ := containers.([]any)
		for _, raw := range cs {
			c, _ := raw.(map[string]any)
			if str(c["name"]) == "main" {
				return str(c["image"])
			}
		}
		return ""
	}
	if !owned || mainImage(dig(cr, "spec", "podTemplate", "spec", "containers")) != out.Spec.Image || mainImage(dig(pod, "spec", "containers")) != out.Spec.Image {
		return out, registrypins.ErrConflict
	}
	host, _, _ := strings.Cut(s.cfg.TemplateRegistry, "/")
	r, err := registrypins.ParseRef(host, out.Spec.Image)
	if err != nil || r.String() != out.Spec.Image || r.Project != project || r.Digest == "" {
		return out, registrypins.ErrScope
	}
	pulled := false
	statuses, _ := dig(pod, "status", "containerStatuses").([]any)
	for _, raw := range statuses {
		c, _ := raw.(map[string]any)
		if str(c["name"]) != "main" {
			continue
		}
		actual := str(c["imageID"])
		if _, rest, ok := strings.Cut(actual, "://"); ok {
			actual = rest
		}
		if actual == out.Spec.Image || actual == r.Digest {
			pulled = true
		}
	}
	if !pulled {
		return out, registrypins.ErrConflict
	}
	return out, ctx.Err()
}
