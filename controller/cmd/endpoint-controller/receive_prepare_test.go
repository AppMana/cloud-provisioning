package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func preparationKey(value byte) string {
	var key [32]byte
	key[0] = value
	return base64.StdEncoding.EncodeToString(key[:])
}

type receivePreparationRaceClient struct {
	client.Client
	beforePatch func(context.Context) error
}

func (c *receivePreparationRaceClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if c.beforePatch != nil {
		f := c.beforePatch
		c.beforePatch = nil
		if err := f(ctx); err != nil {
			return err
		}
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func TestControllerPreparationPreservesForwardingAndInvalidatesStaleReadiness(t *testing.T) {
	for _, fault := range []string{"none", "unacknowledged", "not-ready", "missing-boot", "provider", "node-ref", "machine-uid", "retiring", "unknown-native", "native-collision", "adoption-race", "unknown-ports", "exhausted-ports"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			machine := &unstructured.Unstructured{}
			machine.SetGroupVersionKind(machineGVK)
			machine.SetNamespace("cloud-provisioning")
			machine.SetName("worker")
			machine.SetUID("machine-uid")
			machine.SetAnnotations(map[string]string{join.WireGuardAddrAnnotation: "10.100.0.128/24"})
			_ = unstructured.SetNestedField(machine.Object, "aws:///test/i-worker", "spec", "providerID")
			_ = unstructured.SetNestedField(machine.Object, "cloud-node", "status", "nodeRef", "name")
			_ = unstructured.SetNestedField(machine.Object, "node-uid", "status", "nodeRef", "uid")
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cloud-node", UID: "node-uid"}, Spec: corev1.NodeSpec{ProviderID: "aws:///test/i-worker"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{BootID: "boot-1"}, Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
			previous := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: preparationKey(8), WGAllowedIPs: []string{"10.101.128.0/26"}}}}
			base, _ := json.Marshal(previous)
			adoption := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName(machine.GetName()), Namespace: machine.GetNamespace(), UID: "adoption-uid", Annotations: map[string]string{tunnel.AppliedListAnnotation: tunnel.HashPeerList(base)}}, Data: map[string][]byte{tunnel.CloudPeersKey: base}}
			mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "peers", Namespace: machine.GetNamespace(), UID: "mesh-uid"}, Data: map[string][]byte{
				tunnel.NodePublicKeyPrefix + "site": []byte(preparationKey(16)), tunnel.NodeTunnelAddressPrefix + "site": []byte("10.100.0.2/24"), tunnel.NodeAddressesPrefix + "site": []byte("10.101.0.2"), tunnel.NodePodCIDRsPrefix + "site": []byte("10.101.128.0/26"),
				tunnel.PeerPublicKeyPrefix + "worker": []byte(preparationKey(24)), tunnel.PeerRouteHostsPrefix + "worker": []byte("10.100.0.128"), tunnel.PeerAllowedIPsPrefix + "worker": []byte("10.100.0.128/32"),
			}}
			native := []netip.Prefix{netip.MustParsePrefix("172.29.0.0/24")}
			pool := []int{51821, 51822, 51823, 51824}
			expected := machine.DeepCopy()
			switch fault {
			case "unacknowledged":
				adoption.Annotations[tunnel.AppliedListAnnotation] = "old"
			case "not-ready":
				node.Status.Conditions[0].Status = corev1.ConditionFalse
			case "missing-boot":
				node.Status.NodeInfo.BootID = ""
			case "provider":
				node.Spec.ProviderID = "aws:///test/i-other"
			case "node-ref":
				_ = unstructured.SetNestedField(machine.Object, "other-uid", "status", "nodeRef", "uid")
			case "machine-uid":
				machine.SetUID("replacement")
			case "retiring":
				machine.SetAnnotations(map[string]string{attachment.DrainIntentAnnotation: "drain", join.WireGuardAddrAnnotation: "10.100.0.128/24"})
			case "unknown-native":
				native = nil
			case "native-collision":
				native = []netip.Prefix{netip.MustParsePrefix("10.101.128.0/24")}
			case "unknown-ports":
				pool = nil
			case "exhausted-ports":
				pool = []int{51821}
			}
			r, c := dualStackReconciler(t, "", machine, node, mesh, adoption)
			if fault == "adoption-race" {
				r.Client = &receivePreparationRaceClient{Client: c, beforePatch: func(ctx context.Context) error {
					changed := &corev1.Secret{}
					if err := c.Get(ctx, client.ObjectKeyFromObject(adoption), changed); err != nil {
						return err
					}
					changed.Data[tunnel.CloudPeersKey] = []byte(`{"peers":[]}`)
					return c.Update(ctx, changed)
				}}
			}
			bindings, err := r.prepareRemoteReceivePaths(ctx, expected, native, pool)
			current := &corev1.Secret{}
			if getErr := c.Get(ctx, client.ObjectKeyFromObject(adoption), current); getErr != nil {
				t.Fatal(getErr)
			}
			if fault != "none" {
				if fault == "adoption-race" && string(current.Data[tunnel.CloudPeersKey]) != `{"peers":[]}` {
					t.Fatal("preparation overwrote a concurrent forwarding update")
				}
				if len(current.Data[tunnel.ReceivePrepareKey]) != 0 || bindings != nil {
					t.Fatal("unsafe preparation advanced")
				}
				if fault != "unacknowledged" && fault != "not-ready" && fault != "missing-boot" && err == nil {
					t.Fatal("identity or policy error was not reported")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if bindings != nil || len(current.Data[tunnel.ReceivePrepareKey]) == 0 {
				t.Fatal("first preparation did not wait for readiness")
			}
			assertBase := func(s *corev1.Secret) {
				t.Helper()
				if string(s.Data[tunnel.CloudPeersKey]) != string(base) || s.Annotations[tunnel.AppliedListAnnotation] != tunnel.HashPeerList(base) {
					t.Fatal("preparation changed active forwarding")
				}
			}
			assertBase(current)
			var request tunnel.ReceivePrepare
			if err := json.Unmarshal(current.Data[tunnel.ReceivePrepareKey], &request); err != nil {
				t.Fatal(err)
			}
			var reservations tunnel.ReceivePortReservations
			if err := json.Unmarshal(current.Data[tunnel.ReceivePortsKey], &reservations); err != nil {
				t.Fatal(err)
			}
			if reservations.NodeUID != request.NodeUID || reservations.RootPublicKey != request.RootPublicKey || reservations.MeshUID != request.MeshUID {
				t.Fatal("port reservations lost target identity")
			}
			for key, port := range request.Ports {
				if reservations.Ports[key] != port {
					t.Fatal("proposal did not atomically reserve its listener ports")
				}
			}
			plan, err := tunnel.PlanReceiveHandover(request.MeshUID, previous, request.Desired, native)
			if err != nil {
				t.Fatal(err)
			}
			receipt := tunnel.ReceiveReady{Version: 1, MeshUID: request.MeshUID, SecretUID: string(current.UID), NodeName: request.NodeName, NodeUID: request.NodeUID, BootID: request.BootID, RootPublicKey: request.RootPublicKey, BaseHash: request.BaseHash, RequestHash: tunnel.HashPeerList(current.Data[tunnel.ReceivePrepareKey])}
			for i, path := range plan.Receivers {
				receipt.Bindings = append(receipt.Bindings, tunnel.ReceiveBinding{Interface: path.Interface, PeerPublicKey: path.PublicKey, PublicKey: preparationKey(byte(40 + 8*i)), ListenPort: 51821 + i})
			}
			current.Data[tunnel.ReceiveReadyKey], _ = json.Marshal(receipt)
			if err := c.Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			readyVersion := current.ResourceVersion
			bindings, err = r.prepareRemoteReceivePaths(ctx, expected, native, pool)
			if err != nil || bindings == nil || len(bindings.Bindings) != 2 {
				t.Fatalf("valid preparation not accepted: %v", err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(adoption), current); err != nil {
				t.Fatal(err)
			}
			assertBase(current)
			if current.ResourceVersion != readyVersion {
				t.Fatal("valid readiness was needlessly rewritten")
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(mesh), mesh); err != nil {
				t.Fatal(err)
			}
			mesh.Data[tunnel.NodePodCIDRsPrefix+"site"] = []byte("10.101.128.0/26,10.101.128.64/26")
			if err := c.Update(ctx, mesh); err != nil {
				t.Fatal(err)
			}
			bindings, err = r.prepareRemoteReceivePaths(ctx, expected, native, pool)
			if err != nil || bindings != nil {
				t.Fatalf("changed proposal reused readiness: %v", err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(adoption), current); err != nil {
				t.Fatal(err)
			}
			assertBase(current)
			if len(current.Data[tunnel.ReceiveReadyKey]) != 0 || tunnel.HashPeerList(current.Data[tunnel.ReceivePrepareKey]) == receipt.RequestHash {
				t.Fatal("changed proposal retained old readiness")
			}
		})
	}
}
