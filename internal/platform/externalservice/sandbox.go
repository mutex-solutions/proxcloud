package externalservice

// sandbox.go — typed accessors for the "sandbox" provider kind (config.provider == "sandbox"):
// customer sandboxes as gVisor pods on a dedicated Kubernetes cluster. Document shape:
//
//	{
//	  "name": "Sandboxes AZ1", "type": "CLOUD", "status": "PRIVATE",
//	  "config": {
//	    "provider": "sandbox",
//	    "region":   "az1",
//	    "regions":  {"az1": {}},
//	    "services": {"sandbox": {"az1": true}},
//	    "sandbox": {
//	      "runtimeClass": "gvisor",                    // default
//	      "templates": {                               // the ONLY images offered
//	        "python":  {"image": "python:3.12-slim", "cpu": 1, "memoryGiB": 2, "description": "Python 3.12"},
//	        "pytorch": {"image": "pytorch/pytorch:2.9.1-cuda12.8-cudnn9-runtime", "cpu": 4, "memoryGiB": 16,
//	                    "gpu": {"model": "nvidia-3090", "count": 1}}  // optional default GPU
//	      },
//	      "gpuModels": ["nvidia-3090"],                // node label node-restriction.kubernetes.io/proxcloud-gpu values
//	      "envd":      true,                           // run E2B's envd (E2B-compatible API)
//	      "domain":    "sbx.example.org",              // E2B_DOMAIN served by sandbox-proxy
//	      "templateRegistry": "registry.example.org/t",  // custom templates + snapshots (t/<project>/<template>)
//	      "registryPrune":    "on",                      // "dry-run" logs, "" off (secret.registryGcUser/Password)
//	      "buildStorage": {"endpoint": "http://s3.internal:9000", "bucket": "pc-sandbox-builds", "region": "us-east-1",
//	                       "podEndpoint": ""},           // contexts of template builds (secret.buildStorageAccessKey/SecretKey)
//	      "limits": {"maxCpu": 8, "maxMemoryGiB": 32, "maxGpus": 1, "maxTimeoutSeconds": 86400,
//	                 "defaultTimeoutSeconds": 3600, "maxConcurrent": 10, "maxConcurrentGpus": 1}
//	    }
//	  },
//	  "secret": {"kubeconfig": "<proxcloud SA kubeconfig of the sandbox cluster>",
//	             "proxyWakeToken": "<the bearer sandbox-proxy sends to the wake endpoint (stage 56 generates it)>"}
//	}
//
// Like dbaas, nothing lands on PVE for a project: the sandbox cluster belongs to the platform.
// It goes live only while its release gates hold (docs/managed-sandbox.md §3, admin/sandboxprovider.go).

import (
	"encoding/json"
	"github.com/mutex-solutions/proxcloud/internal/cloud/sandbox"
)

// IsSandbox reports whether this is a sandbox provider (config.provider == "sandbox").
func (e *ExternalService) IsSandbox() bool { return e.Provider() == "sandbox" }

// SandboxProxmoxServiceID is config.sandbox.proxmoxServiceId: the Proxmox provider whose config.sandbox pool holds this
// provider's workers. Its sync fences them (SB2b) and the release gates read them through it.
func (e *ExternalService) SandboxProxmoxServiceID() string {
	sb, _ := e.Config["sandbox"].(map[string]any)
	return str(sb["proxmoxServiceId"])
}

// SandboxQuotaKeys are the per-project sandbox quota keys (decision D10), stored on the project's quota document next
// to the Proxmox keys.
var SandboxQuotaKeys = []string{"sandboxes", "sandboxVcpus", "sandboxRamMb", "sandboxGpus"}

// SandboxProjectQuota resolves a project's sandbox quota key by key: the project's own value, else the provider's
// default (config.sandbox.projectQuota), else sandbox.DefaultProjectQuota. Never unlimited: an absent key falls back,
// it does not lift the limit.
func (e *ExternalService) SandboxProjectQuota(project map[string]any) sandbox.ProjectQuota {
	sb, _ := e.Config["sandbox"].(map[string]any)
	provider, _ := sb["projectQuota"].(map[string]any)
	d := sandbox.DefaultProjectQuota
	pick := func(key string, def int) int {
		for _, m := range []map[string]any{project, provider} {
			if v, ok := m[key]; ok {
				if n := intOf(v); n >= 0 {
					return n
				}
			}
		}
		return def
	}
	return sandbox.ProjectQuota{
		Sandboxes: pick("sandboxes", d.Sandboxes),
		VCPUs:     pick("sandboxVcpus", d.VCPUs),
		RAMMb:     pick("sandboxRamMb", d.RAMMb),
		GPUs:      pick("sandboxGpus", d.GPUs),
	}
}

// SandboxRegion is config.region, falling back to the first configured region.
func (e *ExternalService) SandboxRegion() string {
	if r := str(e.Config["region"]); r != "" {
		return r
	}
	if rs := e.RegionNames(); len(rs) > 0 {
		return rs[0]
	}
	return ""
}

func mirrorCredential(password string) sandbox.RegistryCredential {
	if password == "" {
		return sandbox.RegistryCredential{}
	}
	return sandbox.RegistryCredential{User: sandbox.RegistryMirrorUser, Password: password}
}

// SandboxConfig assembles sandbox.Config: the decrypted kubeconfig (secret.kubeconfig) plus the
// config.sandbox block.
func (e *ExternalService) SandboxConfig() sandbox.Config {
	sb, _ := e.Config["sandbox"].(map[string]any)
	templates := map[string]sandbox.Template{}
	if ts, ok := sb["templates"].(map[string]any); ok {
		for name, raw := range ts {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			templates[name] = sandbox.Template{
				EgressMbps:  intOf(m["egressMbps"]),
				Image:       str(m["image"]),
				Description: str(m["description"]),
				CPU:         intOf(m["cpu"]),
				MemoryGiB:   intOf(m["memoryGiB"]),
				Command:     strList(m["command"]),
				StartCmd:    str(m["startCmd"]),
				ReadyCmd:    str(m["readyCmd"]),
				User:        str(m["user"]),
				Workdir:     str(m["workdir"]),
				StartAsRoot: m["startAsRoot"] == true,
			}
			if g, ok := m["gpu"].(map[string]any); ok && str(g["model"]) != "" {
				t := templates[name]
				t.GPU = &sandbox.GPURequest{Model: str(g["model"]), Count: intOf(g["count"])}
				templates[name] = t
			}
		}
	}
	limits, _ := sb["limits"].(map[string]any)
	bs, _ := sb["buildStorage"].(map[string]any)
	var abuse sandbox.AbusePolicy
	var rotation sandbox.RotationPolicy
	var startup sandbox.StartupConfig
	var build sandbox.BuildConfig
	var warming sandbox.WarmConfig
	var imports sandbox.ImportConfig
	decode := func(key string, dst any) { b, _ := json.Marshal(sb[key]); _ = json.Unmarshal(b, dst) }
	decode("startup", &startup)
	decode("build", &build)
	decode("warming", &warming)
	decode("import", &imports)
	if b, err := json.Marshal(sb["abuse"]); err == nil {
		_ = json.Unmarshal(b, &abuse)
	}
	if b, err := json.Marshal(sb["rotation"]); err == nil {
		_ = json.Unmarshal(b, &rotation)
	}
	rotation.Kubeconfig = str(e.secretMap()["rotationKubeconfig"])
	return sandbox.Config{
		EgressMbps: intOf(sb["egressMbps"]), Abuse: abuse, Rotation: rotation,
		Startup: startup, Build: build, Warming: warming, Import: imports,
		BuildStorage: sandbox.BuildStorage{
			Endpoint: str(bs["endpoint"]), PodEndpoint: str(bs["podEndpoint"]), Bucket: str(bs["bucket"]), Region: str(bs["region"]),
			AccessKey: str(e.secretMap()["buildStorageAccessKey"]), SecretKey: str(e.secretMap()["buildStorageSecretKey"]),
		},
		Kubeconfig:       str(e.secretMap()["kubeconfig"]),
		Region:           e.SandboxRegion(),
		RuntimeClass:     str(sb["runtimeClass"]),
		Templates:        templates,
		GPUModels:        strList(sb["gpuModels"]),
		Envd:             sb["envd"] == true,
		Domain:           str(sb["domain"]),
		RegistryPrune:    str(sb["registryPrune"]),
		ImageAdoption:    str(sb["imageAdoption"]),
		TemplateRegistry: str(sb["templateRegistry"]),
		RegistryCA:       str(sb["registryCa"]),
		ProxyWakeToken:   str(e.secretMap()["proxyWakeToken"]),
		RegistryGC: sandbox.RegistryCredential{
			User: str(e.secretMap()["registryGcUser"]), Password: str(e.secretMap()["registryGcPassword"]),
		},
		// secret.registryMirror: the password of the registry's fixed "mirror" account (installer stage 60).
		RegistryMirror: mirrorCredential(str(e.secretMap()["registryMirror"])),
		Limits: sandbox.Limits{
			MaxCPU:                intOf(limits["maxCpu"]),
			MaxMemoryGiB:          intOf(limits["maxMemoryGiB"]),
			MaxGPUs:               intOf(limits["maxGpus"]),
			MaxTimeoutSeconds:     intOf(limits["maxTimeoutSeconds"]),
			DefaultTimeoutSeconds: intOf(limits["defaultTimeoutSeconds"]),
			MaxConcurrent:         intOf(limits["maxConcurrent"]),
			MaxConcurrentGPUs:     intOf(limits["maxConcurrentGpus"]),
		},
	}
}
