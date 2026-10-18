package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestReceiveReadinessIsBoundToPreparedRequest(t *testing.T) {
	for _, fault := range []string{"none", "malformed", "version", "root", "base", "missing-native", "node", "desired", "prepare-error", "missing-binding", "wrong-interface", "root-binding", "stale-secret", "stale-version", "stale-base", "stale-request", "replaced-node", "patch-conflict", "old-boot", "node-boot", "reboot-during-preparation", "missing-ports", "unrequested-port", "missing-predecessor"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			bootID, err := localReceiveBootID()
			if err != nil {
				t.Fatal(err)
			}
			root, _ := wgtypes.GeneratePrivateKey()
			peer, _ := wgtypes.GeneratePrivateKey()
			base := []byte(`{"peers":[]}`)
			request := tunnel.ReceivePrepare{Version: 1, MeshUID: "mesh-uid", NodeName: "worker", NodeUID: "node-uid", BootID: bootID, RootPublicKey: root.PublicKey().String(), BaseHash: tunnel.HashPeerList(base), Ports: map[string]int{peer.PublicKey().String(): 51821}, Desired: tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: peer.PublicKey().String(), WGAllowedIPs: []string{"10.90.0.10/32"}}}}, Native: []netip.Prefix{netip.MustParsePrefix("172.29.0.0/24")}}
			switch fault {
			case "missing-predecessor":
				request.Predecessor = &tunnel.ReceivePreparePredecessor{ClusterUID: "cluster", MachineUID: "worker", JournalUID: "missing", JournalSHA256: "missing"}
			case "missing-ports":
				request.Ports = nil
			case "old-boot":
				request.BootID = "old-boot"
			case "version":
				request.Version = 2
			case "root":
				request.RootPublicKey = peer.PublicKey().String()
			case "base":
				request.BaseHash = "stale"
			case "missing-native":
				request.Native = nil
			case "node":
				request.NodeUID = "old-node"
			case "desired":
				request.Desired.Peers = nil
			}
			raw, _ := json.Marshal(request)
			if fault == "malformed" {
				raw = []byte(`{"version":1,"unknown":true}`)
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "adoption", Namespace: "test", UID: "secret-uid", ResourceVersion: "7", Annotations: map[string]string{tunnel.AppliedListAnnotation: "old-applied"}}, Data: map[string][]byte{tunnel.CloudPeersKey: base, tunnel.ReceivePrepareKey: raw}}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node-uid"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: bootID}}}
			if fault == "node-boot" {
				node.Status.NodeInfo.BootID = "old-boot"
			}
			k := fake.NewClientset(secret.DeepCopy(), node.DeepCopy())
			calls := 0
			k.PrependReactor("patch", "secrets", func(action ktesting.Action) (bool, runtime.Object, error) {
				var patch struct {
					Metadata struct {
						UID             string `json:"uid"`
						ResourceVersion string `json:"resourceVersion"`
					} `json:"metadata"`
				}
				if err := json.Unmarshal(action.(ktesting.PatchAction).GetPatch(), &patch); err != nil {
					t.Fatal(err)
				}
				if patch.Metadata.UID != "secret-uid" || patch.Metadata.ResourceVersion != "7" {
					t.Fatal("publication omitted compare-and-swap identity")
				}
				if fault == "patch-conflict" {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "adoption", errors.New("version changed"))
				}
				return false, nil, nil
			})
			prepare := func(req tunnel.ReceivePrepare, previous tunnel.PeerListDoc) ([]receiveBinding, error) {
				calls++
				if fault == "prepare-error" {
					return nil, errors.New("kernel preparation failed")
				}
				plan, err := tunnel.PlanReceiveHandover(req.MeshUID, previous, req.Desired, req.Native)
				if err != nil {
					t.Fatal(err)
				}
				path := plan.Receivers[0]
				bindings := []receiveBinding{{Interface: path.Interface, PeerPublicKey: path.PublicKey, PublicKey: receiverKey(root, req.MeshUID, path.PublicKey).PublicKey().String(), ListenPort: 51821}}
				switch fault {
				case "unrequested-port":
					bindings[0].ListenPort = 51900
				case "missing-binding":
					bindings = nil
				case "wrong-interface":
					bindings[0].Interface = "other"
				case "root-binding":
					bindings[0].PublicKey = root.PublicKey().String()
				case "stale-secret", "stale-version", "stale-base", "stale-request":
					changed := secret.DeepCopy()
					switch fault {
					case "stale-secret":
						changed.UID = "replacement"
					case "stale-version":
						changed.ResourceVersion = "8"
					case "stale-base":
						changed.Data[tunnel.CloudPeersKey] = []byte(`{"peers":[],"apiServers":[]}`)
					case "stale-request":
						changed.Data[tunnel.ReceivePrepareKey] = append(changed.Data[tunnel.ReceivePrepareKey], ' ')
					}
					if _, err := k.CoreV1().Secrets("test").Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				case "replaced-node", "reboot-during-preparation":
					changed := node.DeepCopy()
					if fault == "replaced-node" {
						changed.UID = "replacement"
					} else {
						changed.Status.NodeInfo.BootID = "new-boot"
					}
					if _, err := k.CoreV1().Nodes().Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				return bindings, nil
			}
			err = reconcileReceivePreparation(ctx, k, secret, "worker", root, prepare)
			current, getErr := k.CoreV1().Secrets("test").Get(ctx, "adoption", metav1.GetOptions{})
			if getErr != nil {
				t.Fatal(getErr)
			}
			if fault != "none" {
				if err == nil || len(current.Data[tunnel.ReceiveReadyKey]) != 0 {
					t.Fatal("invalid or stale preparation published readiness")
				}
				switch fault {
				case "malformed", "version", "root", "base", "missing-native", "node", "desired", "old-boot", "node-boot", "missing-ports", "missing-predecessor":
					if calls != 0 {
						t.Fatal("invalid request reached kernel preparation")
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var receipt tunnel.ReceiveReady
			if err := json.Unmarshal(current.Data[tunnel.ReceiveReadyKey], &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.RequestHash != tunnel.HashPeerList(raw) || receipt.BaseHash != request.BaseHash || receipt.SecretUID != "secret-uid" || receipt.NodeUID != "node-uid" || receipt.BootID != bootID || receipt.MeshUID != "mesh-uid" || len(receipt.Bindings) != 1 {
				t.Fatal("incomplete readiness identity")
			}
			if string(current.Data[tunnel.CloudPeersKey]) != string(base) || current.Annotations[tunnel.AppliedListAnnotation] != "old-applied" {
				t.Fatal("preparation changed forwarding or applied acknowledgement")
			}
			k.ClearActions()
			if err = reconcileReceivePreparation(ctx, k, current, "worker", root, prepare); err != nil {
				t.Fatal(err)
			}
			for _, action := range k.Actions() {
				if action.GetVerb() == "patch" {
					t.Fatal("unchanged readiness was rewritten")
				}
			}
		})
	}
}

func TestReceiveReadinessAbsentRequestDoesNothing(t *testing.T) {
	secret := &corev1.Secret{}
	if err := reconcileReceivePreparation(context.Background(), nil, secret, "", wgtypes.Key{}, nil); err != nil {
		t.Fatal(err)
	}
}
