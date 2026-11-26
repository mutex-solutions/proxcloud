package policy

// The admission policies of the sandbox chart (deploy/charts/proxcloud-sandbox, mode shared), rendered with helm and
// run through the API server's CEL libraries: the pod policy against the pods the portal's BuildCR makes
// (testdata/buildcr-pods.json, written by internal/cloud/sandbox TestPolicyFixtureIsBuildCR) and against the shapes it
// exists to refuse; the portal identity's namespace and RoleBinding policies; the firewall identity's node policy.
// With functions.enabled (the two -functions examples): the same policies with the fn-* namespaces let in, the pod
// policy's two rules for a function pod against the pods BuildDeployment makes (testdata/function-pods.json, written
// by internal/cloud/sandbox TestPolicyFixtureIsBuildDeployment), and proxcloud-function-objects in both modes.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/utils/ptr"
)

const (
	sbxPortal   = "system:serviceaccount:sandbox-system:proxcloud-sandbox"
	sbxFirewall = "system:serviceaccount:sandbox-system:proxcloud-sandbox-firewall"
	groupLabel  = "node-restriction.kubernetes.io/proxcloud-node-group"
)

var tenantLabels = map[string]string{"proxcloud.io/tenant": "true"}

func sandboxPolicies(t *testing.T) map[string]*admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	return sandboxPoliciesOf(t, "shared")
}

// sandboxPoliciesOf are the policies the chart renders with one of its examples.
func sandboxPoliciesOf(t *testing.T, example string) map[string]*admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	const chart = "../../deploy/charts/proxcloud-sandbox"
	cmd := exec.Command(helm, "template", "sandbox", chart, "-n", "sandbox-system", "-f", chart+"/examples/values-"+example+".yaml")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	policies := map[string]*admissionregistrationv1.ValidatingAdmissionPolicy{}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var m map[string]any
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if m["kind"] != "ValidatingAdmissionPolicy" {
			continue
		}
		b, _ := json.Marshal(m)
		var p admissionregistrationv1.ValidatingAdmissionPolicy
		if err := json.Unmarshal(b, &p); err != nil {
			t.Fatal(err)
		}
		if p.Spec.FailurePolicy == nil || *p.Spec.FailurePolicy != admissionregistrationv1.Fail {
			t.Errorf("%s: failurePolicy must be Fail", p.Name)
		}
		policies[p.Name] = &p
	}
	return policies
}

type sbxRequest struct {
	op          admission.Operation
	gvk         schema.GroupVersionKind
	resource    string
	subresource string
	ns          string
	nsLabels    map[string]string
	name        string
	user        string
	old, obj    runtime.Object
	// gvr is the resource asked for when it is not the kind's own: a scale is asked of apps/v1 deployments.
	gvr *schema.GroupVersionResource
}

func decideSbx(t *testing.T, v validating.Validator, r sbxRequest) (bool, string) {
	t.Helper()
	gvr := r.gvk.GroupVersion().WithResource(r.resource)
	if r.gvr != nil {
		gvr = *r.gvr
	}
	for _, o := range []runtime.Object{r.old, r.obj} {
		if o != nil {
			o.GetObjectKind().SetGroupVersionKind(r.gvk) // as decoded by the API server; NewVersionedAttributes needs it
		}
	}
	attr := admission.NewAttributesRecord(r.obj, r.old, r.gvk, r.ns, r.name, gvr, r.subresource, r.op, nil, false, &user.DefaultInfo{Name: r.user})
	va, err := admission.NewVersionedAttributes(attr, r.gvk, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ns *corev1.Namespace
	if r.ns != "" {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: r.ns, Labels: r.nsLabels}}
	}
	res := v.Validate(context.Background(), gvr, va, nil, ns, celconfig.RuntimeCELCostBudget, nil)
	for _, d := range res.Decisions {
		if d.Evaluation == validating.EvalError {
			t.Errorf("evaluation error: %s", d.Message)
		}
		if d.Action != validating.ActionAdmit {
			return false, d.Message
		}
	}
	return true, ""
}

func buildCRPods(t *testing.T) map[string]*corev1.Pod {
	t.Helper()
	b, err := os.ReadFile("testdata/buildcr-pods.json")
	if err != nil {
		t.Fatal(err)
	}
	var pods map[string]*corev1.Pod
	if err := json.Unmarshal(b, &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods) < 3 {
		t.Fatalf("%d fixture pods", len(pods))
	}
	return pods
}

// Every pod the portal makes is admitted; every way to reach the node from a pod of a tenant namespace is refused, even
// for an identity that can write Sandbox CRs at will (the agent-sandbox controller creates the pod from one).
func TestSandboxTenantPodPolicy(t *testing.T) {
	p := sandboxPolicies(t)["proxcloud-sandbox-tenant-pods"]
	if p == nil {
		t.Fatal("no proxcloud-sandbox-tenant-pods")
	}
	if sel := p.Spec.MatchConstraints.NamespaceSelector; sel == nil || sel.MatchLabels["proxcloud.io/tenant"] != "true" {
		t.Errorf("the pod policy must select the tenant namespaces: %v", sel)
	}
	v := compile(t, p)
	podGVK := schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
	controller := "system:serviceaccount:agent-sandbox-system:agent-sandbox-controller"
	create := func(pod *corev1.Pod) sbxRequest {
		return sbxRequest{op: admission.Create, gvk: podGVK, resource: "pods", ns: "sb-p1", nsLabels: tenantLabels, name: pod.Name, user: controller, obj: pod}
	}
	for name, pod := range buildCRPods(t) {
		if ok, msg := decideSbx(t, v, create(pod)); !ok {
			t.Errorf("BuildCR %s pod refused: %s", name, msg)
		}
		bound := pod.DeepCopy()
		bound.Spec.NodeName = "pc-platform-sandbox-pve1-abc"
		bound.Labels = map[string]string{"x": "y"}
		if ok, msg := decideSbx(t, v, sbxRequest{op: admission.Update, gvk: podGVK, resource: "pods", ns: "sb-p1", nsLabels: tenantLabels, name: pod.Name, user: controller, old: bound, obj: bound}); !ok {
			t.Errorf("BuildCR %s: an update of a scheduled pod refused: %s", name, msg)
		}
	}
	base := buildCRPods(t)["envd"]
	for _, c := range []struct {
		name string
		mut  func(*corev1.Pod)
	}{
		{"no runtimeClassName (runc)", func(p *corev1.Pod) { p.Spec.RuntimeClassName = nil }},
		{"runtimeClassName runc", func(p *corev1.Pod) { p.Spec.RuntimeClassName = ptr.To("runc") }},
		{"privileged", func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
		}},
		{"added capability", func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}}
		}},
		{"privileged init container", func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "i", Image: "x", SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)}}}
		}},
		{"hostNetwork", func(p *corev1.Pod) { p.Spec.HostNetwork = true }},
		{"hostPID", func(p *corev1.Pod) { p.Spec.HostPID = true }},
		{"hostIPC", func(p *corev1.Pod) { p.Spec.HostIPC = true }},
		{"nodeName at create (a kamaji node)", func(p *corev1.Pod) { p.Spec.NodeName = "pc-platform-kamaji-pve1-abc" }},
		{"hostPath /", func(p *corev1.Pod) { p.Spec.Volumes[0].HostPath.Path = "/" }},
		{"hostPath out of the runtime dir by ..", func(p *corev1.Pod) { p.Spec.Volumes[0].HostPath.Path = "/opt/proxcloud-sandbox/bin/../../../etc" }},
		{"runtime dir read-write", func(p *corev1.Pod) { p.Spec.Containers[0].VolumeMounts[0].ReadOnly = false }},
		{"second hostPath volume", func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "etcd", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/etcd"}}})
		}},
		{"nfs volume", func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "n", VolumeSource: corev1.VolumeSource{NFS: &corev1.NFSVolumeSource{Server: "10.0.0.1", Path: "/"}}})
		}},
		{"hostPort", func(p *corev1.Pod) {
			p.Spec.Containers[0].Ports = []corev1.ContainerPort{{ContainerPort: 80, HostPort: 80}}
		}},
		{"tolerates proxcloud.io/unsecured", func(p *corev1.Pod) {
			p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{Key: "proxcloud.io/unsecured", Operator: corev1.TolerationOpExists})
		}},
		{"tolerates every taint", func(p *corev1.Pod) {
			p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{Operator: corev1.TolerationOpExists})
		}},
	} {
		pod := base.DeepCopy()
		c.mut(pod)
		if ok, _ := decideSbx(t, v, create(pod)); ok {
			t.Errorf("%s: admitted", c.name)
		}
	}
	// The other way into a running pod.
	debug := base.DeepCopy()
	debug.Spec.NodeName = "pc-platform-sandbox-pve1-abc"
	withEphemeral := debug.DeepCopy()
	withEphemeral.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
		Name: "dbg", Image: "busybox", SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)}}}}
	if ok, _ := decideSbx(t, v, sbxRequest{op: admission.Update, gvk: podGVK, resource: "pods", subresource: "ephemeralcontainers", ns: "sb-p1", nsLabels: tenantLabels, name: debug.Name, user: sbxPortal, old: debug, obj: withEphemeral}); ok {
		t.Error("a privileged ephemeral container: admitted")
	}
	if !hasRule(p, "pods/ephemeralcontainers") {
		t.Error("the pod policy must match pods/ephemeralcontainers")
	}
}

func hasRule(p *admissionregistrationv1.ValidatingAdmissionPolicy, resource string) bool {
	for _, r := range p.Spec.MatchConstraints.ResourceRules {
		for _, res := range r.Resources {
			if res == resource {
				return true
			}
		}
	}
	return false
}

func TestSandboxPortalNamespacePolicy(t *testing.T) {
	v := compile(t, sandboxPolicies(t)["proxcloud-sandbox-namespaces"])
	gvk := schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	ns := func(name string, labels map[string]string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	req := func(op admission.Operation, u string, old, obj *corev1.Namespace) sbxRequest {
		r := sbxRequest{op: op, gvk: gvk, resource: "namespaces", user: u}
		if obj != nil {
			r.name, r.obj = obj.Name, obj
		}
		if old != nil {
			r.name, r.old = old.Name, old
		}
		return r
	}
	for _, c := range []struct {
		name  string
		r     sbxRequest
		admit bool
	}{
		{"create sb-p1 labelled", req(admission.Create, sbxPortal, nil, ns("sb-p1", tenantLabels)), true},
		{"update sb-p1 keeping the label", req(admission.Update, sbxPortal, ns("sb-p1", tenantLabels), ns("sb-p1", map[string]string{"proxcloud.io/tenant": "true", "a": "b"})), true},
		{"delete sb-p1", req(admission.Delete, sbxPortal, ns("sb-p1", tenantLabels), nil), true},
		{"another identity is not matched", req(admission.Delete, "system:serviceaccount:argocd:x", ns("kamaji-system", nil), nil), true},
		{"create sb-p1 without the label", req(admission.Create, sbxPortal, nil, ns("sb-p1", nil)), false},
		{"drop the label of sb-p1", req(admission.Update, sbxPortal, ns("sb-p1", tenantLabels), ns("sb-p1", nil)), false},
		{"label kamaji-system as a tenant", req(admission.Update, sbxPortal, ns("kamaji-system", nil), ns("kamaji-system", tenantLabels)), false},
		{"delete kamaji-system", req(admission.Delete, sbxPortal, ns("kamaji-system", nil), nil), false},
		{"functions off: create fn-p1 labelled", req(admission.Create, sbxPortal, nil, ns("fn-p1", tenantLabels)), false},
	} {
		if ok, msg := decideSbx(t, v, c.r); ok != c.admit {
			t.Errorf("%s: admitted %v (%s), want %v", c.name, ok, msg, c.admit)
		}
	}
}

func TestSandboxPortalRoleBindingPolicy(t *testing.T) {
	v := compile(t, sandboxPolicies(t)["proxcloud-sandbox-rolebindings"])
	gvk := schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"}
	self := rbacv1.Subject{Kind: "ServiceAccount", Name: "proxcloud-sandbox", Namespace: "sandbox-system"}
	rb := func(ns, role string, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
		return &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "proxcloud-sandbox", Namespace: ns},
			RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: role}, Subjects: subjects}
	}
	create := func(obj *rbacv1.RoleBinding, nsLabels map[string]string) sbxRequest {
		return sbxRequest{op: admission.Create, gvk: gvk, resource: "rolebindings", ns: obj.Namespace, nsLabels: nsLabels, name: obj.Name, user: sbxPortal, obj: obj}
	}
	for _, c := range []struct {
		name  string
		r     sbxRequest
		admit bool
	}{
		{"the namespaced role to itself in sb-p1", create(rb("sb-p1", "proxcloud-sandbox-namespace", self), tenantLabels), true},
		{"in kamaji-system", create(rb("kamaji-system", "proxcloud-sandbox-namespace", self), nil), false},
		{"in an unlabelled sb-p2", create(rb("sb-p2", "proxcloud-sandbox-namespace", self), nil), false},
		{"cluster-admin", create(rb("sb-p1", "cluster-admin", self), tenantLabels), false},
		{"to the namespace's default service account", create(rb("sb-p1", "proxcloud-sandbox-namespace", rbacv1.Subject{Kind: "ServiceAccount", Name: "default", Namespace: "sb-p1"}), tenantLabels), false},
		{"to a user", create(rb("sb-p1", "proxcloud-sandbox-namespace", self, rbacv1.Subject{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "x"}), tenantLabels), false},
		{"functions off: the function role in fn-p1", create(rb("fn-p1", "proxcloud-function-namespace", self), tenantLabels), false},
		{"functions off: the sandbox role in fn-p1", create(rb("fn-p1", "proxcloud-sandbox-namespace", self), tenantLabels), false},
	} {
		if ok, msg := decideSbx(t, v, c.r); ok != c.admit {
			t.Errorf("%s: admitted %v (%s), want %v", c.name, ok, msg, c.admit)
		}
	}
}

func TestSandboxFirewallNodePolicy(t *testing.T) {
	v := compile(t, sandboxPolicies(t)["proxcloud-sandbox-firewall-nodes"])
	gvk := schema.GroupVersionKind{Version: "v1", Kind: "Node"}
	node := func(group string, taints ...corev1.Taint) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{groupLabel: group}}, Spec: corev1.NodeSpec{Taints: taints}}
	}
	unsecured := corev1.Taint{Key: "proxcloud.io/unsecured", Value: "true", Effect: corev1.TaintEffectNoSchedule}
	req := func(op admission.Operation, u string, old, obj *corev1.Node) sbxRequest {
		r := sbxRequest{op: op, gvk: gvk, resource: "nodes", name: "n1", user: u, old: old}
		if obj != nil {
			r.obj = obj
		}
		return r
	}
	for _, c := range []struct {
		name  string
		r     sbxRequest
		admit bool
	}{
		{"untaint a sandbox node", req(admission.Update, sbxFirewall, node("sandbox", unsecured), node("sandbox")), true},
		{"another identity is not matched", req(admission.Update, "system:node:n1", node("kamaji"), node("kamaji", unsecured)), true},
		{"taint a kamaji node", req(admission.Update, sbxFirewall, node("kamaji"), node("kamaji", unsecured)), false},
		{"relabel a kamaji node as sandbox", req(admission.Update, sbxFirewall, node("kamaji"), node("sandbox")), false},
		{"relabel a sandbox node away", req(admission.Update, sbxFirewall, node("sandbox"), node("system")), false},
		{"delete a sandbox node", req(admission.Delete, sbxFirewall, node("sandbox"), nil), false},
	} {
		if ok, msg := decideSbx(t, v, c.r); ok != c.admit {
			t.Errorf("%s: admitted %v (%s), want %v", c.name, ok, msg, c.admit)
		}
	}
}

// ---- functions.enabled ----

func functionPods(t *testing.T) map[string]*corev1.Pod {
	t.Helper()
	b, err := os.ReadFile("testdata/function-pods.json")
	if err != nil {
		t.Fatal(err)
	}
	var pods map[string]*corev1.Pod
	if err := json.Unmarshal(b, &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods) < 2 {
		t.Fatalf("%d fixture pods", len(pods))
	}
	return pods
}

// With functions on, in both modes: every pod BuildDeployment makes is admitted in an fn-* namespace and every pod
// BuildCR makes still is in an sb-* one; a function pod is held to gVisor like any tenant pod and, in fn-* alone, to
// no mounted token and no volume of any kind (a hostPath, an emptyDir, a Secret, a projected token), whoever creates
// it: the Deployment's controller does, from a Deployment the portal's identity wrote.
func TestFunctionPodPolicy(t *testing.T) {
	podGVK := schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
	controller := "system:serviceaccount:kube-system:replicaset-controller"
	for _, example := range []string{"shared-functions", "separate-functions"} {
		p := sandboxPoliciesOf(t, example)["proxcloud-sandbox-tenant-pods"]
		if p == nil {
			t.Fatalf("%s: no proxcloud-sandbox-tenant-pods", example)
		}
		v := compile(t, p)
		in := func(ns string, pod *corev1.Pod) sbxRequest {
			return sbxRequest{op: admission.Create, gvk: podGVK, resource: "pods", ns: ns, nsLabels: tenantLabels, name: pod.Name, user: controller, obj: pod}
		}
		for name, pod := range functionPods(t) {
			if pod.Namespace != "fn-p1" {
				t.Fatalf("fixture pod %s is in %q", name, pod.Namespace)
			}
			if ok, msg := decideSbx(t, v, in("fn-p1", pod)); !ok {
				t.Errorf("%s: BuildDeployment %s pod refused: %s", example, name, msg)
			}
			bound := pod.DeepCopy()
			bound.Spec.NodeName = "pc-platform-sandbox-pve1-abc"
			if ok, msg := decideSbx(t, v, sbxRequest{op: admission.Update, gvk: podGVK, resource: "pods", ns: "fn-p1", nsLabels: tenantLabels, name: pod.Name, user: controller, old: bound, obj: bound}); !ok {
				t.Errorf("%s: BuildDeployment %s: an update of a scheduled pod refused: %s", example, name, msg)
			}
		}
		for name, pod := range buildCRPods(t) {
			if ok, msg := decideSbx(t, v, in("sb-p1", pod)); !ok {
				t.Errorf("%s: BuildCR %s pod refused in sb-p1 with functions on: %s", example, name, msg)
			}
			// A sandbox pod mounts the runtime directory: what fn-* never admits.
			if ok, _ := decideSbx(t, v, in("fn-p1", pod)); ok {
				t.Errorf("%s: BuildCR %s pod (a hostPath volume) admitted in fn-p1", example, name)
			}
		}
		base := functionPods(t)["full"]
		vol := func(name string, src corev1.VolumeSource) func(*corev1.Pod) {
			return func(p *corev1.Pod) { p.Spec.Volumes = []corev1.Volume{{Name: name, VolumeSource: src}} }
		}
		for _, c := range []struct {
			name string
			mut  func(*corev1.Pod)
		}{
			{"no runtimeClassName (runc)", func(p *corev1.Pod) { p.Spec.RuntimeClassName = nil }},
			{"runtimeClassName runc", func(p *corev1.Pod) { p.Spec.RuntimeClassName = ptr.To("runc") }},
			{"a hostPath volume", vol("h", corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}})},
			{"the runtime directory, which a sandbox may mount", vol("r", corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/opt/proxcloud-sandbox/bin"}})},
			{"an emptyDir volume", vol("e", corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}})},
			{"a Secret volume", vol("s", corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "fn-0123456789abcdef"}})},
			{"a projected service account token", vol("t", corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}}}})},
			{"a ConfigMap volume", vol("c", corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}}})},
			{"automountServiceAccountToken unset", func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = nil }},
			{"automountServiceAccountToken true", func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = ptr.To(true) }},
			{"privileged", func(p *corev1.Pod) {
				p.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
			}},
			{"hostNetwork", func(p *corev1.Pod) { p.Spec.HostNetwork = true }},
			{"nodeName at create", func(p *corev1.Pod) { p.Spec.NodeName = "pc-platform-kamaji-pve1-abc" }},
			{"tolerates proxcloud.io/unsecured", func(p *corev1.Pod) {
				p.Spec.Tolerations = []corev1.Toleration{{Key: "proxcloud.io/unsecured", Operator: corev1.TolerationOpExists}}
			}},
		} {
			pod := base.DeepCopy()
			c.mut(pod)
			if ok, _ := decideSbx(t, v, in("fn-p1", pod)); ok {
				t.Errorf("%s: a function pod with %s: admitted", example, c.name)
			}
		}
		// The two function rules hold in fn-* alone: a sandbox pod keeps its emptyDir and its runtime directory.
		withScratch := buildCRPods(t)["envd"].DeepCopy()
		withScratch.Spec.Volumes = append(withScratch.Spec.Volumes, corev1.Volume{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		if ok, msg := decideSbx(t, v, in("sb-p1", withScratch)); !ok {
			t.Errorf("%s: the function rules reach a sandbox pod: %s", example, msg)
		}
		// An ephemeral container is the other way into a running function pod.
		running := base.DeepCopy()
		running.Spec.NodeName = "pc-platform-sandbox-pve1-abc"
		debug := running.DeepCopy()
		debug.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name: "dbg", Image: "busybox", SecurityContext: &corev1.SecurityContext{Privileged: ptr.To(true)}}}}
		if ok, _ := decideSbx(t, v, sbxRequest{op: admission.Update, gvk: podGVK, resource: "pods", subresource: "ephemeralcontainers", ns: "fn-p1", nsLabels: tenantLabels, name: running.Name, user: sbxPortal, old: running, obj: debug}); ok {
			t.Errorf("%s: a privileged ephemeral container in a function pod: admitted", example)
		}
	}
}

// With functions on (shared): the identity may manage sb-* and fn-* namespaces and no other, and an fn-* namespace
// must carry the tenant label and keep it: the label is what puts its pods under the pod policy.
func TestFunctionNamespacePolicy(t *testing.T) {
	v := compile(t, sandboxPoliciesOf(t, "shared-functions")["proxcloud-sandbox-namespaces"])
	gvk := schema.GroupVersionKind{Version: "v1", Kind: "Namespace"}
	ns := func(name string, labels map[string]string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	fnLabels := map[string]string{"proxcloud.io/tenant": "true", "proxcloud.io/function-tenant": "true"}
	req := func(op admission.Operation, old, obj *corev1.Namespace) sbxRequest {
		r := sbxRequest{op: op, gvk: gvk, resource: "namespaces", user: sbxPortal}
		if obj != nil {
			r.name, r.obj = obj.Name, obj
		}
		if old != nil {
			r.name, r.old = old.Name, old
		}
		return r
	}
	for _, c := range []struct {
		name  string
		r     sbxRequest
		admit bool
	}{
		{"create fn-p1 labelled", req(admission.Create, nil, ns("fn-p1", fnLabels)), true},
		{"update fn-p1 keeping the tenant label", req(admission.Update, ns("fn-p1", fnLabels), ns("fn-p1", tenantLabels)), true},
		{"delete fn-p1", req(admission.Delete, ns("fn-p1", fnLabels), nil), true},
		{"create sb-p1 labelled", req(admission.Create, nil, ns("sb-p1", tenantLabels)), true},
		{"create fn-p1 without the tenant label", req(admission.Create, nil, ns("fn-p1", map[string]string{"proxcloud.io/function-tenant": "true"})), false},
		{"create fn-p1 with no label", req(admission.Create, nil, ns("fn-p1", nil)), false},
		{"drop the tenant label of fn-p1", req(admission.Update, ns("fn-p1", fnLabels), ns("fn-p1", map[string]string{"proxcloud.io/function-tenant": "true"})), false},
		{"create xx-p1 labelled", req(admission.Create, nil, ns("xx-p1", fnLabels)), false},
		{"create fnp1 labelled (no dash)", req(admission.Create, nil, ns("fnp1", fnLabels)), false},
		{"label kamaji-system as a function tenant", req(admission.Update, ns("kamaji-system", nil), ns("kamaji-system", fnLabels)), false},
		{"delete sandbox-system", req(admission.Delete, ns("sandbox-system", nil), nil), false},
	} {
		if ok, msg := decideSbx(t, v, c.r); ok != c.admit {
			t.Errorf("%s: admitted %v (%s), want %v", c.name, ok, msg, c.admit)
		}
	}
}

// With functions on (shared): each role only in the namespaces of its own prefix, to the identity alone. The function
// role never in sb-* (it would add Secret writes there), the sandbox role never in fn-* (it would add exec there).
// Deleting a binding stays allowed in both, and nowhere else.
func TestFunctionRoleBindingPolicy(t *testing.T) {
	v := compile(t, sandboxPoliciesOf(t, "shared-functions")["proxcloud-sandbox-rolebindings"])
	gvk := schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"}
	self := rbacv1.Subject{Kind: "ServiceAccount", Name: "proxcloud-sandbox", Namespace: "sandbox-system"}
	const fnRole, sbRole = "proxcloud-function-namespace", "proxcloud-sandbox-namespace"
	rb := func(ns, role string, subjects ...rbacv1.Subject) *rbacv1.RoleBinding {
		return &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "proxcloud-sandbox", Namespace: ns},
			RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: role}, Subjects: subjects}
	}
	create := func(obj *rbacv1.RoleBinding, nsLabels map[string]string) sbxRequest {
		return sbxRequest{op: admission.Create, gvk: gvk, resource: "rolebindings", ns: obj.Namespace, nsLabels: nsLabels, name: obj.Name, user: sbxPortal, obj: obj}
	}
	del := func(ns string, nsLabels map[string]string) sbxRequest {
		return sbxRequest{op: admission.Delete, gvk: gvk, resource: "rolebindings", ns: ns, nsLabels: nsLabels, name: "proxcloud-sandbox", user: sbxPortal, old: rb(ns, fnRole, self)}
	}
	asRole := rb("fn-p1", fnRole, self)
	asRole.RoleRef.Kind = "Role"
	otherGroup := rb("fn-p1", fnRole, self)
	otherGroup.RoleRef.APIGroup = "example.org"
	update := create(rb("fn-p1", sbRole, self), tenantLabels)
	update.op, update.old = admission.Update, rb("fn-p1", fnRole, self)
	for _, c := range []struct {
		name  string
		r     sbxRequest
		admit bool
	}{
		{"the function role in fn-p1", create(rb("fn-p1", fnRole, self), tenantLabels), true},
		{"the sandbox role in sb-p1", create(rb("sb-p1", sbRole, self), tenantLabels), true},
		{"delete a binding in fn-p1", del("fn-p1", tenantLabels), true},
		{"delete a binding in sb-p1", del("sb-p1", tenantLabels), true},
		{"the function role in sb-p1", create(rb("sb-p1", fnRole, self), tenantLabels), false},
		{"the sandbox role in fn-p1", create(rb("fn-p1", sbRole, self), tenantLabels), false},
		{"a binding of fn-p1 changed to the sandbox role", update, false},
		{"the function role in an unlabelled fn-p2", create(rb("fn-p2", fnRole, self), nil), false},
		{"the function role in kamaji-system", create(rb("kamaji-system", fnRole, self), nil), false},
		{"the function role in sandbox-system", create(rb("sandbox-system", fnRole, self), nil), false},
		{"cluster-admin in fn-p1", create(rb("fn-p1", "cluster-admin", self), tenantLabels), false},
		{"a Role named as the function role", create(asRole, tenantLabels), false},
		{"the function role of another API group", create(otherGroup, tenantLabels), false},
		{"the function role to the namespace's default service account", create(rb("fn-p1", fnRole, rbacv1.Subject{Kind: "ServiceAccount", Name: "default", Namespace: "fn-p1"}), tenantLabels), false},
		{"the function role to itself and a user", create(rb("fn-p1", fnRole, self, rbacv1.Subject{Kind: "User", APIGroup: "rbac.authorization.k8s.io", Name: "x"}), tenantLabels), false},
		{"delete a binding in kamaji-system", del("kamaji-system", nil), false},
	} {
		if ok, msg := decideSbx(t, v, c.r); ok != c.admit {
			t.Errorf("%s: admitted %v (%s), want %v", c.name, ok, msg, c.admit)
		}
	}
}

// proxcloud-function-objects, in both modes: the identity's Deployments, their scale and Secrets only in fn-* tenant
// namespaces, and only the two Secret types a function has. In mode separate its rules are cluster-wide and this is
// what holds them: a Deployment in the release namespace would be a pod outside gVisor beside the proxy's key and
// the wake token, and a Secret written there could replace them.
func TestFunctionObjectsPolicy(t *testing.T) {
	depGVK := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}
	scaleGVK := schema.GroupVersionKind{Group: "autoscaling", Version: "v1", Kind: "Scale"}
	secretGVK := schema.GroupVersionKind{Version: "v1", Kind: "Secret"}
	deployments := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	dep := func(op admission.Operation, ns string, nsLabels map[string]string) sbxRequest {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "fn-0123456789abcdef", Namespace: ns}}
		r := sbxRequest{op: op, gvk: depGVK, resource: "deployments", ns: ns, nsLabels: nsLabels, name: d.Name, user: sbxPortal, obj: d}
		switch op {
		case admission.Update:
			r.old = d.DeepCopy()
		case admission.Delete:
			r.old, r.obj = d, nil
		}
		return r
	}
	scale := func(ns string, nsLabels map[string]string) sbxRequest {
		s := &autoscalingv1.Scale{ObjectMeta: metav1.ObjectMeta{Name: "fn-0123456789abcdef", Namespace: ns}, Spec: autoscalingv1.ScaleSpec{Replicas: 1}}
		return sbxRequest{op: admission.Update, gvk: scaleGVK, gvr: &deployments, resource: "deployments", subresource: "scale", ns: ns, nsLabels: nsLabels, name: s.Name, user: sbxPortal, old: s.DeepCopy(), obj: s}
	}
	secret := func(op admission.Operation, ns string, nsLabels map[string]string, typ corev1.SecretType) sbxRequest {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fn-0123456789abcdef", Namespace: ns}, Type: typ}
		r := sbxRequest{op: op, gvk: secretGVK, resource: "secrets", ns: ns, nsLabels: nsLabels, name: s.Name, user: sbxPortal, obj: s}
		if op == admission.Update {
			r.old = s.DeepCopy()
		}
		return r
	}
	as := func(r sbxRequest, u string) sbxRequest { r.user = u; return r }
	for _, example := range []string{"shared-functions", "separate-functions"} {
		p := sandboxPoliciesOf(t, example)["proxcloud-function-objects"]
		if p == nil {
			t.Fatalf("%s: no proxcloud-function-objects", example)
		}
		for _, res := range []string{"deployments", "deployments/scale", "secrets"} {
			if !hasRule(p, res) {
				t.Errorf("%s: the policy does not match %s", example, res)
			}
		}
		for _, r := range p.Spec.MatchConstraints.ResourceRules {
			if len(r.Operations) != 3 {
				t.Errorf("%s: %v matched for %v, want CREATE, UPDATE and DELETE", example, r.Resources, r.Operations)
			}
		}
		if p.Spec.MatchConstraints.NamespaceSelector != nil {
			t.Errorf("%s: the policy must hold in every namespace, the unlabelled ones above all", example)
		}
		v := compile(t, p)
		for _, c := range []struct {
			name  string
			r     sbxRequest
			admit bool
		}{
			{"create a Deployment in fn-p1", dep(admission.Create, "fn-p1", tenantLabels), true},
			{"apply a Deployment in fn-p1 again", dep(admission.Update, "fn-p1", tenantLabels), true},
			{"delete a Deployment in fn-p1", dep(admission.Delete, "fn-p1", tenantLabels), true},
			{"scale a Deployment in fn-p1", scale("fn-p1", tenantLabels), true},
			{"create an Opaque Secret in fn-p1", secret(admission.Create, "fn-p1", tenantLabels, corev1.SecretTypeOpaque), true},
			{"patch an Opaque Secret in fn-p1", secret(admission.Update, "fn-p1", tenantLabels, corev1.SecretTypeOpaque), true},
			{"create a registry login in fn-p1", secret(admission.Create, "fn-p1", tenantLabels, corev1.SecretTypeDockerConfigJson), true},
			{"another identity's Deployment in sandbox-system", as(dep(admission.Create, "sandbox-system", nil), "system:serviceaccount:argocd:argocd-application-controller"), true},
			{"another identity's token Secret in sandbox-system", as(secret(admission.Create, "sandbox-system", nil, corev1.SecretTypeServiceAccountToken), "system:serviceaccount:argocd:argocd-application-controller"), true},

			{"create a Deployment in sandbox-system", dep(admission.Create, "sandbox-system", nil), false},
			{"change a Deployment in sandbox-system (the proxy)", dep(admission.Update, "sandbox-system", nil), false},
			{"delete a Deployment in sandbox-system", dep(admission.Delete, "sandbox-system", nil), false},
			{"create a Deployment in kube-system", dep(admission.Create, "kube-system", nil), false},
			{"create a Deployment in sb-p1", dep(admission.Create, "sb-p1", tenantLabels), false},
			{"create a Deployment in an unlabelled fn-p2", dep(admission.Create, "fn-p2", nil), false},
			{"create a Deployment in a tenant namespace xx-p1", dep(admission.Create, "xx-p1", tenantLabels), false},
			{"scale a Deployment in sandbox-system", scale("sandbox-system", nil), false},
			{"scale a Deployment in kube-system", scale("kube-system", nil), false},
			{"scale a Deployment in sb-p1", scale("sb-p1", tenantLabels), false},
			{"create a Secret in sandbox-system", secret(admission.Create, "sandbox-system", nil, corev1.SecretTypeOpaque), false},
			{"patch a Secret in sandbox-system (the wake token)", secret(admission.Update, "sandbox-system", nil, corev1.SecretTypeOpaque), false},
			{"create a Secret in sb-p1", secret(admission.Create, "sb-p1", tenantLabels, corev1.SecretTypeOpaque), false},
			{"create a Secret in an unlabelled fn-p2", secret(admission.Create, "fn-p2", nil, corev1.SecretTypeOpaque), false},
			{"a service account token Secret in fn-p1", secret(admission.Create, "fn-p1", tenantLabels, corev1.SecretTypeServiceAccountToken), false},
			{"a TLS Secret in fn-p1", secret(admission.Create, "fn-p1", tenantLabels, corev1.SecretTypeTLS), false},
			{"a legacy registry login in fn-p1", secret(admission.Create, "fn-p1", tenantLabels, corev1.SecretTypeDockercfg), false},
			{"a Secret of no type in fn-p1", secret(admission.Create, "fn-p1", tenantLabels, ""), false},
		} {
			if ok, msg := decideSbx(t, v, c.r); ok != c.admit {
				t.Errorf("%s: %s: admitted %v (%s), want %v", example, c.name, ok, msg, c.admit)
			}
		}
	}
	// Mode separate has no policy on the identity's namespaces or RoleBindings (the cluster runs sandboxes alone), and
	// with functions off none on its objects either: it then holds no right on a Deployment or a Secret.
	sep := sandboxPoliciesOf(t, "separate-functions")
	for name := range sep {
		if name != "proxcloud-sandbox-tenant-pods" && name != "proxcloud-noderestriction-probe-dry-run-only" && name != "proxcloud-function-objects" {
			t.Errorf("separate with functions: unexpected policy %s", name)
		}
	}
	for _, example := range []string{"shared", "separate"} {
		if sandboxPoliciesOf(t, example)["proxcloud-function-objects"] != nil {
			t.Errorf("%s (functions off): proxcloud-function-objects renders", example)
		}
	}
}
