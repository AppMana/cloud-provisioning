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
//	node-tunnel-address-<node>   tunnel address (CIDR)
//	node-addresses-<node>     the node's own addresses, so a
//	                          remote gets a host route to each
//
// Controller-written per-remote-machine (the on-prem peer list):
//
//	peer-public-key-<machine>
//	peer-endpoint-<machine>      "pending" until mirrored
//	peer-allowed-ips-<machine>   comma-separated CIDRs
//	peer-route-hosts-<machine>   comma-separated single hosts
//	peer-route-host-<machine>    legacy single-host fallback
const (
	NodePublicKeyPrefix     = "node-public-key-"
	NodeTunnelAddressPrefix = "node-tunnel-address-"
	NodeAddressesPrefix     = "node-addresses-"
	// NodePodCIDRsPrefix carries the pod blocks that node owns, so a
	// peer is permitted exactly the pods behind it and no others. Empty
	// when the network encapsulates: those packets are addressed to the
	// node itself.
	NodePodCIDRsPrefix = "node-pod-cidrs-"

	// SiteAddressesPrefix and SitePodCIDRsPrefix carry a site node that
	// terminates no tunnel: its addresses and the pod blocks it owns.
	//
	// A remote reaches these through an endpoint rather than directly,
	// so they are not peers and never get a tunnel address or a key.
	// They still have to be named. WireGuard's accept list is checked
	// on the way out as well as in, so a block that is routed into the
	// tunnel and permitted by nothing is dropped by the sender, and the
	// route being correct changes nothing.
	SiteAddressesPrefix  = "site-addresses-"
	SitePodCIDRsPrefix   = "site-pod-cidrs-"
	PeerPublicKeyPrefix  = "peer-public-key-"
	PeerEndpointPrefix   = "peer-endpoint-"
	PeerAllowedIPsPrefix = "peer-allowed-ips-"
	PeerRouteHostsPrefix = "peer-route-hosts-"
	PeerRouteHostPrefix  = "peer-route-host-"

	// PeerEndpointPending is the placeholder the join reconciler writes
	// until the endpoint mirror learns the machine's real external IP.
	PeerEndpointPending = "pending"

	// CloudPeersKey is the single data key of a per-machine adoption
	// Secret: a JSON document with a "peers" list (public data only --
	// never a private key), consumed by the cloud dialer's
	// --peers-secret-* override.
	CloudPeersKey = "peers.json"

	// RetiredTunnelAddressesKey lists (comma-separated) the tunnel
	// addresses of nodes that have left the mesh. They are never
	// allocated again while this Secret lives.
	//
	// An address that is freed is an address a later node can be given,
	// and any remote still holding configuration that names it then
	// sends that node's traffic to a different one, encrypted to a key
	// it does not hold. Nothing here can know that no remote holds such
	// configuration: that is exactly the state this reconciler was
	// failing to converge. A /24 per mesh is not scarce enough for the
	// trade to be worth making, and a mesh that exhausts its subnet is
	// one to renumber deliberately rather than by reuse.
	RetiredTunnelAddressesKey = "retired-tunnel-addresses"

	// TunnelAddressReservationPrefix records, per node, the tunnel
	// address that node keeps for as long as it is part of the cluster,
	// whether or not it is currently a selected endpoint.
	//
	// A tunnel address is an identity, not a lease. Every mesh built
	// this way settles on the same rule: Tailscale hands a node an
	// address from 100.64.0.0/10 when it registers and changes it
	// never, and Headscale, which reimplements that contract, returns
	// an address to its allocator only in DeleteNode, the one operation
	// it documents as irreversible. Not on logout, not on expiry, not
	// when a node goes offline.
	//
	// This mesh used to retire an address the moment a node left the
	// endpoint selector and hand it a new one when it came back, so a
	// node that was selected, deselected and selected again was three
	// different peers: one control plane went through four addresses in
	// a day of tests. Every one of those changes has to reach every
	// remote, and a remote learns it over the tunnel, so each change is
	// a window in which a remote permits an address nobody sources from
	// any more. An identity that changes under a peer is the one thing
	// the peer cannot ask about.
	//
	// The reservation is what makes reuse by a DIFFERENT node
	// impossible, which is all RetiredTunnelAddressesKey was ever
	// protecting against.
	TunnelAddressReservationPrefix = "tunnel-address-reservation-"

	// NodeDepartedAtPrefix records, per node, the RFC3339 instant at
	// which that node first stopped being a selected tunnel endpoint
	// while some other endpoint was already usable.
	//
	// A remote reads its peer list from the API server and reaches the
	// API server over the tunnel, so taking a departed endpoint away in
	// the same pass that names its replacement removes the only path the
	// replacement's name could have travelled. The departed node stays a
	// full endpoint, dialer and published entries both, until this
	// instant is far enough behind that a remote polling the Secret has
	// had its chance to see the replacement.
	//
	// It lives in the Secret rather than in the controller's memory
	// because a controller restart during the migration would otherwise
	// either restart the clock forever or drop the node immediately, and
	// which of those happened would depend on when the pod was rescheduled.
	NodeDepartedAtPrefix = "node-departed-at-"

	// APIServersKey lists every control-plane address a remote must be
	// able to reach (comma-separated). k0s workers load-balance across
	// all of them via nllb, so one address is not enough.
	APIServersKey = "api-servers"

	// AppliedListAnnotation is the remote dialer's acknowledgment,
	// written onto its own adoption Secret: the sha256 of the
	// peers.json it most recently applied in full.
	//
	// This is the evidence retention releases on. A departed endpoint
	// is held not for a length of time but until every remote has
	// applied the render that no longer names it, because a clock
	// cannot know whether the remote read the list naming the
	// replacement, and releasing before it did tears down the only
	// path the correction could travel. The comparison is a hash of
	// content against content: no counter, no clock, nothing to drift.
	AppliedListAnnotation = "cloud-provisioning.appmana.com/applied"
)

// HashPeerList is the digest both sides compare: the remote stamps it
// on its adoption Secret after applying, the controller compares it
// against the current render.
func HashPeerList(peersJSON []byte) string {
	sum := sha256.Sum256(peersJSON)
	return fmt.Sprintf("%x", sum)
}

// AdoptionSecretName is the per-machine adoption Secret's name --
// derived from the Machine name on both sides (the endpoint-controller
// creating it, the cloud dialer reading it via the machine-name file
// cloud-init wrote), so the two can never disagree.
func AdoptionSecretName(machineName string) string { return machineName + "-tunnel-peers" }

// PeerSpec is one WireGuard peer a dialer maintains. AllowedIPs and
// RouteHosts are separate: AllowedIPs is WireGuard's cryptokey packet
// filter, RouteHosts are the peer node's own addresses, the only
// things that become kernel routes, each a single host.
type PeerSpec struct {
	PublicKey    string   `json:"publicKey"`
	Endpoint     string   `json:"endpoint,omitempty"`
	WGAllowedIPs []string `json:"allowedIPs"`
	RouteHosts   []string `json:"routeHosts,omitempty"`
	// RouteHost is the legacy single-host form, folded into RouteHosts
	// by consumers.
	RouteHost string `json:"routeHost,omitempty"`
	// Remote marks a peer that is not at this site. Only these are
	// worth telling the site about: a node here can already reach the
	// others by itself.
	Remote bool `json:"remote,omitempty"`
	// Transit and TransitHosts are the subset of WGAllowedIPs and
	// RouteHosts this peer carries by election rather than ownership:
	// the API servers and the site nodes with no tunnel of their own.
	// They ride this peer because some peer must carry them, not
	// because they are its, and that difference is the applier's
	// permission: when its own kernel says this peer's session is dead
	// past WireGuard's horizon and another local is handshaking, it may
	// move exactly this set there. Everything outside it is the peer's
	// own and dies with it.
	Transit      []string `json:"transit,omitempty"`
	TransitHosts []string `json:"transitHosts,omitempty"`
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
	// APIServers is every control plane's host:port, for the node's
	// own loopback balancer: a worker holds all of them and fails over
	// by its own evidence, so no single member's death strands it and
	// no address has to move between nodes to save it.
	APIServers []string `json:"apiServers,omitempty"`
}

// PeerListDoc is the adoption-Secret shape (CloudPeersKey): the peer
// list only, no identity.
type PeerListDoc struct {
	Peers      []PeerSpec `json:"peers"`
	APIServers []string   `json:"apiServers,omitempty"`
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
// family (/32 for IPv4, /128 for IPv6) if addr doesn't already
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

// RemotePeers derives a remote (cloud) node's peer list from the
// shared peer Secret's data: every published local tunnel-endpoint
// node (node-* keys: public key published by that node's own dialer,
// tunnel address and cluster VIPs written by the controller), plus
// each other remote machine (peer-* keys), which are the
// remote-to-remote edges of the fully connected mesh. Isolated remotes
// share no LAN, so without these edges they could not reach each
// other.
//
// selfTunnelAddr identifies which peer-* entry is the caller itself.
// A remote knows its own tunnel address from its identity file rather
// than its Machine name, since Kubernetes node names and Machine names
// differ on most clouds.
//
// apiVIP, when non-empty, is added to the AllowedIPs/RouteHosts of
// the designated transit local (selected before retained, then lowest tunnel address). Control-plane
// nodes carry no tunnel, so remotes reach the API through one local
// worker, which masquerades tunnel-sourced traffic onto the LAN.
//
// Local peers get no Endpoint (the local side is behind NAT and only
// dials out; the remote listens). Remote peers get their real public
// endpoint when known, because two remotes dial each other directly.
//
// The result is public data by construction: the Secret holds no
// private key of any node, and this document lands on internet-facing
// machines.
//
// This function is shared by the join reconciler (bootstrap: snapshot
// rendered once into userdata) and the dialer's adoption mode (live:
// re-derived from the Secret every poll): one derivation, two
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
			// selected by any claim's tunnelEndpoints), so not a mesh
			// member.
			continue
		}
		nodes = append(nodes, localNode{name: nodeName, tunnelAddr: strings.SplitN(addr, "/", 2)[0]})
	}
	sort.Slice(nodes, func(i, j int) bool {
		return relayLess(data, nodes[i].name, nodes[i].tunnelAddr, nodes[j].name, nodes[j].tunnelAddr)
	})

	// What the peers will own on their own entries, which is what the
	// relay must not go on to claim a second time.
	//
	// Being a peer is not the test, and the two families are separate.
	// A retained endpoint is rendered as a peer, because it keeps its
	// key and tunnel address so the tunnel it still holds goes on
	// working, and yet its addresses and pod blocks have already moved
	// to the site entries: treating it as owning them would leave them
	// owned by nobody. What a node owns is exactly what is published
	// under its own keys.
	ownsAddresses := map[string]bool{}
	ownsBlocks := map[string]bool{}
	ownHost := map[string]bool{}
	for _, n := range nodes {
		if strings.TrimSpace(string(data[NodePublicKeyPrefix+n.name])) == "" {
			continue
		}
		ownHost[n.tunnelAddr] = true
		if addrs := SplitList(string(data[NodeAddressesPrefix+n.name])); len(addrs) > 0 {
			ownsAddresses[n.name] = true
			for _, addr := range addrs {
				ownHost[addr] = true
			}
		}
		if len(SplitList(string(data[NodePodCIDRsPrefix+n.name]))) > 0 {
			ownsBlocks[n.name] = true
		}
	}

	var peers []PeerSpec
	relayed := false
	for _, n := range nodes {
		pub := strings.TrimSpace(string(data[NodePublicKeyPrefix+n.name]))
		if pub == "" {
			continue
		}
		allowed := []string{HostCIDR(n.tunnelAddr)}
		routeHosts := []string{n.tunnelAddr}
		for _, addr := range SplitList(string(data[NodeAddressesPrefix+n.name])) {
			allowed = append(allowed, HostCIDR(addr))
			routeHosts = append(routeHosts, addr)
		}
		// This node's own pod blocks, and only its own. They are
		// permitted but never routed: reaching a pod is the network's
		// job, over the session these host routes make possible.
		allowed = append(allowed, SplitList(string(data[NodePodCIDRsPrefix+n.name]))...)
		var transit, transitHosts []string
		if !relayed {
			relayed = true
			// The API server, for a site where no node terminating a
			// tunnel is a control plane: the relay masquerades onto the
			// LAN to reach it. When a control plane does hold a tunnel
			// it is a peer in its own right and already owns the
			// address, so claiming it here as well would name it on two
			// peers, and the accept list would send the API traffic to
			// whichever was configured last.
			for _, api := range apiServers {
				if api = strings.TrimSpace(api); api == "" || containsHost(routeHosts, api) || ownHost[api] {
					continue
				}
				allowed = append(allowed, HostCIDR(api))
				routeHosts = append(routeHosts, api)
				transit = append(transit, HostCIDR(api))
				transitHosts = append(transitHosts, api)
			}
			// The rest of the site: nodes with no tunnel, reached by
			// relaying through this one. They go on a single peer
			// because the accept list has one owner per prefix, and on
			// this one because it is the same peer already designated
			// to relay, so a remote has one path to the site rather
			// than a different one per destination.
			for _, name := range siteNodeNames(data) {
				// A node already owning these on its own entry must not
				// have them relayed here as well, or the same prefix is
				// named on two peers. The two forms are written by
				// different actors: this operator publishes the
				// addresses, the node's own dialer publishes the key
				// that turns it into a peer, and between those writes
				// the Secret holds both. Resolving it here makes the
				// render self-consistent whatever order they land in,
				// rather than making correctness depend on that order.
				if !ownsAddresses[name] {
					for _, addr := range SplitList(string(data[SiteAddressesPrefix+name])) {
						if containsHost(routeHosts, addr) {
							continue
						}
						allowed = append(allowed, HostCIDR(addr))
						routeHosts = append(routeHosts, addr)
						transit = append(transit, HostCIDR(addr))
						transitHosts = append(transitHosts, addr)
					}
				}
				// Permitted, never routed: the route for a pod block
				// comes from the network, over the session these host
				// routes make possible.
				if !ownsBlocks[name] {
					blocks := SplitList(string(data[SitePodCIDRsPrefix+name]))
					allowed = append(allowed, blocks...)
					transit = append(transit, blocks...)
				}
			}
		}
		peers = append(peers, PeerSpec{
			PublicKey:    pub,
			WGAllowedIPs: allowed,
			RouteHosts:   routeHosts,
			Transit:      transit,
			TransitHosts: transitHosts,
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

	return ProjectGateways(data, append(peers, remotes...), remoteSelfKey(data, selfTunnelAddr))
}

// TransitSpec is how a site node with no tunnel of its own reaches the
// remotes: every remote prefix, via the node that relays for the rest
// of the site.
type TransitSpec struct {
	// Via is the relay's own address: the next hop for everything
	// below, reachable over the site's ordinary network.
	Via string
	// Hosts are the remote node addresses, one host each.
	Hosts []string
	// Blocks are the remote pod blocks.
	Blocks []string
}

// DocRelaysNode reports whether a remote's peer list carries the
// named node's addresses on a relay's transit rather than on the
// node's own entry. This is the content behind an acknowledgment: an
// applied hash proves the remote holds this list, and only this
// question decides whether traffic the node sends through the relay
// will be accepted by the remote's cryptokey routing. A list that
// still carries the node's own entry routes it directly, whatever
// else it declares: the accept trie has one owner per prefix, and
// egressing via the relay against a direct-owning list is dropped at
// the remote (measured: 78 seconds of a relayed node's return
// traffic dying after a placement shrink, acknowledged fresh the
// whole time).
func DocRelaysNode(doc PeerListDoc, publicKey string, addrs []string) bool {
	covered := map[string]bool{}
	for _, p := range doc.Peers {
		if p.PublicKey == publicKey {
			// The node's own entry still exists: it owns whatever it
			// carries, so the remote routes it directly.
			for _, allowed := range p.WGAllowedIPs {
				for _, addr := range addrs {
					if HostCIDR(strings.TrimSpace(allowed)) == HostCIDR(addr) {
						return false
					}
				}
			}
			continue
		}
		for _, host := range p.TransitHosts {
			for _, addr := range addrs {
				if HostCIDR(strings.TrimSpace(host)) == HostCIDR(addr) {
					covered[addr] = true
				}
			}
		}
	}
	if len(addrs) == 0 {
		return false
	}
	for _, addr := range addrs {
		if !covered[addr] {
			return false
		}
	}
	return true
}

// relayLess keeps both renderers on the same election. A retained endpoint
// must yield transit to its replacement; otherwise its own site addresses
// stay on its own peer and the retirement acknowledgment can never succeed.
// Retained peers remain candidates when no selected endpoint is published.
func relayLess(data map[string][]byte, aName, aAddr, bName, bAddr string) bool {
	aDeparted := len(data[NodeDepartedAtPrefix+aName]) != 0
	bDeparted := len(data[NodeDepartedAtPrefix+bName]) != 0
	if aDeparted != bDeparted {
		return !aDeparted
	}
	return LessIP(aAddr, bAddr)
}

// SiteTransit derives the transit a no-tunnel site node installs for
// itself, from the same data and the same election as RemotePeers.
//
// The relay it picks is, by construction, the peer whose entry carries
// this node's own prefixes in every remote's accept list, so its
// sources are accepted at the far end. Having a protocol carry this
// choice instead was tried, and left windows in which the choice was
// in flight while the routes it replaced were already gone; a shared
// derivation has no window.
//
// Returns nil when no published endpoint exists: there is nothing to
// carry the traffic, and no route is better than a guessed one.
// notReady names endpoints the cluster reports unhealthy; the election
// passes over them while any candidate remains. It is the one fact a
// node with no tunnel legitimately owns about its neighbours, read from
// the same API server everything else here is read from. With every
// candidate unhealthy the rendered election stands: a maybe-dead relay
// is a path that may come back, and the report may itself be stale.
func SiteTransit(data map[string][]byte, notReady map[string]bool) (*TransitSpec, error) {
	type candidate struct {
		name       string
		tunnelAddr string
	}
	var nodes []candidate
	for key := range data {
		if !strings.HasPrefix(key, NodePublicKeyPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, NodePublicKeyPrefix)
		if strings.TrimSpace(string(data[key])) == "" {
			continue
		}
		addr := strings.TrimSpace(string(data[NodeTunnelAddressPrefix+name]))
		if addr == "" {
			continue
		}
		nodes = append(nodes, candidate{name: name, tunnelAddr: strings.SplitN(addr, "/", 2)[0]})
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	sort.Slice(nodes, func(i, j int) bool {
		return relayLess(data, nodes[i].name, nodes[i].tunnelAddr, nodes[j].name, nodes[j].tunnelAddr)
	})
	relay := nodes[0]
	for _, n := range nodes {
		if !notReady[n.name] {
			relay = n
			break
		}
	}

	// The relay's reachable address. An owning relay publishes it under
	// its node entry; a retained relay's addresses have already moved
	// to the site entry, and it is still the relay and still reachable.
	via := ""
	if addrs := SplitList(string(data[NodeAddressesPrefix+relay.name])); len(addrs) > 0 {
		via = addrs[0]
	} else if addrs := SplitList(string(data[SiteAddressesPrefix+relay.name])); len(addrs) > 0 {
		via = addrs[0]
	}
	if via == "" {
		return nil, nil
	}

	transit := &TransitSpec{Via: via}
	seen := map[string]bool{}
	peers, err := sitePeers(data, false)
	if err != nil {
		return nil, err
	}
	for _, peer := range peers {
		hosts := peer.AllRouteHosts()
		hostSet := map[string]bool{}
		for _, h := range hosts {
			hostSet[strings.SplitN(h, "/", 2)[0]] = true
			if !seen[h] {
				seen[h] = true
				transit.Hosts = append(transit.Hosts, h)
			}
		}
		// Whatever the peer is permitted beyond its own addresses is
		// the pod space behind it.
		for _, allowed := range peer.WGAllowedIPs {
			host := strings.SplitN(allowed, "/", 2)[0]
			if hostSet[host] {
				continue
			}
			if !seen[allowed] {
				seen[allowed] = true
				transit.Blocks = append(transit.Blocks, allowed)
			}
		}
	}
	sort.Strings(transit.Hosts)
	sort.Strings(transit.Blocks)
	return transit, nil
}

// HostSysctlNet is where a node's real /proc/sys/net is mounted into
// the dialer. A container runtime mounts /proc/sys read-only and
// NET_ADMIN does not lift that, so a setting the dialer must make
// rather than merely read has to reach the node through a mount. Both
// the DaemonSet that creates the mount and the dialer that looks for
// it name it here.
const HostSysctlNet = "/host/proc/sys/net"

// siteNodeNames lists the site nodes that terminate no tunnel, in a
// stable order so the accept list does not change shape between passes.
func siteNodeNames(data map[string][]byte) []string {
	var names []string
	for key := range data {
		if strings.HasPrefix(key, SiteAddressesPrefix) {
			names = append(names, strings.TrimPrefix(key, SiteAddressesPrefix))
		}
	}
	sort.Strings(names)
	return names
}

func containsHost(hosts []string, addr string) bool {
	for _, h := range hosts {
		if strings.SplitN(strings.TrimSpace(h), "/", 2)[0] == addr {
			return true
		}
	}
	return false
}

// LessIP orders textual IPs by byte value (string order for
// unparsable input): a stable, family-aware "lowest address" for
// the transit designation, and for any other ordering that several
// nodes have to agree on without coordinating.
func LessIP(a, b string) bool {
	ipa, ipb := net.ParseIP(a), net.ParseIP(b)
	if ipa == nil || ipb == nil {
		return a < b
	}
	return string(ipa.To16()) < string(ipb.To16())
}
