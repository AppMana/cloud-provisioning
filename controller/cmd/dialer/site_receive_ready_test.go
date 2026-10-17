package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestSitePreparesOnlyCurrentCloudReceivers(t *testing.T) {
	for _, fault := range []string{"none", "no-request", "no-receipt", "unapplied", "wrong-root", "reboot", "unready", "replaced-mesh", "changed-mesh", "wrong-local-key", "wrong-port", "node-changes-before-prepare", "proposal-changes-before-prepare", "scoped", "reserved", "scope-before", "scope-after"} {
		t.Run(fault, func(t *testing.T) {
			site, _ := wgtypes.GeneratePrivateKey()
			root, _ := wgtypes.GeneratePrivateKey()
			receiver, _ := wgtypes.GeneratePrivateKey()
			pub := site.PublicKey().String()
			mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "mesh-uid", ResourceVersion: "1"}, Data: map[string][]byte{
				tunnel.NodePublicKeyPrefix + "site":    []byte(pub),
				tunnel.PeerPublicKeyPrefix + "worker":  []byte(root.PublicKey().String()),
				tunnel.PeerEndpointPrefix + "worker":   []byte("198.51.100.10:51820"),
				tunnel.PeerAllowedIPsPrefix + "worker": []byte("172.29.0.10/32"),
				tunnel.PeerRouteHostsPrefix + "worker": []byte("172.29.0.10"),
			}}
			doc := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: pub, WGAllowedIPs: []string{"10.90.0.10/32"}}}}
			base, _ := json.Marshal(doc)
			request := tunnel.ReceivePrepare{Version: 1, MeshUID: string(mesh.UID), NodeName: "cloud", NodeUID: "cloud-uid", BootID: "boot", RootPublicKey: root.PublicKey().String(), BaseHash: tunnel.HashPeerList(base), Desired: doc, Native: []netip.Prefix{netip.MustParsePrefix("172.29.0.0/24")}, Ports: map[string]int{pub: 51821}}
			plan, err := tunnel.PlanReceiveHandover(string(mesh.UID), doc, doc, request.Native)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(request)
			binding := tunnel.ReceiveBinding{PeerPublicKey: pub, PublicKey: receiver.PublicKey().String(), Interface: plan.Receivers[0].Interface, ListenPort: 51821}
			receipt := tunnel.ReceiveReady{Version: 1, MeshUID: string(mesh.UID), SecretUID: "adoption-uid", NodeName: "cloud", NodeUID: "cloud-uid", BootID: "boot", RootPublicKey: root.PublicKey().String(), BaseHash: request.BaseHash, RequestHash: tunnel.HashPeerList(raw), Bindings: []tunnel.ReceiveBinding{binding}}
			if fault == "wrong-port" {
				receipt.Bindings[0].ListenPort = 51822
			}
			receiptRaw, _ := json.Marshal(receipt)
			adoption := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName("worker"), Namespace: "test", UID: "adoption-uid", ResourceVersion: "2", Annotations: map[string]string{tunnel.AppliedListAnnotation: request.BaseHash}}, Data: map[string][]byte{tunnel.CloudPeersKey: base, tunnel.ReceivePrepareKey: raw, tunnel.ReceiveReadyKey: receiptRaw}}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cloud", UID: "cloud-uid"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
			live := mesh.DeepCopy()
			switch fault {
			case "no-request":
				delete(adoption.Data, tunnel.ReceivePrepareKey)
			case "no-receipt":
				delete(adoption.Data, tunnel.ReceiveReadyKey)
			case "unapplied":
				adoption.Annotations[tunnel.AppliedListAnnotation] = "old"
			case "wrong-root":
				mesh.Data[tunnel.PeerPublicKeyPrefix+"worker"] = []byte(pub)
			case "wrong-local-key":
				mesh.Data[tunnel.NodePublicKeyPrefix+"site"] = []byte(root.PublicKey().String())
			case "reboot":
				node.Status.NodeInfo.BootID = "new-boot"
			case "unready":
				node.Status.Conditions = nil
			case "replaced-mesh":
				live.UID = "another-mesh"
			case "changed-mesh":
				live.ResourceVersion = "2"
			}
			k := fake.NewClientset(live, adoption, node)
			nodeReads, adoptionReads := 0, 0
			k.PrependReactor("get", "nodes", func(action ktesting.Action) (bool, runtime.Object, error) {
				nodeReads++
				if fault == "node-changes-before-prepare" && nodeReads == 2 {
					changed := node.DeepCopy()
					changed.Status.NodeInfo.BootID = "new-boot"
					return true, changed, nil
				}
				return false, nil, nil
			})
			k.PrependReactor("get", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
				if action.(ktesting.GetAction).GetName() != adoption.Name {
					return false, nil, nil
				}
				adoptionReads++
				if fault == "proposal-changes-before-prepare" && adoptionReads == 2 {
					changed := adoption.DeepCopy()
					changed.ResourceVersion = "3"
					return true, changed, nil
				}
				return false, nil, nil
			})
			calls := 0
			var scope *siteReceiveScope
			var observe []func(tunnel.SiteReceiveClient) (time.Time, error)
			scopeChecks := 0
			if fault == "scoped" || fault == "reserved" || fault == "scope-before" || fault == "scope-after" {
				scope = &siteReceiveScope{roots: map[string]bool{root.PublicKey().String(): fault != "reserved"}, check: func() error {
					scopeChecks++
					if fault == "scope-before" || fault == "scope-after" && scopeChecks == 2 {
						return errors.New("inventory changed")
					}
					return nil
				}}
				if fault == "scope-after" {
					observe = append(observe, func(tunnel.SiteReceiveClient) (time.Time, error) { return time.Now(), nil })
				}
			}
			err = reconcileSiteReceivePreparationScoped(context.Background(), k, mesh, "site", site, scope, func(cloud string, b receiveBinding, got tunnel.PeerListDoc) error {
				calls++
				_, err := tunnel.PlanSiteReceiveClient(string(mesh.UID), pub, cloud, b, got, got)
				return err
			}, observe...)
			pending := fault == "no-request" || fault == "no-receipt" || fault == "unapplied" || fault == "reserved"
			if fault == "none" || fault == "scoped" {
				if err != nil || calls != 1 {
					t.Fatalf("valid receipt: calls=%d err=%v", calls, err)
				}
			} else if fault == "scope-after" {
				if err == nil || calls != 1 || scopeChecks != 2 {
					t.Fatalf("post-native drift: calls=%d checks=%d err=%v", calls, scopeChecks, err)
				}
			} else if pending {
				if err != nil || calls != 0 {
					t.Fatalf("pending receiver: calls=%d err=%v", calls, err)
				}
			} else if err == nil || calls != 0 {
				t.Fatalf("invalid receipt reached kernel: calls=%d err=%v", calls, err)
			}
			if fault == "reserved" && (adoptionReads != 0 || scopeChecks != 0) {
				t.Fatal("reserved worker entered preparation")
			}
			for _, action := range k.Actions() {
				if action.GetVerb() != "get" {
					t.Fatalf("preparation wrote cluster state: %s", action.GetVerb())
				}
			}
		})
	}
}
