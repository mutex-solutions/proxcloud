// Package deploy embeds deployment files that the API reads at run time. go:embed cannot reach a parent directory, so a
// file under deploy/ that Go code needs is embedded here, as deploy/seed/seed.go does for its documents.
package deploy

import "embed"

// PlatformBOM is the platform bill of materials of this release (platform/platform-bom.yaml, docs/adr/0009). It is
// parsed and validated by internal/platformlife.
//
//go:embed platform/platform-bom.yaml
var PlatformBOM []byte

// PreviousBOMs are the platform BOMs of earlier releases, platform/previous/<release>.yaml, each byte for byte the
// platform-bom.yaml of its tag: a platform records the BOM that installed it by the sha256 of that file, and its
// applies keep running with it (internal/platformlife, Previous). docs/ci-release.md says how one is added.
//
//go:embed platform/previous/*.yaml
var PreviousBOMs embed.FS

// MgmtArgoCDHealth and DbaasArgoCDHealth are the argocd-cm health checks of the kinds the tenant cluster and database
// charts render (mgmt-cluster/argocd-health.yaml, dbaas-cluster/argocd-health.yaml). The platform's workload ArgoCD
// carries them as chart values (internal/platformlife), where installer stage 50 patches them into argocd-cm.
//
//go:embed mgmt-cluster/argocd-health.yaml
var MgmtArgoCDHealth []byte

//go:embed dbaas-cluster/argocd-health.yaml
var DbaasArgoCDHealth []byte
