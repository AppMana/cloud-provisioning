package k0s

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	"github.com/appmana/cloud-provisioning/harness/e2e/rig"
)

type calicoBastion struct {
	rig.Node
	site, tunnel string
}

func (b *calicoBastion) Exec(_ context.Context, args ...string) ([]byte, error) {
	command := strings.Join(args, " ")
	switch {
	case strings.Contains(command, "get nodes"):
		return []byte(`{"items":[{"metadata":{"name":"remote1"}}]}`), nil
	case strings.Contains(command, "k8s-app=calico-node"):
		return []byte(b.site), nil
	case strings.Contains(command, tunnelAgentSelector):
		return []byte(b.tunnel), nil
	}
	return nil, nil
}

func pods(nodes ...string) string {
	var items []string
	for _, n := range nodes {
		items = append(items, `{"spec":{"nodeName":"`+n+`"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

// A cloud node is measured by the product's agent, a site node by the site's,
// and the site's calico-node on a cloud node is a failure.
func TestCalicoReadinessIsMeasuredByTheAgentEachNodeShouldRun(t *testing.T) {
	for _, tc := range []struct {
		site, tunnel string
		ok           bool
	}{
		{pods("w1"), pods("remote1"), true},
		{pods("w1", "remote1"), pods("remote1"), false},
		{pods("w1", "remote1"), pods(), false},
		{pods(), pods("remote1"), false},
	} {
		k := &kube.Client{Bastion: &calicoBastion{site: tc.site, tunnel: tc.tunnel}, ControlPlanes: []string{"10.10.0.10"}}
		err := CalicoNodesReady(context.Background(), k, []string{"w1", "remote1"})
		if (err == nil) != tc.ok {
			t.Errorf("site %s tunnel %s: %v", tc.site, tc.tunnel, err)
		}
	}
}
