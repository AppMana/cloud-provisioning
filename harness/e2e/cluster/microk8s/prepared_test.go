package microk8s

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type preparedRig struct {
	rig.Rig
	t       *testing.T
	bad     string
	visited []string
}

func (r *preparedRig) Node(name string) rig.Node {
	return observedNode{exec: func(argv []string) ([]byte, error) {
		if !reflect.DeepEqual(argv, []string{"snap", "list", "microk8s"}) {
			r.t.Fatalf("preparation validation mutated node %s: %v", name, argv)
		}
		r.visited = append(r.visited, name)
		if name == r.bad {
			return nil, fmt.Errorf("snap not installed")
		}
		return []byte("Name Version Rev Tracking Publisher Notes\nmicrok8s " + Version + " " + Revision + " " + Channel + " canonical classic\n"), nil
	}}
}

func TestPreparedSiteValidationPrecedesMutation(t *testing.T) {
	topology := lab.Topology{Nodes: []lab.Node{{Name: "cp", Role: lab.ControlPlane}, {Name: "worker", Role: lab.Worker}}}
	r := &preparedRig{t: t, bad: "worker"}
	d := cluster.Deps{Topology: topology, Rig: r, Kube: &kube.Client{}}
	err := (Builder{}).Build(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "prepare a guest image") || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("missing actionable preparation failure: %v", err)
	}
	if !reflect.DeepEqual(r.visited, []string{"cp", "worker"}) {
		t.Fatal("did not validate entire site before mutation", r.visited)
	}
	r = &preparedRig{t: t}
	d.Rig = r
	if err := verifyPreparedSite(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(r.visited) != 2 {
		t.Fatal("did not check prepared workers")
	}
}
