package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
)

// The k0s/Calico remote rebooted after w1/w2 -> cp settled. Its host
// service installed the original worker peers, although the durable cache
// named cp. Neither retired worker could carry the API path needed to restart
// the adopting DaemonSet. No Kubernetes client exists at this point in boot.
func TestObservedReboot_HostServiceUsesCachedPeers(t *testing.T) {
	cfg := config{iface: "cldt1775f69e", peersFile: filepath.Join(t.TempDir(), "peers.json")}
	bootstrap := tunnel.PeersFileDoc{Peers: []tunnel.PeerSpec{{PublicKey: "retired-w1"}, {PublicKey: "retired-w2"}}, APIServers: []string{"10.10.0.10:6443"}}
	cached := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: "selected-cp", WGAllowedIPs: []string{"10.100.0.3/32", "10.10.0.10/32"}, RouteHosts: []string{"10.100.0.3", "10.10.0.10"}}}, APIServers: bootstrap.APIServers}
	if got, err := bootPeerList(cfg, bootstrap); err != nil || len(got.Peers) != 2 {
		t.Fatalf("first boot lost bootstrap peers: %+v", got)
	}
	if err := writeCachedPeers(cachePath(cfg), cached); err != nil {
		t.Fatal(err)
	}
	got, err := bootPeerList(cfg, bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Peers) != 1 || got.Peers[0].PublicKey != "selected-cp" {
		t.Fatalf("reboot peers = %+v; want the replacement that can carry API traffic", got.Peers)
	}
	if err := writeCachedPeers(cachePath(cfg), tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{}}); err != nil {
		t.Fatal(err)
	}
	if got, err := bootPeerList(cfg, bootstrap); err != nil || got.Peers == nil || len(got.Peers) != 0 {
		t.Fatalf("explicit cached withdrawal resurrected bootstrap peers: %+v", got)
	}
	for _, raw := range []string{"invalid", `{}`, `{"peers":null}`} {
		if err := os.WriteFile(cachePath(cfg), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if got, err := bootPeerList(cfg, bootstrap); err == nil || len(got.Peers) != 0 {
			t.Fatalf("unusable cache restored bootstrap peers: %+v, err=%v", got, err)
		}
	}
}

func TestCorruptCacheDoesNotSelectBootstrapAPIBackends(t *testing.T) {
	cfg := config{iface: "cldtcachetest", peersFile: filepath.Join(t.TempDir(), "identity.json")}
	if err := os.WriteFile(cfg.peersFile, []byte(`{"peers":[],"apiServers":["10.10.0.10:6443"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := apiProxyBackendsFromFiles(cfg); len(got) != 1 {
		t.Fatalf("missing cache must permit first boot: %v", got)
	}
	for _, raw := range []string{"invalid", `{}`, `{"peers":null}`, `{"peers":[]}`} {
		if err := os.WriteFile(cachePath(cfg), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if got := apiProxyBackendsFromFiles(cfg); len(got) != 0 {
			t.Fatalf("cache %s resurrected bootstrap API backends: %v", raw, got)
		}
	}
	if err := writeCachedPeers(cachePath(cfg), tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{}, APIServers: []string{"10.10.0.13:6443"}}); err != nil {
		t.Fatal(err)
	}
	if got := apiProxyBackendsFromFiles(cfg); len(got) != 1 || got[0] != "10.10.0.13:6443" {
		t.Fatalf("repaired cache not selected: %v", got)
	}
}
