package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster/k0s"
	"github.com/appmana/cloud-provisioning/harness/e2e/cluster/microk8s"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type runtimeNode struct {
	rig.Node
	version       string
	snap          string
	missingSocket bool
	calls         [][]string
}

func (n *runtimeNode) Exec(_ context.Context, args ...string) ([]byte, error) {
	n.calls = append(n.calls, args)
	switch args[0] {
	case "k0s":
		return []byte(n.version), nil
	case "snap":
		return []byte(n.snap), nil
	case "test":
		if n.missingSocket {
			return nil, errors.New("runtime socket absent")
		}
		return nil, nil
	default:
		return nil, errors.New("unexpected command")
	}
}

func TestAWSWorkerUsesSelectedDistributionRuntime(t *testing.T) {
	// Snap-table input is simulated from the existing version/revision contract;
	// it is not evidence of a MicroK8s AWS bringup.
	cases := []struct{ distro, network, socket, importCommand string }{
		{"k0s", "default", "unix:///run/k0s/containerd.sock", "k0s"},
		{"k0s", "kube-router", "unix:///run/k0s/containerd.sock", "k0s"},
		{"microk8s", "default", "unix:///var/snap/microk8s/common/run/containerd.sock", "/snap/bin/microk8s"},
	}
	for _, tc := range cases {
		t.Run(tc.distro+"/"+tc.network, func(t *testing.T) {
			b, p, err := workerProfile(tc.distro, tc.network)
			if err != nil {
				t.Fatal(err)
			}
			if p.Distro != tc.distro || b.CRIEndpoint() != tc.socket || b.ImportArgs()[0] != tc.importCommand {
				t.Fatal("distribution runtime leaked across profile")
			}
			n := &runtimeNode{version: k0s.Version + "\n", snap: "Name Version Rev Tracking Publisher Notes\nmicrok8s " + microk8s.Version + " " + microk8s.Revision + " 1.34/stable canonical classic\n"}
			if err := b.VerifyWorker(context.Background(), n); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(n.calls[len(n.calls)-1], []string{"test", "-S", strings.TrimPrefix(tc.socket, "unix://")}) {
				t.Fatalf("wrong socket checked: %v", n.calls)
			}
			if tc.distro == "microk8s" && n.calls[0][0] != "snap" {
				t.Fatal("MicroK8s observed through k0s")
			}
		})
	}
}
func TestAWSWorkerRejectsWrongReleaseAndMissingRuntime(t *testing.T) {
	cases := []struct {
		distro string
		node   runtimeNode
	}{
		{"k0s", runtimeNode{version: "v1.34.1+k0s.0\n"}},
		{"k0s", runtimeNode{version: k0s.Version, missingSocket: true}},
		{"microk8s", runtimeNode{snap: "Name Version Rev\nmicrok8s " + microk8s.Version + " 0000\n"}},
		{"microk8s", runtimeNode{snap: "Name Version Rev\nother " + microk8s.Version + " " + microk8s.Revision + "\n"}},
		{"microk8s", runtimeNode{snap: "missing snap"}},
		{"microk8s", runtimeNode{snap: "Name Version Rev\nmicrok8s " + microk8s.Version + " " + microk8s.Revision + "\n", missingSocket: true}},
	}
	for _, tc := range cases {
		b, _, err := workerProfile(tc.distro, "default")
		if err != nil {
			t.Fatal(err)
		}
		if err := b.VerifyWorker(context.Background(), &tc.node); err == nil {
			t.Fatalf("accepted wrong %s worker", tc.distro)
		}
	}
	for _, p := range [][2]string{{"microk8s", "flannel"}, {"k0s", "flannel"}, {"okd", "default"}, {"k3s", "flannel"}} {
		if _, _, err := workerProfile(p[0], p[1]); err == nil {
			t.Fatalf("accepted unsupported AWS observer %v", p)
		}
	}
}
