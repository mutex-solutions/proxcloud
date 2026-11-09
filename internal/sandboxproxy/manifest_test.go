package sandboxproxy

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The proxy holds the wildcard key and the wake token, and a node's kubelet can read the Secrets of the pods bound to
// it: the Deployment must be able to land on a system node only (a label the kubelet cannot set), never on a worker
// that runs tenant code, even when no system node exists.
func TestProxyRunsOnSystemNodesOnly(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/sandbox-cluster/sandbox-proxy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	type expr struct{ Key, Operator string }
	var dep struct {
		Kind     string
		Metadata struct{ Name string }
		Spec     struct {
			Template struct {
				Spec struct {
					NodeSelector map[string]string `yaml:"nodeSelector"`
					Tolerations  []struct{ Key, Operator, Effect string }
					Affinity     struct {
						NodeAffinity struct {
							Required *struct {
								NodeSelectorTerms []struct {
									MatchExpressions []expr `yaml:"matchExpressions"`
								} `yaml:"nodeSelectorTerms"`
							} `yaml:"requiredDuringSchedulingIgnoredDuringExecution"`
							Preferred []any `yaml:"preferredDuringSchedulingIgnoredDuringExecution"`
						} `yaml:"nodeAffinity"`
					}
				}
			}
		}
	}
	found := false
	for d := yaml.NewDecoder(bytes.NewReader(raw)); ; {
		var doc yaml.Node
		if err := d.Decode(&doc); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if err := doc.Decode(&dep); err != nil {
			t.Fatal(err)
		}
		if dep.Kind == "Deployment" && dep.Metadata.Name == "sandbox-proxy" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no sandbox-proxy Deployment")
	}
	ps := dep.Spec.Template.Spec
	req := ps.Affinity.NodeAffinity.Required
	if req == nil || len(req.NodeSelectorTerms) == 0 {
		t.Fatal("no required node affinity: the proxy could land next to tenant code")
	}
	// Terms are ORed: every one of them must demand the system label.
	for i, term := range req.NodeSelectorTerms {
		ok := false
		for _, e := range term.MatchExpressions {
			ok = ok || (e.Key == "node-restriction.kubernetes.io/proxcloud-system" && e.Operator == "Exists")
		}
		if !ok {
			t.Errorf("node selector term %d does not require the system label: %+v", i, term)
		}
	}
	if len(ps.Affinity.NodeAffinity.Preferred) > 0 {
		t.Error("a preferred term means a fallback onto other nodes")
	}
	for _, tol := range ps.Tolerations {
		if tol.Key != "proxcloud.io/sandbox-system" {
			t.Errorf("toleration %+v: the proxy tolerates the system taint alone (not unsecured, not everything)", tol)
		}
	}
}

// The account sync may create Secrets anywhere (RBAC cannot limit create by name) and read the name
// sandbox-template-pull: without an admission policy it can create a service-account-token Secret of that name in
// kube-system for a controller's account, let the token controller fill it, and read a cluster-admin token. The policy
// must bind with Deny, cover CREATE and UPDATE of secrets for exactly the identity the ClusterRole is bound to, and
// require the pull-Secret type and the two names.
func TestReviewSyncCannotMintServiceAccountTokens(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/sandbox-cluster/registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	type doc struct {
		Kind     string
		Metadata struct{ Name, Namespace string }
		RoleRef  struct{ Name string } `yaml:"roleRef"`
		Subjects []struct{ Kind, Name, Namespace string }
		Spec     struct {
			PolicyName        string   `yaml:"policyName"`
			ValidationActions []string `yaml:"validationActions"`
			FailurePolicy     string   `yaml:"failurePolicy"`
			MatchConstraints  struct {
				ResourceRules []struct {
					APIGroups  []string `yaml:"apiGroups"`
					Operations []string
					Resources  []string
				} `yaml:"resourceRules"`
			} `yaml:"matchConstraints"`
			MatchConditions []struct{ Expression string } `yaml:"matchConditions"`
			Validations     []struct{ Expression string }
		}
	}
	var docs []doc
	for d := yaml.NewDecoder(bytes.NewReader(raw)); ; {
		var x doc
		if err := d.Decode(&x); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, x)
	}
	var user string
	var policy, binding *doc
	for i := range docs {
		switch x := &docs[i]; x.Kind {
		case "ClusterRoleBinding":
			if x.RoleRef.Name == "sandbox-registry-users" && len(x.Subjects) == 1 {
				user = "system:serviceaccount:" + x.Subjects[0].Namespace + ":" + x.Subjects[0].Name
			}
		case "ValidatingAdmissionPolicy":
			policy = x
		case "ValidatingAdmissionPolicyBinding":
			binding = x
		}
	}
	if user == "" || policy == nil || binding == nil {
		t.Fatalf("want the sync's binding, a ValidatingAdmissionPolicy and its binding; got user=%q policy=%v binding=%v", user, policy != nil, binding != nil)
	}
	if binding.Spec.PolicyName != policy.Metadata.Name || strings.Join(binding.Spec.ValidationActions, ",") != "Deny" {
		t.Errorf("binding %q %v must Deny under policy %q", binding.Spec.PolicyName, binding.Spec.ValidationActions, policy.Metadata.Name)
	}
	if policy.Spec.FailurePolicy != "Fail" {
		t.Errorf("failurePolicy %q, want Fail", policy.Spec.FailurePolicy)
	}
	ops := map[string]bool{}
	for _, r := range policy.Spec.MatchConstraints.ResourceRules {
		if slices.Contains(r.APIGroups, "") && slices.Contains(r.Resources, "secrets") {
			for _, o := range r.Operations {
				ops[o] = true
			}
		}
	}
	if !ops["CREATE"] || !ops["UPDATE"] {
		t.Errorf("policy must cover CREATE and UPDATE of core secrets, covers %v", ops)
	}
	if len(policy.Spec.MatchConditions) != 1 || policy.Spec.MatchConditions[0].Expression != `request.userInfo.username == "`+user+`"` {
		t.Errorf("policy must match exactly %s, got %+v", user, policy.Spec.MatchConditions)
	}
	var all string
	for _, v := range policy.Spec.Validations {
		all += v.Expression + "\n"
	}
	for _, want := range []string{`object.type == "kubernetes.io/dockerconfigjson"`, `"sandbox-template-pull"`,
		`request.namespace.startsWith("sb-")`, `"sandbox-registry-nodes"`, `request.namespace == "sandbox-system"`,
		`matches("^registry-build-[a-z0-9]{1,40}$")`, `request.namespace == "sbx-build"`} {
		if !strings.Contains(all, want) {
			t.Errorf("validations lack %s:\n%s", want, all)
		}
	}
}
