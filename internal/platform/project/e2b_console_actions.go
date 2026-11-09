package project

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
	"github.com/mutex-solutions/proxcloud/pkg/httpx"
)

type consoleRuntimeCapabilities struct {
	CPU          int `json:"cpu"`
	MemoryGiB    int `json:"memoryGiB"`
	MaxCPU       int `json:"maxCpu"`
	MaxMemoryGiB int `json:"maxMemoryGiB"`
}
type consoleBuildCapabilities struct {
	CPU           int `json:"cpu"`
	MemoryGiB     int `json:"memoryGiB"`
	DiskGiB       int `json:"diskGiB"`
	MaxCPU        int `json:"maxCpu"`
	MaxMemoryGiB  int `json:"maxMemoryGiB"`
	MaxDiskGiB    int `json:"maxDiskGiB"`
	MaxConcurrent int `json:"maxConcurrent"`
}
type consoleTemplateCapabilities struct {
	Runtime                consoleRuntimeCapabilities `json:"runtime"`
	Build                  consoleBuildCapabilities   `json:"build"`
	ImportEnabled          bool                       `json:"importEnabled"`
	TrustedWorkerAvailable bool                       `json:"trustedWorkerAvailable"`
	WarmingEnabled         bool                       `json:"warmingEnabled"`
}

func (h *Handler) consoleTemplateCapabilities(w http.ResponseWriter, r *http.Request) {
	c, ok := h.consoleCaller(w, r)
	if !ok {
		return
	}
	cfg := c.ss.Config().WithDefaults()
	_, workerErr := c.ss.ImportWorker(r.Context())
	warming, warmingErr := c.ss.WarmingAvailable(r.Context())
	cpu, memory := templateSize(0, 0)
	httpx.OK(w, consoleTemplateCapabilities{
		Runtime: consoleRuntimeCapabilities{CPU: cpu, MemoryGiB: memory, MaxCPU: cfg.Limits.MaxCPU, MaxMemoryGiB: cfg.Limits.MaxMemoryGiB},
		Build: consoleBuildCapabilities{CPU: cfg.Build.CPU, MemoryGiB: cfg.Build.MemoryGiB, DiskGiB: cfg.Build.DiskGiB,
			MaxCPU: cfg.Build.MaxCPU, MaxMemoryGiB: cfg.Build.MaxMemoryGiB, MaxDiskGiB: cfg.Build.MaxDiskGiB, MaxConcurrent: cfg.Build.MaxConcurrent},
		ImportEnabled: workerErr == nil && h.sbBuildPinsDB != nil, TrustedWorkerAvailable: workerErr == nil, WarmingEnabled: warmingErr == nil && warming,
	})
}

func (h *Handler) consoleTemplateImport(w http.ResponseWriter, r *http.Request) {
	c, ok := h.consoleCaller(w, r)
	if !ok {
		return
	}
	writeConsoleE2B(w, captureConsoleE2B(consoleE2BRequest(r, c, "", "", nil), h.e2bTemplateImport))
}

func (h *Handler) consoleTemplateAliases(w http.ResponseWriter, r *http.Request) {
	c, ok := h.consoleCaller(w, r)
	if !ok {
		return
	}
	t := h.consoleTemplate(w, r, c)
	if t == nil {
		return
	}
	writeConsoleE2B(w, captureConsoleE2B(consoleE2BRequest(r, c, t.ID, "", nil), h.e2bTemplateAliases))
}

func (h *Handler) consoleTemplatePrepare(w http.ResponseWriter, r *http.Request) {
	c, ok := h.consoleCaller(w, r)
	if !ok {
		return
	}
	t := h.consoleTemplate(w, r, c)
	if t == nil {
		return
	}
	writeConsoleE2B(w, captureConsoleE2B(consoleE2BRequest(r, c, t.ID, "", nil), h.e2bPrepareTemplate))
}

// The console deliberately exposes a small, context-free recipe, not the full
// SDK build surface. In particular there is no COPY upload capability, arbitrary
// base image/registry configuration, or ENV editor. Runtime and builder sizes
// stay separate. The route chooses an existing project-owned base template.
type consoleBuildRequest struct {
	Name     string              `json:"name"`
	CPUCount int                 `json:"cpuCount"`
	MemoryMB int                 `json:"memoryMB"`
	Steps    []sandbox.BuildStep `json:"steps"`
	StartCmd string              `json:"startCmd,omitempty"`
	ReadyCmd string              `json:"readyCmd,omitempty"`
	sandbox.BuildResources
}

func decodeConsoleBuild(w http.ResponseWriter, r *http.Request) (*consoleBuildRequest, bool) {
	var b consoleBuildRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&b); err != nil {
		httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, "invalid build recipe")
		return nil, false
	}
	if decoder.Decode(new(any)) != io.EOF {
		httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, "expected one build recipe")
		return nil, false
	}
	if !templateTagRe.MatchString(b.Name) || b.CPUCount < 0 || b.MemoryMB < 0 {
		httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, "invalid template name or runtime size")
		return nil, false
	}
	if len(b.Steps) == 0 || len(b.Steps) > 100 {
		httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, "between 1 and 100 RUN steps are required")
		return nil, false
	}
	for _, step := range b.Steps {
		if step.Type != "RUN" || len(step.Args) < 1 || len(step.Args) > 2 || len(step.Args[0]) > 32<<10 || step.FilesHash != "" || step.Force {
			httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, "only bounded RUN commands with an optional user are supported")
			return nil, false
		}
		for _, arg := range step.Args {
			if strings.ContainsRune(arg, '\x00') {
				httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, "RUN commands must not contain NUL")
				return nil, false
			}
		}
	}
	for _, command := range []string{b.StartCmd, b.ReadyCmd} {
		if len(command) > 32<<10 || strings.ContainsRune(command, '\x00') {
			httpx.Err(w, http.StatusBadRequest, http.StatusBadRequest, "runtime commands must be at most 32768 bytes and contain no NUL")
			return nil, false
		}
	}
	return &b, true
}

func (h *Handler) consoleTemplateBuild(w http.ResponseWriter, r *http.Request) {
	c, ok := h.consoleCaller(w, r)
	if !ok {
		return
	}
	t := h.consoleTemplate(w, r, c)
	if t == nil {
		return
	}
	b, ok := decodeConsoleBuild(w, r)
	if !ok {
		return
	}
	// Preflight the core base/render validators before creating a reservation.
	// The launch handler re-resolves the base to preserve its scope/admission rules.
	base, err := h.resolveBase(r.Context(), c, "", t.ID, nil)
	if err == nil {
		_, err = sandbox.RenderDockerfile(base.Ref, b.Steps)
	}
	if err != nil {
		status, msg := e2bError(err)
		httpx.Err(w, status, status, msg)
		return
	}
	reservation, _ := json.Marshal(struct {
		Name     string `json:"name"`
		CPUCount int    `json:"cpuCount"`
		MemoryMB int    `json:"memoryMB"`
		sandbox.BuildResources
	}{b.Name, b.CPUCount, b.MemoryMB, b.BuildResources})
	request := consoleE2BRequest(r, c, "", "", reservation)
	response := captureConsoleE2B(request, func(w http.ResponseWriter, request *http.Request) {
		h.withTemplateNameLock(w, request, func(w http.ResponseWriter, request *http.Request) {
			// This is a new-template action, never an overwrite. Check names,
			// aliases and IDs under the same lock as import/aliases/delete.
			target, err := h.loadTemplate(request.Context(), c.proj.ID, b.Name)
			if err != nil {
				e2bFail(w, err)
				return
			}
			if target != nil {
				e2bErr(w, http.StatusConflict, "template name already exists; choose a new name")
				return
			}
			h.e2bTemplateRequest(w, request)
		})
	})
	if response.statusCode() >= 400 {
		writeConsoleE2B(w, response)
		return
	}
	var accepted struct {
		TemplateID string `json:"templateID"`
		BuildID    string `json:"buildID"`
	}
	if json.Unmarshal(response.body.Bytes(), &accepted) != nil || accepted.TemplateID == "" || accepted.BuildID == "" {
		httpx.Err(w, http.StatusInternalServerError, http.StatusInternalServerError, "invalid build reservation")
		return
	}
	launch, _ := json.Marshal(struct {
		FromTemplate string              `json:"fromTemplate"`
		Steps        []sandbox.BuildStep `json:"steps"`
		StartCmd     string              `json:"startCmd"`
		ReadyCmd     string              `json:"readyCmd"`
	}{t.ID, b.Steps, b.StartCmd, b.ReadyCmd})
	response = captureConsoleE2B(consoleE2BRequest(r, c, accepted.TemplateID, accepted.BuildID, launch), h.e2bTemplateBuildLocked)
	if response.statusCode() >= 400 {
		// Pre-launch failures (storage/admission/base revalidation) leave an
		// ordinary waiting reservation. Make this failed one-shot action
		// terminal; the core compensator owns already-claimed workers.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), templateMutationTimeout)
		_ = h.sbTemplates.DB().WithTx(cleanup, func(ctx context.Context) error {
			var current sandboxTemplateDoc
			found, err := h.sbTemplates.GetForUpdate(ctx, accepted.TemplateID, &current)
			if err != nil || !found || current.ProjectID != c.proj.ID {
				return err
			}
			if build := current.build(accepted.BuildID); build != nil && build.Status == sandbox.BuildWaiting {
				build.Status, build.Reason = sandbox.BuildError, "build could not be started; retry with a new template name"
				now := time.Now().UTC()
				build.FinishedAt = &now
				_, err = h.sbTemplates.Replace(ctx, current.ID, &current)
				return err
			}
			return nil
		})
		cancel()
		writeConsoleE2B(w, response)
		return
	}
	httpx.Raw(w, http.StatusAccepted, httpx.Response[map[string]string]{Data: &map[string]string{
		"templateID": accepted.TemplateID, "buildID": accepted.BuildID, "status": sandbox.BuildBuilding,
	}})
}
