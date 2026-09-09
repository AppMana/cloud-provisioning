package tunneldevice

import (
	"crypto/rand"
	"encoding/hex"
	"net/netip"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
)

// Exercise the real backend, including route drift repair, on a dedicated
// adapter. Exact test addresses must be unused before any mutation.
func TestNativeApplyLifecycle(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_PEER_CONTINUITY") != "1" {
		t.Skip("requires disposable Windows VM administrator")
	}
	local := netip.MustParsePrefix("192.0.2.220/32")
	remote := netip.MustParsePrefix("192.0.2.221/32")
	routes, err := winipcfg.GetIPForwardTable2(windows.AF_INET)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range routes {
		p := r.DestinationPrefix.Prefix()
		if p == local || p == remote {
			t.Fatal("test address already has a route")
		}
	}
	var nonce [4]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "cldt" + hex.EncodeToString(nonce[:])
	n, err := New(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := n.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Log("owned adapter", name)
	doc := fixture(t)
	doc.LocalAddress = local.String()
	doc.Peers[0].Endpoint = "127.0.0.1:9"
	doc.Peers[0].WGAllowedIPs = []string{remote.String()}
	doc.Peers[0].RouteHosts = []string{remote.String()}
	if err = n.Apply(doc, 0); err != nil {
		t.Fatal(err)
	}
	if err = n.Verify(doc); err != nil {
		t.Fatal(err)
	}
	// Native generation experiments observed Apply reporting success while
	// the requested address was absent. The identity cache must not hide drift.
	if err = n.adapter.LUID().DeleteIPAddress(local); err != nil {
		t.Fatal(err)
	}
	if n.Verify(doc) == nil {
		t.Fatal("missing kernel identity address accepted")
	}
	if err = n.Apply(doc, 0); err != nil {
		t.Fatal("identity address repair failed", err)
	}
	// Reject duplicate identity before changing either generation's address.
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	otherName := "cldt" + hex.EncodeToString(nonce[:])
	other, err := New(otherName)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("owned adapter", otherName)
	t.Cleanup(func() {
		if err := other.Close(); err != nil {
			t.Error(err)
		}
	})
	if err = other.Apply(doc, 0); err == nil || !strings.Contains(err.Error(), "already assigned") {
		t.Fatal("duplicate address owner accepted")
	}
	if err = n.Verify(doc); err != nil {
		t.Fatal("rejecting duplicate ownership disrupted the existing adapter", err)
	}
	otherConfig, err := other.adapter.Configuration()
	if err != nil || otherConfig.PeerCount != 0 {
		t.Fatal("rejected address changed the new adapter's peers", err)
	}
	cfg, err := n.adapter.Configuration()
	if err != nil || cfg.PeerCount != 1 {
		t.Fatal("initial peer absent", err)
	}
	if err = n.adapter.LUID().DeleteRoute(remote, nextHop(remote)); err != nil {
		t.Fatal(err)
	}
	if n.Verify(doc) == nil {
		t.Fatal("missing kernel route accepted")
	}
	if err = n.Apply(doc, 0); err != nil {
		t.Fatal("route repair failed", err)
	}
	if err = n.Verify(doc); err != nil {
		t.Fatal(err)
	}
	invalid := doc
	invalid.LocalAddress = "192.0.2.222/32"
	if n.Apply(invalid, 0) == nil {
		t.Fatal("adapter address changed")
	}
	doc.Peers = nil
	if err = n.Apply(doc, 0); err != nil {
		t.Fatal(err)
	}
	cfg, err = n.adapter.Configuration()
	if err != nil || cfg.PeerCount != 0 {
		t.Fatal("retired peer remains", err)
	}
	actual, err := n.KernelRoutes()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range actual {
		if p == remote {
			t.Fatal("retired route remains")
		}
	}
	t.Log("native apply, route drift repair, immutable address, peer and route retirement passed")
}
