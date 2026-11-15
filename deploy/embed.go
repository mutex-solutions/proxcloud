// Package deploy embeds deployment files that the API reads at run time. go:embed cannot reach a parent directory, so a
// file under deploy/ that Go code needs is embedded here, as deploy/seed/seed.go does for its documents.
package deploy

import _ "embed"

// PlatformBOM is the platform bill of materials of this release (platform/platform-bom.yaml, docs/adr/0009). It is
// parsed and validated by internal/platformlife.
//
//go:embed platform/platform-bom.yaml
var PlatformBOM []byte

// MgmtArgoCDHealth and DbaasArgoCDHealth are the argocd-cm health checks of the kinds the tenant cluster and database
// charts render (mgmt-cluster/argocd-health.yaml, dbaas-cluster/argocd-health.yaml). The platform's workload ArgoCD
// carries them as chart values (internal/platformlife), where installer stage 50 patches them into argocd-cm.
//
//go:embed mgmt-cluster/argocd-health.yaml
var MgmtArgoCDHealth []byte

//go:embed dbaas-cluster/argocd-health.yaml
var DbaasArgoCDHealth []byte
