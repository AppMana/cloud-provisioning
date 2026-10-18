package tunnel

import (
	"encoding/json"
	"strings"
	"testing"
)

// Pure planning coverage. Independent device names do not prove simultaneous
// listeners, authenticated traffic, native retirement or rollback continuity.
func TestReceiveGenerationsKeepIndependentDevices(t *testing.T) {
	seen := map[string]bool{}
	for _, generation := range []string{"", strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		p := routeTransitionFixture(t)
		p.ReceiverGeneration = generation
		scope, err := p.ReceiveScope()
		if err != nil {
			t.Fatal(err)
		}
		if generation == "" && scope != p.MeshUID {
			t.Fatal("legacy derivation changed")
		}
		var base PeerListDoc
		if err := json.Unmarshal(p.Base, &base); err != nil {
			t.Fatal(err)
		}
		paths, err := PlanReceiveHandover(scope, base, p.Desired, p.Native)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths.Receivers {
			binding := p.Bindings[path.PublicKey]
			binding.Interface = path.Interface
			p.Bindings[path.PublicKey] = binding
			site := p.Sites[path.PublicKey]
			site.Client, err = PlanSiteReceiveClient(scope, path.PublicKey, p.RootPublicKey, binding, p.PreviousSitePeers(), p.SiteDesired)
			if err != nil {
				t.Fatal(err)
			}
			p.Sites[path.PublicKey] = site
		}
		routes, err := PlanReceiveRoutes(p)
		if err != nil {
			t.Fatal(err)
		}
		current := map[string]bool{}
		for _, byPrefix := range routes {
			for _, selection := range byPrefix {
				for _, identity := range []string{selection.Interface, selection.Alias} {
					if seen[identity] {
						t.Fatal("receiver generation reused serving device identity")
					}
					current[identity] = true
				}
			}
		}
		for identity := range current {
			seen[identity] = true
		}
		if generation != "" {
			p.ReceiverGeneration = ""
			if _, err := PlanReceiveRoutes(p); err == nil {
				t.Fatal("accepted receiver bindings from another generation")
			}
		}
	}
	for _, invalid := range []string{"x", strings.Repeat("A", 64), strings.Repeat("0", 62)} {
		p := routeTransitionFixture(t)
		p.ReceiverGeneration = invalid
		if _, err := PlanReceiveRoutes(p); err == nil {
			t.Fatal("accepted invalid generation identity")
		}
	}
}
