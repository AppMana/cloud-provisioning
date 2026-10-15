package tunnel

import "net/netip"

const (
	ReceivePrepareKey = "receive-prepare.json"
	ReceiveReadyKey   = "receive-ready.json"
)

// ReceivePrepare requests additive preparation while peers.json remains the
// active forwarding document. BaseHash identifies those exact document bytes.
// It does not authorize applying Desired or withdrawing existing permissions.
type ReceivePrepare struct {
	Version       int            `json:"version"`
	MeshUID       string         `json:"meshUID"`
	NodeName      string         `json:"nodeName"`
	NodeUID       string         `json:"nodeUID"`
	RootPublicKey string         `json:"rootPublicKey"`
	BaseHash      string         `json:"baseHash"`
	Desired       PeerListDoc    `json:"desired"`
	Native        []netip.Prefix `json:"native"`
}

// ReceiveBinding contains public connection details read back from the kernel.
type ReceiveBinding struct {
	Interface     string `json:"interface"`
	PeerPublicKey string `json:"peerPublicKey"`
	PublicKey     string `json:"publicKey"`
	ListenPort    int    `json:"listenPort"`
}

// ReceiveReady is preparation evidence only. AppliedListAnnotation and site
// forwarding receipts remain separate; this value cannot authorize retirement.
type ReceiveReady struct {
	Version       int              `json:"version"`
	MeshUID       string           `json:"meshUID"`
	SecretUID     string           `json:"secretUID"`
	NodeName      string           `json:"nodeName"`
	NodeUID       string           `json:"nodeUID"`
	RootPublicKey string           `json:"rootPublicKey"`
	BaseHash      string           `json:"baseHash"`
	RequestHash   string           `json:"requestHash"`
	Bindings      []ReceiveBinding `json:"bindings"`
}
