package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// testRetention is what the chart ships as the default: six of the
// dialer's 30s polls.
const testRetention = 3 * time.Minute

// testNow is a fixed instant, so a retention window is arithmetic
// rather than a race with the clock.
var testNow = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

// departedSince pre-seeds the record pruneDeparted writes when it first
// finds a node gone from the selector, as of ago before testNow.
func departedSince(data map[string][]byte, name string, ago time.Duration) {
	data[tunnel.NodeDepartedAtPrefix+name] = []byte(testNow.Add(-ago).Format(time.RFC3339))
}

// The selector that says which nodes terminate a tunnel becomes the
// dialer DaemonSet's node affinity. Naming two nodes takes a set based
// term, whose commas belong to the term rather than separating terms,
// so splitting on commas produces fragments that match nothing and an
// affinity with no requirements at all. That does not fail: it places
// a dialer on every node, which is the opposite of what was asked for.
func TestParseSelectorRequirements(t *testing.T) {
	for _, tc := range []struct {
		name  string
		raw   string
		want  []corev1.NodeSelectorRequirement
		empty bool
	}{
		{
			name: "one node by name",
			raw:  "kubernetes.io/hostname=cldt-worker",
			want: []corev1.NodeSelectorRequirement{{
				Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{"cldt-worker"},
			}},
		},
		{
			name: "two nodes, the case that was silently dropped",
			raw:  "kubernetes.io/hostname in (cldt-worker,cldt-worker2)",
			want: []corev1.NodeSelectorRequirement{{
				Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{"cldt-worker", "cldt-worker2"},
			}},
		},
		{
			name: "a label that only has to exist",
			raw:  "node-role.kubernetes.io/control-plane",
			want: []corev1.NodeSelectorRequirement{{
				Key: "node-role.kubernetes.io/control-plane", Operator: corev1.NodeSelectorOpExists,
			}},
		},
		{
			name: "excluding a node",
			raw:  "kubernetes.io/hostname!=cldt-worker",
			want: []corev1.NodeSelectorRequirement{{
				Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpNotIn, Values: []string{"cldt-worker"},
			}},
		},
		{
			// Nothing is better than a term that matches everything.
			name:  "nonsense selects nothing rather than everything",
			raw:   "this is not a selector",
			empty: true,
		},
		{
			// A label parser reads this as "the label all must exist",
			// which places the DaemonSet on no node at all.
			name:  "the every-node sentinel constrains nothing",
			raw:   "all",
			empty: true,
		},
		{
			name:  "the other spelling of it",
			raw:   "*",
			empty: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSelectorRequirements(tc.raw)
			if tc.empty {
				if len(got) != 0 {
					t.Fatalf("parseSelectorRequirements(%q) = %+v, want none", tc.raw, got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseSelectorRequirements(%q) = %+v, want %+v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i].Key != tc.want[i].Key || got[i].Operator != tc.want[i].Operator {
					t.Errorf("requirement %d = %+v, want %+v", i, got[i], tc.want[i])
				}
				if len(got[i].Values) != len(tc.want[i].Values) {
					t.Fatalf("requirement %d values = %v, want %v", i, got[i].Values, tc.want[i].Values)
				}
				for j := range got[i].Values {
					if got[i].Values[j] != tc.want[i].Values[j] {
						t.Errorf("requirement %d value %d = %q, want %q", i, j, got[i].Values[j], tc.want[i].Values[j])
					}
				}
			}
		})
	}
}

// "all" means every node at this site, not every node in the cluster.
//
// A node this operator provisioned is on the far side of a tunnel. It
// is addressed by its tunnel address, because that is the only address
// the site can reach it by, and it already reaches the mesh as a peer.
// Counting it as one of the site's own ends gives it a contradictory
// second identity: measured, its address was pinned to the one the
// site cannot reach it by, and the tunnel address the mesh had
// assigned was overwritten.
func TestIsTunnelEndpoint_NeverAProvisionedNode(t *testing.T) {
	provisioned := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "public-worker",
		Labels: map[string]string{"kubernetes.io/os": "linux", cloudWorkerRoleLabel: cloudWorkerRoleValue},
	}}
	site := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "worker",
		Labels: map[string]string{"kubernetes.io/os": "linux"},
	}}

	for _, raw := range []string{"all", "*", ""} {
		r := &meshReconciler{tunnelEndpointsRaw: raw}
		if r.isTunnelEndpoint(provisioned) {
			t.Errorf("selector %q counted a provisioned node as one of the site's tunnel endpoints", raw)
		}
		if !r.isTunnelEndpoint(site) {
			t.Errorf("selector %q did not count an ordinary site node", raw)
		}
	}
}

// published builds the mesh state this reconciler writes: the endpoint
// nodes with their keys and allocated addresses, and the site nodes
// with theirs.
//
// Every node gets pod blocks of its own, as a real network hands them
// out. Giving two nodes the same block would hide exactly the fault the
// accept list's one-owner-per-prefix rule exists to prevent.
func published(endpoints map[string][2]string, sites map[string]string) map[string][]byte {
	data := map[string][]byte{}
	block := 0
	for _, name := range sortedKeys(endpoints) {
		pair := endpoints[name]
		if pair[0] != "" {
			data[tunnel.NodePublicKeyPrefix+name] = []byte(pair[0])
		}
		if pair[1] != "" {
			data[tunnel.NodeTunnelAddressPrefix+name] = []byte(pair[1])
			// The reconciler records a reservation whenever it allocates,
			// because the address belongs to the node from then on.
			data[tunnel.TunnelAddressReservationPrefix+name] = []byte(pair[1])
		}
		block++
		data[tunnel.NodeAddressesPrefix+name] = []byte(fmt.Sprintf("172.21.0.%d", 30+block))
		data[tunnel.NodePodCIDRsPrefix+name] = []byte(fmt.Sprintf("10.244.%d.0/26", block))
	}
	for _, name := range sortedKeys(sites) {
		data[tunnel.SiteAddressesPrefix+name] = []byte(sites[name])
		block++
		data[tunnel.SitePodCIDRsPrefix+name] = []byte(fmt.Sprintf("10.244.%d.0/26", block))
	}
	return data
}

func sortedKeys[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func members(names ...string) map[string]bool {
	set := map[string]bool{}
	for _, name := range names {
		set[name] = true
	}
	return set
}

// Nothing removed what a node published once it stopped being what it
// published as. Measured: the selector was moved from w1 to cp, cp came
// up and dialed the remote correctly, and the remote's peer list still
// held exactly one peer carrying w1's key and w1's tunnel address, with
// a handshake six minutes stale. It never learned cp, lost its only
// path to the API server, and went NotReady.
func TestPruneDeparted(t *testing.T) {
	const (
		keyW1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA="
		keyCP = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB="
	)
	for _, tc := range []struct {
		name string
		data map[string][]byte
		// departed pre-seeds a node's departure record as of this long
		// before testNow, which is how a case sits inside or past the
		// retention window without waiting for one.
		departed map[string]time.Duration
		want     meshMembership
		changed  bool
		gone     []string
		kept     []string
		retired  string
	}{
		{
			// The measured case. The remote learns where the new
			// endpoint is by reading the Secret over the tunnel the old
			// one carries, so this pass must not take that tunnel away:
			// it records the departure and changes nothing else.
			name: "the pass that first finds an endpoint deselected retains it",
			data: published(
				map[string][2]string{"w1": {keyW1, "10.100.0.1/24"}, "cp": {keyCP, "10.100.0.2/24"}},
				map[string]string{"w2": "172.21.0.17"},
			),
			want:    meshMembership{endpoints: members("cp"), siteNodes: members("w1", "w2")},
			changed: true,
			kept: []string{
				tunnel.NodePublicKeyPrefix + "w1", tunnel.NodeTunnelAddressPrefix + "w1",
				tunnel.NodeAddressesPrefix + "w1", tunnel.NodePodCIDRsPrefix + "w1",
				tunnel.NodeDepartedAtPrefix + "w1",
				tunnel.NodePublicKeyPrefix + "cp", tunnel.NodeTunnelAddressPrefix + "cp",
			},
		},
		{
			name: "a deselected endpoint is still an endpoint inside the window",
			data: published(
				map[string][2]string{"w1": {keyW1, "10.100.0.1/24"}, "cp": {keyCP, "10.100.0.2/24"}},
				nil,
			),
			departed: map[string]time.Duration{"w1": testRetention - time.Minute},
			want:     meshMembership{endpoints: members("cp"), siteNodes: members("w1")},
			changed:  false,
			kept: []string{
				tunnel.NodePublicKeyPrefix + "w1", tunnel.NodeTunnelAddressPrefix + "w1",
				tunnel.NodeDepartedAtPrefix + "w1",
			},
		},
		{
			// Long enough for six of the dialer's polls to have gone by.
			// Whatever a remote has not learned by now it will not learn
			// from this tunnel.
			name: "the window expires and the departed endpoint is released",
			data: published(
				map[string][2]string{"w1": {keyW1, "10.100.0.1/24"}, "cp": {keyCP, "10.100.0.2/24"}},
				map[string]string{"w2": "172.21.0.17"},
			),
			departed: map[string]time.Duration{"w1": testRetention + time.Minute},
			want:     meshMembership{endpoints: members("cp"), siteNodes: members("w1", "w2")},
			changed:  true,
			gone: []string{
				tunnel.NodePublicKeyPrefix + "w1", tunnel.NodeTunnelAddressPrefix + "w1",
				tunnel.NodeAddressesPrefix + "w1", tunnel.NodePodCIDRsPrefix + "w1",
				tunnel.NodeDepartedAtPrefix + "w1",
			},
			kept: []string{
				tunnel.NodePublicKeyPrefix + "cp", tunnel.NodeTunnelAddressPrefix + "cp",
				tunnel.SiteAddressesPrefix + "w2",
				// w1 is still a node of this site, so it keeps the
				// address it has always had. Selecting it again makes it
				// the peer every remote already knows.
				tunnel.TunnelAddressReservationPrefix + "w1",
			},
			// Nothing is retired: retirement is for an address whose
			// node has left the cluster, and w1 has not.
			retired: "",
		},
		{
			// The operator changed their mind, or changed it back. The
			// node never stopped being an endpoint, so there is nothing
			// to undo but the record.
			name: "a node the selector takes back before the window is over keeps everything",
			data: published(
				map[string][2]string{"w1": {keyW1, "10.100.0.1/24"}, "cp": {keyCP, "10.100.0.2/24"}},
				nil,
			),
			departed: map[string]time.Duration{"w1": testRetention - time.Minute},
			want:     meshMembership{endpoints: members("w1", "cp"), siteNodes: members()},
			changed:  true,
			gone:     []string{tunnel.NodeDepartedAtPrefix + "w1"},
			kept: []string{
				tunnel.NodePublicKeyPrefix + "w1", tunnel.NodeTunnelAddressPrefix + "w1",
				tunnel.NodeAddressesPrefix + "w1",
			},
		},
		{
			// And the clock does not go on running underneath it: the
			// window it left behind is not a window it is still in.
			name: "a node taken back and let go again gets the whole window over",
			data: published(
				map[string][2]string{"w1": {keyW1, "10.100.0.1/24"}, "cp": {keyCP, "10.100.0.2/24"}},
				nil,
			),
			departed: map[string]time.Duration{"w1": 10 * testRetention},
			want:     meshMembership{endpoints: members("w1", "cp"), siteNodes: members()},
			changed:  true,
			gone:     []string{tunnel.NodeDepartedAtPrefix + "w1"},
			kept:     []string{tunnel.NodePublicKeyPrefix + "w1", tunnel.NodeTunnelAddressPrefix + "w1"},
		},
		{
			// Make before break. The address is allocated one pass
			// before the node's own dialer publishes its key, and a
			// remote cannot use half a peer.
			name: "the new endpoint has not published its key yet",
			data: published(
				map[string][2]string{"w1": {keyW1, "10.100.0.1/24"}, "cp": {"", "10.100.0.2/24"}},
				nil,
			),
			want:    meshMembership{endpoints: members("cp"), siteNodes: members("w1")},
			changed: false,
			kept: []string{
				tunnel.NodePublicKeyPrefix + "w1", tunnel.NodeTunnelAddressPrefix + "w1",
			},
			// The clock does not start either: retention is time for a
			// remote to learn about a replacement, and there is not one
			// yet to learn about.
			gone: []string{tunnel.NodeDepartedAtPrefix + "w1"},
		},
		{
			name: "a deleted node loses everything it published",
			data: published(
				map[string][2]string{"w1": {keyW1, "10.100.0.1/24"}, "cp": {keyCP, "10.100.0.2/24"}},
				map[string]string{"w2": "172.21.0.17"},
			),
			want:    meshMembership{endpoints: members("cp"), siteNodes: members()},
			changed: true,
			gone: []string{
				tunnel.NodeTunnelAddressPrefix + "w1",
				tunnel.SiteAddressesPrefix + "w2", tunnel.SitePodCIDRsPrefix + "w2",
			},
			retired: "10.100.0.1",
		},
		{
			// One owner per prefix. A node that has become an endpoint
			// carries its own addresses, and the same addresses left on
			// a site entry are permitted a second time, on the peer that
			// relays to the site.
			name:    "a node that became an endpoint loses its site entry",
			data:    published(map[string][2]string{"cp": {keyCP, "10.100.0.2/24"}}, map[string]string{"cp": "172.21.0.18"}),
			want:    meshMembership{endpoints: members("cp"), siteNodes: members()},
			changed: true,
			gone:    []string{tunnel.SiteAddressesPrefix + "cp", tunnel.SitePodCIDRsPrefix + "cp"},
			kept:    []string{tunnel.NodeTunnelAddressPrefix + "cp"},
		},
		{
			// The other half of make before break: until it is a peer in
			// its own right, the remote still reaches it by relaying.
			name: "a node becoming an endpoint keeps its site entry until it is published",
			data: published(
				map[string][2]string{"w1": {keyW1, "10.100.0.1/24"}, "cp": {"", "10.100.0.2/24"}},
				map[string]string{"cp": "172.21.0.18"},
			),
			want:    meshMembership{endpoints: members("w1", "cp"), siteNodes: members()},
			changed: false,
			kept:    []string{tunnel.SiteAddressesPrefix + "cp", tunnel.SitePodCIDRsPrefix + "cp"},
		},
		{
			// A read that returns nothing is a failed read until proven
			// otherwise, the same judgement transitSpeaker.reconcile
			// makes about withdrawing routes.
			name:    "a membership that reads as empty prunes nothing",
			data:    published(map[string][2]string{"w1": {keyW1, "10.100.0.1/24"}}, map[string]string{"w2": "172.21.0.17"}),
			want:    meshMembership{endpoints: members(), siteNodes: members()},
			changed: false,
			kept: []string{
				tunnel.NodePublicKeyPrefix + "w1", tunnel.NodeTunnelAddressPrefix + "w1",
				tunnel.SiteAddressesPrefix + "w2",
			},
		},
		{
			// The last endpoint being taken away is the one case where
			// staleness is cheaper than correctness: a remote with no
			// peer at all cannot be reached to be fixed.
			name:    "the only endpoint departing leaves the remote a stale peer rather than none",
			data:    published(map[string][2]string{"w1": {keyW1, "10.100.0.1/24"}}, nil),
			want:    meshMembership{endpoints: members(), siteNodes: members("w1", "w2")},
			changed: false,
			kept:    []string{tunnel.NodePublicKeyPrefix + "w1", tunnel.NodeTunnelAddressPrefix + "w1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, ago := range tc.departed {
				departedSince(tc.data, name, ago)
			}
			changed := pruneDeparted(tc.data, tc.want, testNow, testRetention, nil)
			if changed != tc.changed {
				t.Errorf("pruneDeparted reported changed=%v, want %v", changed, tc.changed)
			}
			for _, key := range tc.gone {
				if _, ok := tc.data[key]; ok {
					t.Errorf("%s is still published", key)
				}
			}
			for _, key := range tc.kept {
				if _, ok := tc.data[key]; !ok {
					t.Errorf("%s was removed", key)
				}
			}
			retired := string(tc.data[tunnel.RetiredTunnelAddressesKey])
			if retired != tc.retired {
				t.Errorf("retired addresses = %q, want %q", retired, tc.retired)
			}
		})
	}
}

// The remote's peer list is derived from what the Secret holds, so the
// migration is only finished when that list names the new endpoint and
// nothing else. It must also never be empty on the way: the adoption
// Secret is the remote's only path to the API server.
func TestPruneDeparted_TheRemoteFollowsTheNewEndpoint(t *testing.T) {
	const (
		keyW1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA="
		keyCP = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB="
	)
	data := published(map[string][2]string{"w1": {keyW1, "10.100.0.1/24"}}, map[string]string{"cp": "172.21.0.18", "w2": "172.21.0.17"})
	r := &meshReconciler{tunnelEndpointsRaw: "node-role.kubernetes.io/control-plane,kubernetes.io/hostname=cp"}
	selector, err := labels.Parse(r.tunnelEndpointsRaw)
	if err != nil {
		t.Fatalf("parsing the selector: %v", err)
	}
	r.tunnelEndpointSelector = selector
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "cp", Labels: map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/hostname": "cp", controlPlaneLabel: "true"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "w1", Labels: map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/hostname": "w1"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "w2", Labels: map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/hostname": "w2"}}},
	}

	// The pass that allocates cp an address, before its dialer has
	// published a key. w1 still holds the tunnel and must still be the
	// remote's peer.
	pruneDeparted(data, r.membership(nodes), testNow, testRetention, nil)
	data[tunnel.NodeTunnelAddressPrefix+"cp"] = []byte("10.100.0.3/24")
	peers, err := tunnel.RemotePeers(data, "10.100.0.128", nil)
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}
	if len(peers) != 1 || peers[0].PublicKey != keyW1 {
		t.Fatalf("mid-migration the remote has %d peers (%+v), want only w1", len(peers), peers)
	}

	// cp's dialer publishes. w1 is retained for the window, so the
	// remote now has two peers and two working paths, and reads the one
	// naming cp over the one w1 still carries.
	data[tunnel.NodePublicKeyPrefix+"cp"] = []byte(keyCP)
	if !pruneDeparted(data, r.membership(nodes), testNow, testRetention, nil) {
		t.Fatal("the departure of the old endpoint was not recorded")
	}
	peers, err = tunnel.RemotePeers(data, "10.100.0.128", nil)
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("during retention the remote has %d peers (%+v), want w1 and cp", len(peers), peers)
	}

	// And the window runs out.
	if !pruneDeparted(data, r.membership(nodes), testNow.Add(testRetention), testRetention, nil) {
		t.Fatal("nothing was pruned once the window had run out")
	}
	peers, err = tunnel.RemotePeers(data, "10.100.0.128", nil)
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}
	if len(peers) != 1 {
		t.Fatalf("the remote has %d peers (%+v), want only cp", len(peers), peers)
	}
	if peers[0].PublicKey != keyCP {
		t.Errorf("the remote's peer is %q, want cp's key %q", peers[0].PublicKey, keyCP)
	}
	// w1 is a site node now, reached by relaying through cp, and its
	// addresses are permitted on exactly one peer.
	if _, ok := data[tunnel.NodeAddressesPrefix+"w1"]; ok {
		t.Error("w1 is still published as an endpoint")
	}
}

// An outage is not a departure. A node that is NotReady, whose dialer
// pod is gone, or whose handshake has gone stale is expected back, and
// tearing its entry out churns every remote in the mesh while the fault
// is probably elsewhere. Only the selector and the node's existence
// decide membership, so none of that can reach this.
func TestMembership_HealthIsNotIntent(t *testing.T) {
	r := &meshReconciler{tunnelEndpointsRaw: ""}
	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "w1", Labels: map[string]string{"kubernetes.io/os": "linux"}},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
			}},
			Spec: corev1.NodeSpec{Unschedulable: true},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "cp", Labels: map[string]string{"kubernetes.io/os": "linux"}},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			}},
		},
	}
	want := r.membership(nodes)
	if !want.endpoints["w1"] {
		t.Error("a NotReady, cordoned endpoint was treated as departed")
	}

	// And nothing it published is removed for it.
	data := published(map[string][2]string{
		"w1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"},
		"cp": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.2/24"},
	}, nil)
	if pruneDeparted(data, want, testNow, testRetention, nil) {
		t.Error("an unhealthy node's published entries were pruned")
	}
}

// An address a departed node held is never handed to a later node. A
// remote still holding configuration that names it would otherwise send
// that node's traffic somewhere else entirely.
func TestATunnelAddressIsAnIdentityNotALease(t *testing.T) {
	// A node that leaves the endpoint selector but stays in the cluster
	// keeps its address, so that selecting it again makes it the peer
	// every remote is already configured for. This is the contract
	// every mesh of this kind settles on: Tailscale assigns an address
	// at registration and never changes it, and Headscale returns one
	// to its allocator only when the node record is deleted.
	data := published(map[string][2]string{
		"w1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"},
		"cp": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.2/24"},
	}, nil)
	data[tunnel.TunnelAddressReservationPrefix+"w1"] = []byte("10.100.0.1/24")
	data[tunnel.TunnelAddressReservationPrefix+"cp"] = []byte("10.100.0.2/24")

	// w1 leaves the selector but remains a node of this site.
	want := meshMembership{endpoints: members("cp"), siteNodes: members("w1")}
	if !pruneDeparted(data, want, testNow, testRetention, nil) {
		t.Fatal("the departure of the endpoint was not recorded")
	}
	if !pruneDeparted(data, want, testNow.Add(testRetention), testRetention, nil) {
		t.Fatal("the departed endpoint was not pruned once its window was over")
	}
	if got := string(data[tunnel.TunnelAddressReservationPrefix+"w1"]); got != "10.100.0.1/24" {
		t.Errorf("w1 kept %q; a node still in the cluster keeps its address", got)
	}

	// And nobody else may be given it, which is the whole reason the old
	// address was retired on departure.
	used := allocatorUsedSet(data)
	next, err := nextFreeAddress("10.100.0.1/24", used)
	if err != nil {
		t.Fatalf("nextFreeAddress: %v", err)
	}
	if next == "10.100.0.1/24" || next == "10.100.0.2/24" {
		t.Errorf("next free address = %q, which belongs to a node that still holds it", next)
	}
	if next != "10.100.0.3/24" {
		t.Errorf("next free address = %q, want 10.100.0.3/24", next)
	}
}

func TestAnAddressIsRetiredOnlyWhenItsNodeLeavesTheCluster(t *testing.T) {
	data := published(map[string][2]string{
		"w1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"},
		"cp": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.2/24"},
	}, nil)
	data[tunnel.TunnelAddressReservationPrefix+"w1"] = []byte("10.100.0.1/24")

	// w1 is gone from the cluster entirely: not an endpoint, not a site
	// node. Nothing can schedule a dialer on it ever again.
	want := meshMembership{endpoints: members("cp"), siteNodes: members("cp")}
	pruneDeparted(data, want, testNow, testRetention, nil)
	pruneDeparted(data, want, testNow.Add(testRetention), testRetention, nil)

	if got := string(data[tunnel.RetiredTunnelAddressesKey]); got != "10.100.0.1" {
		t.Errorf("retired = %q, want 10.100.0.1: a node that has left the cluster gives its address up", got)
	}
	if _, ok := data[tunnel.TunnelAddressReservationPrefix+"w1"]; ok {
		t.Error("a node that has left the cluster keeps no reservation")
	}
	// Still never handed out again: a remote may hold configuration
	// naming it, and the key behind it is gone.
	used := allocatorUsedSet(data)
	next, err := nextFreeAddress("10.100.0.1/24", used)
	if err != nil {
		t.Fatalf("nextFreeAddress: %v", err)
	}
	if next == "10.100.0.1/24" {
		t.Error("the departed node's address was handed straight to the next node")
	}
}

// allocatorUsedSet builds the set of unavailable addresses exactly as the
// mesh reconciler does, so these tests cannot pass on a set the real
// allocator would not have used.
func allocatorUsedSet(data map[string][]byte) map[string]bool {
	used := map[string]bool{}
	for key, val := range data {
		if strings.HasPrefix(key, tunnel.NodeTunnelAddressPrefix) ||
			strings.HasPrefix(key, tunnel.TunnelAddressReservationPrefix) {
			used[strings.SplitN(strings.TrimSpace(string(val)), "/", 2)[0]] = true
		}
	}
	for _, addr := range tunnel.SplitList(string(data[tunnel.RetiredTunnelAddressesKey])) {
		used[addr] = true
	}
	return used
}

// matchesNode is the scheduler's reading of a node affinity, reduced to
// what this affinity uses: expressions over labels, ORed across terms
// and ANDed within one, and metadata.name as a field.
func matchesNode(sel *corev1.NodeSelector, node *corev1.Node) bool {
	for _, term := range sel.NodeSelectorTerms {
		matched := true
		for _, req := range term.MatchExpressions {
			value, present := node.Labels[req.Key]
			if !requirementMatches(req, value, present) {
				matched = false
				break
			}
		}
		for _, req := range term.MatchFields {
			if req.Key != "metadata.name" {
				matched = false
				break
			}
			if !requirementMatches(req, node.Name, true) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func requirementMatches(req corev1.NodeSelectorRequirement, value string, present bool) bool {
	in := false
	for _, want := range req.Values {
		if want == value {
			in = true
		}
	}
	switch req.Operator {
	case corev1.NodeSelectorOpIn:
		return present && in
	case corev1.NodeSelectorOpNotIn:
		return !present || !in
	case corev1.NodeSelectorOpExists:
		return present
	case corev1.NodeSelectorOpDoesNotExist:
		return !present
	}
	return false
}

// The dialer runs on every Linux node at the site, and placement never
// appears in its affinity. Which nodes hold tunnels is decided by the
// Secret; a node with no tunnel runs the same dialer for the transit
// it derives from that same Secret, and a node the selector let go of
// keeps its dialer through retention and past it, because the process
// that built its interface is the one that tears it down. What the
// affinity still decides is what a site node is at all: a node this
// operator provisioned is on the far side of a tunnel, and Windows
// terminates nothing.
func TestDialerNodeAffinity(t *testing.T) {
	linux := func(name string, extra map[string]string) *corev1.Node {
		labels := map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/hostname": name}
		for k, v := range extra {
			labels[k] = v
		}
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	var (
		cp          = linux("cp", map[string]string{controlPlaneLabel: ""})
		w1          = linux("w1", nil)
		w2          = linux("w2", nil)
		provisioned = linux("remote1", map[string]string{cloudWorkerRoleLabel: cloudWorkerRoleValue})
		windows     = &corev1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:   "win1",
			Labels: map[string]string{"kubernetes.io/os": "windows", "kubernetes.io/hostname": "win1"},
		}}
	)

	affinity := dialerNodeAffinity()
	for _, node := range []*corev1.Node{cp, w1, w2} {
		if !matchesNode(affinity, node) {
			t.Errorf("%s runs no dialer, so it either terminates no tunnel or has no transit to the remotes", node.Name)
		}
	}
	for _, node := range []*corev1.Node{provisioned, windows} {
		if matchesNode(affinity, node) {
			t.Errorf("%s was given a site dialer", node.Name)
		}
	}
}

// During retention a remote has two peers, and the accept list still has
// one owner per prefix: each carries its own node's addresses and blocks,
// and the site nodes that terminate no tunnel go on exactly one of them.
// A prefix on two peers is resolved by whichever was written last, which
// is the failure this reconciler exists to avoid.
func TestRetainedAndNewEndpointCarryDisjointPrefixes(t *testing.T) {
	const (
		keyW1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA="
		keyCP = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB="
	)
	data := published(
		map[string][2]string{"w1": {keyW1, "10.100.0.1/24"}, "cp": {keyCP, "10.100.0.2/24"}},
		map[string]string{"w2": "172.21.0.17"},
	)
	want := meshMembership{endpoints: members("cp"), siteNodes: members("w1", "w2")}
	if !pruneDeparted(data, want, testNow, testRetention, nil) {
		t.Fatal("the departure of the old endpoint was not recorded")
	}

	peers, err := tunnel.RemotePeers(data, "10.100.0.128", []string{"172.21.0.10"})
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("the remote has %d peers (%+v), want the retained one and the new one", len(peers), peers)
	}
	keys := map[string]bool{peers[0].PublicKey: true, peers[1].PublicKey: true}
	if !keys[keyW1] || !keys[keyCP] {
		t.Fatalf("the remote's peers are %+v, want both w1 and cp", peers)
	}
	owner := map[string]string{}
	for _, peer := range peers {
		for _, prefix := range peer.WGAllowedIPs {
			if held, ok := owner[prefix]; ok {
				t.Errorf("%s is permitted on both %s and %s", prefix, held, peer.PublicKey)
				continue
			}
			owner[prefix] = peer.PublicKey
		}
	}
	// Both tunnel addresses are reachable, which is what makes this two
	// paths rather than a gamble on which one the remote picked.
	for _, addr := range []string{"10.100.0.1/32", "10.100.0.2/32"} {
		if owner[addr] == "" {
			t.Errorf("%s is permitted on neither peer", addr)
		}
	}
}

// The release of a retained endpoint is a deadline, and no event
// coincides with it: no node and no Machine changes when the window runs
// out. Without a requeue the entries would sit published until the next
// full resync, which is hours, and the migration would look stuck.
func TestSoonestRelease(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string][]byte
		want time.Duration
	}{
		{
			name: "nothing retained asks for nothing",
			data: map[string][]byte{tunnel.NodePublicKeyPrefix + "w1": []byte("k")},
			want: 0,
		},
		{
			// Just past the deadline rather than exactly on it: a timer
			// that fires a hair early reads as not yet expired, and the
			// node would be held a second whole window.
			name: "the nearest deadline, a moment after it",
			data: func() map[string][]byte {
				data := map[string][]byte{}
				departedSince(data, "w1", testRetention-time.Minute)
				departedSince(data, "w2", testRetention-2*time.Minute)
				return data
			}(),
			want: time.Minute + time.Second,
		},
		{
			// Already expired and still here means it is waiting on a
			// surviving endpoint being published, not on the clock.
			name: "a window already run out comes back on the dialer's cadence",
			data: func() map[string][]byte {
				data := map[string][]byte{}
				departedSince(data, "w1", 10*testRetention)
				return data
			}(),
			want: 30 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := soonestRelease(tc.data, testNow, testRetention); got != tc.want {
				t.Errorf("soonestRelease = %s, want %s", got, tc.want)
			}
		})
	}
}

// A pull secret reference with an empty name is not an empty list. The
// API accepts it, and then name is the merge key for that list and the
// element has none, so every strategic merge patch against the
// DaemonSet is rejected: "does not contain declared merge key: name".
// kubectl rollout restart, set image and apply all fail, which is an
// operator unable to restart the dialers on a cluster where nothing
// looks wrong.
func TestImagePullSecretsOmittedWhenUnset(t *testing.T) {
	if got := imagePullSecrets(""); got != nil {
		t.Errorf("imagePullSecrets(\"\") = %#v, want nil: a reference to no secret breaks every merge patch on the pod spec", got)
	}
	got := imagePullSecrets("regcred")
	if len(got) != 1 || got[0].Name != "regcred" {
		t.Errorf("imagePullSecrets(\"regcred\") = %#v, want one reference named regcred", got)
	}
}

// A toleration decides nothing about where a pod goes; it removes an
// objection. A control plane carries a NoSchedule taint, and its
// dialer has a job there whether or not it holds a tunnel, so the
// toleration is unconditional.
func TestDialerToleratesAControlPlane(t *testing.T) {
	tol := dialerTolerations()
	if len(tol) == 0 {
		t.Fatal("no tolerations: a control plane can never be scheduled a dialer")
	}
	found := false
	for _, tl := range tol {
		if tl.Key == controlPlaneLabel && tl.Effect == corev1.TaintEffectNoSchedule {
			found = true
		}
	}
	if !found {
		t.Errorf("tolerations %#v do not cover the control-plane NoSchedule taint", tol)
	}
}

// Retention exists so a remote can move off a departing endpoint before
// that endpoint's tunnel goes away. It can only move if something else
// already owns the prefixes it needs, so ownership has to change at the
// START of the window while the old tunnel still carries traffic.
//
// Holding both together meant the handover happened at the same instant
// as the teardown. Measured on cp, the node carrying the API server's
// address: the remote permitted 10.10.0.10 on cp's peer entry and
// reached the API; six seconds later cp was unpublished and its
// interface swept, the remote still named cp for 10.10.0.10, and the
// list that would have corrected it was only reachable through cp.
func TestADepartingEndpointStopsOwningPrefixesAtOnce(t *testing.T) {
	data := published(map[string][2]string{
		"cp": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.2/24"},
		"w1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"},
	}, nil)
	addr := string(data[tunnel.NodeAddressesPrefix+"cp"])
	if addr == "" {
		t.Fatal("fixture published no node address for cp")
	}

	if !stopOwningAsEndpoint(data, "cp") {
		t.Fatal("nothing changed; cp still owns the prefixes a remote needs to reach the site")
	}
	// Gone from cp's own peer entry, so the site entries published in the
	// same pass are the only owner. One prefix, one owner: the accept
	// list keeps whichever was written last, so naming it twice decides
	// the path by map ordering.
	for _, key := range []string{
		tunnel.NodeAddressesPrefix + "cp",
		tunnel.NodePodCIDRsPrefix + "cp",
	} {
		if _, ok := data[key]; ok {
			t.Errorf("%s is still owned by the departing endpoint", key)
		}
	}
	// Kept, because they are what makes the tunnel it still holds usable
	// for everything already crossing it.
	for _, key := range []string{
		tunnel.NodePublicKeyPrefix + "cp",
		tunnel.NodeTunnelAddressPrefix + "cp",
	} {
		if len(data[key]) == 0 {
			t.Errorf("%s went; the departing tunnel has to keep working through the window", key)
		}
	}
	// Idempotent: a second pass has nothing left to move.
	if stopOwningAsEndpoint(data, "cp") {
		t.Error("reported a change with nothing left to change, so every pass would rewrite the Secret")
	}
	// And a surviving endpoint is untouched.
	if len(data[tunnel.NodeAddressesPrefix+"w1"]) == 0 {
		t.Error("w1 lost its addresses; only the departing endpoint hands its prefixes over")
	}
}

// The handover has to run both ways. A node that leaves the selector
// hands its prefixes to the site entries so a surviving endpoint relays
// them; a node that returns takes them back. Doing only the first half
// left a returning endpoint owning its block twice, on its own peer
// entry and on whichever endpoint relays the site, and WireGuard
// resolves a prefix named on two peers by keeping whichever was written
// last. Measured after one placement change: w1 published as both
// node-pod-cidrs-w1 and site-pod-cidrs-w1, the same block.
func TestAPrefixIsOwnedInExactlyOnePlaceAcrossAReturn(t *testing.T) {
	data := published(map[string][2]string{
		"cp": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.2/24"},
		"w1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"},
	}, nil)
	block := string(data[tunnel.NodePodCIDRsPrefix+"w1"])
	addr := string(data[tunnel.NodeAddressesPrefix+"w1"])

	// w1 leaves: its prefixes move to the site entries, as the reconciler
	// publishes them, and off its own entry.
	data[tunnel.SiteAddressesPrefix+"w1"] = []byte(addr)
	data[tunnel.SitePodCIDRsPrefix+"w1"] = []byte(block)
	if !stopOwningAsEndpoint(data, "w1") {
		t.Fatal("w1 kept owning its prefixes while departing")
	}
	if _, ok := data[tunnel.NodePodCIDRsPrefix+"w1"]; ok {
		t.Fatal("departing endpoint still owns its block")
	}

	// w1 returns.
	if !stopBeingRelayed(data, "w1") {
		t.Fatal("nothing was taken back; the returning endpoint is relayed and self-owned at once")
	}
	data[tunnel.NodeAddressesPrefix+"w1"] = []byte(addr)
	data[tunnel.NodePodCIDRsPrefix+"w1"] = []byte(block)

	for _, pair := range [][2]string{
		{tunnel.NodeAddressesPrefix + "w1", tunnel.SiteAddressesPrefix + "w1"},
		{tunnel.NodePodCIDRsPrefix + "w1", tunnel.SitePodCIDRsPrefix + "w1"},
	} {
		_, own := data[pair[0]]
		_, relayed := data[pair[1]]
		if own && relayed {
			t.Errorf("%s and %s both name the same prefix: two owners, and the accept list keeps the last one written", pair[0], pair[1])
		}
		if !own && !relayed {
			t.Errorf("neither %s nor %s names the prefix: a remote cannot reach it at all", pair[0], pair[1])
		}
	}
}

// A node moving back into the selector is published by two actors: this
// operator writes its addresses and allocates its tunnel address, its
// own dialer writes the key that makes it a peer. The render is what a
// remote acts on, so the invariant belongs there: at every step of the
// return, every prefix has exactly one owner. Zero owners is the one
// that cannot recover, because the remote prunes the route to the site
// and the list that would correct it is only reachable over the site.
func TestNoPrefixIsUnownedWhileAnEndpointReturns(t *testing.T) {
	const (
		cpAddr  = "10.10.0.10"
		cpBlock = "10.244.242.64/26"
	)
	data := map[string][]byte{
		tunnel.NodePublicKeyPrefix + "w1":     []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA="),
		tunnel.NodeTunnelAddressPrefix + "w1": []byte("10.100.0.17/24"),
		tunnel.NodeAddressesPrefix + "w1":     []byte("10.10.0.11"),
		tunnel.NodePodCIDRsPrefix + "w1":      []byte("10.244.190.64/26"),
		// cp holds no tunnel: w1 relays to it.
		tunnel.SiteAddressesPrefix + "cp": []byte(cpAddr),
		tunnel.SitePodCIDRsPrefix + "cp":  []byte(cpBlock),
	}

	check := func(step string) {
		t.Helper()
		peers, err := tunnel.RemotePeers(data, "10.100.0.128", []string{cpAddr})
		if err != nil {
			t.Fatalf("%s: RemotePeers: %v", step, err)
		}
		for _, prefix := range []string{tunnel.HostCIDR(cpAddr), cpBlock} {
			n := 0
			for _, p := range peers {
				for _, cidr := range p.WGAllowedIPs {
					if cidr == prefix {
						n++
					}
				}
			}
			if n != 1 {
				t.Errorf("%s: %s is permitted on %d peers, want exactly 1", step, prefix, n)
			}
		}
	}
	check("relayed through w1")

	// cp is selected. This operator allocates its address and publishes
	// its own addresses in the same pass.
	data[tunnel.NodeTunnelAddressPrefix+"cp"] = []byte("10.100.0.24/24")
	data[tunnel.NodeAddressesPrefix+"cp"] = []byte(cpAddr)
	data[tunnel.NodePodCIDRsPrefix+"cp"] = []byte(cpBlock)
	if takeBackFromRelay(data, "cp") {
		t.Error("cp stopped being relayed before it was a peer: for as long as its dialer takes to publish a key, nothing owns its address")
	}
	check("selected, no key yet")

	// cp's dialer publishes the key. Until the next reconcile the
	// Secret holds both forms.
	data[tunnel.NodePublicKeyPrefix+"cp"] = []byte("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=")
	check("key published, relayed entry not yet cleaned up")

	if !takeBackFromRelay(data, "cp") {
		t.Fatal("cp is a peer in its own right and is still relayed")
	}
	check("returned")
}

// Retention is a floor, not a ceiling. The window exists so a remote
// can read the list naming the replacement while the old path still
// carries it, and a clock cannot know whether that read happened: a
// controller outage or a slow render can eat the entire window, and a
// departed endpoint released on schedule then strands every remote
// that still names only it. Measured twice: both remotes NotReady,
// holding a list whose one peer had been swept, the correction
// rendered seconds after their last successful read.
//
// Nor is a live handshake evidence. A retained-era config keeps bare
// peers handshaking on keepalive alone, so a remote can handshake a
// current endpoint continuously while routing every packet by a list
// two placements old. Measured: a hold built on handshakes released
// on schedule, into exactly the stranding it existed to prevent.
//
// The only honest evidence is the remote's own acknowledgment: it
// stamps the hash of the list it applied, and it has moved when that
// hash matches the current render, which no longer names the departed
// node as an owner.
func TestADepartedEndpointIsHeldUntilEveryRemoteAcknowledges(t *testing.T) {
	data := published(map[string][2]string{
		"cp": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.2/24"},
		"w1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"},
	}, nil)
	data["peer-public-key-remote1"] = []byte("R1KEY")
	departedSince(data, "cp", testRetention+time.Minute)
	want := meshMembership{endpoints: members("w1"), siteNodes: members("cp", "w1")}

	// The clock is long expired, and w1 may have been handshaking
	// remote1 on keepalive the whole time; none of that says remote1
	// applied the list that moved cp's prefixes.
	pruneDeparted(data, want, testNow, testRetention, map[string]map[string]bool{"remote1": {"cp": false}})
	if _, ok := data[tunnel.NodePublicKeyPrefix+"cp"]; !ok {
		t.Fatal("cp was released without remote1's acknowledgment: the remote's only working path was torn down on a clock")
	}

	// remote1 stamps the current render's hash: it has provably
	// applied the list that no longer names cp, and the hold releases.
	pruneDeparted(data, want, testNow, testRetention, map[string]map[string]bool{"remote1": {"cp": true}})
	if _, ok := data[tunnel.NodePublicKeyPrefix+"cp"]; ok {
		t.Fatal("cp is still held after every remote acknowledged the render that moved it")
	}
}

// A machine with no acknowledgment at all holds the release: absence
// of evidence is the state the hold exists for, not an exemption.
func TestAnUnacknowledgedRemoteHoldsTheRelease(t *testing.T) {
	data := published(map[string][2]string{
		"cp": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.2/24"},
		"w1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"},
	}, nil)
	data["peer-public-key-remote1"] = []byte("R1KEY")
	departedSince(data, "cp", testRetention+time.Minute)
	want := meshMembership{endpoints: members("w1"), siteNodes: members("cp", "w1")}
	pruneDeparted(data, want, testNow, testRetention, nil)
	if _, ok := data[tunnel.NodePublicKeyPrefix+"cp"]; !ok {
		t.Fatal("cp was released with no acknowledgment from any remote")
	}
}
