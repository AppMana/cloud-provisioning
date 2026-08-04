// Package tunnel is the wire contract shared by the dialer binary,
// the endpoint-controller, and the join reconciler: the Secret key
// naming scheme, the peers-file/peer-list JSON shape, and the
// deterministic per-mesh interface name. One package so the producers
// and the consumer can never drift.
package tunnel

import (
	"crypto/sha256"
	"fmt"
	"net"
	"sort"
	"strings"
)

// Secret key shapes.
//
// Node-published (each dialer writes its own; the private key never
// leaves the node):
//
//	node-public-key-<node>
//
// Controller-written per-node (read back by that node's dialer):
//
//	node-tunnel-address-<node>   -- tunnel address (CIDR)
//	node-cluster-vips-<node>     -- comma-separated node addresses
//
// Controller-written per-remote-machine (the on-prem peer list):
//
//	peer-public-key-<machine>
//	peer-endpoint-<machine>      -- "pending" until mirrored
//	peer-allowed-ips-<machine>   -- comma-separated CIDRs
//	peer-route-hosts-<machine>   -- comma-separated single hosts
//	peer-route-host-<machine>    -- legacy single-host fallback
const (
	NodePublicKeyPrefix     = "node-public-key-"
	NodeTunnelAddressPrefix = "node-tunnel-address-"
	NodeClusterVIPsPrefix   = "node-cluster-vips-"
	PeerPublicKeyPrefix     = "peer-public-key-"
	PeerEndpointPrefix      = "peer-endpoint-"
	PeerAllowedIPsPrefix    = "peer-allowed-ips-"
	PeerRouteHostsPrefix    = "peer-route-hosts-"
	PeerRouteHostPrefix     = "peer-route-host-"

	// PeerEndpointPending is the placeholder the join reconciler writes
	// until the endpoint mirror learns the machine's real external IP.
	PeerEndpointPending = "pending"

	// CloudPeersKey is the single data key of a per-machine adoption
	// Secret: a JSON document with a "peers" list (public data only --
	// never a private key), consumed by the cloud dialer's
	// --peers-secret-* override.
	CloudPeersKey = "peers.json"

	// APIServersKey lists every control-plane address a remote must be
	// able to reach (comma-separated). k0s workers load-balance across
	// all of them via nllb, so one address is not enough.
	APIServersKey = "api-servers"
)

// AdoptionSecretName is the per-machine adoption Secret's name --
// derived from the Machine name on both sides (the endpoint-controller
// creating it, the cloud dialer reading it via the machine-name file
// cloud-init wrote), so the two can never disagree.
func AdoptionSecretName(machineName string) string { return machineName + "-tunnel-peers" }

// PeerSpec is one WireGuard peer a dialer maintains. AllowedIPs and
// RouteHosts are deliberately separate: AllowedIPs is WireGuard's
// cryptokey packet filter, RouteHosts are the peer node's own
// addresses -- the only things that ever become kernel routes, each a
// single host.
type PeerSpec struct {
	PublicKey    string   `json:"publicKey"`
	Endpoint     string   `json:"endpoint,omitempty"`
	WGAllowedIPs []string `json:"allowedIPs"`
	RouteHosts   []string `json:"routeHosts,omitempty"`
	// RouteHost is the legacy single-host form, folded into RouteHosts
	// by consumers.
	RouteHost string `json:"routeHost,omitempty"`
}

// AllRouteHosts folds the legacy single-host field into the list.
func (p *PeerSpec) AllRouteHosts() []string {
	hosts := append([]string{}, p.RouteHosts...)
	if p.RouteHost != "" {
		hosts = append(hosts, p.RouteHost)
	}
	return hosts
}

// PeersFileDoc is the on-disk shape of the cloud node's --peers-file:
// its identity (private key + tunnel address, written once by
// cloud-init) plus the bootstrap peer list.
type PeersFileDoc struct {
	PrivateKey   string     `json:"privateKey"`
	LocalAddress string     `json:"localAddress"`
	Peers        []PeerSpec `json:"peers"`
}

// PeerListDoc is the adoption-Secret shape (CloudPeersKey): the peer
// list only, no identity.
type PeerListDoc struct {
	Peers []PeerSpec `json:"peers"`
}

// InterfaceName derives the unique per-mesh WireGuard interface name:
// "cldt" + the first 8 hex chars of sha256 over the mesh identity
// (the peer Secret's namespace/name). 12 chars, under IFNAMSIZ (15),
// deterministic on every node of the mesh, and never colliding with
// pre-existing wg0/wgX/tailscale interfaces.
func InterfaceName(meshID string) string {
	sum := sha256.Sum256([]byte(meshID))
	return fmt.Sprintf("cldt%08x", sum[:4])
}

// HostCIDR appends the correct single-host prefix length for addr's
// family -- /32 for IPv4, /128 for IPv6 -- if addr doesn't already
// carry a prefix. The cluster is dual-stack; hardcoding /32 against an
// IPv6 literal produces an entry that fails to parse or silently
// matches the wrong host count.
func HostCIDR(addr string) string {
	if strings.Contains(addr, "/") {
		return addr
	}
	if ip := net.ParseIP(addr); ip != nil && ip.To4() == nil {
		return addr + "/128"
	}
	return addr + "/32"
}

// SplitList splits any number of comma-separated list values into one
// flat, trimmed slice, skipping blanks.
func SplitList(lists ...string) []string {
	var out []string
	for _, list := range lists {
		for _, item := range strings.Split(list, ",") {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
	}
	return out
}

// RemotePeers derives a REMOTE (cloud) node's peer list from the
// shared peer Secret's data: every published local tunnel-endpoint
// node (node-* keys: public key published by that node's own dialer,
// tunnel address and cluster VIPs written by the controller), plus
// every OTHER remote machine (peer-* keys) -- the remote-to-remote
// edges of the fully connected mesh (isolated remotes share no LAN;
// without these edges they could never reach each other).
//
// selfTunnelAddr identifies which peer-* entry is the caller itself
// (a remote knows its own tunnel address from its identity file, not
// its Machine name -- Kubernetes node names and Machine names differ
// on most clouds).
//
// apiVIP, when non-empty, is added to the AllowedIPs/RouteHosts of
// the designated transit local (lowest tunnel address): control-plane
// nodes deliberately carry no tunnel, so remotes reach the API
// through exactly one local worker, which masquerades tunnel-sourced
// traffic onto the LAN.
//
// Local peers get no Endpoint (the local side is behind NAT and only
// ever dials out; the remote listens). Remote peers get their real
// public endpoint when known -- two remotes must dial each other
// directly.
//
// PUBLIC data only by construction: the Secret holds no private key
// of any node, and this document lands on internet-facing machines.
//
// This function is shared by the join reconciler (bootstrap: snapshot
// rendered once into userdata) and the dialer's adoption mode (live:
// re-derived from the Secret every poll) -- one derivation, two
// freshness tiers, no drift.
func RemotePeers(data map[string][]byte, selfTunnelAddr string, apiServers []string) ([]PeerSpec, error) {
	type localNode struct {
		name       string
		tunnelAddr string
	}
	var nodes []localNode
	for key := range data {
		if !strings.HasPrefix(key, NodePublicKeyPrefix) {
			continue
		}
		nodeName := strings.TrimPrefix(key, NodePublicKeyPrefix)
		addr := strings.TrimSpace(string(data[NodeTunnelAddressPrefix+nodeName]))
		if addr == "" {
			// Published a key but not allocated an address (not
			// selected by any claim's tunnelEndpoints) -- not a mesh
			// member.
			continue
		}
		nodes = append(nodes, localNode{name: nodeName, tunnelAddr: strings.SplitN(addr, "/", 2)[0]})
	}
	sort.Slice(nodes, func(i, j int) bool { return lessIP(nodes[i].tunnelAddr, nodes[j].tunnelAddr) })

	var peers []PeerSpec
	for i, n := range nodes {
		pub := strings.TrimSpace(string(data[NodePublicKeyPrefix+n.name]))
		if pub == "" {
			continue
		}
		allowed := []string{HostCIDR(n.tunnelAddr)}
		routeHosts := []string{n.tunnelAddr}
		for _, vip := range SplitList(string(data[NodeClusterVIPsPrefix+n.name])) {
			allowed = append(allowed, HostCIDR(vip))
			routeHosts = append(routeHosts, vip)
		}
		if i == 0 {
			for _, api := range apiServers {
				if api = strings.TrimSpace(api); api == "" || containsHost(routeHosts, api) {
					continue
				}
				allowed = append(allowed, HostCIDR(api))
				routeHosts = append(routeHosts, api)
			}
		}
		peers = append(peers, PeerSpec{
			PublicKey:    pub,
			WGAllowedIPs: allowed,
			RouteHosts:   routeHosts,
		})
	}
	if len(peers) == 0 {
		return nil, nil
	}

	var remotes []PeerSpec
	for key := range data {
		if !strings.HasPrefix(key, PeerPublicKeyPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, PeerPublicKeyPrefix)
		var routeHosts []string
		if raw, ok := data[PeerRouteHostsPrefix+name]; ok {
			routeHosts = SplitList(string(raw))
		} else if raw, ok := data[PeerRouteHostPrefix+name]; ok {
			routeHosts = []string{strings.TrimSpace(string(raw))}
		}
		if selfTunnelAddr != "" && containsHost(routeHosts, selfTunnelAddr) {
			continue // this entry is the caller itself
		}
		endpoint := strings.TrimSpace(string(data[PeerEndpointPrefix+name]))
		if endpoint == PeerEndpointPending {
			endpoint = ""
		}
		remotes = append(remotes, PeerSpec{
			PublicKey:    strings.TrimSpace(string(data[key])),
			Endpoint:     endpoint,
			WGAllowedIPs: SplitList(string(data[PeerAllowedIPsPrefix+name])),
			RouteHosts:   routeHosts,
		})
	}
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].PublicKey < remotes[j].PublicKey })

	return append(peers, remotes...), nil
}

func containsHost(hosts []string, addr string) bool {
	for _, h := range hosts {
		if strings.SplitN(strings.TrimSpace(h), "/", 2)[0] == addr {
			return true
		}
	}
	return false
}

// lessIP orders textual IPs by byte value (string order for
// unparsable input) -- a stable, family-aware "lowest address" for
// the transit designation.
func lessIP(a, b string) bool {
	ipa, ipb := net.ParseIP(a), net.ParseIP(b)
	if ipa == nil || ipb == nil {
		return a < b
	}
	return string(ipa.To16()) < string(ipb.To16())
}
