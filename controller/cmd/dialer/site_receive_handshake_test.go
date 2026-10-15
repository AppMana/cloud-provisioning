package main

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type readOnlyReceiverDevice struct{ device *wgtypes.Device }

func (d readOnlyReceiverDevice) Device(string) (*wgtypes.Device, error) { return d.device, nil }
func (d readOnlyReceiverDevice) ConfigureDevice(string, wgtypes.Config) error {
	panic("readiness must not configure a device")
}

func TestSiteTransportReadinessRequiresCurrentExactKernelState(t *testing.T) {
	for _, fault := range []string{"none", "missing-handshake", "old-handshake", "future-handshake", "private-key", "public-key", "endpoint", "permission", "mark"} {
		t.Run(fault, func(t *testing.T) {
			key, _ := wgtypes.GeneratePrivateKey()
			peer, _ := wgtypes.GeneratePrivateKey()
			now := time.Now()
			handshake := now.Add(-time.Second)
			_, prefix, _ := net.ParseCIDR("172.29.0.10/32")
			plan := tunnel.SiteReceiveClient{Interface: "cldctest", SitePublicKey: key.PublicKey().String(), PublicKey: peer.PublicKey().String(), Endpoint: "198.51.100.10:51821", AllowedSources: []string{"172.29.0.10/32"}}
			live := &wgtypes.Device{PrivateKey: key, FirewallMark: 518, Peers: []wgtypes.Peer{{PublicKey: peer.PublicKey(), Endpoint: &net.UDPAddr{IP: net.ParseIP("198.51.100.10"), Port: 51821}, AllowedIPs: []net.IPNet{*prefix}, LastHandshakeTime: handshake}}}
			switch fault {
			case "missing-handshake":
				live.Peers[0].LastHandshakeTime = time.Time{}
			case "old-handshake":
				live.Peers[0].LastHandshakeTime = now.Add(-receiveHandshakeMaxAge - time.Second)
			case "future-handshake":
				live.Peers[0].LastHandshakeTime = now.Add(time.Second)
			case "private-key":
				live.PrivateKey = peer
			case "public-key":
				live.Peers[0].PublicKey = key.PublicKey()
			case "endpoint":
				live.Peers[0].Endpoint.Port++
			case "permission":
				live.Peers[0].AllowedIPs = nil
			case "mark":
				live.FirewallMark = 0
			}
			got, err := siteReceiveHandshake(readOnlyReceiverDevice{live}, key, plan, 518, now)
			pending := fault == "missing-handshake" || fault == "old-handshake" || fault == "future-handshake"
			if fault == "none" {
				if err != nil || !got.Equal(handshake) {
					t.Fatalf("valid handshake: %v %v", got, err)
				}
			} else if pending {
				if err != nil || !got.IsZero() {
					t.Fatalf("unproven path ready: %v %v", got, err)
				}
			} else if err == nil {
				t.Fatal("changed kernel state accepted")
			}
		})
	}
}

func TestSiteTransportReceiptBindsProposalAndLocalBoot(t *testing.T) {
	for _, fault := range []string{"none", "old-boot", "unready", "changed-mesh", "old-handshake"} {
		t.Run(fault, func(t *testing.T) {
			boot, err := localReceiveBootID()
			if err != nil {
				t.Fatal(err)
			}
			key, _ := wgtypes.GeneratePrivateKey()
			mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "mesh-uid", ResourceVersion: "1"}, Data: map[string][]byte{}}
			adoption := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "worker-tunnel-peers", Namespace: "test", UID: "adoption-uid", ResourceVersion: "2"}, Data: map[string][]byte{tunnel.ReceivePrepareKey: []byte("exact request")}}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "site", UID: "site-uid"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: boot}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
			live := mesh.DeepCopy()
			handshake := time.Now().Add(-time.Second)
			switch fault {
			case "old-boot":
				node.Status.NodeInfo.BootID = "old"
			case "unready":
				node.Status.Conditions = nil
			case "changed-mesh":
				live.ResourceVersion = "3"
			case "old-handshake":
				handshake = time.Now().Add(-time.Hour)
			}
			k := fake.NewClientset(live, adoption.DeepCopy(), node)
			plan := tunnel.SiteReceiveClient{SitePublicKey: key.PublicKey().String(), Interface: "cldctest"}
			err = publishSiteReceiveReady(context.Background(), k, mesh, adoption, "site", plan, handshake)
			got, getErr := k.CoreV1().Secrets("test").Get(context.Background(), adoption.Name, metav1.GetOptions{})
			if getErr != nil {
				t.Fatal(getErr)
			}
			raw := got.Data[tunnel.SiteReceiveReadyKey(plan.SitePublicKey)]
			if fault != "none" {
				if err == nil || len(raw) != 0 {
					t.Fatal("invalid transport receipt published")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var receipt tunnel.SiteReceiveReady
			if err := json.Unmarshal(raw, &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.Version != 1 || receipt.MeshUID != string(mesh.UID) || receipt.SecretUID != string(adoption.UID) || receipt.NodeUID != string(node.UID) || receipt.BootID != boot || receipt.RequestHash != tunnel.HashPeerList(adoption.Data[tunnel.ReceivePrepareKey]) || !receipt.HandshakeAt.Equal(handshake) {
				t.Fatalf("receipt lost identity: %+v", receipt)
			}
			if len(got.Annotations) != 0 || string(got.Data[tunnel.ReceivePrepareKey]) != "exact request" {
				t.Fatal("transport receipt changed forwarding or proposal")
			}
		})
	}
}
