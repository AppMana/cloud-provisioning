package tunnelhost

import (
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"os"
	"reflect"
	"testing"
)

func TestAPIBackendsFollowDurablePublicState(t *testing.T) {
	s, _ := fixture(t)
	s.Identity.APIServers = []string{"10.0.0.1:6443", "[fd00::1]:6443"}
	backends, err := APIBackends(s.Identity, s.CachePath)
	if err != nil || !reflect.DeepEqual(backends, s.Identity.APIServers) {
		t.Fatal("bootstrap API endpoints unavailable")
	}
	write(t, s.CachePath, tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{}, APIServers: []string{"10.0.0.2:6443"}})
	backends, err = APIBackends(s.Identity, s.CachePath)
	if err != nil || !reflect.DeepEqual(backends, []string{"10.0.0.2:6443"}) {
		t.Fatal("durable API update lost")
	}
	write(t, s.CachePath, tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{}})
	backends, err = APIBackends(s.Identity, s.CachePath)
	if err != nil || !reflect.DeepEqual(backends, s.Identity.APIServers) {
		t.Fatal("legacy public state lost bootstrap API endpoints")
	}
	if err = os.WriteFile(s.CachePath, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = APIBackends(s.Identity, s.CachePath); err == nil {
		t.Fatal("corrupt public state ignored")
	}
}

func TestInvalidAPIUpdateCannotBecomeDurable(t *testing.T) {
	for _, endpoint := range []string{"dns.invalid:6443", "10.0.0.1:0", "0.0.0.0:6443", "[::ffff:10.0.0.1]:6443", "[ff02::1]:6443"} {
		t.Run(endpoint, func(t *testing.T) {
			s, d := fixture(t)
			if err := s.Start(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.CachePath)
			if err != nil {
				t.Fatal(err)
			}
			write(t, s.UpdatesPath, tunnel.PeerListDoc{Peers: s.Identity.Peers, APIServers: []string{endpoint}})
			if err = s.Reconcile(); err == nil {
				t.Fatal("invalid API endpoint accepted")
			}
			after, err := os.ReadFile(s.CachePath)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) || len(d.docs) != 1 {
				t.Fatal("invalid backend published")
			}
		})
	}
}
