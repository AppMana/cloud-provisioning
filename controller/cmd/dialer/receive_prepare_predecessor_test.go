package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Reuses validated journal data, but the API and native preparation callback are
// simulated. This tests receipt fencing, not native coexistence or atomic writes
// across the journal and Secret.
func verifySuccessorReadinessFence(t *testing.T, terminal, unfinished *corev1.ConfigMap, adoption *corev1.Secret, node *corev1.Node, root wgtypes.Key, clusterUID string) {
	t.Helper()
	for _, fault := range []string{"none", "changed-during-preparation", "recreated-before-preparation"} {
		t.Run("successor-"+fault, func(t *testing.T) {
			ctx := context.Background()
			api := fake.NewClientset(terminal.DeepCopy(), adoption.DeepCopy(), node.DeepCopy())
			store := handover.Store{ConfigMaps: api.CoreV1().ConfigMaps(terminal.Namespace), Name: terminal.Name, ClusterUID: clusterUID}
			snapshot, err := store.Load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			predecessor, err := tunnel.NewReceivePreparePredecessor(snapshot, clusterUID)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := tunnel.DecodeReceiveTransition(snapshot, clusterUID)
			if err != nil {
				t.Fatal(err)
			}
			request := tunnel.ReceivePrepare{Version: 1, Predecessor: &predecessor, ReceiverGeneration: strings.Repeat("a", 64), MeshUID: plan.MeshUID, NodeName: node.Name, NodeUID: string(node.UID), BootID: node.Status.NodeInfo.BootID, RootPublicKey: plan.RootPublicKey, BaseHash: tunnel.HashPeerList(plan.Base), Desired: plan.Desired, Native: plan.Native, Ports: map[string]int{}}
			for key, port := range plan.Ports {
				request.Ports[key] = port + 100
			}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			secret := adoption.DeepCopy()
			secret.Data = map[string][]byte{tunnel.CloudPeersKey: plan.Base, tunnel.ReceivePrepareKey: raw}
			secret, err = api.CoreV1().Secrets(secret.Namespace).Update(ctx, secret, metav1.UpdateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if fault == "recreated-before-preparation" {
				changed := terminal.DeepCopy()
				changed.UID = "recreated-journal"
				if _, err := store.ConfigMaps.Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			err = reconcileReceivePreparation(ctx, api, secret, node.Name, root, func(req tunnel.ReceivePrepare, previous tunnel.PeerListDoc) ([]receiveBinding, error) {
				calls++
				scope, err := req.ReceiveScope()
				if err != nil {
					return nil, err
				}
				paths, err := tunnel.PlanReceiveHandover(scope, previous, req.Desired, req.Native)
				if err != nil {
					return nil, err
				}
				var bindings []receiveBinding
				for _, path := range paths.Receivers {
					bindings = append(bindings, receiveBinding{Interface: path.Interface, PeerPublicKey: path.PublicKey, PublicKey: receiverKey(root, scope, path.PublicKey).PublicKey().String(), ListenPort: req.Ports[path.PublicKey]})
				}
				if fault == "changed-during-preparation" {
					changed := terminal.DeepCopy()
					changed.Data = unfinished.DeepCopy().Data
					if _, err := store.ConfigMaps.Update(ctx, changed, metav1.UpdateOptions{}); err != nil {
						return nil, err
					}
				}
				return bindings, nil
			})
			current, getErr := api.CoreV1().Secrets(secret.Namespace).Get(ctx, secret.Name, metav1.GetOptions{})
			if getErr != nil {
				t.Fatal(getErr)
			}
			if fault == "none" {
				if err != nil || len(current.Data[tunnel.ReceiveReadyKey]) == 0 || calls != 1 {
					t.Fatal("valid successor readiness rejected", err)
				}
			} else {
				if err == nil || len(current.Data[tunnel.ReceiveReadyKey]) != 0 {
					t.Fatal("changed predecessor earned readiness")
				}
				if fault == "recreated-before-preparation" && calls != 0 {
					t.Fatal("recreated predecessor reached preparation")
				}
				if fault == "changed-during-preparation" && calls != 1 {
					t.Fatal("test did not reach preparation")
				}
			}
		})
	}
}
