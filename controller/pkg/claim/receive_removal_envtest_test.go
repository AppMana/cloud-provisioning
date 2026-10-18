package claim

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reuse isolated API storage to exercise standalone deletion's durable capture,
// cordon, drain marker and withdrawal capture. An intentionally missing journal
// must stop compute deletion. No native cleanup, completed receipt sequence,
// live CAPI, interruption bound or orphan recovery is established by this test.
func verifyStandaloneReceiveDeletion(t *testing.T, api client.Client, core kubernetes.Interface) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	claim := fakeClaim("reserved-standalone")
	claim.Finalizers = []string{Finalizer}
	must(api.Create(ctx, claim))
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: claim.Name}, Spec: corev1.NodeSpec{ProviderID: "test:///standalone"}}
	must(api.Create(ctx, node))
	site := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "standalone-site"}}
	must(api.Create(ctx, site))
	machine := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"providerID": node.Spec.ProviderID}, "status": map[string]any{"nodeRef": map[string]any{"name": node.Name, "uid": string(node.UID)}}}}
	machine.SetGroupVersionKind(machineGVK)
	machine.SetName(claim.Name)
	machine.SetNamespace(claim.Namespace)
	machine.SetFinalizers([]string{"test/compute"})
	machine.SetAnnotations(map[string]string{ClaimAnnotation: claim.Namespace + "/" + claim.Name, ClaimUIDAnnotation: string(claim.UID), "cloud-provisioning.appmana.com/wireguard-addr4": "10.100.0.2/24"})
	must(api.Create(ctx, machine))
	adoption := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName(claim.Name), Namespace: claim.Namespace}}
	must(api.Create(ctx, adoption))
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "standalone-mesh", Namespace: claim.Namespace}, Data: map[string][]byte{tunnel.PeerPublicKeyPrefix + claim.Name: []byte("root"), tunnel.NodePublicKeyPrefix + site.Name: []byte("site")}}
	must(api.Create(ctx, mesh))
	cluster, err := core.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	must(err)
	inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: string(cluster.UID), Namespace: mesh.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), MachineUIDs: []string{string(machine.GetUID())}, Publications: map[string]tunnel.ReceiveJournalPublication{string(machine.GetUID()): {JournalUID: "missing-journal"}}}
	raw, err := json.Marshal(inventory)
	must(err)
	index := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveInventoryName(string(mesh.UID)), Namespace: mesh.Namespace}, Data: map[string]string{"inventory.json": string(raw)}}
	must(api.Create(ctx, index))
	must(api.Delete(ctx, claim))
	var record claimReceiveRemoval
	step := func() error {
		r := &Reconciler{Client: api, Reader: api, Workload: api, RetirementAPI: core, MeshName: mesh.Name}
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(claim)})
		must(api.Get(ctx, client.ObjectKeyFromObject(claim), claim))
		if data := claim.Annotations[receiveRemovalAnnotation]; data != "" {
			record = claimReceiveRemoval{}
			must(json.Unmarshal([]byte(data), &record))
		}
		must(api.Get(ctx, client.ObjectKeyFromObject(machine), machine))
		if machine.GetDeletionTimestamp() != nil {
			t.Fatal("reserved compute deleted before verified retirement")
		}
		return err
	}
	must(step()) // persist target before touching its Node
	if record.NodeUID != string(node.UID) || record.InventoryUID != string(index.UID) {
		t.Fatal("target identity not retained")
	}
	must(api.Get(ctx, client.ObjectKeyFromObject(node), node))
	if node.Spec.Unschedulable {
		t.Fatal("capture cordoned before durable identity")
	}
	must(step()) // cordon
	must(api.Get(ctx, client.ObjectKeyFromObject(node), node))
	if !node.Spec.Unschedulable {
		t.Fatal("target not cordoned")
	}
	must(step()) // drain marker
	if machine.GetAnnotations()[attachment.DrainIntentAnnotation] != string(claim.UID)+"/delete" {
		t.Fatal("drain owner missing")
	}
	must(step()) // retain survivor inventory
	if record.Withdrawal == nil || len(record.Withdrawal.Consumers) != 1 || record.Withdrawal.Consumers[0].NodeUID != string(site.UID) {
		t.Fatal("withdrawal did not retain survivor")
	}
	if err := step(); err == nil {
		t.Fatal("missing journal permitted retirement")
	}
	saved := claim.Annotations[receiveRemovalAnnotation]
	for _, damage := range []string{"malformed", "node", "marker", "removing-without-intent"} {
		current := claim.DeepCopy()
		must(json.Unmarshal([]byte(saved), &record))
		switch damage {
		case "malformed":
			current.Annotations[receiveRemovalAnnotation] = "{}"
		case "node":
			changed := record
			changed.NodeUID = "replacement"
			raw, err := json.Marshal(changed)
			must(err)
			current.Annotations[receiveRemovalAnnotation] = string(raw)
		case "marker":
			annotations := machine.GetAnnotations()
			annotations[attachment.DrainIntentAnnotation] = "other"
			machine.SetAnnotations(annotations)
			must(api.Update(ctx, machine))
		case "removing-without-intent":
			changed := record
			changed.Removing = true
			changed.Withdrawal = nil
			raw, err := json.Marshal(changed)
			must(err)
			current.Annotations[receiveRemovalAnnotation] = string(raw)
		}
		must(api.Update(ctx, current))
		if err := step(); err == nil {
			t.Fatal("invalid retirement accepted", damage)
		}
		claim.Annotations[receiveRemovalAnnotation] = saved
		must(api.Update(ctx, claim))
		annotations := machine.GetAnnotations()
		annotations[attachment.DrainIntentAnnotation] = string(claim.UID) + "/delete"
		machine.SetAnnotations(annotations)
		must(api.Update(ctx, machine))
	}
	must(api.Get(ctx, client.ObjectKeyFromObject(adoption), adoption))
	must(api.Get(ctx, client.ObjectKeyFromObject(mesh), mesh))
	if string(mesh.Data[tunnel.PeerPublicKeyPrefix+claim.Name]) != "root" {
		t.Fatal("failed retirement removed peer")
	}
}
