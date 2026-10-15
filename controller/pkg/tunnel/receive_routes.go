package tunnel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"reflect"
)

// ReceiveRouteSelection identifies an already prepared device. It carries no
// permission to configure peers, expand source ranges, or retire old devices.
type ReceiveRouteSelection struct {
	Interface     string `json:"interface"`
	Alias         string `json:"alias"`
	PublicKey     string `json:"publicKey"`
	PeerPublicKey string `json:"peerPublicKey"`
}

// PlanReceiveRoutes derives desired egress by Node UID from the committed
// public plan. This is configuration, not authorization to execute a phase:
// callers still require a committed round, current identities and native proof.
func PlanReceiveRoutes(p ReceiveTransitionPlan) (map[string]map[string]ReceiveRouteSelection, error) {
	if p.Version != 1 || p.MeshUID == "" || p.CloudNodeUID == "" || p.Native == nil || p.SiteDesired.Peers == nil {
		return nil, fmt.Errorf("receiver routes require a complete transport plan")
	}
	var previous PeerListDoc
	if err := json.Unmarshal(p.Base, &previous); err != nil {
		return nil, err
	}
	preparation, err := PlanReceiveHandover(p.MeshUID, previous, p.Desired, p.Native)
	if err != nil {
		return nil, err
	}
	bindings := make([]ReceiveBinding, 0, len(p.Bindings))
	for key, binding := range p.Bindings {
		if key != binding.PeerPublicKey || binding.ListenPort != p.Ports[key] {
			return nil, fmt.Errorf("receiver binding differs from its reserved identity or port")
		}
		bindings = append(bindings, binding)
	}
	if err := ValidateReceivePorts(preparation.Receivers, p.Ports); err != nil {
		return nil, err
	}
	if err := ValidateReceiveBindings(preparation.Receivers, p.RootPublicKey, bindings); err != nil {
		return nil, err
	}
	cloudRoutes := map[string]ReceiveRouteSelection{}
	result := map[string]map[string]ReceiveRouteSelection{p.CloudNodeUID: cloudRoutes}
	sites := 0
	for _, peer := range p.Desired.Peers {
		if peer.Remote {
			continue // Native cloud-to-cloud paths do not become site tunnels.
		}
		site, found := p.Sites[peer.PublicKey]
		if !found || site.NodeUID == "" || site.NodeName == "" || result[site.NodeUID] != nil {
			return nil, fmt.Errorf("missing or duplicate receiver site identity")
		}
		binding := p.Bindings[peer.PublicKey]
		client, err := PlanSiteReceiveClient(p.MeshUID, peer.PublicKey, p.RootPublicKey, binding, p.SiteDesired, p.SiteDesired)
		if err != nil || !reflect.DeepEqual(client, site.Client) {
			return nil, fmt.Errorf("site receiver differs from the committed peer document: %v", err)
		}
		sum := sha256.Sum256([]byte(p.MeshUID + "\x00" + peer.PublicKey))
		selection := ReceiveRouteSelection{Interface: binding.Interface, Alias: fmt.Sprintf("cloud-provisioning:receive:v1:%x", sum), PublicKey: binding.PublicKey, PeerPublicKey: peer.PublicKey}
		if err := addReceiveRoutes(cloudRoutes, peer, selection); err != nil {
			return nil, err
		}
		sum = sha256.Sum256([]byte(p.MeshUID + "\x00" + peer.PublicKey + "\x00" + p.RootPublicKey))
		selection = ReceiveRouteSelection{Interface: client.Interface, Alias: fmt.Sprintf("cloud-provisioning:receive-client:v1:%x", sum), PublicKey: peer.PublicKey, PeerPublicKey: binding.PublicKey}
		routes := map[string]ReceiveRouteSelection{}
		for _, cloud := range p.SiteDesired.Peers {
			if cloud.PublicKey == p.RootPublicKey {
				if err := addReceiveRoutes(routes, cloud, selection); err != nil {
					return nil, err
				}
			}
		}
		result[site.NodeUID] = routes
		sites++
	}
	if sites == 0 || sites != len(p.Sites) {
		return nil, fmt.Errorf("receiver plan has missing or extra site paths")
	}
	return result, nil
}

func addReceiveRoutes(routes map[string]ReceiveRouteSelection, peer PeerSpec, selection ReceiveRouteSelection) error {
	var allowed []netip.Prefix
	for _, raw := range peer.WGAllowedIPs {
		// PlanReceiveHandover has validated these prefixes and their ownership.
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return err
		}
		allowed = append(allowed, prefix)
		routes[prefix.String()] = selection
	}
	for _, raw := range peer.AllRouteHosts() {
		prefix, err := netip.ParsePrefix(HostCIDR(raw))
		if err != nil || prefix.Bits() != prefix.Addr().BitLen() || prefix.Addr().Is4In6() || !prefix.Addr().IsGlobalUnicast() {
			return fmt.Errorf("receiver route host is not an explicit unicast host")
		}
		covered := false
		for _, source := range allowed {
			covered = covered || source.Contains(prefix.Addr())
		}
		if !covered {
			return fmt.Errorf("receiver route host is outside its peer's source permissions")
		}
		routes[prefix.String()] = selection
	}
	return nil
}
