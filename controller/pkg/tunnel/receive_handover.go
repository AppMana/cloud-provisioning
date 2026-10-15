package tunnel

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// ReceivePath authorizes ingress on an independent WireGuard device. It has no
// route fields: preparing a receiver must not move an existing egress path.
// Private identities and listen ports are bound separately by the runtime.
type ReceivePath struct {
	Interface      string   `json:"interface"`
	PublicKey      string   `json:"publicKey"`
	AllowedSources []string `json:"allowedSources"`
}

// ReceiveHandover is a preparation plan, not an applied-state receipt. Applying
// Desired still requires native receiver readiness; retiring previous receive
// permissions requires both remote and site forwarding acknowledgments. Neither
// transition is authorized merely by constructing this value.
type ReceiveHandover struct {
	Receivers []ReceivePath `json:"receivers"`
	Desired   PeerListDoc   `json:"desired"`
}

// PlanReceiveHandover keeps old ingress permissions during an asynchronous route
// transition. Each site's public identity has its own receiver, so overlapping
// source permissions during transfer do not compete in one WireGuard trie.
// Cloud peers remain in Desired unchanged and never become site receivers.
// native contains protected cloud-native prefixes, not site pod/service ranges.
func PlanReceiveHandover(meshID string, previous, desired PeerListDoc, native []netip.Prefix) (ReceiveHandover, error) {
	if strings.TrimSpace(meshID) == "" {
		return ReceiveHandover{}, fmt.Errorf("mesh identity required")
	}
	for _, p := range native {
		if !p.IsValid() || p != p.Masked() || p.Bits() == 0 || p.Addr().Is4In6() {
			return ReceiveHandover{}, fmt.Errorf("invalid native prefix boundary")
		}
	}
	if previous.Peers == nil || desired.Peers == nil {
		return ReceiveHandover{}, fmt.Errorf("explicit previous and desired peer lists required")
	}
	permissions := map[string]map[string]bool{}
	roles := map[string]bool{}
	type owner struct {
		prefix netip.Prefix
		key    string
		remote bool
	}
	var overlapOwners []owner
	for _, doc := range []PeerListDoc{previous, desired} {
		seen := map[string]bool{}
		var owners []owner
		for _, peer := range doc.Peers {
			if !validReceivePublicKey(peer.PublicKey) {
				return ReceiveHandover{}, fmt.Errorf("invalid peer identity")
			}
			if seen[peer.PublicKey] {
				return ReceiveHandover{}, fmt.Errorf("duplicate peer identity")
			}
			seen[peer.PublicKey] = true
			if remote, ok := roles[peer.PublicKey]; ok && remote != peer.Remote {
				return ReceiveHandover{}, fmt.Errorf("peer changed site/cloud role")
			}
			roles[peer.PublicKey] = peer.Remote
			if !peer.Remote && permissions[peer.PublicKey] == nil {
				permissions[peer.PublicKey] = map[string]bool{}
			}
			for _, raw := range peer.WGAllowedIPs {
				prefix, err := netip.ParsePrefix(raw)
				if err != nil || prefix != prefix.Masked() || prefix.Bits() == 0 || prefix.Addr().Is4In6() || !prefix.Addr().IsGlobalUnicast() {
					return ReceiveHandover{}, fmt.Errorf("invalid peer source prefix %q", raw)
				}
				for _, boundary := range native {
					if !peer.Remote && prefix.Overlaps(boundary) {
						return ReceiveHandover{}, fmt.Errorf("site receiver overlaps a protected native prefix")
					}
				}
				for _, prior := range owners {
					if prior.key != peer.PublicKey && prefix.Overlaps(prior.prefix) {
						return ReceiveHandover{}, fmt.Errorf("ambiguous source ownership within one generation")
					}
				}
				// Old receivers remain active while the desired generation is
				// prepared. That overlap must not transfer authority between
				// site and cloud peers, even across document generations.
				for _, prior := range overlapOwners {
					if prior.remote != peer.Remote && prefix.Overlaps(prior.prefix) {
						return ReceiveHandover{}, fmt.Errorf("source ownership crosses site/cloud boundary during handover")
					}
				}
				entry := owner{prefix, peer.PublicKey, peer.Remote}
				owners = append(owners, entry)
				overlapOwners = append(overlapOwners, entry)
				if !peer.Remote {
					permissions[peer.PublicKey][prefix.String()] = true
				}
			}
		}
	}
	result := ReceiveHandover{Desired: PeerListDoc{Peers: make([]PeerSpec, len(desired.Peers)), APIServers: slices.Clone(desired.APIServers)}}
	for i, p := range desired.Peers {
		p.WGAllowedIPs = slices.Clone(p.WGAllowedIPs)
		p.RouteHosts = slices.Clone(p.RouteHosts)
		p.Transit = slices.Clone(p.Transit)
		p.TransitHosts = slices.Clone(p.TransitHosts)
		result.Desired.Peers[i] = p
	}
	interfaces := map[string]bool{}
	for key, sources := range permissions {
		sum := sha256.Sum256([]byte(meshID + "\x00" + key))
		name := fmt.Sprintf("cldr%x", sum[:5])
		if interfaces[name] {
			return ReceiveHandover{}, fmt.Errorf("receiver interface identity collision")
		}
		interfaces[name] = true
		path := ReceivePath{Interface: name, PublicKey: key, AllowedSources: make([]string, 0, len(sources))}
		for prefix := range sources {
			path.AllowedSources = append(path.AllowedSources, prefix)
		}
		slices.Sort(path.AllowedSources)
		result.Receivers = append(result.Receivers, path)
	}
	slices.SortFunc(result.Receivers, func(a, b ReceivePath) int { return strings.Compare(a.PublicKey, b.PublicKey) })
	return result, nil
}

func validReceivePublicKey(value string) bool {
	key, err := base64.StdEncoding.DecodeString(value)
	return err == nil && len(key) == 32 && !bytes.Equal(key, make([]byte, 32)) && base64.StdEncoding.EncodeToString(key) == value
}
