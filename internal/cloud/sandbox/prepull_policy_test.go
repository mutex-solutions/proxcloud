package sandbox

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The portal's DaemonSet writes are fenced by a ValidatingAdmissionPolicy rendered by installer stage 55
// (installer/playbooks/templates/sandbox-prepull.yml.j2). Its behaviour is evaluated with k8s.io/apiserver's CEL
// libraries in test/policy (its own module, run by installer/tests and CI); this checks the policy TEXT against what
// SetPrepullImage sends: the names it patches, the identity, the image grammar and the field lists.
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
	// The field lists live in group_vars (shared with the stage-55 schema check) and cover the fields the portal's
	// patch could smuggle a change into, including the Kubernetes 1.35 additions.
	fields, err := os.ReadFile("../../../installer/playbooks/group_vars/all/prepull-policy.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"containers", "volumes", "imagePullSecrets", "serviceAccountName", "hostNetwork", "securityContext",
		"nodeSelector", "tolerations", "runtimeClassName", "automountServiceAccountToken", "command", "args", "env", "volumeMounts",
		"hostnameOverride", "workloadRef", "restartPolicyRules"} {
		if !regexp.MustCompile(`[\[ ,]` + f + `[,\]]`).Match(fields) {
			t.Errorf("the policy does not compare %s", f)
		}
	}
	if !strings.Contains(text, "pf.pod") || !strings.Contains(text, "pf.container") || !strings.Contains(text, "o.image.find(variables.repo) == c.image.find(variables.repo)") {
		t.Error("the policy does not use the shared field lists or the same-repository rule")
	}
	// The image grammar the policy accepts (registry rendered in) is exactly what SetPrepullImage writes.
	m := regexp.MustCompile(`r'\^\{\{ pc\.sandbox\.registry\.address \| regex_escape \}\}(/library/sandbox-[^']*@sha256[^']*)'`).FindStringSubmatch(text)
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
