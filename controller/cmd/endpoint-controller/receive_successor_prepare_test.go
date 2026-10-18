package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"reflect"
	"testing"

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

}
