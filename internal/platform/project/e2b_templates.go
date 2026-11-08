package project

// e2b_templates.go — custom templates (SB6b): the v3 build flow of the E2B SDKs and CLI (docs/managed-sandbox.md §7).
//
//	POST /v3/templates                                {name, tags, cpuCount, memoryMB} → template + build ids
//	GET  /templates/{id}/files/{hash}                 is this COPY context uploaded? else an upload URL
//	PUT  /templates/{id}/files/{hash}/upload?token=   the SDK uploads the context tar (no API key: the token)
//	POST /v2/templates/{id}/builds/{buildID}          steps → Dockerfile → build pod (sandbox.StartBuild)
//	GET  /templates/{id}/builds/{buildID}/status      state + logs (logsOffset)
//	GET|DELETE /templates/{id}, GET /templates/{id}/tags, POST|DELETE /templates/tags
//
// Contexts live in the provider's build bucket (config.sandbox.buildStorage). The portal streams uploads into it, so the
// bucket's keys never leave the portal and the bucket need not be reachable from the internet; build pods fetch their
// contexts with presigned GETs (the nodes reach the bucket, not the portal). Images go to the template registry as
// <templateRegistry>/<project>/<template>:<build> with the project's own build account, and sandboxes pin them by digest.
// Ported from the earlier implementation (EE); snapshots (pause, fork) are records here too (e2b_snapshots.go).

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/mutex-solutions/proxcloud/internal/cloud"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

const (
	maxTemplatesPerProject = 20
	maxConcurrentBuilds    = 2
	maxContextBytes        = 2 << 30 // per COPY context upload
	// maxProjectContextBytes caps all of a project's stored contexts: they share the object store with the registry's
	// blobs. Over it, contexts older than contextEvictAge go first, oldest first.
	maxProjectContextBytes = 10 << 30
	contextEvictAge        = sandbox.BuildTimeout + 10*time.Minute // a presigned fetch URL's lifetime
	maxStoredLogLines      = 2000
	keptBuilds             = 10
	buildPodGrace          = 2 * time.Minute // a finished build pod outlives one charge tick
	defaultBaseImage       = "e2bdev/base:latest"
	// contextPartSize: a context up to this size is one PUT, a larger one a multipart upload in parts of this size.
	contextPartSize = 16 << 20
)

// SetSandboxTemplates wires the sandbox_templates collection (nil = custom templates off).
func (h *Handler) SetSandboxTemplates(s *pgdoc.Store) { h.sbTemplates = s }

type templateBuild struct {
	BuildID  string `json:"buildID"`
	Status   string `json:"status"`
	From     string `json:"from,omitempty"`
	Image    string `json:"image,omitempty"` // ref@digest once ready
	StartCmd string `json:"startCmd,omitempty"`
	ReadyCmd string `json:"readyCmd,omitempty"`
	User     string `json:"user,omitempty"`
	Workdir  string `json:"workdir,omitempty"`
	// StartAsRoot: the start command runs as root (inherited from a parent such as code-interpreter-v1 whose server
	// needs it).
	StartAsRoot bool       `json:"startAsRoot,omitempty"`
	Reason      string     `json:"reason,omitempty"`
	Step        string     `json:"step,omitempty"`
	Tags        []string   `json:"tags,omitempty"` // assigned when the build becomes ready
	Logs        []string   `json:"logs,omitempty"` // kept once the build pod is gone
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	SizeBytes   int64      `json:"sizeBytes,omitempty"` // a snapshot's layer as captured (billed while ready)
}

type sandboxTemplateDoc struct {
	ID          string            `json:"id"`
	ProjectID   string            `json:"projectId"`
	Name        string            `json:"name"`
	CPU         int               `json:"cpu"`
	MemoryGiB   int               `json:"memoryGiB"`
	Tags        map[string]string `json:"tags"` // tag → buildID
	UploadToken string            `json:"uploadToken"`
	Builds      []templateBuild   `json:"builds"`
	// Kind "snapshot": a sandbox's filesystem (e2b_snapshots.go), taken from SourceSandbox. Internal: a pause's or a
	// fork's snapshot, hidden from the template lists and from their limit.
	Kind          string              `json:"kind,omitempty"`
	Internal      bool                `json:"internal,omitempty"`
	SourceSandbox string              `json:"sourceSandbox,omitempty"`
	GPU           *sandbox.GPURequest `json:"gpu,omitempty"`
	// ForkUntil: a fork's snapshot, which a fork of the same sandbox in the same project reuses until then (the first
	// fork answered 503 while the image was assembled); job sandboxBuildSync drops it after then if no fork took it.
	ForkUntil *time.Time `json:"forkUntil,omitempty"`
	CreatedAt *time.Time `json:"createdAt"`
	UpdatedAt *time.Time `json:"updatedAt"`
}

func (t *sandboxTemplateDoc) build(id string) *templateBuild {
	for i := range t.Builds {
		if t.Builds[i].BuildID == id {
			return &t.Builds[i]
		}
	}
	return nil
}

// newToken is n random bytes in hex (crypto/rand does not fail on supported platforms).
func newToken(n int) string {
	s, _ := randomHex(n)
	return s
}

// newTemplateID is E2B-shaped: 20 lowercase alphanumerics.
func newTemplateID() string { return newToken(10) }

var templateTagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func (h *Handler) templatesOn(w http.ResponseWriter) bool {
	if h.sbTemplates == nil {
		e2bErr(w, http.StatusNotImplemented, "custom templates are not available")
		return false
	}
	return true
}

// buildsOn: this location can build (a template registry and a build bucket).
func buildsOn(cfg sandbox.Config) bool {
	return cfg.TemplateRegistry != "" && cfg.BuildStorage.Bucket != "" && cfg.BuildStorage.Endpoint != ""
}

func (h *Handler) loadTemplate(ctx context.Context, projectID, idOrName string) (*sandboxTemplateDoc, error) {
	var t sandboxTemplateDoc
	found, err := h.sbTemplates.FindOne(ctx, pgdoc.M{"projectId": projectID, "$or": []pgdoc.M{{"_id": idOrName}, {"name": idOrName}}}, &t)
	if err != nil || !found {
		return nil, err
	}
	return &t, nil
}

func (h *Handler) saveTemplate(ctx context.Context, t *sandboxTemplateDoc) error {
	return h.sbTemplates.DB().WithTx(ctx, func(tc context.Context) error {
		var cur sandboxTemplateDoc
		found, err := h.sbTemplates.GetForUpdate(tc, t.ID, &cur)
		if err != nil {
			return err
		}
		if found {
			mergeConcurrent(t, &cur)
		}
		now := time.Now().UTC()
		t.UpdatedAt = &now
		trimBuilds(t)
		_, err = h.sbTemplates.Replace(tc, t.ID, t)
		return err
	})
}

// trimBuilds drops the oldest builds beyond keptBuilds, but never one a tag names (the template would stop resolving
// and the prune would lose its image) nor one that is building (its pod is running and billed). Waiting builds are
// trimmed like finished ones: every POST /v3/templates adds one, so keeping them all would let retries grow the record
// without bound; the newest keptBuilds of them survive.
// A snapshot keeps no ready build but the tagged one: a named snapshot taken again supersedes the previous image, which
// the prune then deletes, so the stored size billed is that of the image kept.
func trimBuilds(t *sandboxTemplateDoc) {
	keep := map[string]bool{}
	for _, id := range t.Tags {
		keep[id] = true
	}
	drop := len(t.Builds) - keptBuilds
	out := t.Builds[:0]
	for _, b := range t.Builds { // oldest first (mergeConcurrent sorts by createdAt)
		if t.Kind == "snapshot" && b.Status == sandbox.BuildReady && !keep[b.BuildID] {
			drop--
			continue
		}
		if drop > 0 && !keep[b.BuildID] && b.Status != sandbox.BuildBuilding {
			drop--
			continue
		}
		out = append(out, b)
	}
	t.Builds = out
}

// mergeConcurrent folds what others saved since t was read into t, so a request writing back its copy cannot undo
// them: a build finished meanwhile stays finished (with the tags its result set), and a build added meanwhile stays.
// Builds are never removed except by trimming.
func mergeConcurrent(t, cur *sandboxTemplateDoc) {
	if t.Tags == nil {
		t.Tags = map[string]string{}
	}
	finished := func(b *templateBuild) bool { return b.Status == sandbox.BuildReady || b.Status == sandbox.BuildError }
	for i := range t.Builds {
		b := &t.Builds[i]
		if finished(b) {
			continue
		}
		if cb := cur.build(b.BuildID); cb != nil && finished(cb) {
			*b = *cb
			if cb.Status == sandbox.BuildReady {
				for tg, id := range cur.Tags {
					if id == cb.BuildID {
						t.Tags[tg] = id
					}
				}
			}
		}
	}
	for _, cb := range cur.Builds {
		if t.build(cb.BuildID) == nil {
			t.Builds = append(t.Builds, cb)
		}
	}
	sort.SliceStable(t.Builds, func(i, j int) bool {
		a, b := t.Builds[i].CreatedAt, t.Builds[j].CreatedAt
		return a != nil && b != nil && a.Before(*b)
	})
}

// ---- POST /v3/templates ------------------------------------------------------------------------

func (h *Handler) e2bTemplateRequest(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	if !h.templatesOn(w) {
		return
	}
	if !buildsOn(c.ss.Config()) {
		e2bErr(w, http.StatusServiceUnavailable, "template builds are not available at this location")
		return
	}
	var b struct {
		Name     string   `json:"name"`
		Alias    string   `json:"alias"`
		Tags     []string `json:"tags"`
		CPUCount int      `json:"cpuCount"`
		MemoryMB int      `json:"memoryMB"`
	}
	if !decodeE2B(w, r, &b) {
		return
	}
	name, tags := splitNameTag(firstNonEmpty(b.Name, b.Alias))
	if tags != "" {
		b.Tags = append(b.Tags, tags)
	}
	if !templateTagRe.MatchString(name) {
		e2bErr(w, http.StatusBadRequest, "name must be 1-63 lowercase letters, digits, '.', '_' or '-'")
		return
	}
	if _, builtin := c.ss.Config().Templates[name]; builtin {
		e2bErr(w, http.StatusConflict, fmt.Sprintf("%q is a built-in template name", name))
		return
	}
	for _, tg := range b.Tags {
		if !templateTagRe.MatchString(tg) {
			e2bErr(w, http.StatusBadRequest, fmt.Sprintf("invalid tag %q", tg))
			return
		}
	}
	lim := c.ss.Config().WithDefaults().Limits
	cpu, mem := templateSize(b.CPUCount, b.MemoryMB)
	if cpu > lim.MaxCPU || mem > lim.MaxMemoryGiB {
		e2bErr(w, http.StatusBadRequest, fmt.Sprintf("cpuCount must be at most %d and memoryMB at most %d", lim.MaxCPU, lim.MaxMemoryGiB*1024))
		return
	}
	// A new build is new usage (it runs and is billed): the same gate as a sandbox create.
	if err := h.sandboxUsageGate(r.Context(), c.proj, c.es); err != nil {
		e2bFail(w, err)
		return
	}
	ctx := r.Context()
	t, err := h.loadTemplate(ctx, c.proj.ID, name)
	if err != nil {
		e2bFail(w, err)
		return
	}
	now := time.Now().UTC()
	if t == nil {
		n, err := h.sbTemplates.Count(ctx, pgdoc.M{"projectId": c.proj.ID, "internal": pgdoc.M{"$ne": true}})
		if err != nil {
			e2bFail(w, err)
			return
		}
		if n >= maxTemplatesPerProject {
			e2bErr(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d templates per project", maxTemplatesPerProject))
			return
		}
		t = &sandboxTemplateDoc{ID: newTemplateID(), ProjectID: c.proj.ID, Name: name, Tags: map[string]string{},
			UploadToken: newToken(24), CreatedAt: &now}
		if _, err := h.sbTemplates.InsertOne(ctx, pgdoc.M{"_id": t.ID, "projectId": t.ProjectID, "name": name}); err != nil {
			e2bFail(w, err)
			return
		}
	}
	t.CPU, t.MemoryGiB = cpu, mem
	if len(b.Tags) == 0 {
		b.Tags = []string{"default"}
	}
	bl := templateBuild{BuildID: uuid.NewString(), Status: sandbox.BuildWaiting, Tags: b.Tags, CreatedAt: &now}
	t.Builds = append(t.Builds, bl)
	if err := h.saveTemplate(ctx, t); err != nil {
		e2bFail(w, err)
		return
	}
	e2bJSON(w, http.StatusAccepted, map[string]any{
		"templateID": t.ID, "buildID": bl.BuildID, "public": false,
		"names": []string{t.Name}, "aliases": []string{t.Name}, "tags": b.Tags,
	})
}

// templateSize is the template's default sandbox size (and its build's): what the caller asked for, memory rounded up
// to whole GiB, 2 vCPU / 2 GiB when not given.
func templateSize(cpuCount, memoryMB int) (cpu, memGiB int) {
	cpu, memGiB = 2, 2
	if cpuCount > 0 {
		cpu = cpuCount
	}
	if memoryMB > 0 {
		memGiB = (memoryMB + 1023) / 1024
	}
	return cpu, memGiB
}

func splitNameTag(s string) (string, string) {
	name, tag, _ := strings.Cut(strings.TrimSpace(s), ":")
	return name, tag
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---- contexts (S3) -----------------------------------------------------------------------------

type buildStore struct {
	client  *s3.Client // the portal's endpoint
	presign *s3.PresignClient
	bucket  string
}

func s3Client(endpoint, region, access, secret string) *s3.Client {
	if region == "" {
		region = "us-east-1"
	}
	return s3.New(s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(access, secret, ""),
		BaseEndpoint: awsv2.String(endpoint),
		UsePathStyle: true,
		// No default CRC trailers (aws-chunked): RustFS and RGW are asked for plain requests.
		RequestChecksumCalculation: awsv2.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: awsv2.ResponseChecksumValidationWhenRequired,
	})
}

// buildStorage is the provider's build bucket (nil error only when it is configured).
func buildStorage(cfg sandbox.Config) (*buildStore, error) {
	b := cfg.BuildStorage
	if b.Endpoint == "" || b.Bucket == "" || b.AccessKey == "" {
		return nil, errors.New("sandbox build storage is not configured")
	}
	pod := b.PodEndpoint
	if pod == "" {
		pod = b.Endpoint
	}
	return &buildStore{
		client:  s3Client(b.Endpoint, b.Region, b.AccessKey, b.SecretKey),
		presign: s3.NewPresignClient(s3Client(pod, b.Region, b.AccessKey, b.SecretKey)),
		bucket:  b.Bucket,
	}, nil
}

func contextKey(projectID, templateID, hash string) string {
	return projectID + "/" + templateID + "/" + hash + ".tar"
}

func (bs *buildStore) exists(ctx context.Context, key string) bool {
	_, err := bs.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bs.bucket, Key: &key})
	return err == nil
}

// unsignedPayload: the body's SHA-256 is not computed or sent (x-amz-content-sha256: UNSIGNED-PAYLOAD), so a part is
// sent as it is read. RustFS and RGW both accept it (docs/managed-sandbox.md §7.6).
var unsignedPayload = s3.WithAPIOptions(v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)

// put stores a context: one PUT when it fits in one part, else a multipart upload (a context may be 2 GiB and is
// read once, as it arrives). A failed multipart upload is aborted, so no parts are left in the bucket.
func (bs *buildStore) put(ctx context.Context, key string, body io.Reader) error {
	buf := make([]byte, contextPartSize)
	n, err := io.ReadFull(body, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		_, err = bs.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &bs.bucket, Key: &key, Body: bytes.NewReader(buf[:n]),
			ContentLength: awsv2.Int64(int64(n))}, unsignedPayload)
		return err
	}
	if err != nil {
		return err
	}
	up, err := bs.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &bs.bucket, Key: &key})
	if err != nil {
		return err
	}
	abort := func(cause error) error {
		_, _ = bs.client.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{Bucket: &bs.bucket, Key: &key, UploadId: up.UploadId})
		return cause
	}
	var parts []s3types.CompletedPart
	for num := int32(1); n > 0; num++ {
		p, err := bs.client.UploadPart(ctx, &s3.UploadPartInput{Bucket: &bs.bucket, Key: &key, UploadId: up.UploadId, PartNumber: awsv2.Int32(num),
			Body: bytes.NewReader(buf[:n]), ContentLength: awsv2.Int64(int64(n))}, unsignedPayload)
		if err != nil {
			return abort(err)
		}
		parts = append(parts, s3types.CompletedPart{ETag: p.ETag, PartNumber: awsv2.Int32(num)})
		if n, err = io.ReadFull(body, buf); err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return abort(err)
		}
	}
	_, err = bs.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: &bs.bucket, Key: &key, UploadId: up.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: parts}})
	if err != nil {
		return abort(err)
	}
	return nil
}

// fetchURL is a presigned GET of a context for the build pod (it outlives the build's deadline).
func (bs *buildStore) fetchURL(ctx context.Context, key string) (string, error) {
	req, err := bs.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &bs.bucket, Key: &key}, s3.WithPresignExpires(sandbox.BuildTimeout+10*time.Minute))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// contextBudget is how many bytes the next upload of the project may store (at most maxContextBytes). Over the
// project's cap it first deletes the project's contexts older than contextEvictAge, oldest first, until want bytes
// (maxContextBytes when unknown) fit.
// ponytail: listed per upload and not locked, so parallel uploads can overshoot the cap by one context each; a usage
// row per project if that matters.
func (bs *buildStore) contextBudget(ctx context.Context, projectID string, want int64) (int64, error) {
	if want <= 0 || want > maxContextBytes {
		want = maxContextBytes
	}
	prefix := projectID + "/"
	var objs []s3types.Object
	var used int64
	p := s3.NewListObjectsV2Paginator(bs.client, &s3.ListObjectsV2Input{Bucket: &bs.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return 0, err
		}
		for _, o := range page.Contents {
			objs = append(objs, o)
			used += awsv2.ToInt64(o.Size)
		}
	}
	sort.Slice(objs, func(i, j int) bool {
		return awsv2.ToTime(objs[i].LastModified).Before(awsv2.ToTime(objs[j].LastModified))
	})
	for _, o := range objs {
		if maxProjectContextBytes-used >= want || time.Since(awsv2.ToTime(o.LastModified)) < contextEvictAge {
			break
		}
		if _, err := bs.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bs.bucket, Key: o.Key}); err != nil {
			return 0, err
		}
		used -= awsv2.ToInt64(o.Size)
	}
	return min(int64(maxContextBytes), maxProjectContextBytes-used), nil
}

// deletePrefix removes a template's contexts (best effort; at most one listing page).
func (bs *buildStore) deletePrefix(ctx context.Context, prefix string) {
	out, err := bs.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: &bs.bucket, Prefix: &prefix})
	if err != nil {
		slog.Warn("e2b: list template contexts", "prefix", prefix, "err", err)
		return
	}
	for _, o := range out.Contents {
		if _, err := bs.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bs.bucket, Key: o.Key}); err != nil {
			slog.Warn("e2b: delete template context", "key", awsv2.ToString(o.Key), "err", err)
		}
	}
}

var filesHashRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,128}$`)

// GET /templates/{templateID}/files/{hash}
func (h *Handler) e2bTemplateFile(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	if !h.templatesOn(w) {
		return
	}
	t, err := h.loadTemplate(r.Context(), c.proj.ID, chi.URLParam(r, "templateID"))
	if err != nil || t == nil {
		e2bErr(w, http.StatusNotFound, "template not found")
		return
	}
	hash := chi.URLParam(r, "hash")
	if !filesHashRe.MatchString(hash) {
		e2bErr(w, http.StatusBadRequest, "invalid files hash")
		return
	}
	// Stored contexts are usage: a suspended project gets no upload URL.
	if err := h.sandboxUsageGate(r.Context(), c.proj, c.es); err != nil {
		e2bFail(w, err)
		return
	}
	bs, err := buildStorage(c.ss.Config())
	if err != nil {
		e2bErr(w, http.StatusServiceUnavailable, "template builds are not available at this location")
		return
	}
	if bs.exists(r.Context(), contextKey(t.ProjectID, t.ID, hash)) {
		e2bJSON(w, http.StatusCreated, map[string]any{"present": true})
		return
	}
	url := fmt.Sprintf("%s/e2b/templates/%s/files/%s/upload?token=%s", h.publicBase(r), t.ID, hash, t.UploadToken)
	e2bJSON(w, http.StatusCreated, map[string]any{"present": false, "url": url})
}

// publicBase is where callers reach this API: the configured self URL, else the request's own host.
func (h *Handler) publicBase(r *http.Request) string {
	if h.apiBaseURL != "" {
		return strings.TrimRight(h.apiBaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// templateService is the sandbox service of a template's project (the upload route has no caller), after the same
// usage gate as a build: the upload URL outlives a suspension.
func (h *Handler) templateService(ctx context.Context, projectID string) (*sandbox.Service, error) {
	p, err := h.svc.repo.FindByID(ctx, projectID)
	if err != nil || p == nil {
		return nil, errors.New("project not found")
	}
	es, err := h.esSvc.Get(ctx, h.sandboxServiceOf(ctx, p))
	if err != nil || es == nil || h.sandboxFor == nil {
		return nil, errors.New("sandboxes are not available")
	}
	if err := h.sandboxUsageGate(ctx, p, es); err != nil {
		return nil, err
	}
	return h.sandboxFor(es)
}

// PUT /templates/{templateID}/files/{hash}/upload?token=
func (h *Handler) e2bTemplateUpload(w http.ResponseWriter, r *http.Request) {
	var t sandboxTemplateDoc
	found := false
	if h.sbTemplates != nil {
		found, _ = h.sbTemplates.Get(r.Context(), chi.URLParam(r, "templateID"), &t)
	}
	hash := chi.URLParam(r, "hash")
	if !found || t.UploadToken == "" || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(t.UploadToken)) != 1 || !filesHashRe.MatchString(hash) {
		e2bErr(w, http.StatusForbidden, "invalid upload URL")
		return
	}
	if r.ContentLength > maxContextBytes {
		e2bErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a build context may be at most %d bytes", maxContextBytes))
		return
	}
	ss, err := h.templateService(r.Context(), t.ProjectID)
	if err != nil {
		if status, msg := e2bError(err); status != http.StatusInternalServerError && status != http.StatusServiceUnavailable {
			e2bErr(w, status, msg) // the usage gate's refusal
			return
		}
	}
	var bs *buildStore
	if err == nil {
		bs, err = buildStorage(ss.Config())
	}
	limit := int64(maxContextBytes)
	if err == nil {
		limit, err = bs.contextBudget(r.Context(), t.ProjectID, r.ContentLength)
	}
	if err == nil && (limit <= 0 || r.ContentLength > limit) {
		e2bErr(w, http.StatusInsufficientStorage, fmt.Sprintf("the project's build contexts may use at most %d bytes; delete templates you no longer build", maxProjectContextBytes))
		return
	}
	if err == nil {
		err = bs.put(r.Context(), contextKey(t.ProjectID, t.ID, hash), &limitedReader{r: r.Body, n: limit})
	}
	if errors.Is(err, errContextTooLarge) {
		e2bErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("a build context may be at most %d bytes (and the project's at most %d)", maxContextBytes, maxProjectContextBytes))
		return
	}
	if err != nil {
		slog.Error("e2b: context upload", "template", t.ID, "err", err)
		e2bErr(w, http.StatusServiceUnavailable, "the upload could not be stored")
		return
	}
	w.WriteHeader(http.StatusOK)
}

var errContextTooLarge = errors.New("build context too large")

// limitedReader fails a body longer than n (io.LimitReader would cut it short and store a truncated tar).
type limitedReader struct {
	r io.Reader
	n int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.n < 0 {
		return 0, errContextTooLarge
	}
	if int64(len(p)) > l.n+1 {
		p = p[:l.n+1]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	if l.n < 0 {
		return n, errContextTooLarge
	}
	return n, err
}

// ---- POST /v2/templates/{templateID}/builds/{buildID} ------------------------------------------

func (h *Handler) e2bTemplateBuild(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	if !h.templatesOn(w) {
		return
	}
	ctx := r.Context()
	t, err := h.loadTemplate(ctx, c.proj.ID, chi.URLParam(r, "templateID"))
	if err != nil || t == nil {
		e2bErr(w, http.StatusNotFound, "template not found")
		return
	}
	bl := t.build(chi.URLParam(r, "buildID"))
	if bl == nil {
		e2bErr(w, http.StatusNotFound, "build not found")
		return
	}
	if bl.Status != sandbox.BuildWaiting {
		e2bErr(w, http.StatusConflict, "this build has already been started")
		return
	}
	var b struct {
		FromImage         string              `json:"fromImage"`
		FromTemplate      string              `json:"fromTemplate"`
		FromImageRegistry json.RawMessage     `json:"fromImageRegistry"`
		Steps             []sandbox.BuildStep `json:"steps"`
		StartCmd          string              `json:"startCmd"`
		ReadyCmd          string              `json:"readyCmd"`
	}
	if !decodeE2B(w, r, &b) {
		return
	}
	cfg := c.ss.Config()
	if !buildsOn(cfg) {
		e2bErr(w, http.StatusServiceUnavailable, "template builds are not available at this location")
		return
	}
	base, err := h.resolveBase(ctx, c, b.FromImage, b.FromTemplate, b.FromImageRegistry)
	if err != nil {
		if errors.Is(err, sandbox.ErrBadBuild) {
			e2bErr(w, http.StatusBadRequest, strings.TrimPrefix(err.Error(), sandbox.ErrBadBuild.Error()+": "))
			return
		}
		e2bFail(w, err)
		return
	}
	res, err := sandbox.RenderDockerfile(base.Ref, b.Steps)
	if err != nil {
		e2bErr(w, http.StatusBadRequest, err.Error())
		return
	}
	bs, err := buildStorage(cfg)
	if err != nil {
		e2bErr(w, http.StatusServiceUnavailable, "template builds are not available at this location")
		return
	}
	fetch := map[string]string{}
	for _, hsh := range res.Hashes {
		key := contextKey(t.ProjectID, t.ID, hsh)
		if !bs.exists(ctx, key) {
			e2bErr(w, http.StatusBadRequest, fmt.Sprintf("files %s were not uploaded (GET /templates/%s/files/%s first)", hsh, t.ID, hsh))
			return
		}
		if fetch[hsh], err = bs.fetchURL(ctx, key); err != nil {
			e2bFail(w, err)
			return
		}
	}
	if err := h.sandboxUsageGate(ctx, c.proj, c.es); err != nil {
		e2bFail(w, err)
		return
	}
	bl.From, bl.StartCmd, bl.ReadyCmd = base.Ref, b.StartCmd, b.ReadyCmd
	bl.User, bl.Workdir = res.User, res.Workdir
	inheritFromParent(bl, base.Parent, b.StartCmd, b.ReadyCmd)
	// The count and the start hold one lock per project, across API replicas: without it parallel requests all pass
	// the count, and one project could fill the build namespace's quota that every project shares.
	errBusy := errors.New("busy")
	err = h.sbTemplates.DB().WithTx(ctx, func(tc context.Context) error {
		if err := h.sbTemplates.DB().LockXact(tc, "sbx-build:"+c.proj.ID); err != nil {
			return err
		}
		running, err := c.ss.ListBuilds(tc, c.proj.ID)
		if err != nil {
			return err
		}
		active := 0
		for _, d := range running {
			if d["status"] != sandbox.StatusDeleted {
				active++
			}
		}
		if active >= maxConcurrentBuilds {
			return errBusy
		}
		return c.ss.StartBuild(tc, sandbox.BuildRequest{
			ProjectID: t.ProjectID, TemplateID: t.ID, BuildID: bl.BuildID, Image: templateImage(cfg, t, bl.BuildID), Result: res,
			CPU: t.CPU, MemoryGiB: t.MemoryGiB,
			Base: base.Ref, BaseAuth: base.Auth, BaseDockerConfig: base.DockerConfig,
			FetchURL: func(hsh string) string { return fetch[hsh] },
		})
	})
	if errors.Is(err, errBusy) {
		e2bErr(w, http.StatusTooManyRequests, fmt.Sprintf("at most %d builds may run at once", maxConcurrentBuilds))
		return
	}
	if err != nil {
		e2bFail(w, err)
		return
	}
	bl.Status = sandbox.BuildBuilding
	if err := h.saveTemplate(ctx, t); err != nil {
		e2bFail(w, err)
		return
	}
	// The build is billed like a sandbox of its size while it runs (sandbox.buildData); the sync keeps the row.
	now := time.Now().UTC()
	data, _ := c.ss.BuildData(ctx, bl.BuildID)
	if data == nil {
		data = map[string]any{"kind": "build", "status": sandbox.StatusPending}
	}
	if _, err := h.cloud.Insert(ctx, &cloud.CloudResource{
		ServiceID: c.es.ID, Region: c.es.SandboxRegion(), ProjectID: c.proj.ID, Type: cloud.TypeSandbox,
		ExternalID: sandbox.BuildResourceID(bl.BuildID), Data: data, CreatedAt: &now, UpdatedAt: &now,
	}); err != nil {
		slog.Error("e2b: build cache row", "build", bl.BuildID, "err", err)
	}
	w.WriteHeader(http.StatusAccepted)
}

// templateImage is where a build pushes: the project's own repository under the template registry.
func templateImage(cfg sandbox.Config, t *sandboxTemplateDoc, buildID string) string {
	return fmt.Sprintf("%s/%s/%s:%s", strings.TrimRight(cfg.TemplateRegistry, "/"), t.ProjectID, t.ID, buildID)
}

// ---- GET /templates/{templateID}/builds/{buildID}/status ---------------------------------------

func (h *Handler) e2bTemplateBuildStatus(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	if !h.templatesOn(w) {
		return
	}
	ctx := r.Context()
	t, err := h.loadTemplate(ctx, c.proj.ID, chi.URLParam(r, "templateID"))
	if err != nil || t == nil {
		e2bErr(w, http.StatusNotFound, "template not found")
		return
	}
	bl := t.build(chi.URLParam(r, "buildID"))
	if bl == nil {
		e2bErr(w, http.StatusNotFound, "build not found")
		return
	}
	logs := bl.Logs
	if t.Kind == "snapshot" && bl.Status == sandbox.BuildBuilding { // its result starts the storage billing
		if done, _ := h.finishSnapshot(ctx, c, t, bl, false); done {
			if ft, _ := h.loadTemplate(ctx, c.proj.ID, t.ID); ft != nil {
				adoptBuild(t, bl, ft)
			}
		}
		logs = bl.Logs
	} else if bl.Status == sandbox.BuildBuilding || bl.Status == sandbox.BuildReady && bl.FinishedAt == nil {
		logs = h.syncBuildPod(ctx, c, t, bl, true)
	} else if bl.FinishedAt != nil && time.Since(*bl.FinishedAt) > buildPodGrace {
		_ = c.ss.DeleteBuild(ctx, bl.BuildID)
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("logsOffset"))
	if offset < 0 || offset > len(logs) {
		offset = len(logs)
	}
	page := logs[offset:]
	entries := make([]any, 0, len(page))
	for _, l := range page {
		entries = append(entries, logEntry(l))
	}
	out := map[string]any{"templateID": t.ID, "buildID": bl.BuildID, "status": bl.Status, "logs": page, "logEntries": entries}
	if bl.Status == sandbox.BuildError {
		out["reason"] = map[string]any{"message": bl.Reason, "step": bl.Step, "logEntries": entries}
	}
	e2bJSON(w, http.StatusOK, out)
}

// logEntry parses kaniko's "LEVEL[time] message" lines loosely; anything else is info.
func logEntry(line string) map[string]any {
	level := "info"
	low := strings.ToLower(line)
	switch {
	case strings.Contains(low, "error"):
		level = "error"
	case strings.Contains(low, "warn"):
		level = "warn"
	}
	return map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339), "level": level, "message": line}
}

// syncBuildPod reads the build pod and records a finished build. markGone: a missing pod means the build failed (the
// status endpoint); the background job leaves that to the next poll.
func (h *Handler) syncBuildPod(ctx context.Context, c *e2bCaller, t *sandboxTemplateDoc, bl *templateBuild, markGone bool) []string {
	st, err := c.ss.BuildPodState(ctx, t.ProjectID, bl.BuildID)
	if err != nil {
		return bl.Logs
	}
	if st == nil {
		if markGone {
			ft, _, _ := h.finishBuild(ctx, t.ProjectID, t.ID, bl.BuildID, func(_ *sandboxTemplateDoc, b *templateBuild) {
				b.Status, b.Reason = sandbox.BuildError, "the build disappeared before it finished"
			})
			adoptBuild(t, bl, ft)
		}
		return bl.Logs
	}
	logs := c.ss.BuildLogs(ctx, bl.BuildID, maxStoredLogLines)
	if data, err := c.ss.BuildData(ctx, bl.BuildID); err == nil && data != nil {
		if cr := h.e2bRow(ctx, c, sandbox.BuildResourceID(bl.BuildID)); cr != nil {
			cr.Data = data
			if _, err := h.cloud.Update(ctx, cr); err != nil {
				slog.Warn("e2b: build cache update", "build", bl.BuildID, "err", err)
			}
		}
	}
	if st.Status == sandbox.BuildReady || st.Status == sandbox.BuildError {
		reg := strings.TrimRight(c.ss.Config().TemplateRegistry, "/")
		ft, _, err := h.finishBuild(ctx, t.ProjectID, t.ID, bl.BuildID, func(ft *sandboxTemplateDoc, b *templateBuild) {
			b.Logs = logs
			b.Status, b.Reason, b.Step = st.Status, st.Reason, st.Step
			if st.Status == sandbox.BuildReady {
				b.Image = fmt.Sprintf("%s/%s/%s@%s", reg, ft.ProjectID, ft.ID, st.Digest)
				for _, tg := range b.Tags {
					ft.Tags[tg] = b.BuildID
				}
			}
		})
		if err != nil {
			slog.Error("e2b: save build result", "build", bl.BuildID, "err", err)
		}
		adoptBuild(t, bl, ft)
	}
	return logs
}

// adoptBuild copies the recorded state of bl (and the tags) from the template as saved into the caller's copy, so a
// handler answering from it reports the result it just recorded.
func adoptBuild(t *sandboxTemplateDoc, bl *templateBuild, saved *sandboxTemplateDoc) {
	if saved == nil {
		return
	}
	if b := saved.build(bl.BuildID); b != nil {
		*bl = *b
	}
	t.Tags = saved.Tags
}

// finishBuild records a build's result on the template as it is now, under a row lock: other requests may have added
// builds or moved tags since the caller read it, and two finishers (the status endpoint, the background job) may race.
// changed=false: the build was already finished or is gone. FinishedAt is set here.
func (h *Handler) finishBuild(ctx context.Context, projectID, templateID, buildID string, apply func(t *sandboxTemplateDoc, b *templateBuild)) (*sandboxTemplateDoc, bool, error) {
	ctx = context.WithoutCancel(ctx) // a half-recorded result is worse than a late one
	var out *sandboxTemplateDoc
	changed := false
	err := h.sbTemplates.DB().WithTx(ctx, func(tc context.Context) error {
		var t sandboxTemplateDoc
		found, err := h.sbTemplates.GetForUpdate(tc, templateID, &t)
		if err != nil || !found || t.ProjectID != projectID {
			return err
		}
		out = &t
		b := t.build(buildID)
		if b == nil || b.Status == sandbox.BuildReady || b.Status == sandbox.BuildError {
			return nil
		}
		if t.Tags == nil {
			t.Tags = map[string]string{}
		}
		apply(&t, b)
		fin := time.Now().UTC()
		b.FinishedAt = &fin
		changed = true
		return h.saveTemplate(tc, &t)
	})
	return out, changed, err
}

// ---- templates: list / get / delete / tags -----------------------------------------------------

func (h *Handler) projectTemplates(ctx context.Context, projectID string) []sandboxTemplateDoc {
	if h.sbTemplates == nil {
		return nil
	}
	var all, ts []sandboxTemplateDoc
	_ = h.sbTemplates.Find(ctx, pgdoc.M{"projectId": projectID}, &all)
	for _, t := range all {
		if !t.Internal {
			ts = append(ts, t)
		}
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
	return ts
}

func (t *sandboxTemplateDoc) defaultBuild() *templateBuild {
	if id, ok := t.Tags["default"]; ok {
		if b := t.build(id); b != nil && b.Status == sandbox.BuildReady {
			return b
		}
	}
	for i := len(t.Builds) - 1; i >= 0; i-- {
		if t.Builds[i].Status == sandbox.BuildReady {
			return &t.Builds[i]
		}
	}
	return nil
}

func (t *sandboxTemplateDoc) e2bView() map[string]any {
	tags := make([]string, 0, len(t.Tags))
	for tg := range t.Tags {
		tags = append(tags, tg)
	}
	sort.Strings(tags)
	status, buildID := "waiting", ""
	if n := len(t.Builds); n > 0 {
		status, buildID = t.Builds[n-1].Status, t.Builds[n-1].BuildID
	}
	ts := func(p *time.Time) string {
		if p == nil {
			return ""
		}
		return p.Format(time.RFC3339)
	}
	return map[string]any{
		"templateID": t.ID, "buildID": buildID, "cpuCount": t.CPU, "memoryMB": t.MemoryGiB * 1024, "diskSizeMB": 0,
		"public": false, "aliases": []string{t.Name}, "names": []string{t.Name}, "tags": tags,
		"envdVersion": sandbox.EnvdVersion, "buildStatus": status, "buildCount": len(t.Builds), "spawnCount": 0,
		"createdAt": ts(t.CreatedAt), "updatedAt": ts(t.UpdatedAt), "createdBy": nil,
	}
}

// resolveCustomTemplate maps a create's template ("name", "name:tag", "<id>", "<id>:tag") to a project template's
// ready build. nil = not a project template (the caller tries built-ins). Only the project's own records are read, so
// another project's template is "not found".
func (h *Handler) resolveCustomTemplate(ctx context.Context, projectID, ref string) (*sandbox.Template, string, error) {
	if h.sbTemplates == nil {
		return nil, "", nil
	}
	name, tag := splitNameTag(ref)
	t, err := h.loadTemplate(ctx, projectID, name)
	if err != nil || t == nil {
		return nil, "", err
	}
	var bl *templateBuild
	if tag == "" {
		bl = t.defaultBuild()
	} else if id, ok := t.Tags[tag]; ok {
		bl = t.build(id)
	}
	if bl == nil || bl.Status != sandbox.BuildReady || bl.Image == "" {
		return nil, "", fmt.Errorf("%w: template %q has no ready build", sandbox.ErrBadTemplate, ref)
	}
	return &sandbox.Template{
		Image: bl.Image, CPU: t.CPU, MemoryGiB: t.MemoryGiB, GPU: t.GPU, StartCmd: bl.StartCmd, ReadyCmd: bl.ReadyCmd,
		User: bl.User, Workdir: bl.Workdir, StartAsRoot: bl.StartAsRoot, Description: "custom template " + t.Name,
	}, t.Name, nil
}

func (h *Handler) e2bTemplateDelete(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	if !h.templatesOn(w) {
		return
	}
	t, err := h.loadTemplate(r.Context(), c.proj.ID, chi.URLParam(r, "templateID"))
	if err != nil || t == nil || t.Internal { // a paused sandbox's snapshot goes with the sandbox (DELETE /sandboxes/{id})
		e2bErr(w, http.StatusNotFound, "template not found")
		return
	}
	if err := h.deleteTemplate(r.Context(), c.ss, t); err != nil {
		e2bFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteTemplate removes a template's build pods, its contexts and its record, and ends a snapshot's storage billing.
// Its images go with the next registry prune (nothing references them any more).
func (h *Handler) deleteTemplate(ctx context.Context, ss *sandbox.Service, t *sandboxTemplateDoc) error {
	ctx = context.WithoutCancel(ctx)
	if t.Kind == "snapshot" {
		cr, err := h.cloud.FindByServiceIDAndExternalID(ctx, ss.ServiceID(), t.ID)
		if err != nil {
			return err
		}
		if cr != nil && cr.ProjectID == t.ProjectID && cr.Type == cloud.TypeSandboxSnapshot {
			if err := h.cloud.DeleteAndArchive(ctx, cr, time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	for _, b := range t.Builds {
		_ = ss.DeleteBuild(ctx, b.BuildID) // running or finished: no pod outlives its template
	}
	if bs, err := buildStorage(ss.Config()); err == nil {
		bs.deletePrefix(ctx, t.ProjectID+"/"+t.ID+"/")
	}
	_, err := h.sbTemplates.DeleteOne(ctx, pgdoc.M{"_id": t.ID, "projectId": t.ProjectID})
	return err
}

func (h *Handler) e2bTemplateTags(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	if !h.templatesOn(w) {
		return
	}
	t, err := h.loadTemplate(r.Context(), c.proj.ID, chi.URLParam(r, "templateID"))
	if err != nil || t == nil {
		e2bErr(w, http.StatusNotFound, "template not found")
		return
	}
	out := []any{}
	for tg, id := range t.Tags {
		var created string
		if b := t.build(id); b != nil && b.FinishedAt != nil {
			created = b.FinishedAt.Format(time.RFC3339)
		}
		out = append(out, map[string]any{"tag": tg, "buildID": id, "createdAt": created})
	}
	e2bJSON(w, http.StatusOK, out)
}

// POST /templates/tags {target: "name:tag", tags: [...]} — point more tags at target's build.
func (h *Handler) e2bTemplateTagsAssign(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	if !h.templatesOn(w) {
		return
	}
	var b struct {
		Target string   `json:"target"`
		Tags   []string `json:"tags"`
	}
	if !decodeE2B(w, r, &b) {
		return
	}
	name, tag := splitNameTag(b.Target)
	t, err := h.loadTemplate(r.Context(), c.proj.ID, name)
	if err != nil || t == nil {
		e2bErr(w, http.StatusNotFound, "template not found")
		return
	}
	var bl *templateBuild
	if tag == "" {
		bl = t.defaultBuild()
	} else if id, ok := t.Tags[tag]; ok {
		bl = t.build(id)
	}
	if bl == nil {
		e2bErr(w, http.StatusNotFound, "target build not found")
		return
	}
	for _, tg := range b.Tags {
		if !templateTagRe.MatchString(tg) {
			e2bErr(w, http.StatusBadRequest, fmt.Sprintf("invalid tag %q", tg))
			return
		}
		t.Tags[tg] = bl.BuildID
	}
	if err := h.saveTemplate(r.Context(), t); err != nil {
		e2bFail(w, err)
		return
	}
	e2bJSON(w, http.StatusCreated, map[string]any{"buildID": bl.BuildID, "tags": b.Tags})
}

// DELETE /templates/tags {name, tags}
func (h *Handler) e2bTemplateTagsRemove(w http.ResponseWriter, r *http.Request) {
	c := e2bFrom(r)
	if !h.templatesOn(w) {
		return
	}
	var b struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if !decodeE2B(w, r, &b) {
		return
	}
	name, _ := splitNameTag(b.Name)
	t, err := h.loadTemplate(r.Context(), c.proj.ID, name)
	if err != nil || t == nil {
		e2bErr(w, http.StatusNotFound, "template not found")
		return
	}
	for _, tg := range b.Tags {
		delete(t.Tags, tg)
	}
	if err := h.saveTemplate(r.Context(), t); err != nil {
		e2bFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
