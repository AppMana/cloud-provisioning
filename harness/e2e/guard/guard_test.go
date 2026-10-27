package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func refused(t *testing.T, args string, stdin string) {
	t.Helper()
	if err := Kubectl(strings.Fields(args), []byte(stdin)); !errors.Is(err, ErrRefused) {
		t.Errorf("kubectl %s: %v, want refused", args, err)
	}
}

func allowed(t *testing.T, args string, stdin string) {
	t.Helper()
	if err := Kubectl(strings.Fields(args), []byte(stdin)); err != nil {
		t.Errorf("kubectl %s: %v", args, err)
	}
}

// The stand-ins the audit found are refused: each was the harness doing what
// a real cluster needs the product to do.
func TestTheAuditedStandInsAreRefused(t *testing.T) {
	refused(t, "--server=https://10.10.0.10:6443 label node cp node-role.kubernetes.io/control-plane= --overwrite", "")
	refused(t, "patch node remote1 --type merge -p {}", "")
	refused(t, "-n kube-system set env daemonset/calico-node IP_AUTODETECTION_METHOD=can-reach=10.10.0.10", "")
	refused(t, "-n kube-system rollout restart daemonset/calico-node", "")
	refused(t, "patch ippools.crd.projectcalico.org default-ipv4-ippool --type merge -p {}", "")
	refused(t, "apply -f -", `{"apiVersion":"crd.projectcalico.org/v1","kind":"BGPConfiguration","metadata":{"name":"default"}}`)
	refused(t, "apply -f -", `{"apiVersion":"crd.projectcalico.org/v1","kind":"IPPool","metadata":{"name":"cldt-native-pods"}}`)
	refused(t, "apply -f -", `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"aws-node","namespace":"kube-system"}}`)
	refused(t, "apply -f -", `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"cldt-konnectivity-default-route-linux","namespace":"kube-system"}}`)
	refused(t, "-n cloud-provisioning patch labcluster site --subresource=status --type merge -p {}", "")
	refused(t, "-n cloud-provisioning patch awscluster site --subresource=status --type merge -p {}", "")
	refused(t, "-n cloud-provisioning create configmap native-pod-scope --from-literal=scope.json={}", "")
	refused(t, "apply -f -", `{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"x"}},{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"native-pod-scope","namespace":"cloud-provisioning"}}]}`)
	refused(t, "delete namespace kube-system", "")
	allowed(t, "-n cloud-provisioning create -f -", `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"gateway-aws-win2022","namespace":"cloud-provisioning"}}`)
	refused(t, "apply -f /tmp/manifest.yaml", "")
}

// What an operator gives the product, the lab's own provider, fault
// injection and the harness's probe workloads are allowed.
func TestOperatorInputsLabObjectsAndProbesAreAllowed(t *testing.T) {
	allowed(t, "apply --server-side --field-manager=cldt-operator -f -", `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"cloud-provisioning-aws","namespace":"cloud-provisioning"}}`)
	allowed(t, "apply -f -", `{"apiVersion":"infrastructure.cluster.x-k8s.io/v1beta2","kind":"AWSClusterRoleIdentity","metadata":{"name":"cldt-run"}}`)
	allowed(t, "apply -f -", "apiVersion: cluster.x-k8s.io/v1beta2\nkind: Cluster\nmetadata:\n  name: site\n  namespace: cloud-provisioning\n---\napiVersion: cloud-provisioning.appmana.com/v1alpha1\nkind: ProvisionedNodeClaim\nmetadata:\n  name: remote1\n  namespace: cloud-provisioning\n")
	allowed(t, "-n cloud-provisioning delete provisionednodeclaim remote1 --wait=false", "")
	allowed(t, "-n cloud-provisioning patch labmachine remote1 --subresource=status --type merge -p {}", "")
	allowed(t, "-n cloud-provisioning patch labcluster site --type merge -p {}", "")
	allowed(t, "-n cloud-provisioning patch secret remote1-tunnel-peers --type=json -p []", "")
	allowed(t, "-n cloud-provisioning create configmap cldt-vm-slots --from-literal=pool.json={} -o json", "")
	allowed(t, "apply -f -", `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"cldt-probe"}}`)
	allowed(t, "apply -f -", `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"hc-w1","namespace":"cldt-probe"}}`)
	allowed(t, "delete namespace cldt-probe --wait=false", "")
	allowed(t, "-n kube-system get daemonset calico-node -o json", "")
	allowed(t, "-n kube-system rollout status daemonset/calico-node --timeout=40s", "")
	allowed(t, "apply --dry-run=server -f -", `{"apiVersion":"apps/v1","kind":"DaemonSet","metadata":{"name":"x","namespace":"kube-system"}}`)
}

// Every allowed write says why it is not product work.
func TestEveryAllowanceIsExplained(t *testing.T) {
	for _, list := range [][]Allowed{Policy, StandIns} {
		for _, a := range list {
			if a.Why == "" || a.Resource == "" {
				t.Errorf("unexplained allowance %+v", a)
			}
		}
	}
	for _, a := range StandIns {
		if item, _, ok := strings.Cut(a.Why, ":"); !ok || !strings.HasPrefix(item, "C") {
			t.Errorf("stand-in %+v names no tracker item", a)
		}
	}
}

// A pinned prerequisite is admitted whole, and only byte for byte.
func TestPinnedPrerequisitesAreAdmittedWhole(t *testing.T) {
	manifest := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"capa-controller-manager","namespace":"capa-system"}}`
	sum := sha256.Sum256([]byte(manifest))
	Prerequisites[hex.EncodeToString(sum[:])] = "test prerequisite"
	defer delete(Prerequisites, hex.EncodeToString(sum[:]))
	allowed(t, "apply -f -", manifest)
	refused(t, "apply -f -", manifest+" ")
}
