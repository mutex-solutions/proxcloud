package policy

// The admission policies of the sandbox chart (deploy/charts/proxcloud-sandbox, mode shared), rendered with helm and
// run through the API server's CEL libraries: the pod policy against the pods the portal's BuildCR makes
// (testdata/buildcr-pods.json, written by internal/cloud/sandbox TestPolicyFixtureIsBuildCR) and against the shapes it
// exists to refuse; the portal identity's namespace and RoleBinding policies; the firewall identity's node policy.

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
	helm, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI, or this test proves nothing")
		}
		t.Skip("helm not installed")
	}
	const chart = "../../deploy/charts/proxcloud-sandbox"
	cmd := exec.Command(helm, "template", "sandbox", chart, "-n", "sandbox-system", "-f", chart+"/examples/values-shared.yaml")
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
}

func decideSbx(t *testing.T, v validating.Validator, r sbxRequest) (bool, string) {
	t.Helper()
	gvr := r.gvk.GroupVersion().WithResource(r.resource)
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
