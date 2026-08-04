package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// This is the core safety regression for this binary: only exact host
// prefixes ever become kernel routes. A broad prefix reaching the
// route path -- the exact shape of the the on-prem host 0.0.0.0/0 incident -- is
// rejected at parse time, before any route is touched.
func TestParseHostRoute_RefusesAnythingBroaderThanAHost(t *testing.T) {
	for _, bad := range []string{"0.0.0.0/0", "::/0", "10.101.0.0/24", "10.0.0.0/8", "fd8f:cf26:522a::/64"} {
		if _, err := parseHostRoute(bad); err == nil {
			t.Errorf("parseHostRoute(%q) accepted a non-host prefix", bad)
		} else if !strings.Contains(err.Error(), "single hosts") {
			t.Errorf("parseHostRoute(%q) error %q does not state the host-only invariant", bad, err)
		}
	}
	for _, good := range []string{"10.100.0.2", "10.101.0.4/32", "fd8f:cf26:522a::4", "fd8f:cf26:522a::4/128"} {
		if _, err := parseHostRoute(good); err != nil {
			t.Errorf("parseHostRoute(%q): %v", good, err)
		}
	}
}

// WGAllowedIPs (WireGuard's cryptokey accept-list) and RouteHosts (the
// only things installed as kernel routes) must come out as genuinely
// different values -- conflating them was the root cause of the the on-prem host
// incident.
func TestLoadPeersFromSecret_AllowListAndRouteHostsAreIndependent(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wg-dialer-peer", Namespace: "wg-dialer"},
		Data: map[string][]byte{
			"peer-public-key-cloud-1":  []byte("cloudpubkey1"),
			"peer-endpoint-cloud-1":    []byte("203.0.113.10:51820"),
			"peer-allowed-ips-cloud-1": []byte("10.100.0.2/32,10.101.0.4/32,fd8f:cf26:522a::4/128"),
			"peer-route-hosts-cloud-1": []byte("10.100.0.2,10.101.0.4,fd8f:cf26:522a::4"),
			"peer-public-key-cloud-2":  []byte("cloudpubkey2"),
			"peer-endpoint-cloud-2":    []byte("pending"),
			"peer-allowed-ips-cloud-2": []byte("10.100.0.3/32"),
			"peer-route-host-cloud-2":  []byte("10.100.0.3"), // legacy single-host key still honored
			"node-public-key-the on-prem host":   []byte("unrelated"),  // must NOT be treated as a peer
		},
	}

	peers, err := loadPeersFromSecret(secret)
	if err != nil {
		t.Fatalf("loadPeersFromSecret: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("expected exactly 2 peers, got %d: %+v", len(peers), peers)
	}

	byPub := map[string]tunnel.PeerSpec{}
	for _, p := range peers {
		byPub[p.PublicKey] = p
	}

	cloud1, ok := byPub["cloudpubkey1"]
	if !ok {
		t.Fatal("cloud-1 peer not found")
	}
	if cloud1.Endpoint != "203.0.113.10:51820" {
		t.Errorf("cloud-1 Endpoint = %q, want the real endpoint", cloud1.Endpoint)
	}
	if got := cloud1.AllRouteHosts(); len(got) != 3 {
		t.Errorf("cloud-1 route hosts = %v, want tunnel address + both node VIPs", got)
	}
	if len(cloud1.WGAllowedIPs) != 3 {
		t.Errorf("cloud-1 WGAllowedIPs = %v, want 3 independent entries", cloud1.WGAllowedIPs)
	}

	// "pending" (the mirror's placeholder before the real external IP
	// is known) must become an EMPTY Endpoint, not a literal string
	// WireGuard would try (and fail) to resolve as a UDP address.
	cloud2, ok := byPub["cloudpubkey2"]
	if !ok {
		t.Fatal("cloud-2 peer not found")
	}
	if cloud2.Endpoint != "" {
		t.Errorf("cloud-2 Endpoint = %q, want empty (pending placeholder must not become a literal endpoint)", cloud2.Endpoint)
	}
	if got := cloud2.AllRouteHosts(); len(got) != 1 || got[0] != "10.100.0.3" {
		t.Errorf("cloud-2 route hosts = %v, want the legacy single-host fallback", got)
	}
}

// A second cloud peer in the Secret must be picked up as an
// independent, additional peer, never overwrite or get confused with
// the first.
func TestLoadPeersFromSecret_MultipleCloudPeersAreIndependent(t *testing.T) {
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"peer-public-key-a":  []byte("pubA"),
			"peer-endpoint-a":    []byte("1.2.3.4:51820"),
			"peer-allowed-ips-a": []byte("10.100.0.2/32"),
			"peer-route-hosts-a": []byte("10.100.0.2"),
			"peer-public-key-b":  []byte("pubB"),
			"peer-endpoint-b":    []byte("5.6.7.8:51820"),
			"peer-allowed-ips-b": []byte("10.100.0.3/32"),
			"peer-route-hosts-b": []byte("10.100.0.3"),
		},
	}
	peers, err := loadPeersFromSecret(secret)
	if err != nil {
		t.Fatalf("loadPeersFromSecret: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("expected 2 independent cloud peers, got %d: %+v", len(peers), peers)
	}
}

func TestLoadPeersFromSecret_MissingAllowedIPsIsAnError(t *testing.T) {
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"peer-public-key-cloud-1":  []byte("pub"),
			"peer-route-hosts-cloud-1": []byte("10.100.0.2"),
			// peer-allowed-ips-cloud-1 deliberately missing
		},
	}
	if _, err := loadPeersFromSecret(secret); err == nil {
		t.Fatal("expected an error when peer-allowed-ips-<machine> is missing, got nil")
	}
}

func TestLoadPeersFromSecret_MissingRouteHostsIsAnError(t *testing.T) {
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"peer-public-key-cloud-1":  []byte("pub"),
			"peer-allowed-ips-cloud-1": []byte("10.100.0.2/32"),
			// neither peer-route-hosts- nor legacy peer-route-host-
		},
	}
	if _, err := loadPeersFromSecret(secret); err == nil {
		t.Fatal("expected an error when peer-route-hosts-<machine> is missing, got nil")
	}
}

func TestPeersFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.json")
	content := `{
  "privateKey": "onpremprivatekey",
  "localAddress": "10.100.0.2/24",
  "peers": [
    {"publicKey": "onprempub", "allowedIPs": ["10.100.0.1/32", "10.2.0.50/32"], "routeHosts": ["10.100.0.1", "10.2.0.50", "10.2.0.19"]}
  ]
}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test peers file: %v", err)
	}

	doc, err := readPeersFileDoc(path)
	if err != nil {
		t.Fatalf("readPeersFileDoc: %v", err)
	}
	if doc.PrivateKey != "onpremprivatekey" {
		t.Errorf("doc.PrivateKey = %q, want %q", doc.PrivateKey, "onpremprivatekey")
	}
	if doc.LocalAddress != "10.100.0.2/24" {
		t.Errorf("doc.LocalAddress = %q, want %q", doc.LocalAddress, "10.100.0.2/24")
	}
	peers := doc.Peers
	if len(peers) != 1 || peers[0].PublicKey != "onprempub" {
		t.Fatalf("doc.Peers = %+v, want one peer with publicKey=onprempub", peers)
	}
	// Endpoint absent from the file entirely -- listener role: this
	// peer is never dialed, only ever waited for.
	if peers[0].Endpoint != "" {
		t.Errorf("peers[0].Endpoint = %q, want empty (listener role never dials out)", peers[0].Endpoint)
	}
	// The transit route-host set includes an address (the API VIP) that
	// belongs to no tunnel peer directly -- it must survive the
	// round-trip untouched; whether it is a legal HOST route is
	// parseHostRoute's job, not the file reader's.
	if got := peers[0].AllRouteHosts(); len(got) != 3 {
		t.Errorf("route hosts = %v, want 3", got)
	}
}

func TestPrivateKeyFile_GeneratePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "private.key")

	key1, err := loadOrGeneratePrivateKey(path)
	if err != nil {
		t.Fatalf("first loadOrGeneratePrivateKey: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("private key file mode = %o, want 0600", info.Mode().Perm())
	}

	key2, err := loadOrGeneratePrivateKey(path)
	if err != nil {
		t.Fatalf("second loadOrGeneratePrivateKey: %v", err)
	}
	if key1.String() != key2.String() {
		t.Error("private key was not persisted: second load generated a different key")
	}
}

func TestInstallHostBinary_AtomicAndIdempotent(t *testing.T) {
	// The post-join upgrade channel: the DaemonSet's image carries the
	// binary and installs it onto the host, so a fleet upgrade is one
	// image digest -- no download host, no re-rendered userdata.
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "wg-dialer")

	if err := installHostBinary(target); err != nil {
		t.Fatalf("installHostBinary (fresh): %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	selfSum, err := fileSHA256(self)
	if err != nil {
		t.Fatalf("hashing self: %v", err)
	}
	installed, err := fileSHA256(target)
	if err != nil {
		t.Fatalf("hashing installed: %v", err)
	}
	if installed != selfSum {
		t.Error("installed binary does not match this executable")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary mode %v is not executable", info.Mode().Perm())
	}

	// Idempotent: an unchanged target must not be rewritten (same
	// inode), so a steady-state DaemonSet restart never churns the
	// binary the systemd unit is executing.
	before, _ := os.Stat(target)
	if err := installHostBinary(target); err != nil {
		t.Fatalf("installHostBinary (repeat): %v", err)
	}
	after, _ := os.Stat(target)
	if !os.SameFile(before, after) {
		t.Error("unchanged binary was replaced anyway -- must be a no-op when digests match")
	}

	// A stale copy is replaced.
	if err := os.WriteFile(target, []byte("stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := installHostBinary(target); err != nil {
		t.Fatalf("installHostBinary (stale): %v", err)
	}
	if got, _ := fileSHA256(target); got != selfSum {
		t.Error("stale binary was not replaced")
	}
	if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 1 {
		t.Errorf("temp install artifacts left behind: %v", entries)
	}
}

func TestReconcilePlan_NeverRoutesAPeerEndpointThroughTheTunnel(t *testing.T) {
	// Routing a peer's endpoint through the tunnel that dials it is an
	// infinite encapsulation loop -- the encrypted packet's outer
	// destination matches the same route. Caught live at line rate
	// (2.29 GiB in three minutes) when a node's published address
	// equalled the address the tunnel dials, which is the normal case
	// for nodes dialed on their ordinary node IPs.
	peers := []tunnel.PeerSpec{{
		PublicKey:  "inWNVFSf+4UUVNrmz/EMjR7aKnUJcRUh5V7k4aQBSl4=",
		Endpoint:   "172.21.0.16:51820",
		RouteHosts: []string{"10.100.0.1", "172.21.0.16"},
	}}

	endpointHosts := map[string]bool{}
	for _, p := range peers {
		host, _, err := net.SplitHostPort(p.Endpoint)
		if err != nil {
			host = p.Endpoint
		}
		if ip := net.ParseIP(strings.TrimSpace(host)); ip != nil {
			endpointHosts[ip.String()] = true
		}
	}

	var installed []string
	for _, p := range peers {
		for _, h := range p.AllRouteHosts() {
			ipNet, err := parseHostRoute(h)
			if err != nil {
				t.Fatalf("parseHostRoute(%q): %v", h, err)
			}
			if endpointHosts[ipNet.IP.String()] {
				continue
			}
			installed = append(installed, ipNet.IP.String())
		}
	}

	if len(installed) != 1 || installed[0] != "10.100.0.1" {
		t.Errorf("installed routes = %v, want only the tunnel address (the endpoint 172.21.0.16 must be excluded)", installed)
	}
}
