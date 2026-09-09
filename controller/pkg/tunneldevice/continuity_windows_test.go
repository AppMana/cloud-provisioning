package tunneldevice

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"golang.zx2c4.com/wireguard/windows/driver"
)

// Opt-in native test: owns two temporary adapters, uses loopback UDP, and never
// assigns IP addresses or routes. Existing workload adapters are not opened.
func TestNativePeerHandshakeContinuity(t *testing.T) {
	if os.Getenv("CLDT_NATIVE_PEER_CONTINUITY") != "1" {
		t.Skip("requires Windows VM administrator and signed WireGuardNT DLL")
	}
	makeAdapter := func() *driver.Adapter {
		var nonce [4]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			t.Fatal(err)
		}
		name := "cldt" + hex.EncodeToString(nonce[:])
		a, err := driver.CreateAdapter(name, "WireGuard", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := a.Close(); err != nil {
				t.Error(err)
			}
		})
		t.Log("owned adapter", name)
		return a
	}
	port := func() uint16 {
		c, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if e != nil {
			t.Fatal(e)
		}
		p := uint16(c.LocalAddr().(*net.UDPAddr).Port)
		c.Close()
		return p
	}
	a, b := makeAdapter(), makeAdapter()
	ap, bp := port(), port()
	for bp == ap {
		bp = port()
	}
	ak, e := wgtypes.GeneratePrivateKey()
	if e != nil {
		t.Fatal(e)
	}
	bk, e := wgtypes.GeneratePrivateKey()
	if e != nil {
		t.Fatal(e)
	}
	pa := Plan{Key: ak, Peers: []Peer{{Key: bk.PublicKey(), Endpoint: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), bp), Allowed: []netip.Prefix{netip.MustParsePrefix("192.0.2.2/32")}}}}
	pb := Plan{Key: bk, Peers: []Peer{{Key: ak.PublicKey(), Endpoint: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), ap), Allowed: []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32")}}}}
	apply := func(a *driver.Adapter, p Plan, port uint16) {
		old, err := a.Configuration()
		if err != nil {
			t.Fatal(err)
		}
		var keys [][32]byte
		q := old.FirstPeer()
		for i := uint32(0); i < old.PeerCount; i++ {
			keys = append(keys, q.PublicKey)
			q = q.NextPeer()
		}
		cfg, size, err := buildPeerUpdate(p, port, keys)
		if err != nil {
			t.Fatal(err)
		}
		if err = a.SetConfiguration(cfg, size); err != nil {
			t.Fatal(err)
		}
		if err = a.SetAdapterState(driver.AdapterStateUp); err != nil {
			t.Fatal(err)
		}
	}
	read := func(a *driver.Adapter, key [32]byte) (driver.Peer, uint32) {
		cfg, err := a.Configuration()
		if err != nil {
			t.Fatal(err)
		}
		q := cfg.FirstPeer()
		for i := uint32(0); i < cfg.PeerCount; i++ {
			if q.PublicKey == key {
				return *q, cfg.PeerCount
			}
			q = q.NextPeer()
		}
		return driver.Peer{}, cfg.PeerCount
	}
	apply(b, pb, bp)
	apply(a, pa, ap)
	deadline := time.Now().Add(45 * time.Second)
	var baseline driver.Peer
	for time.Now().Before(deadline) {
		baseline, _ = read(a, bk.PublicKey())
		if baseline.LastHandshake != 0 && baseline.TxBytes > 0 && baseline.RxBytes > 0 {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if baseline.LastHandshake == 0 || baseline.TxBytes == 0 || baseline.RxBytes == 0 {
		t.Fatal("no real loopback handshake and bidirectional counters")
	}
	remoteBaseline, _ := read(b, ak.PublicKey())
	extra, e := wgtypes.GeneratePrivateKey()
	if e != nil {
		t.Fatal(e)
	}
	pa.Peers = append(pa.Peers, Peer{Key: extra.PublicKey(), Allowed: []netip.Prefix{netip.MustParsePrefix("192.0.2.3/32")}})
	for _, stage := range []string{"add", "remove", "unchanged"} {
		if stage == "remove" {
			pa.Peers = pa.Peers[:1]
		}
		apply(a, pa, ap)
		after, count := read(a, bk.PublicKey())
		if count != uint32(len(pa.Peers)) || after.LastHandshake != baseline.LastHandshake || after.TxBytes < baseline.TxBytes || after.RxBytes < baseline.RxBytes {
			t.Fatalf("%s changed survivor session or wrong peer membership", stage)
		}
		t.Logf("%s: peers=%d handshake preserved, tx=%d rx=%d", stage, count, after.TxBytes, after.RxBytes)
	}
	// A preserved timestamp alone is insufficient: require an authenticated
	// keepalive to traverse the surviving session after the membership updates.
	deadline = time.Now().Add(35 * time.Second)
	delivered := false
	for time.Now().Before(deadline) {
		local, _ := read(a, bk.PublicKey())
		remote, _ := read(b, ak.PublicKey())
		if local.LastHandshake != baseline.LastHandshake || remote.LastHandshake != remoteBaseline.LastHandshake {
			t.Fatal("session re-handshook after membership updates")
		}
		if (local.TxBytes > baseline.TxBytes && remote.RxBytes > remoteBaseline.RxBytes) ||
			(remote.TxBytes > remoteBaseline.TxBytes && local.RxBytes > baseline.RxBytes) {
			delivered = true
			t.Log("authenticated keepalive delivered after membership updates without a new handshake")
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !delivered {
		t.Fatal("no authenticated keepalive delivered after membership updates")
	}
	pa.Peers = nil
	apply(a, pa, ap)
	_, count := read(a, bk.PublicKey())
	if count != 0 {
		t.Fatal("retired final peer remains")
	}
}
