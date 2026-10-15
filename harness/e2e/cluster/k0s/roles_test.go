package k0s

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type roleBastion struct {
	rig.Node
	labels []string
}

func (b *roleBastion) Exec(_ context.Context, args ...string) ([]byte, error) {
	command := strings.Join(args, " ")
	if strings.Contains(command, "label node") {
		b.labels = append(b.labels, command)
	}
	return nil, nil
}
func TestControlPlaneLabelsDoNotPromoteWorkers(t *testing.T) {
	b := &roleBastion{}
	topo := lab.Default()
	d := cluster.Deps{Topology: topo, Kube: &kube.Client{Bastion: b, ControlPlanes: []string{"10.10.0.10"}}}
	if err := labelControlPlanes(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	cps := topo.NodesInRole(lab.ControlPlane)
	if len(b.labels) != len(cps) || len(cps) == 0 {
		t.Fatalf("unexpected label calls %v", b.labels)
	}
	for i, n := range cps {
		if !strings.Contains(b.labels[i], "label node "+n.Name+" node-role.kubernetes.io/control-plane= --overwrite") {
			t.Fatalf("wrong target: %s", b.labels[i])
		}
	}
}
