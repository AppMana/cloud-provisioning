package main

// Making every node reachable at its own address.
//
// A CNI assumes the nodes share an underlay: that any node can open a
// session to any other at the address the cluster knows it by, and that
// a route whose next hop is a node address resolves. That assumption is
// what a tunnel breaks. A remote node's address is its tunnel address,
// and a node terminating no tunnel has no route to it.
//
// Everything follows from that one gap, in order. The node cannot reach
// the remote, so their session never establishes, so it never learns the
// remote's pod blocks, so it has neither the blocks nor a next hop for
// them. Restoring the address restores the rest without anything else
// being said: the session comes up and the CNI distributes the blocks
// over it, the way it does for any other node.
//
// So a node terminating a tunnel advertises the addresses reachable
// through it, with itself as the next hop, to every node that cannot
// reach them directly. The CNI's own router receives them like any other
// route, which leaves withdrawal, and the choice between two endpoints,
// where it belongs.
//
// The node addresses alone would be enough if the site learned them by
// any other means. They do not: nothing of ours runs on a node with no
// tunnel, so BGP is the only way to reach it, and a router will not
// resolve one BGP route's next hop with another BGP route. The address
// therefore arrives and the blocks behind it stay unreachable. So the
// blocks are carried here too, with this node as the next hop, which
// resolves against a directly connected address and asks nothing of
// recursion. See blockRoute.
//
// The speaker listens on its own port. A node running a CNI that speaks
// BGP already has something on 179.

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"

	apb "google.golang.org/protobuf/types/known/anypb"

	api "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
)

// transitSpeaker advertises the remote node addresses reachable through
// this node.
type transitSpeaker struct {
	server *server.BgpServer
	asn    uint32
	// nextHop is this node's own address, which is what the rest of the
	// site can reach and where their packets have to arrive.
	nextHop string

	peers      map[string]bool // site nodes this speaker talks to
	advertised map[string]bool // node addresses currently advertised
}

// startTransitSpeaker brings up a speaker on the given port. A zero port
// disables it, which is the case on a cluster whose CNI has no router to
// tell.
func startTransitSpeaker(ctx context.Context, port int, asn uint32, nextHop string) (*transitSpeaker, error) {
	if port == 0 {
		return nil, nil
	}
	if nextHop == "" {
		return nil, fmt.Errorf("a transit speaker needs this node's own address as the next hop")
	}
	s := server.NewBgpServer()
	go s.Serve()
	if err := s.StartBgp(ctx, &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        asn,
			RouterId:   nextHop,
			ListenPort: int32(port),
		},
	}); err != nil {
		return nil, fmt.Errorf("starting the transit speaker: %w", err)
	}
	return &transitSpeaker{
		server:     s,
		asn:        asn,
		nextHop:    nextHop,
		peers:      map[string]bool{},
		advertised: map[string]bool{},
	}, nil
}

// reconcile makes the speaker's peers and advertisements match the mesh:
// one peering with every node at this site that cannot reach the
// remotes itself, and one route for everything reachable through here.
//
// An empty route set withdraws nothing. Every route the site has to a
// remote depends on these, and the session it then builds to that
// remote depends on the route, so a withdrawal costs far more than the
// staleness it prevents. A mesh that reads as empty is a failed read
// until proven otherwise; a remote that is genuinely gone is removed
// when some other remote is still there to prove the read was good.
func (t *transitSpeaker) reconcile(ctx context.Context, sitePeers []string, routes []transitRoute) error {
	if t == nil {
		return nil
	}
	for _, addr := range sitePeers {
		addr = strings.TrimSpace(addr)
		if addr == "" || addr == t.nextHop || t.peers[addr] {
			continue
		}
		if err := t.server.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{
			Conf: &api.PeerConf{NeighborAddress: addr, PeerAsn: t.asn},
			// This side never initiates. A node at the site dials the
			// speaker, the same way it dials its CNI's other peers.
			Transport: &api.Transport{PassiveMode: true},
		}}); err != nil {
			return fmt.Errorf("adding transit peer %s: %w", addr, err)
		}
		t.peers[addr] = true
	}

	want := map[string]transitRoute{}
	for _, r := range routes {
		if r.prefix = strings.TrimSpace(r.prefix); r.prefix != "" {
			want[r.prefix] = r
		}
	}
	for _, r := range want {
		if t.advertised[r.prefix] {
			continue
		}
		if err := t.advertise(ctx, r, false); err != nil {
			return err
		}
		t.advertised[r.prefix] = true
	}
	if len(want) == 0 {
		return nil
	}
	for prefix := range t.advertised {
		if _, ok := want[prefix]; ok {
			continue
		}
		if err := t.advertise(ctx, transitRoute{prefix: prefix}, true); err != nil {
			return err
		}
		delete(t.advertised, prefix)
	}
	return nil
}

// transitRoute is one thing reachable through this node.
//
// med orders the endpoints that can carry it. Every site node runs the
// same selection on the same advertisements, so a shared preference is
// what makes them agree on which endpoint to use, rather than each
// picking independently and sending traffic whose replies come back
// another way.
type transitRoute struct {
	prefix string
	med    uint32
}

// hostRoute is a remote node's own address: the underlay repair that
// lets a session to that node establish at all.
func hostRoute(addr string, med uint32) (transitRoute, error) {
	ip := net.ParseIP(strings.TrimSpace(addr))
	if ip == nil {
		return transitRoute{}, fmt.Errorf("refusing to advertise %q: not an address", addr)
	}
	length := 32
	if ip.To4() == nil {
		length = 128
	}
	return transitRoute{prefix: fmt.Sprintf("%s/%d", ip.String(), length), med: med}, nil
}

// blockRoute is a remote node's pod block.
//
// Advertising these is not redundant with the node address, though it
// looks it. A site node with no tunnel learns the node address from
// this speaker, over BGP. Its router will not resolve one BGP route's
// next hop with another BGP route, since that is how resolution loops
// form, so the block it learns from the remote directly stays
// unreachable no matter that the address underneath it is now routed.
// Carrying the block here with this node as the next hop resolves it
// against a directly connected address instead, and needs no recursion.
func blockRoute(cidr string, med uint32) (transitRoute, error) {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
	if err != nil {
		return transitRoute{}, fmt.Errorf("refusing to advertise %q: %w", cidr, err)
	}
	if prefix.Bits() == 0 {
		return transitRoute{}, fmt.Errorf("refusing to advertise %q: it would take every destination", cidr)
	}
	return transitRoute{prefix: prefix.Masked().String(), med: med}, nil
}

// advertise announces or withdraws one route, with this node as the
// next hop.
func (t *transitSpeaker) advertise(ctx context.Context, route transitRoute, withdraw bool) error {
	prefix, err := netip.ParsePrefix(route.prefix)
	if err != nil {
		return fmt.Errorf("refusing to advertise %q: %w", route.prefix, err)
	}
	family := &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}
	if prefix.Addr().Is6() {
		family = &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST}
	}
	nlri, err := apb.New(&api.IPAddressPrefix{Prefix: prefix.Addr().String(), PrefixLen: uint32(prefix.Bits())})
	if err != nil {
		return err
	}
	origin, err := apb.New(&api.OriginAttribute{Origin: 0})
	if err != nil {
		return err
	}
	nextHop, err := apb.New(&api.NextHopAttribute{NextHop: t.nextHop})
	if err != nil {
		return err
	}
	med, err := apb.New(&api.MultiExitDiscAttribute{Med: route.med})
	if err != nil {
		return err
	}
	path := &api.Path{
		Family: family,
		Nlri:   nlri,
		Pattrs: []*apb.Any{origin, nextHop, med},
	}
	if withdraw {
		return t.server.DeletePath(ctx, &api.DeletePathRequest{Path: path})
	}
	_, err = t.server.AddPath(ctx, &api.AddPathRequest{Path: path})
	return err
}

func (t *transitSpeaker) stop(ctx context.Context) {
	if t == nil {
		return
	}
	if err := t.server.StopBgp(ctx, &api.StopBgpRequest{}); err != nil {
		fmt.Fprintf(os.Stderr, "stopping the transit speaker: %v\n", err)
	}
}
