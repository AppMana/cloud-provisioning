package main

// Telling the rest of the site how to reach a remote node.
//
// A node that terminates no tunnel already has the route it needs for a
// remote's pods: its CNI installed "remote pod block via remote node
// address" from the cluster's own records. What it does not have is a
// route to that next hop, so the route resolves to nothing.
//
// It cannot learn one by peering with the remote either. The remote's
// address, as far as the CNI is concerned, is its tunnel address, and a
// node with no tunnel has no route to that. The session could never be
// established. Something already on the tunnel has to say it.
//
// So a node that does terminate a tunnel advertises the remote node
// addresses with itself as the next hop. The CNI's own router receives
// them like any other route, which is what makes withdrawal, and
// choosing between two endpoints, its problem rather than ours.
//
// Only node addresses are advertised, never pod blocks. The blocks are
// already distributed by the CNI, and a prefix with two owners belongs
// to whichever spoke last; the blocks also churn as pods come and go,
// while a node address changes only when a node does.
//
// The speaker listens on its own port. A node running a CNI that speaks
// BGP already has something on 179.

import (
	"context"
	"fmt"
	"net"
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
// one peering with every other node at this site, and one route for
// every remote node address reachable through this one.
func (t *transitSpeaker) reconcile(ctx context.Context, sitePeers, remoteAddrs []string) error {
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

	want := map[string]bool{}
	for _, addr := range remoteAddrs {
		if addr = strings.TrimSpace(addr); addr != "" {
			want[addr] = true
		}
	}
	for addr := range want {
		if t.advertised[addr] {
			continue
		}
		if err := t.advertise(ctx, addr, false); err != nil {
			return err
		}
		t.advertised[addr] = true
	}
	// A remote that has gone is withdrawn, so the site stops sending
	// its traffic here.
	for addr := range t.advertised {
		if want[addr] {
			continue
		}
		if err := t.advertise(ctx, addr, true); err != nil {
			return err
		}
		delete(t.advertised, addr)
	}
	return nil
}

// advertise announces or withdraws one host route for a remote node.
func (t *transitSpeaker) advertise(ctx context.Context, addr string, withdraw bool) error {
	ip := net.ParseIP(addr)
	if ip == nil {
		return fmt.Errorf("refusing to advertise %q: not an address", addr)
	}
	family := &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST}
	length := uint32(32)
	if ip.To4() == nil {
		family = &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST}
		length = 128
	}
	nlri, err := apb.New(&api.IPAddressPrefix{Prefix: ip.String(), PrefixLen: length})
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
	path := &api.Path{
		Family: family,
		Nlri:   nlri,
		Pattrs: []*apb.Any{origin, nextHop},
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
