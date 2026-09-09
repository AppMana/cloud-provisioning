package claim

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type removalNode struct {
	rig.Node
	ref                   string
	remaining             string
	deleted               bool
	checkedInfrastructure bool
}

func (n *removalNode) Exec(ctx context.Context, args ...string) ([]byte, error) {
	command := strings.Join(args, " ")
	if strings.Contains(command, "get machine worker -o json") {
		return []byte(n.ref), nil
	}
	if strings.Contains(command, "delete provisionednodeclaim worker") {
		n.deleted = true
		return nil, nil
	}
	if strings.Contains(command, "get AWSMachine.infrastructure.cluster.x-k8s.io infra-worker") {
		n.checkedInfrastructure = true
		if n.remaining == "infrastructure" {
			return []byte("awsmachine/infra-worker"), nil
		}
	}
	if strings.Contains(command, "peer-public-key-worker") && n.remaining == "peer" {
		return []byte("old-key"), nil
	}
	if strings.Contains(command, "containernetmachine") {
		return nil, fmt.Errorf("wrong provider")
	}
	return nil, nil
}

// Real CAPA deletion leaves AWSMachine present while EC2 is shutting down.
// Checking the local provider resource would report a false success.
func TestRemovalWaitsForActualInfrastructureAndPeerCleanup(t *testing.T) {
	for _, remaining := range []string{"", "infrastructure", "peer"} {
		t.Run(remaining, func(t *testing.T) {
			n := &removalNode{ref: `{"spec":{"infrastructureRef":{"kind":"AWSMachine","apiGroup":"infrastructure.cluster.x-k8s.io","name":"infra-worker"}}}`, remaining: remaining}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			err := (&Remover{Kube: &kube.Client{Bastion: n, ControlPlanes: []string{"10.10.0.10"}}}).Delete(ctx, "worker", "ip-172-29-0-71")
			if (err != nil) != (remaining != "") {
				t.Fatalf("remaining=%s, error=%v", remaining, err)
			}
			if !n.deleted || !n.checkedInfrastructure {
				t.Fatal("did not follow actual infrastructure identity")
			}
		})
	}
}
func TestRemovalRequiresInfrastructureIdentityBeforeDeleting(t *testing.T) {
	n := &removalNode{ref: `{"spec":{}}`}
	err := (&Remover{Kube: &kube.Client{Bastion: n, ControlPlanes: []string{"10.10.0.10"}}}).Delete(context.Background(), "worker", "node")
	if err == nil || n.deleted {
		t.Fatalf("missing infrastructure identity must not initiate deletion: %v", err)
	}
}
