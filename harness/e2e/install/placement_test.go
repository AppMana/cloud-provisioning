package install

import (
	"slices"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"testing"
)

// The placements a campaign walks only ever add endpoints. A remote
// that has joined stays joined, and what changes between rows is
// which site nodes hold tunnels — so a row measures a placement
// change rather than a rebuild.
func TestPlacementsOnlyGrow(t *testing.T) {
	widths := map[string]int{
		"control-plane": 1,
		"one-worker":    1,
		"two-workers":   2,
		"all-nodes":     99, // every node, however many that is
	}
	last := 0
	for _, p := range Placements {
		w, ok := widths[p.Name]
		if !ok {
			t.Fatalf("no width recorded for %q; add it deliberately", p.Name)
		}
		if w < last {
			t.Errorf("%s narrows the placement from %d to %d", p.Name, last, w)
		}
		last = w
	}
}

// A set selector carries commas, and helm reads a comma in --set as
// the separator between values. The chart's own README says the same,
// so a placement that names two nodes has to survive being passed.
func TestASetSelectorSurvivesHelm(t *testing.T) {
	got := escapeHelmValue(OnTwoWorkers.Endpoints)
	if !strings.Contains(got, `w1\,w2`) {
		t.Errorf("the comma was not escaped: %q", got)
	}
	// And a selector with no comma is passed through unchanged.
	if escapeHelmValue(OnAllNodes.Endpoints) != "all" {
		t.Errorf("a plain selector was altered: %q", escapeHelmValue(OnAllNodes.Endpoints))
	}
}

// Every placement is findable by the name a row spells, and an
// unknown one is an error naming what exists rather than a silent
// default that would run the wrong row.
func TestAnUnknownPlacementIsAnError(t *testing.T) {
	if _, err := PlacementNamed("two-workers", "k0s"); err != nil {
		t.Errorf("a known placement was not found: %v", err)
	}
	_, err := PlacementNamed("nowhere", "k0s")
	if err == nil {
		t.Fatal("an unknown placement was accepted")
	}
	if !strings.Contains(err.Error(), "control-plane") {
		t.Errorf("the error does not say what exists: %v", err)
	}
}

// Observed after the real k0s/kube-router all-nodes -> one-worker transition:
// every site dialer republished its key, but only w1 retained a tunnel address.
func TestActiveEndpointsAfterRetirement(t *testing.T) {
	data := map[string][]byte{}
	for _, name := range []string{"cp", "cp2", "cp3", "w1", "w2"} {
		data[tunnel.NodePublicKeyPrefix+name] = []byte("public-key")
		data[tunnel.TunnelAddressReservationPrefix+name] = []byte("10.100.0.1/24")
	}
	data[tunnel.NodeTunnelAddressPrefix+"w1"] = []byte("10.100.0.1/24")
	if got := activeEndpoints(data); !slices.Equal(got, []string{"w1"}) {
		t.Fatalf("active endpoints = %v", got)
	}
	data[tunnel.NodeTunnelAddressPrefix+"new"] = []byte("10.100.0.6/24")
	if got := activeEndpoints(data); !slices.Equal(got, []string{"w1"}) {
		t.Fatalf("unpublished replacement counted: %v", got)
	}
}

// A stock control plane carries only its distribution's own role label.
// A hostname selector alone does not opt in under the product's
// control-plane placement policy, so the row names the role the way the
// distribution publishes it.
func TestControlPlanePlacementNamesTheDistributionsRole(t *testing.T) {
	for distro, role := range map[string]string{
		"k0s":      "node.k0sproject.io/role=control-plane",
		"microk8s": "node.kubernetes.io/microk8s-controlplane",
		"kubeadm":  "node-role.kubernetes.io/control-plane",
	} {
		p, err := PlacementNamed("control-plane", distro)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(p.Endpoints, role+",") {
			t.Errorf("%s: control-plane row selects %q", distro, p.Endpoints)
		}
	}
	if _, err := PlacementNamed("control-plane", "unknown"); err == nil {
		t.Error("a distribution with no known role label was accepted")
	}
}
