package k0s

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type egressNode struct {
	rig.Node
	run      func(...string) ([]byte, error)
	manifest []byte
}

func (n *egressNode) Exec(_ context.Context, a ...string) ([]byte, error) { return n.run(a...) }
func (n *egressNode) Pipe(_ context.Context, r io.Reader, a ...string) ([]byte, error) {
	var err error
	n.manifest, err = io.ReadAll(r)
	return nil, err
}

type egressRig struct {
	rig.Rig
	n *egressNode
}

func (r egressRig) Node(string) rig.Node { return r.n }

// Native metrics established that Pod readiness alone is weaker than the
// all-controller connection gate. Missing connections must fail installation.
func TestInstallDefaultRouteConnectionAndPlacementGates(t *testing.T) {
	for _, mode := range []string{"ready", "windows", "busy ports", "missing server"} {
		t.Run(mode, func(t *testing.T) {
			native := nativeAgent(t)
			bastion := &egressNode{run: func(a ...string) ([]byte, error) {
				s := strings.Join(a, " ")
				switch {
				case strings.Contains(s, "/readyz"):
					return []byte("ok"), nil
				case strings.Contains(s, "kubernetes\\.io/os"):
					if mode == "windows" {
						return []byte("windows"), nil
					}
					return []byte("linux"), nil
				case strings.Contains(s, "kubernetes\\.io/hostname"):
					for _, h := range []string{"cp", "cp2", "cp3"} {
						if strings.Contains(s, "node "+h+" ") {
							return []byte(h), nil
						}
					}
				case strings.Contains(s, "get daemonset konnectivity-agent"):
					return native, nil
				case strings.Contains(s, "rollout status"):
					return nil, nil
				}
				return nil, fmt.Errorf("unexpected bastion command %v", a)
			}}
			guest := &egressNode{run: func(a ...string) ([]byte, error) {
				switch a[0] {
				case "ss":
					if mode == "busy ports" {
						return []byte("LISTEN"), nil
					}
					return nil, nil
				case "curl":
					if mode == "missing server" {
						return []byte("konnectivity_network_proxy_agent_open_server_connections 2\n"), nil
					}
					return []byte("konnectivity_network_proxy_agent_open_server_connections 3\n"), nil
				}
				return nil, fmt.Errorf("unexpected guest command %v", a)
			}}
			d := cluster.Deps{Topology: lab.Default(), Rig: egressRig{n: guest}, Kube: &kube.Client{Bastion: bastion, ControlPlanes: []string{"10.10.0.10"}}, WorkDir: t.TempDir()}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			err := InstallDefaultRouteAgents(ctx, d)
			if mode == "ready" {
				if err != nil {
					t.Fatal(err)
				}
				if len(bastion.manifest) == 0 {
					t.Fatal("manifest was not attached")
				}
			} else {
				if err == nil {
					t.Fatal("gate accepted unsafe state")
				}
				if (mode == "windows" || mode == "busy ports") && len(bastion.manifest) != 0 {
					t.Fatal("mutated cluster before preflight passed")
				}
			}
		})
	}
}
