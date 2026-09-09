package install

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"github.com/appmana/cloud-provisioning/harness/e2e/cluster"
	"github.com/appmana/cloud-provisioning/harness/e2e/wait"
)

// Placement is which site nodes terminate tunnels.
//
// This is the one value a placement row changes, and the claim it
// tests is that changing it is invisible from the pod network: a node
// with no tunnel reaches a remote by transiting one that has one, so
// every ordered pair passes whatever the selector says. A row that
// fails is that claim being wrong.
//
// It is also the change under which the mesh has historically been
// most fragile, because it moves traffic between paths while both
// exist: a node that stops being an endpoint has to keep carrying
// what it carried until the remotes have learned the new way round.
type Placement struct {
	// Name is what a row calls this placement.
	Name string
	// Endpoints is the selector, in the form the chart takes: a label
	// selector, a set such as "kubernetes.io/hostname in (w1,w2)", or
	// the word "all".
	Endpoints string
}

// Common placements, in the order a campaign walks them: the number
// of endpoints only grows, so a remote that has joined stays joined
// and what changes between rows is which site nodes hold tunnels.
var (
	OnControlPlane = Placement{Name: "control-plane", Endpoints: "node-role.kubernetes.io/control-plane,kubernetes.io/hostname=cp"}
	OnOneWorker    = Placement{Name: "one-worker", Endpoints: "kubernetes.io/hostname=w1"}
	OnTwoWorkers   = Placement{Name: "two-workers", Endpoints: "kubernetes.io/hostname in (w1,w2)"}
	OnAllNodes     = Placement{Name: "all-nodes", Endpoints: "all"}
)

// Placements is the walk a campaign takes.
var Placements = []Placement{OnControlPlane, OnOneWorker, OnTwoWorkers, OnAllNodes}

// PlacementNamed returns one by name.
func PlacementNamed(name string) (Placement, error) {
	for _, p := range Placements {
		if p.Name == name {
			return p, nil
		}
	}
	var have []string
	for _, p := range Placements {
		have = append(have, p.Name)
	}
	return Placement{}, fmt.Errorf("no placement named %q (have %v)", name, have)
}

// Move changes which site nodes terminate tunnels, by upgrading the
// release the way an operator would.
//
// Through helm, with the same chart and the same values but one: a
// harness that edited a Deployment or restarted a pod to change this
// would be testing its own ability to move a tunnel rather than the
// product's.
func (p *Product) Move(ctx context.Context, to Placement, opts Options, dialerSHA string) error {
	opts.TunnelEndpoints = to.Endpoints
	if err := p.Install(ctx, opts, dialerSHA); err != nil {
		return fmt.Errorf("moving the tunnels to %s: %w", to.Name, err)
	}
	return p.WaitPlacement(ctx, to)
}

// WaitPlacement prevents a green matrix from using retained endpoints from the
// previous placement. It observes published membership, not just Helm values.
func (p *Product) WaitPlacement(ctx context.Context, to Placement) error {
	var want []string
	switch to.Name {
	case "control-plane":
		want = []string{"cp"}
	case "one-worker":
		want = []string{"w1"}
	case "two-workers":
		want = []string{"w1", "w2"}
	case "all-nodes":
		for _, n := range cluster.SiteNodes(p.Topology) {
			want = append(want, n.Name)
		}
	default:
		return fmt.Errorf("unknown placement %s", to.Name)
	}
	slices.Sort(want)
	return wait.Until(ctx, 8*time.Minute, "published endpoints did not settle to "+to.Name, func(ctx context.Context) error {
		out, err := p.Kube.Run(ctx, "-n", Namespace, "get", "secret", Namespace+"-peers", "-o", "json")
		if err != nil {
			return err
		}
		var secret struct {
			Data map[string][]byte `json:"data"`
		}
		if err := json.Unmarshal(out, &secret); err != nil {
			return fmt.Errorf("decoding endpoint membership: %w", err)
		}
		got := activeEndpoints(secret.Data)
		if !slices.Equal(got, want) {
			return fmt.Errorf("published endpoints %v; want %v", got, want)
		}
		// Membership can settle before a dialer has removed its old device.
		// Observe the kernel too, so no retained tunnel can mask an outage.
		iface := tunnel.InterfaceName(Namespace + "/" + Namespace + "-peers")
		for _, node := range cluster.SiteNodes(p.Topology) {
			raw, err := p.Rig.Node(node.Name).Exec(ctx, "ip", "-json", "link", "show", "type", "wireguard")
			if err != nil {
				return err
			}
			var links []struct {
				Name string `json:"ifname"`
			}
			if err := json.Unmarshal(raw, &links); err != nil {
				return err
			}
			present := slices.ContainsFunc(links, func(link struct {
				Name string `json:"ifname"`
			}) bool {
				return link.Name == iface
			})
			if present != slices.Contains(want, node.Name) {
				return fmt.Errorf("%s device %s present=%v; endpoint=%v", node.Name, iface, present, slices.Contains(want, node.Name))
			}
		}
		return nil
	})
}

// Site transit dialers also publish keys. Only a key paired with a controller-
// allocated address is an active endpoint; a reserved address does not qualify.
func activeEndpoints(data map[string][]byte) []string {
	var names []string
	for key, value := range data {
		name, ok := strings.CutPrefix(key, tunnel.NodePublicKeyPrefix)
		if ok && strings.TrimSpace(string(value)) != "" && strings.TrimSpace(string(data[tunnel.NodeTunnelAddressPrefix+name])) != "" {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
