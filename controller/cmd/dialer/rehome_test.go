package main

import (
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// The render elects one local to carry the transit set, and the remote
// applies that election for as long as its own kernel agrees the relay
// is alive. When the relay has been silent past WireGuard's own session
// horizon and another local is handshaking, the remote moves exactly
// the declared set: the render said what may move, the kernel said
// when, and nobody guessed.
//
// This is the whole of node-local load balancing here. No proxy, no
// second address for the API server, no health checker beside the one
// the kernel already is.

func rehomeFixture() []tunnel.PeerSpec {
	return []tunnel.PeerSpec{
		{
			PublicKey:    "W1KEY",
			WGAllowedIPs: []string{"10.100.0.1/32", "10.10.0.11/32", "10.244.1.0/26", "10.10.0.10/32", "10.244.0.0/26"},
			RouteHosts:   []string{"10.100.0.1", "10.10.0.11", "10.10.0.10"},
			Transit:      []string{"10.10.0.10/32", "10.244.0.0/26"},
			TransitHosts: []string{"10.10.0.10"},
		},
		{
			PublicKey:    "W2KEY",
			WGAllowedIPs: []string{"10.100.0.2/32", "10.10.0.12/32", "10.244.2.0/26"},
			RouteHosts:   []string{"10.100.0.2", "10.10.0.12"},
		},
		{
			PublicKey:    "R2KEY",
			Endpoint:     "192.0.2.10:51820",
			Remote:       true,
			WGAllowedIPs: []string{"10.100.0.129/32"},
			RouteHosts:   []string{"10.100.0.129"},
		},
	}
}

func handshakes(m map[string]time.Duration, now time.Time) func(string) (time.Time, bool) {
	return func(pub string) (time.Time, bool) {
		age, ok := m[pub]
		if !ok {
			return time.Time{}, false
		}
		return now.Add(-age), true
	}
}

func TestADeadRelaysTransitMovesToALiveLocal(t *testing.T) {
	now := time.Unix(1754500000, 0)
	peers := rehomeTransit(rehomeFixture(), handshakes(map[string]time.Duration{
		"W1KEY": 10 * time.Minute,
		"W2KEY": 20 * time.Second,
		"R2KEY": 20 * time.Second,
	}, now), now)

	w1, w2 := peers[0], peers[1]
	for _, cidr := range w1.WGAllowedIPs {
		if cidr == "10.10.0.10/32" || cidr == "10.244.0.0/26" {
			t.Errorf("the dead relay still carries transit %s", cidr)
		}
	}
	if len(w1.WGAllowedIPs) != 3 {
		t.Errorf("the dead relay's own prefixes changed: %v; only the transit set may move", w1.WGAllowedIPs)
	}
	gained := map[string]bool{}
	for _, cidr := range w2.WGAllowedIPs {
		gained[cidr] = true
	}
	if !gained["10.10.0.10/32"] || !gained["10.244.0.0/26"] {
		t.Errorf("the live local did not gain the transit set: %v", w2.WGAllowedIPs)
	}
	hosts := map[string]bool{}
	for _, h := range w2.RouteHosts {
		hosts[h] = true
	}
	if !hosts["10.10.0.10"] {
		t.Errorf("the live local does not route the transit host: %v", w2.RouteHosts)
	}
	for _, h := range w1.RouteHosts {
		if h == "10.10.0.10" {
			t.Error("the dead relay still routes the transit host")
		}
	}
}

func TestALiveRelayKeepsItsTransit(t *testing.T) {
	now := time.Unix(1754500000, 0)
	peers := rehomeTransit(rehomeFixture(), handshakes(map[string]time.Duration{
		"W1KEY": 30 * time.Second,
		"W2KEY": 10 * time.Second,
	}, now), now)
	found := false
	for _, cidr := range peers[0].WGAllowedIPs {
		if cidr == "10.10.0.10/32" {
			found = true
		}
	}
	if !found {
		t.Error("a live relay lost its transit; the election moved on kernel evidence that says nothing is wrong")
	}
}

// A remote peer never becomes the relay for the site's transit, no
// matter how fresh its handshake: the transit set is addresses at the
// site, and another cloud cannot carry them there.
func TestARemotePeerIsNeverTheTransitCandidate(t *testing.T) {
	now := time.Unix(1754500000, 0)
	peers := rehomeTransit(rehomeFixture(), handshakes(map[string]time.Duration{
		"W1KEY": 10 * time.Minute,
		"W2KEY": 10 * time.Minute,
		"R2KEY": 5 * time.Second,
	}, now), now)
	for _, cidr := range peers[2].WGAllowedIPs {
		if cidr == "10.10.0.10/32" {
			t.Fatal("the transit set moved to a peer in another cloud")
		}
	}
}

// Nobody alive means nothing moves. The rendered election stands: a
// maybe-dead relay is a path that may come back, and moving the set to
// an equally dead peer trades a known black hole for a shuffled one.
func TestNoLiveCandidateLeavesTheTransitAlone(t *testing.T) {
	now := time.Unix(1754500000, 0)
	peers := rehomeTransit(rehomeFixture(), handshakes(map[string]time.Duration{
		"W1KEY": 10 * time.Minute,
		"W2KEY": 10 * time.Minute,
	}, now), now)
	found := false
	for _, cidr := range peers[0].WGAllowedIPs {
		if cidr == "10.10.0.10/32" {
			found = true
		}
	}
	if !found {
		t.Error("the transit set left the rendered relay with nowhere to go")
	}
}

// A peer that has never handshaken is not live. At boot every
// handshake is zero; moving the transit on that evidence would shuffle
// it before the first session even forms.
func TestANeverHandshakenPeerIsNotACandidate(t *testing.T) {
	now := time.Unix(1754500000, 0)
	peers := rehomeTransit(rehomeFixture(), handshakes(map[string]time.Duration{
		"W1KEY": 10 * time.Minute,
	}, now), now)
	found := false
	for _, cidr := range peers[0].WGAllowedIPs {
		if cidr == "10.10.0.10/32" {
			found = true
		}
	}
	if !found {
		t.Error("the transit set moved to a peer that has never completed a handshake")
	}
}
