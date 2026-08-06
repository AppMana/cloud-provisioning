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
			},
			// Retired when the entry finally goes, not when the node
			// first departed: it was in use for the whole window.
			retired: "10.100.0.1",
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
			changed := pruneDeparted(tc.data, tc.want, testNow, testRetention)
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
	pruneDeparted(data, r.membership(nodes), testNow, testRetention)
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
	if !pruneDeparted(data, r.membership(nodes), testNow, testRetention) {
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
	if !pruneDeparted(data, r.membership(nodes), testNow.Add(testRetention), testRetention) {
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
	if pruneDeparted(data, want, testNow, testRetention) {
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
	want := meshMembership{endpoints: members("cp"), siteNodes: members("w1")}
	// An address is retired when the entry finally goes, not when the
	// node departed: it belongs to a working tunnel until then.
	if !pruneDeparted(data, want, testNow, testRetention) {
		t.Fatal("the departure of the endpoint was not recorded")
	}
	if got := string(data[tunnel.RetiredTunnelAddressesKey]); got != "" {
		t.Errorf("retired %q while the address was still in use", got)
	}
	if !pruneDeparted(data, want, testNow.Add(testRetention), testRetention) {
		t.Fatal("the departed endpoint was not pruned once its window was over")
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

// The dialer's affinity is the other half of a two phase migration, and
// the half without which the first is useless. A remote reads its peer
// list from the API server over the tunnel, so the node the selector has
// just let go of has to keep running its dialer while the remote reads
// the list naming its replacement. Measured: the selector moved from w1
// to cp, w1's dialer went with it, and the remote sat NotReady for
// eighteen minutes holding w1 as its only peer.
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

	for _, tc := range []struct {
		name     string
		raw      string
		retained []string
		runs     []*corev1.Node
		declines []*corev1.Node
	}{
		{
			// The migration itself: cp is what the selector now says, w1
			// is what the mesh still publishes, and both dial until the
			// entries are pruned.
			name:     "a node the selector has let go of keeps its dialer while its entries stand",
			raw:      "kubernetes.io/hostname=cp," + controlPlaneLabel,
			retained: []string{"cp", "w1"},
			runs:     []*corev1.Node{cp, w1},
			declines: []*corev1.Node{w2, provisioned, windows},
		},
		{
			// A retained control plane keeps its dialer. The exclusion
			// is about not putting a tunnel on a control plane nobody
			// chose, and a node is only ever retained because it was
			// chosen and is carrying one right now. Dropping it the
			// instant the selector moves is the stranding this whole
			// mechanism exists to prevent, and it was measured: the
			// term read "metadata.name In [cp] and control-plane
			// DoesNotExist", which nothing can satisfy.
			name:     "a retained control plane keeps the tunnel it already has",
			raw:      "kubernetes.io/hostname=w2",
			retained: []string{"cp", "w1", "w2"},
			runs:     []*corev1.Node{cp, w1, w2},
			declines: []*corev1.Node{provisioned, windows},
		},
		{
			// And one that was never chosen still gets nothing.
			name:     "a control plane nobody named and nobody retained",
			raw:      "kubernetes.io/hostname=w2",
			retained: []string{"w1"},
			runs:     []*corev1.Node{w1, w2},
			declines: []*corev1.Node{cp, provisioned, windows},
		},
		{
			// A provisioned node is on the far side of a tunnel and
			// terminates none of its own, whatever the Secret says.
			name:     "every node at this site, and never a provisioned one",
			raw:      "all",
			retained: []string{"remote1"},
			runs:     []*corev1.Node{cp, w1, w2},
			declines: []*corev1.Node{provisioned, windows},
		},
		{
			name:     "nothing retained is the selector on its own",
			raw:      "kubernetes.io/hostname=w1",
			retained: nil,
			runs:     []*corev1.Node{w1},
			declines: []*corev1.Node{cp, w2, provisioned, windows},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			affinity := dialerNodeAffinity(tc.raw, tc.retained)
			for _, node := range tc.runs {
				if !matchesNode(affinity, node) {
					t.Errorf("%s runs no dialer", node.Name)
				}
			}
			for _, node := range tc.declines {
				if matchesNode(affinity, node) {
					t.Errorf("%s was given a dialer", node.Name)
				}
			}
			// A field selector takes exactly one value, so a term with
			// several would be rejected by the API server rather than
			// mis-scheduled.
			for _, term := range affinity.NodeSelectorTerms {
				for _, req := range term.MatchFields {
					if len(req.Values) != 1 {
						t.Errorf("field requirement %+v has %d values, want exactly one", req, len(req.Values))
					}
				}
			}
		})
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
	if !pruneDeparted(data, want, testNow, testRetention) {
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
