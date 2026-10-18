package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Uses the preparation fixture and real journal API. Secret writes are backed
// by the fixture client. This proves proposal binding and retry stability, not
// cross-object atomicity, native preparation, agent admission or publication.
func verifySuccessorPreparationProposal(t *testing.T, r *meshReconciler, machine *unstructured.Unstructured, native []netip.Prefix, store handover.Store, predecessor handover.Snapshot) {
	t.Helper()
	ctx := context.Background()
	key := client.ObjectKey{Namespace: r.secretNamespace, Name: tunnel.AdoptionSecretName(machine.GetName())}
	original := &corev1.Secret{}
	if err := r.reader.Get(ctx, key, original); err != nil {
		t.Fatal(err)
	}
	defer func() {
		current := &corev1.Secret{}
		if err := r.reader.Get(ctx, key, current); err != nil {
			t.Error(err)
			return
		}
		current.Data = original.DeepCopy().Data
		if err := r.Update(ctx, current); err != nil {
			t.Error(err)
		}
	}()
	plan, err := tunnel.DecodeReceiveTransition(predecessor, store.ClusterUID)
	if err != nil {
		t.Fatal(err)
	}
	pool := []int{58300, 58301, 58302, 58303, 58304, 58305, 58306, 58307}
	before, err := predecessor.Transition().Bytes()
	if err != nil {
		t.Fatal(err)
	}
	var first tunnel.ReceivePrepare
	var revision string
	for attempt := 0; attempt < 2; attempt++ {
		prepared, err := r.prepareRemoteReceiveGeneration(ctx, machine, native, pool, &predecessor)
		if err != nil || prepared != nil {
			t.Fatal("successor proposal should wait for fresh readiness", err)
		}
		current := &corev1.Secret{}
		if err := r.reader.Get(ctx, key, current); err != nil {
			t.Fatal(err)
		}
		var request tunnel.ReceivePrepare
		if err := json.Unmarshal(current.Data[tunnel.ReceivePrepareKey], &request); err != nil {
			t.Fatal(err)
		}
		if request.Predecessor == nil || request.ReceiverGeneration == "" {
			t.Fatal("proposal omitted terminal generation binding")
		}
		if _, err := request.ValidatePredecessor(predecessor); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(current.Data[tunnel.CloudPeersKey], original.Data[tunnel.CloudPeersKey]) || current.Annotations[tunnel.AppliedListAnnotation] != original.Annotations[tunnel.AppliedListAnnotation] || len(current.Data[tunnel.ReceiveReadyKey]) != 0 {
			t.Fatal("proposal changed serving document or reused readiness")
		}
		for _, port := range request.Ports {
			for _, occupied := range plan.Ports {
				if port == occupied {
					t.Fatal("proposal reused serving listener")
				}
			}
		}
		var reservations tunnel.ReceivePortReservations
		if err := json.Unmarshal(current.Data[tunnel.ReceivePortsKey], &reservations); err != nil {
			t.Fatal(err)
		}
		if err := reservations.Validate(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(reservations.Generations[request.ReceiverGeneration], request.Ports) {
			t.Fatal("generation listener reservation missing")
		}
		if attempt == 0 {
			first, revision = request, current.ResourceVersion
		} else if !reflect.DeepEqual(first, request) || current.ResourceVersion != revision {
			t.Fatal("retry changed generation, listeners or proposal")
		}
		live, err := store.Load(ctx)
		if err != nil {
			t.Fatal(err)
		}
		after, err := live.Transition().Bytes()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) || !bytes.Equal(predecessor.Plan(), live.Plan()) || predecessor.Metadata().UID != live.Metadata().UID {
			t.Fatal("additive proposal changed serving journal")
		}
	}
	// A new boot invalidates readiness without consuming another receiver
	// generation or listener reservation for the same desired transport.
	nodeKey := client.ObjectKey{Name: plan.CloudNodeName}
	node := &corev1.Node{}
	if err := r.reader.Get(ctx, nodeKey, node); err != nil {
		t.Fatal(err)
	}
	originalStatus := node.DeepCopy().Status
	defer func() {
		current := &corev1.Node{}
		if err := r.reader.Get(ctx, nodeKey, current); err != nil {
			t.Error(err)
			return
		}
		current.Status = originalStatus
		if err := r.Status().Update(ctx, current); err != nil {
			t.Error(err)
		}
	}()
	node.Status.NodeInfo.BootID = "next-boot"
	if err := r.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	if prepared, err := r.prepareRemoteReceiveGeneration(ctx, machine, native, pool, &predecessor); err != nil || prepared != nil {
		t.Fatal("boot refresh did not wait for readiness", err)
	}
	current := &corev1.Secret{}
	if err := r.reader.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	var refreshed tunnel.ReceivePrepare
	if err := json.Unmarshal(current.Data[tunnel.ReceivePrepareKey], &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.BootID != "next-boot" || refreshed.ReceiverGeneration != first.ReceiverGeneration || !reflect.DeepEqual(refreshed.Ports, first.Ports) {
		t.Fatal("boot refresh changed the receiver generation or listeners")
	}

	verifySuccessorPreparedPlan(t, r, machine, native, pool, predecessor)

}

// Simulated readiness and Secret storage, with the real predecessor journal.
// A changed worker allocation must preserve the exact serving graph and old
// relay destinations. Building a plan permits no interruption or forwarding
// change; rejection is synchronous and leaves the predecessor selected, without
// requiring rollback. Native handshakes, publication and recovery deadlines are
// not established by this test.
func verifySuccessorPreparedPlan(t *testing.T, r *meshReconciler, machine *unstructured.Unstructured, native []netip.Prefix, ports []int, predecessor handover.Snapshot) {
	t.Helper()
	ctx := context.Background()
	mesh := &corev1.Secret{}
	meshKey := client.ObjectKey{Namespace: r.secretNamespace, Name: r.secretName}
	if err := r.reader.Get(ctx, meshKey, mesh); err != nil {
		t.Fatal(err)
	}
	original := mesh.DeepCopy().Data
	defer func() {
		current := &corev1.Secret{}
		if err := r.reader.Get(ctx, meshKey, current); err != nil {
			t.Fatal(err)
		}
		current.Data = original
		if err := r.Update(ctx, current); err != nil {
			t.Fatal(err)
		}
	}()
	mesh.Data[tunnel.PeerAllowedIPsPrefix+machine.GetName()] = append(mesh.Data[tunnel.PeerAllowedIPsPrefix+machine.GetName()], []byte(",198.18.42.0/24")...)
	if err := r.Update(ctx, mesh); err != nil {
		t.Fatal(err)
	}
	if prepared, err := r.prepareRemoteReceiveGeneration(ctx, machine, native, ports, &predecessor); err != nil || prepared != nil {
		t.Fatal("changed allocation did not wait for readiness", err)
	}
	adoption := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: tunnel.AdoptionSecretName(machine.GetName())}, adoption); err != nil {
		t.Fatal(err)
	}
	var request tunnel.ReceivePrepare
	if err := json.Unmarshal(adoption.Data[tunnel.ReceivePrepareKey], &request); err != nil {
		t.Fatal(err)
	}
	previous, err := request.ValidatePredecessor(predecessor)
	if err != nil {
		t.Fatal(err)
	}
	view, err := predecessor.Transition().View()
	if err != nil {
		t.Fatal(err)
	}
	oldSite, err := previous.CompletedSitePeers(view)
	if err != nil {
		t.Fatal(err)
	}
	var base tunnel.PeerListDoc
	if err := json.Unmarshal(adoption.Data[tunnel.CloudPeersKey], &base); err != nil {
		t.Fatal(err)
	}
	scope, err := request.ReceiveScope()
	if err != nil {
		t.Fatal(err)
	}
	paths, err := tunnel.PlanReceiveHandover(scope, base, request.Desired, request.Native)
	if err != nil {
		t.Fatal(err)
	}
	cloud := tunnel.ReceiveReady{Version: 1, MeshUID: request.MeshUID, SecretUID: string(adoption.UID), NodeName: request.NodeName, NodeUID: request.NodeUID, BootID: request.BootID, RootPublicKey: request.RootPublicKey, BaseHash: request.BaseHash, RequestHash: tunnel.HashPeerList(adoption.Data[tunnel.ReceivePrepareKey])}
	for i, path := range paths.Receivers {
		cloud.Bindings = append(cloud.Bindings, tunnel.ReceiveBinding{Interface: path.Interface, PeerPublicKey: path.PublicKey, PublicKey: preparationKey(byte(80 + 8*i)), ListenPort: request.Ports[path.PublicKey]})
	}
	adoption.Data[tunnel.ReceiveReadyKey], err = json.Marshal(cloud)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reader.Get(ctx, meshKey, mesh); err != nil {
		t.Fatal(err)
	}
	peers, err := tunnel.SitePeers(mesh.Data)
	if err != nil {
		t.Fatal(err)
	}
	desired := tunnel.PeerListDoc{Peers: peers}
	siteHash, err := tunnel.SitePeerHash(mesh.Data)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, binding := range cloud.Bindings {
		site := previous.Sites[binding.PeerPublicKey]
		node := &corev1.Node{}
		if err := r.reader.Get(ctx, client.ObjectKey{Name: site.NodeName}, node); err != nil {
			t.Fatal(err)
		}
		path, err := tunnel.PlanSiteReceiveClient(scope, binding.PeerPublicKey, request.RootPublicKey, binding, oldSite, desired)
		if err != nil {
			t.Fatal(err)
		}
		receipt := tunnel.SiteReceiveReady{Version: 1, MeshUID: request.MeshUID, SecretUID: string(adoption.UID), RequestHash: cloud.RequestHash, SiteHash: siteHash, NodeName: node.Name, NodeUID: string(node.UID), BootID: node.Status.NodeInfo.BootID, Client: path, HandshakeAt: now.Add(-time.Second)}
		adoption.Data[tunnel.SiteReceiveReadyKey(binding.PeerPublicKey)], err = json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Update(ctx, adoption); err != nil {
		t.Fatal(err)
	}
	prepared, err := r.prepareRemoteReceiveGeneration(ctx, machine, native, ports, &predecessor)
	if err != nil || prepared == nil {
		t.Fatal("successor readiness rejected", err)
	}
	plan, err := r.buildReceiveTransitionPlan(ctx, machine, prepared, now)
	if err != nil {
		t.Fatal("successor plan rejected", err)
	}
	if !reflect.DeepEqual(plan.SiteDesired, desired) || plan.SiteBase == nil || !reflect.DeepEqual(*plan.SiteBase, oldSite) {
		t.Fatal("successor lost old or new destination permissions")
	}
	serving, root, err := previous.SelectedReceiveTransport(view)
	if err != nil {
		t.Fatal(err)
	}
	if root {
		if plan.Version != 1 || plan.PreviousReceiver != nil {
			t.Fatal("root predecessor acquired a receiver graph")
		}
	} else {
		serving.Version, serving.PreviousReceiver = 1, nil
		if plan.Version != 2 || plan.PreviousReceiver == nil || plan.PreviousReceiver.Generation != view.ServingGeneration || !reflect.DeepEqual(*plan.PreviousReceiver.Plan, serving) {
			t.Fatal("successor lost the exact serving receiver graph")
		}
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := tunnel.ReceiveTransitionSpec(plan, view.Round.ClusterUID, fmt.Sprintf("%x", sha256.Sum256(raw)))
	if err != nil || spec.FromGeneration != view.ServingGeneration {
		t.Fatal("successor starts from a different generation", err)
	}
}
