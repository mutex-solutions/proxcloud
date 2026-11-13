// Package policy runs the sandbox prepull ValidatingAdmissionPolicy (rendered by installer stage 55,
// docs/template-updates.md §5.3) through the Kubernetes API server's own CEL admission libraries — compile, match
// conditions, variables, validations — without a cluster. The policy is read from POLICY_YAML (the rendered
// sandbox-prepull manifest stream; installer/tests/test_sandbox_mirror.py renders it and runs this test).
//
// This is a separate module so k8s.io/apiserver never enters the API's dependency graph.
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/utils/ptr"
)

const (
	reg    = "192.0.2.226"
	portal = "system:serviceaccount:proxcloud-system:proxcloud"
)

var (
	d1 = "sha256:" + strings.Repeat("1", 64)
	d2 = "sha256:" + strings.Repeat("2", 64)
)

func loadPolicy(t *testing.T) *admissionregistrationv1.ValidatingAdmissionPolicy {
	t.Helper()
	path := os.Getenv("POLICY_YAML")
	if path == "" {
		t.Skip("POLICY_YAML is unset: run through installer/tests/test_sandbox_mirror.py, which renders the policy")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
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
		return &p
	}
	t.Fatal("no ValidatingAdmissionPolicy in " + path)
	return nil
}

// compile mirrors k8s.io/apiserver validating.compilePolicy (unexported) with the same environment.
func compile(t *testing.T, p *admissionregistrationv1.ValidatingAdmissionPolicy) validating.Validator {
	t.Helper()
	env, err := cel.NewCompositionEnv(cel.VariablesTypeName, environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
	if err != nil {
		t.Fatal(err)
	}
	opts := cel.OptionalVariableDeclarations{HasParams: false, HasAuthorizer: true}
	compiler := cel.NewCompositedCompilerFromTemplate(env)
	vars := make([]cel.NamedExpressionAccessor, len(p.Spec.Variables))
	for i, v := range p.Spec.Variables {
		vars[i] = &validating.Variable{Name: v.Name, Expression: v.Expression}
	}
	compiler.CompileAndStoreVariables(vars, opts, environment.StoredExpressions)
	mc := make([]cel.ExpressionAccessor, len(p.Spec.MatchConditions))
	for i := range p.Spec.MatchConditions {
		mc[i] = (*matchconditions.MatchCondition)(&p.Spec.MatchConditions[i])
	}
	vals := make([]cel.ExpressionAccessor, len(p.Spec.Validations))
	for i, v := range p.Spec.Validations {
		vals[i] = &validating.ValidationCondition{Expression: v.Expression, Message: v.Message}
	}
	return validating.NewValidator(
		compiler.CompileCondition(vals, opts, environment.StoredExpressions),
		matchconditions.NewMatcher(compiler.CompileCondition(mc, opts, environment.StoredExpressions), p.Spec.FailurePolicy, "policy", "validate", p.Name),
		compiler.CompileCondition(nil, opts, environment.StoredExpressions),
		compiler.CompileCondition(make([]cel.ExpressionAccessor, len(vals)), cel.OptionalVariableDeclarations{}, environment.StoredExpressions),
		p.Spec.FailurePolicy,
	)
}

func hardened() *corev1.SecurityContext {
	return &corev1.SecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)), AllowPrivilegeEscalation: ptr.To(false),
		ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
}

func initC(name, image string) corev1.Container {
	return corev1.Container{Name: name, Image: image, Command: []string{"sh", "-c", "true"}, SecurityContext: hardened(),
		Resources: corev1.ResourceRequirements{}}
}

func prepull() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: "template-prepull", Namespace: "sandbox-system", Labels: map[string]string{"app": "template-prepull"}, ResourceVersion: "7"},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "template-prepull"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "template-prepull"}},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
					Tolerations:                  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					ImagePullSecrets:             []corev1.LocalObjectReference{{Name: "sandbox-registry-nodes"}},
					InitContainers: []corev1.Container{
						initC("p0", reg+"/library/python:3.12-slim@"+d1),
						initC("p1", reg+"/library/sandbox-base:20261101-aaaaaaa@"+d1),
					},
					Containers: []corev1.Container{{Name: "pause", Image: "pause"}},
				},
			},
		},
	}
}

func decide(t *testing.T, v validating.Validator, op admission.Operation, ns, username string, oldObj, newObj *appsv1.DaemonSet) (admit bool, msg string) {
	t.Helper()
	gvk := schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"}
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}
	var o, n runtime.Object
	if oldObj != nil {
		o = oldObj
	}
	if newObj != nil {
		n = newObj
	}
	name := "template-prepull"
	if newObj != nil {
		name = newObj.Name
	} else if oldObj != nil {
		name = oldObj.Name
	}
	attr := admission.NewAttributesRecord(n, o, gvk, ns, name, gvr, "", op, nil, false, &user.DefaultInfo{Name: username})
	va, err := admission.NewVersionedAttributes(attr, gvk, nil)
	if err != nil {
		t.Fatal(err)
	}
	res := v.Validate(context.Background(), gvr, va, nil, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, 1_000_000_000, nil)
	for _, d := range res.Decisions {
		if d.Action != validating.ActionAdmit {
			return false, d.Message
		}
	}
	return true, ""
}

func TestPrepullPolicyCases(t *testing.T) {
	p := loadPolicy(t)
	if p.Spec.FailurePolicy == nil || *p.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatal("failurePolicy must be Fail")
	}
	v := compile(t, p)
	newRef := reg + "/library/sandbox-base:20261112-9d00563@" + d2
	type tc struct {
		name  string
		op    admission.Operation
		ns    string
		user  string
		mut   func(*appsv1.DaemonSet)
		admit bool
	}
	cases := []tc{
		{"image change to a mirrored digest ref in the same repository", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.InitContainers[1].Image = newRef }, true},
		{"image change without a tag", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) {
				d.Spec.Template.Spec.InitContainers[1].Image = reg + "/library/sandbox-base@" + d2
			}, true},
		{"no change at all", admission.Update, "sandbox-system", portal, func(*appsv1.DaemonSet) {}, true},
		{"add one hardened p2 shaped like the others", admission.Update, "sandbox-system", portal, func(d *appsv1.DaemonSet) {
			d.Spec.Template.Spec.InitContainers = append(d.Spec.Template.Spec.InitContainers, initC("p2", reg+"/library/sandbox-node:20261112-9d00563@"+d2))
		}, true},
		{"the API server's template-generation annotation moves", admission.Update, "sandbox-system", portal, func(d *appsv1.DaemonSet) {
			d.Annotations = map[string]string{"deprecated.daemonset.template.generation": "9"}
			d.Spec.Template.Spec.InitContainers[1].Image = newRef
		}, true},
		{"another identity is not matched", admission.Update, "sandbox-system", "system:serviceaccount:kube-system:x",
			func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.HostNetwork = true }, true},

		{"a tag-only ref", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) {
				d.Spec.Template.Spec.InitContainers[1].Image = reg + "/library/sandbox-base:20261112-9d00563"
			}, false},
		{"a project template ref under t/", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.InitContainers[1].Image = reg + "/t/p/x@" + d2 }, false},
		{"a platform image under library/proxcloud-*", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) {
				d.Spec.Template.Spec.InitContainers[1].Image = reg + "/library/proxcloud-sandbox-import@" + d2
			}, false},
		{"another registry", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) {
				d.Spec.Template.Spec.InitContainers[1].Image = "198.51.100.1/library/sandbox-base@" + d2
			}, false},
		{"ghcr.io directly", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) {
				d.Spec.Template.Spec.InitContainers[1].Image = "ghcr.io/mutex-solutions/sandbox-base@" + d2
			}, false},
		{"another repository in an existing container", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) {
				d.Spec.Template.Spec.InitContainers[1].Image = reg + "/library/sandbox-node:20261112-9d00563@" + d2
			}, false},
		{"repoint python (not a sandbox image) container", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.InitContainers[0].Image = newRef }, false},
		{"change command", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) {
				d.Spec.Template.Spec.InitContainers[1].Command = []string{"sh", "-c", "evil"}
			}, false},
		{"change env", admission.Update, "sandbox-system", portal, func(d *appsv1.DaemonSet) {
			d.Spec.Template.Spec.InitContainers[1].Env = []corev1.EnvVar{{Name: "X", Value: "1"}}
		}, false},
		{"change securityContext", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) {
				d.Spec.Template.Spec.InitContainers[1].SecurityContext.Privileged = ptr.To(true)
			}, false},
		{"change volumeMounts", admission.Update, "sandbox-system", portal, func(d *appsv1.DaemonSet) {
			d.Spec.Template.Spec.InitContainers[1].VolumeMounts = []corev1.VolumeMount{{Name: "h", MountPath: "/h"}}
		}, false},
		{"change restartPolicyRules", admission.Update, "sandbox-system", portal, func(d *appsv1.DaemonSet) {
			d.Spec.Template.Spec.InitContainers[1].RestartPolicyRules = []corev1.ContainerRestartRule{{Action: corev1.ContainerRestartRuleActionRestart}}
		}, false},
		{"remove an init container", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) {
				d.Spec.Template.Spec.InitContainers = d.Spec.Template.Spec.InitContainers[:1]
			}, false},
		{"add two init containers", admission.Update, "sandbox-system", portal, func(d *appsv1.DaemonSet) {
			d.Spec.Template.Spec.InitContainers = append(d.Spec.Template.Spec.InitContainers,
				initC("p2", reg+"/library/sandbox-node:20261112-9d00563@"+d2), initC("p3", reg+"/library/sandbox-amp:20261112-9d00563@"+d2))
		}, false},
		{"add a p2 without the hardened securityContext", admission.Update, "sandbox-system", portal, func(d *appsv1.DaemonSet) {
			c := initC("p2", reg+"/library/sandbox-node:20261112-9d00563@"+d2)
			c.SecurityContext = nil
			d.Spec.Template.Spec.InitContainers = append(d.Spec.Template.Spec.InitContainers, c)
		}, false},
		{"add a container not named p<N>", admission.Update, "sandbox-system", portal, func(d *appsv1.DaemonSet) {
			d.Spec.Template.Spec.InitContainers = append(d.Spec.Template.Spec.InitContainers, initC("evil", reg+"/library/sandbox-node:20261112-9d00563@"+d2))
		}, false},
		{"change containers", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.Containers[0].Image = "evil" }, false},
		{"add a volume", admission.Update, "sandbox-system", portal, func(d *appsv1.DaemonSet) {
			d.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "h", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
		}, false},
		{"set serviceAccountName", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.ServiceAccountName = "admin" }, false},
		{"set hostNetwork", admission.Update, "sandbox-system", portal, func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.HostNetwork = true }, false},
		{"set hostnameOverride", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.HostnameOverride = ptr.To("x") }, false},
		{"change tolerations", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.Tolerations = nil }, false},
		{"set nodeSelector", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) { d.Spec.Template.Spec.NodeSelector = map[string]string{"x": "y"} }, false},
		{"change imagePullSecrets", admission.Update, "sandbox-system", portal, func(d *appsv1.DaemonSet) {
			d.Spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "admin-registry"}}
		}, false},
		{"change a label", admission.Update, "sandbox-system", portal, func(d *appsv1.DaemonSet) { d.Labels["app"] = "x" }, false},
		{"add an annotation", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) { d.Annotations = map[string]string{"x": "y"} }, false},
		{"change the selector", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) { d.Spec.Selector.MatchLabels["x"] = "y" }, false},
		{"change the pod template labels", admission.Update, "sandbox-system", portal,
			func(d *appsv1.DaemonSet) { d.Spec.Template.Labels["x"] = "y" }, false},
		{"another DaemonSet", admission.Update, "sandbox-system", portal, func(d *appsv1.DaemonSet) { d.Name = "node-setup" }, false},
		{"another namespace", admission.Update, "kube-system", portal, func(*appsv1.DaemonSet) {}, false},
	}
	for _, c := range cases {
		old := prepull()
		cur := prepull()
		c.mut(cur)
		if cur.Name != old.Name {
			old.Name = cur.Name
		}
		admit, msg := decide(t, v, c.op, c.ns, c.user, old, cur)
		if admit != c.admit {
			t.Errorf("%s: admit=%v (%s), want %v", c.name, admit, msg, c.admit)
		}
	}
	// CREATE and DELETE by the portal identity are denied.
	if admit, _ := decide(t, v, admission.Create, "sandbox-system", portal, nil, prepull()); admit {
		t.Error("CREATE admitted")
	}
	if admit, _ := decide(t, v, admission.Delete, "sandbox-system", portal, prepull(), nil); admit {
		t.Error("DELETE admitted")
	}
}
