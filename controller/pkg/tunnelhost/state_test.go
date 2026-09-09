package tunnelhost

import (
	"encoding/json"
	"errors"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type observedDevice struct {
	docs []tunnel.PeersFileDoc
	err  error
}

func (d *observedDevice) Apply(doc tunnel.PeersFileDoc, _ uint16) error {
	d.docs = append(d.docs, doc)
	return d.err
}
func (d *observedDevice) Close() error { return nil }
func fixture(t *testing.T) (*State, *observedDevice) {
	t.Helper()
	key, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	peer, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dev := &observedDevice{}
	return &State{Device: dev, Port: 51820, UpdatesPath: filepath.Join(dir, "updates.json"), CachePath: filepath.Join(dir, "cache.json"), Identity: tunnel.PeersFileDoc{PrivateKey: key.String(), LocalAddress: "10.254.254.1/32", Peers: []tunnel.PeerSpec{{PublicKey: peer.PublicKey().String(), Endpoint: "192.0.2.2:51820", WGAllowedIPs: []string{"10.254.254.2/32"}, RouteHosts: []string{"10.254.254.2/32"}}}}}, dev
}
func write(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// Live 2022/2025 probes observed peer removal/re-addition changing TCP paths
// and kernel routes. The host must preserve that desired state across restart.
func TestRemovedPeersStayRemovedAcrossServiceRestart(t *testing.T) {
	s, d := fixture(t)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	write(t, s.UpdatesPath, tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{}})
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(d.docs[len(d.docs)-1].Peers) != 0 {
		t.Fatal("peer removal not applied")
	}
	if err := os.Remove(s.UpdatesPath); err != nil {
		t.Fatal(err)
	}
	restart := *s
	next := &observedDevice{}
	restart.Device = next
	if err := restart.Start(); err != nil {
		t.Fatal(err)
	}
	if len(next.docs[0].Peers) != 0 {
		t.Fatal("restart resurrected removed bootstrap peers")
	}
	write(t, s.UpdatesPath, tunnel.PeerListDoc{Peers: s.Identity.Peers})
	if err := restart.Reconcile(); err != nil {
		t.Fatal(err)
	}
	latest := next.docs[len(next.docs)-1]
	if len(latest.Peers) != 1 || latest.PrivateKey != s.Identity.PrivateKey || latest.LocalAddress != s.Identity.LocalAddress {
		t.Fatal("re-addition changed identity or lost peer")
	}
	cache, err := os.ReadFile(s.CachePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cache), s.Identity.PrivateKey) {
		t.Fatal("private key leaked into public cache")
	}
}
func TestInvalidUpdateDoesNotTouchDeviceOrDurableState(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"peers":null}`, `{"peers":[],"privateKey":"replacement"}`, `{"peers":[]} {}`, `{"peers":[{"publicKey":"bad"}]}`} {
		t.Run(raw, func(t *testing.T) {
			s, d := fixture(t)
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.CachePath)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(s.UpdatesPath, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if err = s.Reconcile(); err == nil {
				t.Fatal("invalid update accepted")
			}
			after, err := os.ReadFile(s.CachePath)
			if err != nil {
				t.Fatal(err)
			}
			if len(d.docs) != 1 || string(before) != string(after) {
				t.Fatal("invalid update altered state")
			}
		})
	}
}
func TestDeviceFailureRetainsDesiredUpdateForRetry(t *testing.T) {
	s, d := fixture(t)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	write(t, s.UpdatesPath, tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{}})
	d.err = errors.New("native route update failed")
	if err := s.Reconcile(); err == nil {
		t.Fatal("native failure lost")
	}
	if err := os.Remove(s.UpdatesPath); err != nil {
		t.Fatal(err)
	}
	d.err = nil
	if err := s.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if len(d.docs[len(d.docs)-1].Peers) != 0 {
		t.Fatal("retry reverted desired removal")
	}
}
func TestCorruptCacheCannotFallBackToBootstrap(t *testing.T) {
	s, d := fixture(t)
	if err := os.WriteFile(s.CachePath, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err == nil {
		t.Fatal("silently ignored corrupt durable state")
	}
	if len(d.docs) != 0 {
		t.Fatal("installed stale bootstrap peers")
	}
}
func TestPersistenceFailurePreventsApply(t *testing.T) {
	s, d := fixture(t)
	s.CachePath = filepath.Join(t.TempDir(), "missing", "cache.json")
	if err := s.Start(); err == nil {
		t.Fatal("missing parent unexpectedly writable")
	}
	if len(d.docs) != 0 {
		t.Fatal("installed update without durable state")
	}
}

func TestReadIdentityAndConfigurationErrors(t *testing.T) {
	s, _ := fixture(t)
	path := filepath.Join(t.TempDir(), "identity.json")
	if _, err := ReadIdentity(path); err == nil {
		t.Fatal("missing identity accepted")
	}
	write(t, path, s.Identity)
	got, err := ReadIdentity(path)
	if err != nil || got.PrivateKey != s.Identity.PrivateKey {
		t.Fatal("identity round trip failed")
	}
	if err = os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadIdentity(path); err == nil {
		t.Fatal("malformed identity accepted")
	}
	write(t, path, tunnel.PeersFileDoc{PrivateKey: "invalid", LocalAddress: "10.0.0.1/32"})
	if _, err = ReadIdentity(path); err == nil {
		t.Fatal("invalid identity key accepted")
	}
	s.Port = 0
	if err = s.Start(); err == nil {
		t.Fatal("zero UDP port accepted")
	}
}

func TestUnchangedPublicStateDoesNotResetLivePeers(t *testing.T) {
	s, d := fixture(t)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Reconcile(); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.docs) != 1 {
		t.Fatal("unchanged polling reconfigured live peers")
	}
}
