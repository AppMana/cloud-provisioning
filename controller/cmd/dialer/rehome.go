package main

import (
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// wgRejectAfterTime is WireGuard's REJECT_AFTER_TIME: the protocol
// refuses to use a session whose last handshake is older than this, so
// it is also the protocol's own definition of a dead peer. Liveness
// here is read from the kernel's handshake clock and judged by the
// kernel's own constant, not by a threshold this program invented.
const wgRejectAfterTime = 180 * time.Second

// rehomeTransit applies the render's election for as long as the
// kernel agrees the elected relay is alive, and moves the declared
// transit set to a live local when it does not. The render said what
// may move (PeerSpec.Transit, prefixes carried by election rather than
// ownership); the kernel says when (a session silent past WireGuard's
// horizon while another local is handshaking); this function only
// carries the two facts into one list. Stateless: it runs on the
// rendered list every pass, so the moment the relay handshakes again
// the rendered election simply stands.
//
// A peer in another cloud is never a candidate. The transit set is
// addresses at the site, and a remote cannot carry them there.
func rehomeTransit(peers []tunnel.PeerSpec, lastHandshake func(string) (time.Time, bool), now time.Time) []tunnel.PeerSpec {
	owner := -1
	for i := range peers {
		if len(peers[i].Transit) > 0 || len(peers[i].TransitHosts) > 0 {
			owner = i
			break
		}
	}
	if owner < 0 {
		return peers
	}
	live := func(pub string) bool {
		t, ok := lastHandshake(pub)
		return ok && !t.IsZero() && now.Sub(t) < wgRejectAfterTime
	}
	if live(peers[owner].PublicKey) {
		return peers
	}
	// The live locals, and among them the same order the render
	// elects by: lowest tunnel address, which is RouteHosts' first
	// entry by construction. The same rule in both places means the
	// applier and the render converge on the same relay as soon as
	// both see the same liveness.
	best := -1
	for i := range peers {
		if i == owner || peers[i].Remote || peers[i].Endpoint != "" {
			continue
		}
		if !live(peers[i].PublicKey) {
			continue
		}
		if len(peers[i].RouteHosts) == 0 {
			continue
		}
		if best < 0 || tunnel.LessIP(peers[i].RouteHosts[0], peers[best].RouteHosts[0]) {
			best = i
		}
	}
	if best < 0 {
		return peers
	}

	out := make([]tunnel.PeerSpec, len(peers))
	copy(out, peers)
	moveCIDRs := map[string]bool{}
	for _, cidr := range peers[owner].Transit {
		moveCIDRs[cidr] = true
	}
	moveHosts := map[string]bool{}
	for _, h := range peers[owner].TransitHosts {
		moveHosts[h] = true
	}

	keepAllowed := make([]string, 0, len(peers[owner].WGAllowedIPs))
	for _, cidr := range peers[owner].WGAllowedIPs {
		if !moveCIDRs[cidr] {
			keepAllowed = append(keepAllowed, cidr)
		}
	}
	keepHosts := make([]string, 0, len(peers[owner].RouteHosts))
	for _, h := range peers[owner].RouteHosts {
		if !moveHosts[h] {
			keepHosts = append(keepHosts, h)
		}
	}
	out[owner].WGAllowedIPs = keepAllowed
	out[owner].RouteHosts = keepHosts
	out[owner].Transit = nil
	out[owner].TransitHosts = nil

	gainAllowed := append([]string{}, peers[best].WGAllowedIPs...)
	for _, cidr := range peers[owner].Transit {
		if !containsString(gainAllowed, cidr) {
			gainAllowed = append(gainAllowed, cidr)
		}
	}
	gainHosts := append([]string{}, peers[best].RouteHosts...)
	for _, h := range peers[owner].TransitHosts {
		if !containsString(gainHosts, h) {
			gainHosts = append(gainHosts, h)
		}
	}
	out[best].WGAllowedIPs = gainAllowed
	out[best].RouteHosts = gainHosts
	out[best].Transit = append([]string{}, peers[owner].Transit...)
	out[best].TransitHosts = append([]string{}, peers[owner].TransitHosts...)
	return out
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
