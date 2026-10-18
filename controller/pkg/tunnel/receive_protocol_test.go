package tunnel

import (
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
)

// Protocol admission only; receipt decoding is not authenticated native
// completion, forwarding continuity or controller successor publication.
func TestReceiveReadinessBindsReceiverGeneration(t *testing.T) {
	peer := receiverTestKey(8)
	target := ReceiveTarget{MeshUID: "mesh", SecretUID: "secret", NodeName: "worker", NodeUID: "node", BootID: "boot", RootPublicKey: receiverTestKey(40), Ready: true}
	base := PeerListDoc{Peers: []PeerSpec{}}
	rawBase, _ := json.Marshal(base)
	r := ReceivePrepare{Version: 1, MeshUID: target.MeshUID, NodeName: target.NodeName, NodeUID: target.NodeUID, BootID: target.BootID, RootPublicKey: target.RootPublicKey, BaseHash: HashPeerList(rawBase), ReceiverGeneration: strings.Repeat("a", 64), Desired: PeerListDoc{Peers: []PeerSpec{{PublicKey: peer, WGAllowedIPs: []string{"10.90.0.1/32"}}}}, Native: []netip.Prefix{}, Ports: map[string]int{peer: 51823}}
	scope, err := r.ReceiveScope()
	if err != nil {
		t.Fatal(err)
	}
	paths, err := PlanReceiveHandover(scope, base, r.Desired, r.Native)
	if err != nil {
		t.Fatal(err)
	}
	rawRequest, _ := json.Marshal(r)
	receipt := ReceiveReady{Version: 1, MeshUID: target.MeshUID, SecretUID: target.SecretUID, NodeName: target.NodeName, NodeUID: target.NodeUID, BootID: target.BootID, RootPublicKey: target.RootPublicKey, BaseHash: r.BaseHash, RequestHash: HashPeerList(rawRequest), Bindings: []ReceiveBinding{{Interface: paths.Receivers[0].Interface, PeerPublicKey: peer, PublicKey: receiverTestKey(24), ListenPort: 51823}}}
	rawReceipt, _ := json.Marshal(receipt)
	if _, err := ValidateReceiveReadiness(target, rawBase, rawRequest, rawReceipt); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []string{"", strings.Repeat("b", 64), "invalid"} {
		r.ReceiverGeneration = generation
		rawRequest, _ = json.Marshal(r)
		receipt.RequestHash = HashPeerList(rawRequest)
		rawReceipt, _ = json.Marshal(receipt)
		if _, err := ValidateReceiveReadiness(target, rawBase, rawRequest, rawReceipt); err == nil {
			t.Fatal("accepted bindings for another or invalid generation")
		}
	}
}

func TestReceiveReadinessRequiresCurrentBootRequestAndBindings(t *testing.T) {
	for _, fault := range []string{"none", "not-ready", "target-boot", "receipt-boot", "request-boot", "secret", "node", "mesh", "root", "request-hash", "base", "unsafe-desired", "missing-native", "missing-binding", "extra-binding", "duplicate-port", "duplicate-key", "root-key", "site-key", "interface", "peer", "bad-port", "unknown-field", "trailing", "missing-ports", "wrong-port"} {
		t.Run(fault, func(t *testing.T) {
			target := ReceiveTarget{MeshUID: "mesh", SecretUID: "secret", NodeName: "worker", NodeUID: "node", BootID: "boot-1", RootPublicKey: receiverTestKey(40), Ready: true}
			previous := PeerListDoc{Peers: []PeerSpec{{PublicKey: receiverTestKey(8), WGAllowedIPs: []string{"10.90.0.10/32"}}}}
			desired := PeerListDoc{Peers: []PeerSpec{{PublicKey: receiverTestKey(16), WGAllowedIPs: []string{"10.90.0.10/32"}}}}
			baseRaw, _ := json.Marshal(previous)
			request := ReceivePrepare{Version: 1, MeshUID: target.MeshUID, NodeName: target.NodeName, NodeUID: target.NodeUID, BootID: target.BootID, RootPublicKey: target.RootPublicKey, BaseHash: HashPeerList(baseRaw), Desired: desired, Native: []netip.Prefix{}}
			plan, err := PlanReceiveHandover(target.MeshUID, previous, desired, request.Native)
			if err != nil {
				t.Fatal(err)
			}
			bindings := make([]ReceiveBinding, len(plan.Receivers))
			for i, path := range plan.Receivers {
				bindings[i] = ReceiveBinding{Interface: path.Interface, PeerPublicKey: path.PublicKey, PublicKey: receiverTestKey(byte(24 + 8*i)), ListenPort: 51820 + i}
			}
			request.Ports = map[string]int{}
			for _, binding := range bindings {
				request.Ports[binding.PeerPublicKey] = binding.ListenPort
			}
			receipt := ReceiveReady{Version: 1, MeshUID: target.MeshUID, SecretUID: target.SecretUID, NodeName: target.NodeName, NodeUID: target.NodeUID, BootID: target.BootID, RootPublicKey: target.RootPublicKey, BaseHash: request.BaseHash, Bindings: bindings}
			switch fault {
			case "missing-ports":
				request.Ports = nil
			case "wrong-port":
				request.Ports[receipt.Bindings[0].PeerPublicKey] = 51900
			case "not-ready":
				target.Ready = false
			case "target-boot":
				target.BootID = "boot-2"
			case "receipt-boot":
				receipt.BootID = "boot-0"
			case "request-boot":
				request.BootID = "boot-0"
			case "secret":
				target.SecretUID = "replacement"
			case "node":
				target.NodeUID = "replacement"
			case "mesh":
				target.MeshUID = "replacement"
			case "root":
				target.RootPublicKey = receiverTestKey(48)
			case "base":
				baseRaw = append(baseRaw, ' ')
			case "unsafe-desired":
				request.Desired.Peers[0].WGAllowedIPs = []string{"0.0.0.0/0"}
			case "missing-native":
				request.Native = nil
			case "missing-binding":
				receipt.Bindings = bindings[:1]
			case "extra-binding":
				receipt.Bindings = append(bindings, bindings[0])
			case "duplicate-port":
				receipt.Bindings[1].ListenPort = receipt.Bindings[0].ListenPort
			case "duplicate-key":
				receipt.Bindings[1].PublicKey = receipt.Bindings[0].PublicKey
			case "root-key":
				receipt.Bindings[0].PublicKey = target.RootPublicKey
			case "site-key":
				receipt.Bindings[0].PublicKey = receipt.Bindings[1].PeerPublicKey
			case "interface":
				receipt.Bindings[0].Interface = "other"
			case "peer":
				receipt.Bindings[0].PeerPublicKey = receiverTestKey(48)
			case "bad-port":
				receipt.Bindings[0].ListenPort = 65536
			}
			requestRaw, _ := json.Marshal(request)
			receipt.RequestHash = HashPeerList(requestRaw)
			if fault == "request-hash" {
				receipt.RequestHash = "stale"
			}
			receiptRaw, _ := json.Marshal(receipt)
			if fault == "unknown-field" {
				receiptRaw = append(receiptRaw[:len(receiptRaw)-1], []byte(`,"unknown":true}`)...)
			}
			if fault == "trailing" {
				receiptRaw = append(receiptRaw, []byte(` {}`)...)
			}
			got, err := ValidateReceiveReadiness(target, baseRaw, requestRaw, receiptRaw)
			if fault != "none" {
				if err == nil || got != nil {
					t.Fatal("stale or invalid readiness accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 {
				t.Fatal("validated readiness omitted receiver")
			}
			for _, binding := range receipt.Bindings {
				if got[binding.PeerPublicKey] != binding {
					t.Fatal("validated binding changed")
				}
			}
		})
	}
}
