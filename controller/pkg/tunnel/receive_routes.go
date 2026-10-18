package tunnel

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
)

// ReceiveRouteSelection identifies an already prepared device. It carries no
// permission to configure peers, expand source ranges, or retire old devices.
type ReceiveRouteSelection struct {
	Interface      string   `json:"interface"`
	Alias          string   `json:"alias"`
	PublicKey      string   `json:"publicKey"`
	PeerPublicKey  string   `json:"peerPublicKey"`
	AllowedSources []string `json:"allowedSources"`
}

// PlanReceiveRoutes derives receiver bindings by Node UID from the committed
// public plan. A site's Forward.Transit overrides workload egress; its receiver
// can still carry source exceptions. This is configuration, not phase authority:
// callers still require a committed round, current identities and native proof.
func PlanReceiveRoutes(p ReceiveTransitionPlan) (map[string]map[string]ReceiveRouteSelection, error) {
	receiveScope, err := p.ReceiveScope()
	if err != nil {
		return nil, err
	}
	if err := validateReceiveLocalNetwork(p); err != nil {
		return nil, err
	}
	if p.Version != 1 || p.MeshUID == "" || p.CloudNodeUID == "" || p.Native == nil || p.SiteDesired.Peers == nil {
		return nil, fmt.Errorf("receiver routes require a complete transport plan")
	}
	oldCloudFound := false
	for _, peer := range p.PreviousSitePeers().Peers {
		if peer.PublicKey == p.RootPublicKey {
			oldCloudFound = true
			if err := addReceiveRoutes(map[string]ReceiveRouteSelection{}, peer, ReceiveRouteSelection{}); err != nil {
				return nil, err
			}
		}
	}
	if !oldCloudFound {
		return nil, fmt.Errorf("previous site document lacks the cloud identity")
	}
	var previous PeerListDoc
	if err := json.Unmarshal(p.Base, &previous); err != nil {
		return nil, err
	}
	preparation, err := PlanReceiveHandover(receiveScope, previous, p.Desired, p.Native)
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
	hasForward := false
	for _, site := range p.Sites {
		hasForward = hasForward || site.Forward != nil
	}
	if hasForward {
		for _, site := range p.Sites {
			if site.Forward == nil {
				return nil, fmt.Errorf("desired site forwarding projection is incomplete")
			}
		}
	}
	cloudRoutes := map[string]ReceiveRouteSelection{}
	result := map[string]map[string]ReceiveRouteSelection{p.CloudNodeUID: cloudRoutes}
	desired := map[string]PeerSpec{}
	for _, peer := range p.Desired.Peers {
		if !peer.Remote {
			desired[peer.PublicKey] = peer
		}
	}
	// Retained sites can still send on the old path. They must participate in
	// switch/drain/retire even when they no longer own desired cloud egress.
	for _, path := range preparation.Receivers {
		site, found := p.Sites[path.PublicKey]
		if !found || site.NodeUID == "" || site.NodeName == "" || result[site.NodeUID] != nil {
			return nil, fmt.Errorf("missing or duplicate receiver site identity")
		}
		binding := p.Bindings[path.PublicKey]
		client, err := PlanSiteReceiveClient(receiveScope, path.PublicKey, p.RootPublicKey, binding, p.PreviousSitePeers(), p.SiteDesired)
		if err != nil || !reflect.DeepEqual(client, site.Client) {
			return nil, fmt.Errorf("site receiver differs from the committed peer document: %v", err)
		}
		sum := sha256.Sum256([]byte(receiveScope + "\x00" + path.PublicKey))
		selection := ReceiveRouteSelection{Interface: binding.Interface, Alias: fmt.Sprintf("cloud-provisioning:receive:v1:%x", sum), PublicKey: binding.PublicKey, PeerPublicKey: path.PublicKey, AllowedSources: slices.Clone(path.AllowedSources)}
		if peer, wanted := desired[path.PublicKey]; wanted {
			if err := addReceiveRoutes(cloudRoutes, peer, selection); err != nil {
				return nil, err
			}
		}
		sum = sha256.Sum256([]byte(receiveScope + "\x00" + path.PublicKey + "\x00" + p.RootPublicKey))
		selection = ReceiveRouteSelection{Interface: client.Interface, Alias: fmt.Sprintf("cloud-provisioning:receive-client:v1:%x", sum), PublicKey: path.PublicKey, PeerPublicKey: binding.PublicKey, AllowedSources: slices.Clone(client.AllowedSources)}
		routes := map[string]ReceiveRouteSelection{}
		for _, cloud := range p.SiteDesired.Peers {
			if cloud.PublicKey == p.RootPublicKey {
				if site.Legacy != nil {
					var oldCloud *PeerSpec
					for _, peer := range p.PreviousSitePeers().Peers {
						if peer.PublicKey == p.RootPublicKey {
							copy := peer
							oldCloud = &copy
						}
					}
					if oldCloud == nil {
						return nil, fmt.Errorf("legacy site cloud identity is missing")
					}
					if err := ValidateReceiveSiteLegacy(*site.Legacy, path.PublicKey, previous, *oldCloud); err != nil {
						return nil, err
					}
				}
				if err := ValidateReceiveSiteForward(site, path.PublicKey, p.Desired, cloud); err != nil {
					return nil, err
				}
				if err := addReceiveRoutes(routes, cloud, selection); err != nil {
					return nil, err
				}
			}
		}
		result[site.NodeUID] = routes
	}
	if len(preparation.Receivers) == 0 || len(preparation.Receivers) != len(p.Sites) {
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
