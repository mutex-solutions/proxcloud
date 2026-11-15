// Package deploy embeds deployment files that the API reads at run time. go:embed cannot reach a parent directory, so a
// file under deploy/ that Go code needs is embedded here, as deploy/seed/seed.go does for its documents.
package deploy

import _ "embed"

// PlatformBOM is the platform bill of materials of this release (platform/platform-bom.yaml, docs/adr/0009). It is
// parsed and validated by internal/platformlife.
//
//go:embed platform/platform-bom.yaml
var PlatformBOM []byte
