package tunnel

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"time"
)

const (
	ReceivePrepareKey = "receive-prepare.json"
	ReceiveReadyKey   = "receive-ready.json"
	ReceivePortsKey   = "receive-ports.json"
)

// ReceivePrepare requests additive preparation while peers.json remains the
// active forwarding document. BaseHash identifies those exact document bytes.
// It does not authorize applying Desired or withdrawing existing permissions.
type ReceivePrepare struct {
	Predecessor        *ReceivePreparePredecessor `json:"predecessor,omitempty"`
	Version            int                        `json:"version"`
	ReceiverGeneration string                     `json:"receiverGeneration,omitempty"`
	MeshUID            string                     `json:"meshUID"`
	NodeName           string                     `json:"nodeName"`
	BootID             string                     `json:"bootID"`
	NodeUID            string                     `json:"nodeUID"`
	RootPublicKey      string                     `json:"rootPublicKey"`
	BaseHash           string                     `json:"baseHash"`
	Desired            PeerListDoc                `json:"desired"`
	Native             []netip.Prefix             `json:"native"`
	Ports              map[string]int             `json:"ports"`
}

func (r ReceivePrepare) ReceiveScope() (string, error) {
	return (ReceiveTransitionPlan{MeshUID: r.MeshUID, ReceiverGeneration: r.ReceiverGeneration}).ReceiveScope()
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
	BootID        string           `json:"bootID"`
	NodeUID       string           `json:"nodeUID"`
	RootPublicKey string           `json:"rootPublicKey"`
	BaseHash      string           `json:"baseHash"`
	RequestHash   string           `json:"requestHash"`
	Bindings      []ReceiveBinding `json:"bindings"`
}

// ReceiveTarget is observed identity supplied by the controller, not copied
// from a receipt. BootID must come from the current Node status; Ready excludes
// terminating or non-ready nodes. A valid result only proves preparation.
type ReceiveTarget struct {
	MeshUID       string
	SecretUID     string
	NodeName      string
	NodeUID       string
	BootID        string
	RootPublicKey string
	Ready         bool
}

// ValidateReceiveReadiness checks an exact request against the current target
// and public kernel bindings. It neither selects egress nor releases retention.
func ValidateReceiveReadiness(target ReceiveTarget, baseRaw, requestRaw, receiptRaw []byte) (map[string]ReceiveBinding, error) {
	if !target.Ready || target.MeshUID == "" || target.SecretUID == "" || target.NodeName == "" || target.NodeUID == "" || target.BootID == "" || !validReceivePublicKey(target.RootPublicKey) {
		return nil, fmt.Errorf("receive readiness requires a current ready target identity")
	}
	decode := func(raw []byte, value any) error {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(value); err != nil {
			return err
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return fmt.Errorf("trailing receive protocol input")
		}
		return nil
	}
	var request ReceivePrepare
	var receipt ReceiveReady
	if err := decode(requestRaw, &request); err != nil {
		return nil, fmt.Errorf("invalid receive request")
	}
	if err := decode(receiptRaw, &receipt); err != nil {
		return nil, fmt.Errorf("invalid receive readiness")
	}
	if request.Version != 1 || request.MeshUID != target.MeshUID || request.NodeName != target.NodeName || request.NodeUID != target.NodeUID || request.BootID != target.BootID || request.RootPublicKey != target.RootPublicKey || request.Native == nil || request.BaseHash != HashPeerList(baseRaw) {
		return nil, fmt.Errorf("receive request does not match the current target or forwarding base")
	}
	if receipt.Version != 1 || receipt.MeshUID != target.MeshUID || receipt.SecretUID != target.SecretUID || receipt.NodeName != target.NodeName || receipt.NodeUID != target.NodeUID || receipt.BootID != target.BootID || receipt.RootPublicKey != target.RootPublicKey || receipt.BaseHash != request.BaseHash || receipt.RequestHash != HashPeerList(requestRaw) {
		return nil, fmt.Errorf("receive readiness does not match the current target and request")
	}
	var previous PeerListDoc
	if err := json.Unmarshal(baseRaw, &previous); err != nil {
		return nil, fmt.Errorf("invalid receive forwarding base")
	}
	receiveScope, err := request.ReceiveScope()
	if err != nil {
		return nil, err
	}
	plan, err := PlanReceiveHandover(receiveScope, previous, request.Desired, request.Native)
	if err != nil {
		return nil, err
	}
	if err := ValidateReceivePorts(plan.Receivers, request.Ports); err != nil {
		return nil, err
	}
	if err := ValidateReceiveBindings(plan.Receivers, target.RootPublicKey, receipt.Bindings); err != nil {
		return nil, err
	}
	result := make(map[string]ReceiveBinding, len(receipt.Bindings))
	for _, binding := range receipt.Bindings {
		if binding.ListenPort != request.Ports[binding.PeerPublicKey] {
			return nil, fmt.Errorf("receiver listen port differs from the requested transport")
		}
		result[binding.PeerPublicKey] = binding
	}
	return result, nil
}

// ValidateReceiveBindings requires exactly one distinct kernel listener per
// planned peer. It rejects reuse of a root or site identity as a receiver key.
func ValidateReceiveBindings(paths []ReceivePath, root string, bindings []ReceiveBinding) error {
	if !validReceivePublicKey(root) || len(bindings) != len(paths) {
		return fmt.Errorf("incomplete receiver preparation")
	}
	expected := map[string]string{}
	reserved := map[string]bool{root: true}
	for _, path := range paths {
		expected[path.PublicKey] = path.Interface
		reserved[path.PublicKey] = true
	}
	seenPeers := map[string]bool{}
	seenKeys := map[string]bool{}
	seenPorts := map[int]bool{}
	for _, binding := range bindings {
		iface, found := expected[binding.PeerPublicKey]
		if !found || iface != binding.Interface || seenPeers[binding.PeerPublicKey] {
			return fmt.Errorf("prepared receiver identity mismatch")
		}
		if !validReceivePublicKey(binding.PublicKey) || reserved[binding.PublicKey] || seenKeys[binding.PublicKey] || binding.ListenPort <= 0 || binding.ListenPort > 65535 || seenPorts[binding.ListenPort] {
			return fmt.Errorf("invalid prepared receiver binding")
		}
		seenPeers[binding.PeerPublicKey] = true
		seenKeys[binding.PublicKey] = true
		seenPorts[binding.ListenPort] = true
	}
	return nil
}

// ReceivePortReservations are durable until acknowledged receiver retirement.
// They are scoped to the mesh, node and root identity rather than to one boot.
type ReceivePortReservations struct {
	Version       int                       `json:"version"`
	MeshUID       string                    `json:"meshUID"`
	NodeUID       string                    `json:"nodeUID"`
	RootPublicKey string                    `json:"rootPublicKey"`
	Ports         map[string]int            `json:"ports"`
	Generations   map[string]map[string]int `json:"generations,omitempty"`
}

// SiteReceiveReady records authenticated transport preparation only. It does
// not acknowledge forwarding, authorize route changes, or release retention.
type SiteReceiveReady struct {
	Version     int               `json:"version"`
	MeshUID     string            `json:"meshUID"`
	SecretUID   string            `json:"secretUID"`
	RequestHash string            `json:"requestHash"`
	SiteHash    string            `json:"siteHash"`
	NodeName    string            `json:"nodeName"`
	NodeUID     string            `json:"nodeUID"`
	BootID      string            `json:"bootID"`
	Client      SiteReceiveClient `json:"client"`
	HandshakeAt time.Time         `json:"handshakeAt"`
}

func SiteReceiveReadyKey(publicKey string) string {
	return fmt.Sprintf("receive-site-ready-%x", sha256.Sum256([]byte(publicKey)))
}
