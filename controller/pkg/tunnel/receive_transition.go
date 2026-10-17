package tunnel

import "net/netip"

// ReceiveTransitionPlan is the public configuration persisted with a handover
// round. Boot IDs and receipt timestamps are observations, not configuration;
// each phase must independently revalidate them before acting.
type ReceiveTransitionPlan struct {
	Namespace      string                           `json:"namespace"`
	MeshName       string                           `json:"meshName"`
	MachineName    string                           `json:"machineName"`
	AdoptionName   string                           `json:"adoptionName"`
	CloudNodeName  string                           `json:"cloudNodeName"`
	Version        int                              `json:"version"`
	MeshUID        string                           `json:"meshUID"`
	MachineUID     string                           `json:"machineUID"`
	AdoptionUID    string                           `json:"adoptionUID"`
	CloudNodeUID   string                           `json:"cloudNodeUID"`
	RootPublicKey  string                           `json:"rootPublicKey"`
	CloudAddresses []string                         `json:"cloudAddresses,omitempty"`
	Base           []byte                           `json:"base"`
	Desired        PeerListDoc                      `json:"desired"`
	SiteDesired    PeerListDoc                      `json:"siteDesired"`
	Native         []netip.Prefix                   `json:"native"`
	Ports          map[string]int                   `json:"ports"`
	Bindings       map[string]ReceiveBinding        `json:"bindings"`
	Sites          map[string]ReceiveTransitionSite `json:"sites"`
}

type ReceiveTransitionSite struct {
	NodeName     string             `json:"nodeName"`
	NodeUID      string             `json:"nodeUID"`
	RouteSources []string           `json:"routeSources,omitempty"`
	Client       SiteReceiveClient  `json:"client"`
	Legacy       *ReceiveSiteLegacy `json:"legacy,omitempty"`
	Forward      *ReceiveSitePath   `json:"forward,omitempty"`
}
