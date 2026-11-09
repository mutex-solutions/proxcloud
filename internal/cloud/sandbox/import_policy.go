package sandbox

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/mutex-solutions/proxcloud/internal/sandboximport"
)

type ImportConfig struct {
	SourceRepositories map[string][]string `json:"sourceRepositories,omitempty"`
	AllowAnonymous     bool                `json:"allowAnonymous,omitempty"`
}

func (s *Service) importPolicy() sandboximport.Policy {
	return sandboximport.Policy{TemplateRegistry: s.cfg.TemplateRegistry, SourceRepositories: s.cfg.Import.SourceRepositories, AllowAnonymous: s.cfg.Import.AllowAnonymous}
}

// ValidateImportSource is a pure admission check; StartImport and the trusted
// worker independently enforce the same policy before selecting credentials.
func (s *Service) ValidateImportSource(ref, project string, hasExplicitCredentials bool) (bool, error) {
	managed, err := s.importPolicy().Source(ref, project)
	if err != nil {
		return false, badBuild("%s", err)
	}
	if managed && hasExplicitCredentials {
		return false, badBuild("managed project images require project pull verification, not caller credentials")
	}
	if !managed && !hasExplicitCredentials && !s.cfg.Import.AllowAnonymous {
		return false, badBuild("external imports require explicit source credentials; anonymous access is not enabled")
	}
	return managed, nil
}

// copyProjectPullSecret copies only this project's pull-only identity, never a
// shared robot or its push account. Worker and warming namespaces are trusted;
// customer images never mount this Secret. Missing credentials fail closed.
func (s *Service) copyProjectPullSecret(ctx context.Context, project, targetNS, name string) (bool, error) {
	if !(s.importPolicy().ManagedRepository(s.cfg.TemplateRegistry+"/"+project+"/00000000000000000000", project)) {
		return false, badBuild("invalid project registry scope")
	}
	data, err := s.api.GetSecretData(ctx, NamespaceFor(project), TemplatePullSecret)
	if err != nil {
		return false, fmt.Errorf("project pull identity is unavailable")
	}
	dc := data[".dockerconfigjson"]
	if len(dc) == 0 {
		return false, fmt.Errorf("project pull identity is unavailable")
	}
	host, _, _ := strings.Cut(s.cfg.TemplateRegistry, "/")
	if _, err := sandboximport.DockerCredential(dc, host); err != nil {
		return false, fmt.Errorf("project pull identity is invalid")
	}
	err = s.api.ApplyCore(ctx, "secrets", map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson", "metadata": map[string]any{"name": name, "namespace": targetNS, "labels": map[string]any{LabelManagedBy: ManagedByValue, LabelService: s.serviceID, LabelProject: project}}, "data": map[string]any{".dockerconfigjson": base64.StdEncoding.EncodeToString(dc)}})
	return err == nil, err
}
