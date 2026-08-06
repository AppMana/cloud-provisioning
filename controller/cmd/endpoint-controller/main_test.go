package main

import (
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

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
func published(endpoints map[string][2]string, sites map[string]string) map[string][]byte {
	data := map[string][]byte{}
	for name, pair := range endpoints {
		if pair[0] != "" {
			data[tunnel.NodePublicKeyPrefix+name] = []byte(pair[0])
		}
		if pair[1] != "" {
			data[tunnel.NodeTunnelAddressPrefix+name] = []byte(pair[1])
		}
		data[tunnel.NodeAddressesPrefix+name] = []byte("172.21.0.1" + name[len(name)-1:])
		data[tunnel.NodePodCIDRsPrefix+name] = []byte("10.244.1.0/26")
	}
	for name, addr := range sites {
		data[tunnel.SiteAddressesPrefix+name] = []byte(addr)
		data[tunnel.SitePodCIDRsPrefix+name] = []byte("10.244.2.0/26")
	}
	return data
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
		name    string
		data    map[string][]byte
		want    meshMembership
		changed bool
		gone    []string
		kept    []string
		retired string
	}{
		{
			// The measured case, once the new endpoint is up.
			name: "the selector moved the tunnel and the new endpoint is published",
			data: published(
				map[string][2]string{"w1": {keyW1, "10.100.0.1/24"}, "cp": {keyCP, "10.100.0.2/24"}},
				map[string]string{"w2": "172.21.0.17"},
			),
			want:    meshMembership{endpoints: members("cp"), siteNodes: members("w1", "w2")},
			changed: true,
			gone: []string{
				tunnel.NodePublicKeyPrefix + "w1", tunnel.NodeTunnelAddressPrefix + "w1",
				tunnel.NodeAddressesPrefix + "w1", tunnel.NodePodCIDRsPrefix + "w1",
			},
			kept: []string{
				tunnel.NodePublicKeyPrefix + "cp", tunnel.NodeTunnelAddressPrefix + "cp",
				tunnel.SiteAddressesPrefix + "w2",
			},
			retired: "10.100.0.1",
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
			changed := pruneDeparted(tc.data, tc.want)
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
	r := &meshReconciler{tunnelEndpointsRaw: "kubernetes.io/hostname=cp"}
	selector, err := labels.Parse(r.tunnelEndpointsRaw)
	if err != nil {
		t.Fatalf("parsing the selector: %v", err)
	}
	r.tunnelEndpointSelector = selector
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "cp", Labels: map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/hostname": "cp"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "w1", Labels: map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/hostname": "w1"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "w2", Labels: map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/hostname": "w2"}}},
	}

	// The pass that allocates cp an address, before its dialer has
	// published a key. w1 still holds the tunnel and must still be the
	// remote's peer.
	pruneDeparted(data, r.membership(nodes))
	data[tunnel.NodeTunnelAddressPrefix+"cp"] = []byte("10.100.0.3/24")
	peers, err := tunnel.RemotePeers(data, "10.100.0.128", nil)
	if err != nil {
		t.Fatalf("RemotePeers: %v", err)
	}
	if len(peers) != 1 || peers[0].PublicKey != keyW1 {
		t.Fatalf("mid-migration the remote has %d peers (%+v), want only w1", len(peers), peers)
	}

	// cp's dialer publishes, and the next pass finishes the move.
	data[tunnel.NodePublicKeyPrefix+"cp"] = []byte(keyCP)
	if !pruneDeparted(data, r.membership(nodes)) {
		t.Fatal("nothing was pruned once the new endpoint was published")
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
	if pruneDeparted(data, want) {
		t.Error("an unhealthy node's published entries were pruned")
	}
}

// An address a departed node held is never handed to a later node. A
// remote still holding configuration that names it would otherwise send
// that node's traffic somewhere else entirely.
func TestRetiredTunnelAddressesAreNeverAllocatedAgain(t *testing.T) {
	data := published(map[string][2]string{
		"w1": {"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaA=", "10.100.0.1/24"},
		"cp": {"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbB=", "10.100.0.2/24"},
	}, nil)
	if !pruneDeparted(data, meshMembership{endpoints: members("cp"), siteNodes: members("w1")}) {
		t.Fatal("the departed endpoint was not pruned")
	}

	used := map[string]bool{}
	for key, val := range data {
		if strings.HasPrefix(key, tunnel.NodeTunnelAddressPrefix) {
			used[strings.SplitN(string(val), "/", 2)[0]] = true
		}
	}
	for _, addr := range tunnel.SplitList(string(data[tunnel.RetiredTunnelAddressesKey])) {
		used[addr] = true
	}
	next, err := nextFreeAddress("10.100.0.1/24", used)
	if err != nil {
		t.Fatalf("nextFreeAddress: %v", err)
	}
	if next == "10.100.0.1/24" {
		t.Error("the departed node's address was handed straight to the next node")
	}
	if next != "10.100.0.3/24" {
		t.Errorf("next free address = %q, want 10.100.0.3/24", next)
	}
}
