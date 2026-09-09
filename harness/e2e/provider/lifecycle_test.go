package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
)

type pendingBootstrap struct {
	fakeNode
	denied  bool
	queried bool
}

func (n *pendingBootstrap) Exec(_ context.Context, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case strings.Contains(joined, "/readyz"):
		return []byte("ok"), nil
	case strings.Contains(joined, "get machine"):
		return []byte("remote1-bootstrap"), nil
	case strings.Contains(joined, "get secret"):
		n.queried = true
		if !strings.Contains(joined, "--ignore-not-found") {
			return nil, fmt.Errorf("NotFound: secret does not exist yet")
		}
		if n.denied {
			return nil, fmt.Errorf("Forbidden")
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected mutation or request: %s", joined)
	}
}

// Observed during the first kube-router lifecycle bringup: the Machine already
// referenced remote1-bootstrap while the join reconciler had not created it.
func TestBootstrapReferenceCanPrecedeTheSecret(t *testing.T) {
	for _, denied := range []bool{false, true} {
		n := &pendingBootstrap{denied: denied}
		c := &Controller{Kube: &kube.Client{Bastion: n, ControlPlanes: []string{"10.10.0.10"}}}
		ready, err := c.provision(context.Background(), "cloud-provisioning", "remote1", "remote1", "observed-uid", nil, []string{instanceFinalizer}, false)
		if ready {
			t.Fatal("unlaunched instance reported ready")
		}
		if (err != nil) != denied {
			t.Fatalf("denied=%v err=%v", denied, err)
		}
		if !n.queried {
			t.Fatal("bootstrap reference was not followed")
		}
	}
}
