package sandbox

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The portal's DaemonSet writes are fenced by a ValidatingAdmissionPolicy rendered by installer stage 55
// (installer/playbooks/templates/sandbox-prepull.yml.j2). There is no CEL runtime in this repository and no cluster in
// CI, so this is a kubectl-free check of the policy TEXT against what SetPrepullImage sends: the names it patches, the
// identity, and the image grammar. The behavioural cases (docs/template-updates.md §4, "Admission policy test cases")
// need a real API server (kind/k3s): open item.
func TestPrepullPolicyTextMatchesThePatch(t *testing.T) {
	raw, err := os.ReadFile("../../../installer/playbooks/templates/sandbox-prepull.yml.j2")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		// RBAC: get/patch by name on exactly the two DaemonSets SetPrepullImage touches.
		"resources: [daemonsets]", "resourceNames: [" + PrepullDaemonSet + ", " + PrepullGPUDaemonSet + "]", "verbs: [get, patch]",
		// bound to the sandbox provider's identity (deploy/sandbox-cluster/rbac.yaml)
		"subjects: [{kind: ServiceAccount, name: proxcloud, namespace: proxcloud-system}]",
		`request.userInfo.username == "system:serviceaccount:proxcloud-system:proxcloud"`,
		"failurePolicy: Fail", "validationActions: [Deny]", "operations: [CREATE, UPDATE, DELETE]",
		`object.metadata.name in ["` + PrepullDaemonSet + `", "` + PrepullGPUDaemonSet + `"]`,
		`request.operation == "UPDATE"`,
		// no init container removed, at most one added, a new one is p<N>
		"variables.oldInit.all(o, variables.newInit.exists(c, c.name == o.name))",
		"variables.newInit.size() <= variables.oldInit.size() + 1",
		`c.name.matches(r'^p[0-9]{1,4}$')`,
		"c.image.matches(variables.ref)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the prepull policy lacks %q", want)
		}
	}
	// The pod/container field lists cover every field the portal's patch could smuggle a change into.
	for _, f := range []string{"'containers'", "'volumes'", "'imagePullSecrets'", "'serviceAccountName'", "'hostNetwork'", "'securityContext'",
		"'nodeSelector'", "'tolerations'", "'runtimeClassName'", "'automountServiceAccountToken'", "'command'", "'args'", "'env'", "'volumeMounts'"} {
		if !strings.Contains(text, f) {
			t.Errorf("the policy does not compare %s", f)
		}
	}
	// The image grammar the policy accepts (registry rendered in) is exactly what SetPrepullImage writes.
	m := regexp.MustCompile(`r'\^\{\{ pc\.sandbox\.registry\.address \| regex_escape \}\}(/library/sandbox-[^']*)'`).FindStringSubmatch(text)
	if m == nil {
		t.Fatal("no ref regex in the policy")
	}
	rx := regexp.MustCompile("^" + regexp.QuoteMeta(mirrorReg) + m[1])
	for _, ok := range []string{mirrorReg + "/library/sandbox-base:20261112-9d00563@" + dNew, mirrorReg + "/library/sandbox-base@" + dNew} {
		if !rx.MatchString(ok) {
			t.Errorf("policy refuses %s", ok)
		}
		if !builtinRepoRE.MatchString(strings.SplitN(strings.SplitN(ok, "@", 2)[0], ":20", 2)[0]) {
			t.Errorf("SetPrepullImage refuses %s", ok)
		}
	}
	for _, bad := range []string{mirrorReg + "/library/sandbox-base:20261112-9d00563", mirrorReg + "/library/proxcloud-sandbox-import@" + dNew,
		mirrorReg + "/t/p/x@" + dNew, "ghcr.io/mutex-solutions/sandbox-base@" + dNew, mirrorReg + "/library/sandbox-base:latest@" + dNew} {
		if rx.MatchString(bad) {
			t.Errorf("policy accepts %s", bad)
		}
	}
}
