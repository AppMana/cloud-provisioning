package tunnel

import (
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
)

// ReceiveTransitionPlan is the public configuration persisted with a handover
// round. Boot IDs and receipt timestamps are observations, not configuration;
// each phase must independently revalidate them before acting.
type ReceiveTransitionPlan struct {
	Namespace          string                           `json:"namespace"`
	MeshName           string                           `json:"meshName"`
	MachineName        string                           `json:"machineName"`
	AdoptionName       string                           `json:"adoptionName"`
	CloudNodeName      string                           `json:"cloudNodeName"`
	Version            int                              `json:"version"`
	ReceiverGeneration string                           `json:"receiverGeneration,omitempty"`
	MeshUID            string                           `json:"meshUID"`
	MachineUID         string                           `json:"machineUID"`
	AdoptionUID        string                           `json:"adoptionUID"`
	CloudNodeUID       string                           `json:"cloudNodeUID"`
	RootPublicKey      string                           `json:"rootPublicKey"`
	CloudAddresses     []string                         `json:"cloudAddresses,omitempty"`
	Base               []byte                           `json:"base"`
	Desired            PeerListDoc                      `json:"desired"`
	SiteDesired        PeerListDoc                      `json:"siteDesired"`
	SiteBase           *PeerListDoc                     `json:"siteBase,omitempty"`
	Native             []netip.Prefix                   `json:"native"`
	Ports              map[string]int                   `json:"ports"`
	Bindings           map[string]ReceiveBinding        `json:"bindings"`
	Sites              map[string]ReceiveTransitionSite `json:"sites"`
}

// ReceiveScope separates receiver devices, aliases and derived keys across
// generations without changing mesh ownership or stable tunnel addresses.
// The generation is committed intent; it must never be minted by an agent.
func (p ReceiveTransitionPlan) ReceiveScope() (string, error) {
	if p.MeshUID == "" || strings.TrimSpace(p.MeshUID) != p.MeshUID || strings.ContainsRune(p.MeshUID, '\x00') {
		return "", fmt.Errorf("invalid receiver mesh identity")
	}
	if p.ReceiverGeneration == "" {
		return p.MeshUID, nil
	}
	raw, err := hex.DecodeString(p.ReceiverGeneration)
	if err != nil || len(raw) != 32 || hex.EncodeToString(raw) != p.ReceiverGeneration {
		return "", fmt.Errorf("invalid receiver generation identity")
	}
	return p.MeshUID + "\x00generation\x00" + p.ReceiverGeneration, nil
}

// PreviousSitePeers preserves old journals, which committed only one site
// document. New transitions can bind old permissions independently of desired
// allocations. The returned document is read-only input to planners.
func (p ReceiveTransitionPlan) PreviousSitePeers() PeerListDoc {
	if p.SiteBase != nil {
		return *p.SiteBase
	}
	return p.SiteDesired
}

type ReceiveTransitionSite struct {
	NodeName     string             `json:"nodeName"`
	NodeUID      string             `json:"nodeUID"`
	RouteSources []string           `json:"routeSources,omitempty"`
	Client       SiteReceiveClient  `json:"client"`
	Legacy       *ReceiveSiteLegacy `json:"legacy,omitempty"`
	Forward      *ReceiveSitePath   `json:"forward,omitempty"`
}
