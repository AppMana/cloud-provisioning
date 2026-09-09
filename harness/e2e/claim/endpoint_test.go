package claim

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/lab"
	"github.com/appmana/cloud-provisioning/harness/e2e/provider"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

var endpointObserved = errors.New("endpoint captured before provisioning")

type endpointNode struct {
	rig.Node
	patch string
}

func (n *endpointNode) Pipe(context.Context, io.Reader, ...string) ([]byte, error) {
	return nil, nil
}

func (n *endpointNode) Exec(_ context.Context, args ...string) ([]byte, error) {
	for _, arg := range args {
		if strings.Contains(arg, `"controlPlaneEndpoint"`) {
			n.patch = arg
			return nil, endpointObserved
		}
	}
	return nil, nil
}

// Native MicroK8s uses 16443. A successful imported CAPI association through
// kubernetes.default.svc must not hide an incorrect infrastructure endpoint.
func TestClaimPublishesDistributionAPIPort(t *testing.T) {
	for _, tc := range []struct {
		name             string
		configured, want int
	}{{"default", 0, 6443}, {"microk8s", 16443, 16443}} {
		t.Run(tc.name, func(t *testing.T) {
			n := &endpointNode{}
			k := &kube.Client{Bastion: n, ControlPlanes: []string{"10.10.0.10"}, APIPort: tc.configured}
			c := &Claimer{Kube: k, Topology: lab.Default(), LabName: "cldt", Provider: &provider.Controller{Kube: k}}
			if err := c.Claim(context.Background(), "remote1", "remote1"); !errors.Is(err, endpointObserved) {
				t.Fatalf("did not reach infrastructure endpoint publication: %v", err)
			}
			var patch struct {
				Spec struct {
					Endpoint struct {
						Host string
						Port int
					} `json:"controlPlaneEndpoint"`
				}
			}
			if err := json.Unmarshal([]byte(n.patch), &patch); err != nil {
				t.Fatal(err)
			}
			if patch.Spec.Endpoint.Host != "10.10.0.10" || patch.Spec.Endpoint.Port != tc.want {
				t.Fatalf("incorrect infrastructure endpoint: %+v", patch.Spec.Endpoint)
			}
		})
	}
}
