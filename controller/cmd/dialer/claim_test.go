package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// A remote node runs two dialers on one interface: the cloud-init
// systemd unit, which is never disabled on purpose, and the DaemonSet,
// which is the only one able to read the live peer list. Without
// arbitration each overwrites the other every poll, and the node
// alternates between the current mesh and the one that existed when its
// userdata was rendered. On the lab that showed up as a tunnel endpoint
// unable to reach its own peer, with a correct Secret and correct
// routes on both sides.
func TestClaimHeldOnlyWhileFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cldt0.claim")
	poll := 30 * time.Second

	if held, _ := claimHeld(path, poll); held {
		t.Fatal("no claim file at all must not hold the interface: at boot the floor is the only dialer there is")
	}

	if err := writeClaim(path); err != nil {
		t.Fatalf("writeClaim: %v", err)
	}
	if held, _ := claimHeld(path, poll); !held {
		t.Error("a claim just written must hold, or the floor keeps overwriting the live peer list")
	}

	stale := time.Now().UTC().Add(-2 * claimStale(poll)).Format(time.RFC3339)
	if err := os.WriteFile(path, []byte(stale+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if held, _ := claimHeld(path, poll); held {
		t.Error("a claim that stopped being refreshed must expire: that is the case the bootstrap unit exists for")
	}

	if err := os.WriteFile(path, []byte("not a timestamp\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if held, _ := claimHeld(path, poll); held {
		t.Error("an unparsable claim must not hold: standing off for a file we cannot read would strand the node")
	}
}

// Three polls, so one missed pass does not hand the interface back and
// forth, with a floor so a short poll interval does not make the claim
// expire almost immediately.
func TestClaimStaleneverShorterThanTheFloor(t *testing.T) {
	if got := claimStale(time.Second); got != 90*time.Second {
		t.Errorf("claimStale(1s) = %v, want the 90s floor", got)
	}
	if got := claimStale(time.Minute); got != 3*time.Minute {
		t.Errorf("claimStale(1m) = %v, want three polls", got)
	}
}

// Two meshes on one node must not contend for one claim file.
func TestClaimPathIsPerInterface(t *testing.T) {
	a := claimPath(config{peersFile: "/etc/wg-dialer/peers.json", iface: "cldtaaaa"})
	b := claimPath(config{peersFile: "/etc/wg-dialer/peers.json", iface: "cldtbbbb"})
	if a == b {
		t.Errorf("both interfaces claim %s", a)
	}
	if dir := filepath.Dir(a); dir != "/etc/wg-dialer" {
		t.Errorf("claim in %s, but only the peers file directory is visible to both dialers", dir)
	}
	if p := claimPath(config{iface: "cldtaaaa"}); p != "" {
		t.Errorf("a Secret-mode dialer has no peers file and no second writer to arbitrate with, got %q", p)
	}
}

// The adopting dialer must not stand off for its own claim. Keying the
// stand-off on whether the last Secret read succeeded did exactly that:
// it wrote the claim while the API server was reachable, fell into the
// file-only branch when the API server blinked, saw a fresh claim, and
// disabled itself. Both dialers then deferred to a writer that no
// longer existed. Which dialer this is has to come from configuration.
func TestBothDialersArbitrateOverOneFile(t *testing.T) {
	adopting := config{peersFile: "/etc/wg-dialer/peers.json", iface: "cldt0", peersSecretNamespace: "cloud-provisioning"}
	floor := config{peersFile: "/etc/wg-dialer/peers.json", iface: "cldt0"}
	if claimPath(adopting) != claimPath(floor) {
		t.Error("the two dialers must arbitrate over the same file, or neither ever sees the other")
	}
	if cachePath(adopting) == claimPath(adopting) {
		t.Error("the cache and the claim must be separate files")
	}
}

// A cached list survives a restart and an API outage. The bootstrap file
// is correct only at boot: it names the site as it was when the userdata
// was rendered, so applying it after an endpoint has moved prunes the
// host routes carrying this node's API traffic, which is what made the
// API server unreachable to begin with.
func TestCachedPeersBeatTheBootstrapFile(t *testing.T) {
	dir := t.TempDir()
	cfg := config{peersFile: filepath.Join(dir, "peers.json"), iface: "cldt0"}

	if _, err := readCachedPeers(cachePath(cfg)); err == nil {
		t.Error("no cache yet must be an error, so a node that has never read the cluster falls back to its bootstrap file")
	}

	want := tunnel.PeerListDoc{
		Peers:      []tunnel.PeerSpec{{PublicKey: "k", RouteHosts: []string{"10.10.0.10"}, WGAllowedIPs: []string{"10.10.0.10/32"}}},
		APIServers: []string{"10.10.0.10:6443", "10.10.0.13:6443", "10.10.0.14:6443"},
	}
	if err := writeCachedPeers(cachePath(cfg), want); err != nil {
		t.Fatalf("writeCachedPeers: %v", err)
	}
	got, err := readCachedPeers(cachePath(cfg))
	if err != nil {
		t.Fatalf("readCachedPeers: %v", err)
	}
	if len(got.Peers) != 1 || got.Peers[0].PublicKey != "k" || len(got.Peers[0].RouteHosts) != 1 || got.Peers[0].RouteHosts[0] != "10.10.0.10" {
		t.Errorf("cached peers round-tripped as %#v, want the route hosts that keep the API reachable", got.Peers)
	}
	// The API server list rides the same cache: it is how the pod's
	// cluster reads reach the host unit that serves the loopback
	// balancer, which has no cluster access of its own.
	if len(got.APIServers) != 3 || got.APIServers[1] != "10.10.0.13:6443" {
		t.Errorf("cached API servers round-tripped as %v, want all three", got.APIServers)
	}
}

// A route host that no peer can carry yet is not the same as one no
// peer should ever carry, and the difference decides whether a route
// already serving it survives the pass.
//
// The endpoint case must win over the not-yet case. A host serving as
// some peer's tunnel endpoint must never have a route through the
// tunnel, whatever the state of the peer claiming it, because that
// route sends the tunnel's own packets into the tunnel. Ordering the
// checks the other way would claim such a host from an un-handshaked
// peer and so protect exactly the route that has to go.
func TestARouteHostThatIsNotReadyIsNotARouteHostThatIsWrong(t *testing.T) {
	cases := []struct {
		name           string
		marked         bool
		isEndpointHost bool
		peerCanCarry   bool
		want           routeHostDisposition
	}{
		{"a peer that can carry it", false, false, true, routeInstall},
		{"a peer still arriving", false, false, false, routeNotYet},
		{"an endpoint, peer ready, unmarked", false, true, true, routeIsAnEndpoint},
		{"an endpoint, peer still arriving, unmarked", false, true, false, routeIsAnEndpoint},
		// With the tunnel's own packets marked and exempted, an
		// endpoint address is an ordinary route host: the loop the
		// old refusal guarded against is broken by the mark, and an
		// encapsulating network's node-addressed packets need exactly
		// this route.
		{"an endpoint, peer ready, marked", true, true, true, routeInstall},
		{"an endpoint, peer still arriving, marked", true, true, false, routeNotYet},
	}
	for _, c := range cases {
		if got := disposeRouteHost(c.marked, c.isEndpointHost, c.peerCanCarry); got != c.want {
			t.Errorf("%s: disposition %d, want %d", c.name, got, c.want)
		}
	}
}
