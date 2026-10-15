package main

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	api "github.com/osrg/gobgp/v3/api"
	"github.com/osrg/gobgp/v3/pkg/server"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/wgctrl"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Called only by the isolated real-API fixture after native forwarding setup.
func checkNativeBGPRecovery(t *testing.T, ctx context.Context, live, offline *kubernetes.Clientset, devices *wgctrl.Client, cfg config, sources []string) {
	t.Helper()
	cfg.bgpListenPort = 11790
	cfg.bgpASN = 64512
	cfg.bgpNextHop = strings.Join(sources, ",")
	nodes, err := live.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	peerIP := ""
	for _, node := range nodes.Items {
		if node.Name != "w1" {
			continue
		}
		for _, a := range node.Status.Addresses {
			if a.Type == "InternalIP" && net.ParseIP(a.Address).To4() != nil {
				peerIP = a.Address
			}
		}
	}
	if peerIP == "" {
		t.Fatal("missing isolated BGP peer address")
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		t.Fatal(err)
	}
	addr, err := netlink.ParseAddr(peerIP + "/32")
	if err != nil {
		t.Fatal(err)
	}
	if err := netlink.AddrAdd(lo, addr); err != nil {
		t.Fatal(err)
	}
	speaker, err := startTransitSpeaker(ctx, cfg.bgpListenPort, uint32(cfg.bgpASN), cfg.bgpNextHop)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { speaker.stop(context.Background()) }()
	if err := reconcileNode(ctx, live, devices, cfg, speaker); err != nil {
		t.Fatal("initial BGP reconciliation", err)
	}
	peer := server.NewBgpServer()
	go peer.Serve()
	if err := peer.StartBgp(ctx, &api.StartBgpRequest{Global: &api.Global{Asn: 64512, RouterId: peerIP, ListenPort: -1}}); err != nil {
		t.Fatal(err)
	}
	defer peer.StopBgp(context.Background(), &api.StopBgpRequest{})
	families := []*api.AfiSafi{}
	for _, afi := range []api.Family_Afi{api.Family_AFI_IP, api.Family_AFI_IP6} {
		families = append(families, &api.AfiSafi{Config: &api.AfiSafiConfig{Family: &api.Family{Afi: afi, Safi: api.Family_SAFI_UNICAST}, Enabled: true}})
	}
	if err := peer.AddPeer(ctx, &api.AddPeerRequest{Peer: &api.Peer{Conf: &api.PeerConf{NeighborAddress: speaker.nextHop, PeerAsn: 64512}, Transport: &api.Transport{LocalAddress: peerIP, RemotePort: uint32(cfg.bgpListenPort)}, AfiSafis: families, Timers: &api.Timers{Config: &api.TimersConfig{ConnectRetry: 1}}}}); err != nil {
		t.Fatal(err)
	}
	waitRoutes := func(label string, want bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		found := map[string]bool{}
		for time.Now().Before(deadline) {
			found = map[string]bool{}
			for _, afi := range []api.Family_Afi{api.Family_AFI_IP, api.Family_AFI_IP6} {
				if err := peer.ListPath(ctx, &api.ListPathRequest{TableType: api.TableType_GLOBAL, Family: &api.Family{Afi: afi, Safi: api.Family_SAFI_UNICAST}}, func(d *api.Destination) {
					if len(d.Paths) > 0 {
						found[d.Prefix] = true
					}
				}); err != nil {
					t.Fatal(err)
				}
			}
			if (want && found["10.88.0.3/32"] && found["fd88::3/128"]) || (!want && len(found) == 0) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s: receiving BGP peer has unexpected routes: %v", label, found)
	}
	waitRoutes("before restart", true)
	speaker.stop(ctx)
	waitRoutes("session withdrawal", false)
	speaker, err = startTransitSpeaker(ctx, cfg.bgpListenPort, uint32(cfg.bgpASN), cfg.bgpNextHop)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileNode(ctx, offline, devices, cfg, speaker); err != nil {
		t.Fatal("offline BGP restart", err)
	}
	waitRoutes("after offline restart", true)
	mesh, err := live.CoreV1().Secrets(cfg.secretNamespace).Get(ctx, cfg.secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"uid": mesh.UID, "resourceVersion": mesh.ResourceVersion}, "data": map[string][]byte{tunnel.PeerPublicKeyPrefix + "worker": nil, tunnel.PeerEndpointPrefix + "worker": nil, tunnel.PeerAllowedIPsPrefix + "worker": nil, tunnel.PeerRouteHostsPrefix + "worker": nil}})
	if _, err := live.CoreV1().Secrets(cfg.secretNamespace).Patch(ctx, cfg.secretName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reconcileNode(ctx, live, devices, cfg, speaker); err != nil {
		t.Fatal("BGP withdrawal", err)
	}
	waitRoutes("authoritative withdrawal", false)
	speaker.stop(ctx)
	speaker, err = startTransitSpeaker(ctx, cfg.bgpListenPort, uint32(cfg.bgpASN), cfg.bgpNextHop)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileNode(ctx, offline, devices, cfg, speaker); err != nil {
		t.Fatal("empty offline BGP recovery", err)
	}
	if len(speaker.advertised) != 0 {
		t.Fatal("offline restart resurrected withdrawn BGP routes")
	}
	t.Log("Receiving BGP peer learned both families before and after speaker restart without API access")
}
