package main

import (
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestHostCIDR(t *testing.T) {
	cases := map[string]string{
		"10.100.0.2":        "10.100.0.2/32",
		"fd8f:cf26:522a::1": "fd8f:cf26:522a::1/128",
		"10.100.0.2/32":     "10.100.0.2/32", // already has a prefix -- left alone
		"10.100.0.0/24":     "10.100.0.0/24", // never narrowed if already broader
	}
	for in, want := range cases {
		if got := hostCIDR(in); got != want {
			t.Errorf("hostCIDR(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitCIDRs(t *testing.T) {
	got := splitCIDRs("10.244.0.0/16, ", "10.96.0.0/12,fd00::/108", "")
	want := []string{"10.244.0.0/16", "10.96.0.0/12", "fd00::/108"}
	if len(got) != len(want) {
		t.Fatalf("splitCIDRs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitCIDRs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// This is the core regression test for the actual incident this
// redesign fixes: WGAllowedIPs (WireGuard's own cryptokey-routing
// accept-list) and RouteHost (the one thing installed as a real kernel
// route) must come out as genuinely different values -- WGAllowedIPs
// broad (cluster pod/service CIDRs plus this peer's own address),
// RouteHost always exactly one host address, never derived from
// WGAllowedIPs. Conflating these two was the root cause of the jarvis
// incident (a single --allowed-ips value fed both a WireGuard peer
// config and a literal kernel route installation loop).
func TestLoadPeersFromSecret_AllowListAndRouteHostAreIndependent(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "wg-dialer-peer", Namespace: "wg-dialer"},
		Data: map[string][]byte{
			"peer-public-key-cloud-1":   []byte("cloudpubkey1"),
			"peer-endpoint-cloud-1":     []byte("203.0.113.10:51820"),
			"peer-allowed-ips-cloud-1":  []byte("10.100.0.2/32"),
			"peer-route-host-cloud-1":   []byte("10.100.0.2"),
			"peer-public-key-cloud-2":   []byte("cloudpubkey2"),
			"peer-endpoint-cloud-2":     []byte("pending"),
			"peer-allowed-ips-cloud-2":  []byte("10.100.0.3/32"),
			"peer-route-host-cloud-2":   []byte("10.100.0.3"),
			"dialer-private-key-jarvis": []byte("unrelated"), // must NOT be treated as a peer
		},
	}

	peers, err := loadPeersFromSecret(secret)
	if err != nil {
		t.Fatalf("loadPeersFromSecret: %v", err)
	}
	if len(peers) != 2 {
		t.Fatalf("expected exactly 2 peers, got %d: %+v", len(peers), peers)
	}

	byPub := map[string]peerSpec{}
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
	if cloud1.RouteHost != "10.100.0.2" {
		t.Errorf("cloud-1 RouteHost = %q, want just the tunnel address", cloud1.RouteHost)
	}

	// "pending" (the endpoint-controller's own placeholder before it
	// learns the real external IP) must become an EMPTY Endpoint, not a
	// literal string WireGuard would try (and fail) to resolve as a
	// UDP address -- this is what lets the dialer keep running before
	// the real endpoint is known, same as before this redesign.
	cloud2, ok := byPub["cloudpubkey2"]
	if !ok {
		t.Fatal("cloud-2 peer not found")
	}
	if cloud2.Endpoint != "" {
		t.Errorf("cloud-2 Endpoint = %q, want empty (pending placeholder must not become a literal endpoint)", cloud2.Endpoint)
	}
}

// This is the multi-cloud-Machine regression test: a second cloud peer
// in the Secret must be picked up as an independent, additional peer,
// never overwrite or get confused with the first -- the actual
// architectural gap this redesign closes (the old flat
// peer-public-key/peer-endpoint keys supported exactly one cloud peer;
// a second Machine reconciling would have clobbered the first's entry
// before ever reaching this dialer).
func TestLoadPeersFromSecret_MultipleCloudPeersAreIndependent(t *testing.T) {
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"peer-public-key-a":  []byte("pubA"),
			"peer-endpoint-a":    []byte("1.2.3.4:51820"),
			"peer-allowed-ips-a": []byte("10.100.0.2/32"),
			"peer-route-host-a":  []byte("10.100.0.2"),
			"peer-public-key-b":  []byte("pubB"),
			"peer-endpoint-b":    []byte("5.6.7.8:51820"),
			"peer-allowed-ips-b": []byte("10.100.0.3/32"),
			"peer-route-host-b":  []byte("10.100.0.3"),
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
			"peer-public-key-cloud-1": []byte("pub"),
			"peer-route-host-cloud-1": []byte("10.100.0.2"),
			// peer-allowed-ips-cloud-1 deliberately missing
		},
	}
	if _, err := loadPeersFromSecret(secret); err == nil {
		t.Fatal("expected an error when peer-allowed-ips-<machine> is missing, got nil")
	}
}

func TestPeersFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.json")
	content := `{
  "privateKey": "onpremprivatekey",
  "localAddress": "10.100.0.2/24",
  "peers": [
    {"publicKey": "cloudpub", "allowedIPs": ["10.100.0.2/32"], "routeHost": "10.100.0.2"}
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
	if len(peers) != 1 || peers[0].PublicKey != "cloudpub" {
		t.Fatalf("doc.Peers = %+v, want one peer with publicKey=cloudpub", peers)
	}
	// Endpoint absent from the file entirely -- this is the listener
	// role: this peer is never dialed, only ever waited for.
	if peers[0].Endpoint != "" {
		t.Errorf("peers[0].Endpoint = %q, want empty (listener role never dials out)", peers[0].Endpoint)
	}
}
