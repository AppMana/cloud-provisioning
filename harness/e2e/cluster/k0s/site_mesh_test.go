package k0s

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Calico peers every node with every other by default, remotes included,
// and the tunnel refuses BGP, so a remote's sessions never establish.
// Calico reports a node whose configured peers never establish as never
// ready, and one with no peers as ready. Peering the site's nodes with
// each other only leaves the remotes with none.
func TestTheSiteMeshPeersOnlySiteNodes(t *testing.T) {
	objects := SiteMeshObjects()
	if len(objects) != 2 {
		t.Fatalf("%d objects", len(objects))
	}
	config := objects[0].(*unstructured.Unstructured)
	if config.GetKind() != "BGPConfiguration" || config.GetName() != "default" {
		t.Fatalf("first object %s/%s", config.GetKind(), config.GetName())
	}
	if mesh, found, _ := unstructured.NestedBool(config.Object, "spec", "nodeToNodeMeshEnabled"); !found || mesh {
		t.Errorf("node-to-node mesh = %v (set %v), want disabled", mesh, found)
	}
	peer := objects[1].(*unstructured.Unstructured)
	for _, field := range []string{"nodeSelector", "peerSelector"} {
		if got, _, _ := unstructured.NestedString(peer.Object, "spec", field); got != `!has(cloud-provisioning.appmana.com/role)` {
			t.Errorf("BGPPeer %s = %q", field, got)
		}
	}
}

type podListBastion struct {
	rig.Node
	out string
}

func (b podListBastion) Exec(_ context.Context, args ...string) ([]byte, error) {
	if strings.Contains(strings.Join(args, " "), "/readyz") {
		return nil, nil
	}
	return []byte(b.out), nil
}

// Observed on the dual-stack lab with two remotes joined and the default
// full mesh: every site calico-node ready, each remote's not.
func TestCalicoNodesReadyNamesTheNodesThatAreNot(t *testing.T) {
	out := `{"items":[
 {"metadata":{"name":"calico-node-cw7fj"},"spec":{"nodeName":"cp"},"status":{"conditions":[{"type":"Ready","status":"True"}]}},
 {"metadata":{"name":"calico-node-psbgx"},"spec":{"nodeName":"remote1"},"status":{"conditions":[{"type":"Ready","status":"False"}]}},
 {"metadata":{"name":"calico-node-l8nrx"},"spec":{"nodeName":"remote2"},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}`
	k := &kube.Client{Bastion: podListBastion{out: out}, ControlPlanes: []string{"10.101.0.1"}}
	err := CalicoNodesReady(context.Background(), k, []string{"cp", "remote1", "remote2"})
	if err == nil || !strings.Contains(err.Error(), "remote1") || !strings.Contains(err.Error(), "remote2") || strings.Contains(err.Error(), "cp,") {
		t.Fatalf("err = %v, want remote1 and remote2 named", err)
	}
	// A node with no calico-node pod at all is not ready either.
	err = CalicoNodesReady(context.Background(), k, []string{"cp", "w1"})
	if err == nil || !strings.Contains(err.Error(), "w1") {
		t.Fatalf("err = %v, want w1 named as missing", err)
	}
	ready := strings.ReplaceAll(out, `"False"`, `"True"`)
	k = &kube.Client{Bastion: podListBastion{out: ready}, ControlPlanes: []string{"10.101.0.1"}}
	if err := CalicoNodesReady(context.Background(), k, []string{"cp", "remote1", "remote2"}); err != nil {
		t.Fatal(err)
	}
}
