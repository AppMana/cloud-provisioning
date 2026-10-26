package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
)

type associationReader struct {
	fakeNode
	refName    string
	providerID string
}

func (n *associationReader) Exec(_ context.Context, args ...string) ([]byte, error) {
	command := strings.Join(args, " ")
	id := n.providerID
	if id == "" {
		id = "labcontainers://cldt/remote1"
	}
	switch {
	case strings.Contains(command, "/readyz"):
		return []byte("ok"), nil
	case strings.Contains(command, "get machine remote1"):
		return []byte(fmt.Sprintf(`{"spec":{"providerID":%q},"status":{"phase":"Running","nodeRef":{"name":%q}}}`, id, n.refName)), nil
	case strings.Contains(command, "get node remote1"):
		return []byte(fmt.Sprintf(`{"metadata":{"uid":"new-node"},"spec":{"providerID":%q}}`, id)), nil
	default:
		return nil, fmt.Errorf("unexpected operation: %s", command)
	}
}

// Node readiness cannot replace the imported cluster's CAPI association.
func TestReadyNodeDoesNotSubstituteForCAPIAssociation(t *testing.T) {
	for _, name := range []string{"", "different-node", "remote1"} {
		t.Run("name="+name, func(t *testing.T) {
			reader := &associationReader{refName: name}
			c := &Controller{Kube: &kube.Client{Bastion: reader, ControlPlanes: []string{"10.10.0.10"}}}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err := c.WaitAssociation(ctx, "cloud-provisioning", "remote1", "remote1")
			if (err == nil) != (name == "remote1") {
				t.Fatalf("nodeRef name %q: %v", name, err)
			}
		})
	}
}

// CAPA's observed provider ID must pass the same identity gate as a local VM.
func TestCAPAAssociationUsesObservedProviderIdentity(t *testing.T) {
	reader := &associationReader{refName: "remote1", providerID: "aws:///us-west-2a/i-0c1487c421cdf67e8"}
	c := &kube.Client{Bastion: reader, ControlPlanes: []string{"10.10.0.10"}}
	if err := kube.WaitMachineAssociation(context.Background(), c, "cloud-provisioning", "remote1", "remote1"); err != nil {
		t.Fatal(err)
	}
}
