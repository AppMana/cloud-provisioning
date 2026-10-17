package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// The isolated native phase fixture supplies actual kernel and WireGuard
// state. Its graph is synthetic; this verifies BGP RIB restoration against
// native readback, not API admission or external neighbor convergence.
func checkNativeReceiveAdvertisements(t *testing.T, devices receiverDevices, cfg config, key wgtypes.Key, nodeUID string, plan tunnel.ReceiveTransitionPlan, intent tunnel.ReceiveJournalRoutes) {
	t.Helper()
	ctx := context.Background()
	cfg.secretNamespace, cfg.secretName = plan.Namespace, plan.MeshName
	cfg.privateKeyFile = filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(cfg.privateKeyFile, []byte(key.String()), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.bgpListenPort = -1
	data := map[string][]byte{}
	for public, site := range plan.Sites {
		data[tunnel.NodePublicKeyPrefix+site.NodeName] = []byte(public)
		data[tunnel.NodeTunnelAddressPrefix+site.NodeName] = []byte(strings.Join(site.Legacy.Tunnel.Addresses, ","))
		data[tunnel.NodeAddressesPrefix+site.NodeName] = []byte(strings.Join(site.RouteSources, ","))
	}
	for _, peer := range plan.SiteDesired.Peers {
		if peer.PublicKey != plan.RootPublicKey {
			continue
		}
		data[tunnel.PeerPublicKeyPrefix+plan.MachineName] = []byte(peer.PublicKey)
		data[tunnel.PeerAllowedIPsPrefix+plan.MachineName] = []byte(strings.Join(peer.WGAllowedIPs, ","))
		data[tunnel.PeerRouteHostsPrefix+plan.MachineName] = []byte(strings.Join(peer.AllRouteHosts(), ","))
	}
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.secretNamespace, Name: cfg.secretName, UID: types.UID(plan.MeshUID), ResourceVersion: "1"}, Data: data}
	if err := saveSiteForwardingCache(cfg, key, mesh, nil, map[string]bool{}, nil); err != nil {
		t.Fatal(err)
	}
	// Start empty speakers twice to exercise recovery independently of the old
	// process's advertised map; do not change forwarding between restarts.
	for i := 0; i < 2; i++ {
		speaker, err := startTransitSpeaker(ctx, -1, 64512, "10.90.0.1,fd90::1")
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer speaker.stop(ctx)
			if err := saveTransitAdvertisements(cfg, speaker, mesh, []string{}); err != nil {
				t.Fatal(err)
			}
			if err := reconcileReceiveAdvertisements(ctx, devices, cfg, speaker, plan.MachineUID, nodeUID, intent); err != nil {
				t.Fatal("native advertisement recovery", err)
			}
			routes, _, err := transitAdvertisementPlan(data, cfg.nodeName)
			if err != nil {
				t.Fatal(err)
			}
			if len(routes) == 0 || len(speaker.advertised) != len(routes) {
				t.Fatal("lost native advertisements")
			}
			for _, route := range routes {
				med, err := advertisedMED(ctx, speaker, route.prefix)
				if err != nil || med != route.med {
					t.Fatal("BGP RIB differs from verified forwarding", route, med, err)
				}
			}
		}()
	}
}
