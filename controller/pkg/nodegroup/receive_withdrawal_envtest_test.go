package nodegroup

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Real API persistence across freshly constructed reconcilers: freeze, reserve,
// publish and wait for receipts. Synthetic completed transport history does not
// establish native cleanup, authenticated completion, provider deletion or any
// workload/management interruption bound. The required policy is to keep blocking
// while receipts are absent; these retries do not measure a timeout or prove
// indefinite behavior.
func verifyReceiveRemovalStages(t *testing.T, api client.Client, core kubernetes.Interface) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	cluster, err := core.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	must(err)
	group := groupFixture()
	group.Name = "reserved-removal"
	group.Namespace = "default"
	group.UID = ""
	zero := int32(0)
	group.Spec.Replicas = &zero
	must(api.Create(ctx, group))
	child, err := BuildChild(group, 0)
	must(err)
	must(api.Create(ctx, child))
	action, err := ProposeAction(group, []v1alpha1.ProvisionedNodeClaim{*child})
	must(err)
	group, err = ReserveAction(ctx, api, group, action)
	must(err)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "reserved-worker"}, Spec: corev1.NodeSpec{ProviderID: "test:///reserved-worker", Unschedulable: true}}
	must(api.Create(ctx, node))
	site := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "reserved-site"}}
	must(api.Create(ctx, site))
	gvk := schema.GroupVersionKind{Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine"}
	machine := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"providerID": node.Spec.ProviderID}, "status": map[string]any{"nodeRef": map[string]any{"name": node.Name, "uid": string(node.UID)}}}}
	machine.SetGroupVersionKind(gvk)
	machine.SetName(child.Name)
	machine.SetNamespace(group.Namespace)
	machine.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(child, v1alpha1.GroupVersion.WithKind("ProvisionedNodeClaim"))})
	machine.SetAnnotations(map[string]string{attachment.DrainIntentAnnotation: string(group.UID) + "/" + group.Status.PendingAction.ID})
	must(api.Create(ctx, machine))
	key := func(b byte) string { var k wgtypes.Key; k[0] = b; return k.String() }
	siteKey, root := key(1), key(2)
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "reserved-mesh", Namespace: group.Namespace}, Data: map[string][]byte{
		tunnel.NodePublicKeyPrefix + site.Name:   []byte(siteKey),
		tunnel.PeerPublicKeyPrefix + child.Name:  []byte(root),
		tunnel.PeerEndpointPrefix + child.Name:   []byte("198.51.100.1:51820"),
		tunnel.PeerAllowedIPsPrefix + child.Name: []byte("172.29.0.1/32"),
		tunnel.PeerRouteHostsPrefix + child.Name: []byte("172.29.0.1"),
	}}
	must(api.Create(ctx, mesh))
	a := group.Status.PendingAction
	a.MachineUID = string(machine.GetUID())
	a.NodeName = node.Name
	a.NodeUID = string(node.UID)
	a.ProviderID = node.Spec.ProviderID
	a.Gateways = &v1alpha1.GroupGatewayInventory{Mesh: mesh.Name, Requests: []v1alpha1.GroupGatewayRequest{}}
	a.Withdrawal = &v1alpha1.GroupPeerWithdrawal{MeshName: mesh.Name, MeshUID: string(mesh.UID), SourceVersion: mesh.ResourceVersion, PublicKey: root, Consumers: []v1alpha1.GroupPeerConsumer{{Site: true, NodeName: site.Name, NodeUID: string(site.UID), PublicKey: siteKey}}}
	must(api.Status().Update(ctx, group))
	peers := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: siteKey, WGAllowedIPs: []string{"10.90.0.0/24"}}}}
	base, err := json.Marshal(peers)
	must(err)
	plan := tunnel.ReceiveTransitionPlan{Version: 1, Namespace: group.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), MachineName: child.Name, MachineUID: a.MachineUID, AdoptionName: tunnel.AdoptionSecretName(child.Name), AdoptionUID: "adoption-uid", CloudNodeName: node.Name, CloudNodeUID: string(node.UID), RootPublicKey: root, Base: base, Desired: peers, Native: []netip.Prefix{}, Ports: map[string]int{}, Bindings: map[string]tunnel.ReceiveBinding{}, Sites: map[string]tunnel.ReceiveTransitionSite{}}
	plan.SiteDesired = tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: root, Remote: true, Endpoint: "198.51.100.1:51820", WGAllowedIPs: []string{"172.29.0.1/32"}}}}
	prep, err := tunnel.PlanReceiveHandover(plan.MeshUID, peers, peers, nil)
	must(err)
	for _, path := range prep.Receivers {
		binding := tunnel.ReceiveBinding{Interface: path.Interface, PeerPublicKey: siteKey, PublicKey: key(3), ListenPort: 51821}
		plan.Bindings[siteKey] = binding
		plan.Ports[siteKey] = binding.ListenPort
		receiver, err := tunnel.PlanSiteReceiveClient(plan.MeshUID, siteKey, root, binding, plan.SiteDesired, plan.SiteDesired)
		must(err)
		plan.Sites[siteKey] = tunnel.ReceiveTransitionSite{NodeName: site.Name, NodeUID: string(site.UID), Client: receiver}
	}
	raw, err := json.Marshal(plan)
	must(err)
	spec, err := tunnel.ReceiveTransitionSpec(plan, string(cluster.UID), tunnel.HashPeerList(raw))
	must(err)
	tr, err := handover.NewTransition(spec, time.Now().Add(-time.Minute), handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: time.Minute, DrainInterval: time.Second})
	must(err)
	for steps := 0; ; steps++ {
		if steps > 10 {
			t.Fatal("transition did not finish")
		}
		view, err := tr.View()
		must(err)
		if view.Complete {
			break
		}
		at := view.Round.IssuedAt.Add(time.Second)
		if view.Round.Phase == handover.Drain {
			at = view.DrainUntil
		}
		digest, err := view.Round.Digest()
		must(err)
		var receipts []handover.AuthenticatedReceipt
		for _, member := range view.Round.Members {
			receipts = append(receipts, handover.AuthenticatedReceipt{AuthenticatedNodeUID: member.NodeUID, Receipt: handover.Receipt{RoundHash: digest, NodeUID: member.NodeUID, DeviceID: member.DeviceID, FromKey: member.FromKey, ToKey: member.ToKey, FromPeerKey: member.FromPeerKey, ToPeerKey: member.ToPeerKey, ObservedAt: at}})
		}
		tr, err = tr.Advance(at, receipts)
		must(err)
	}
	history, err := tr.Bytes()
	must(err)
	journal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveTransitionName(a.MachineUID), Namespace: group.Namespace}, Data: map[string]string{"plan.json": string(raw), "transition.json": string(history)}}
	must(api.Create(ctx, journal))
	inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: string(cluster.UID), Namespace: group.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), MachineUIDs: []string{a.MachineUID}, Publications: map[string]tunnel.ReceiveJournalPublication{a.MachineUID: {JournalUID: string(journal.UID)}}}
	raw, err = json.Marshal(inventory)
	must(err)
	index := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveInventoryName(string(mesh.UID)), Namespace: group.Namespace}, Data: map[string]string{"inventory.json": string(raw)}}
	must(api.Create(ctx, index))
	store := handover.Store{ConfigMaps: core.CoreV1().ConfigMaps(group.Namespace), ClusterUID: string(cluster.UID), Name: journal.Name, ExpectedUID: string(journal.UID)}
	var publishedVersion string
	for stage := 0; stage < 5; stage++ {
		// Neither the caller's status nor an in-memory reconciler survives a retry.
		current := &v1alpha1.ProvisionedNodeGroupClaim{}
		must(api.Get(ctx, client.ObjectKeyFromObject(group), current))
		r := &Reconciler{API: api, Workload: api, MachineGVK: gvk, RetirementAPI: core}
		handled, err := r.beginReceiveRemoval(ctx, current)
		must(err)
		if !handled {
			t.Fatal("reserved worker fell through")
		}
		snapshot, err := store.Load(ctx)
		must(err)
		if !snapshot.Frozen() {
			t.Fatal("journal not frozen")
		}
		stored, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, group.Namespace, mesh.Name, string(mesh.UID))
		must(err)
		publication := stored.Publications[a.MachineUID]
		if (len(publication.Retirement) > 0) != (stage >= 1) {
			t.Fatalf("stage %d: wrong retirement persistence", stage)
		}
		if len(stored.MachineUIDs) != 1 || publication.JournalUID != string(journal.UID) || len(publication.RetirementCompletion) > 0 {
			t.Fatal("reservation lost or completion fabricated")
		}
		must(api.Get(ctx, client.ObjectKeyFromObject(mesh), mesh))
		if (len(mesh.Data[tunnel.PeerPublicKeyPrefix+child.Name]) == 0) != (stage >= 2) {
			t.Fatalf("stage %d: wrong publication ordering", stage)
		}
		if stage == 2 {
			publishedVersion = mesh.ResourceVersion
		}
		if stage > 2 && mesh.ResourceVersion != publishedVersion {
			t.Fatal("waiting rewrote mesh")
		}
		must(api.Get(ctx, client.ObjectKeyFromObject(group), current))
		if current.Status.PendingAction.Removing {
			t.Fatal("missing receipts authorized deletion")
		}
		must(api.Get(ctx, client.ObjectKeyFromObject(child), child))
		must(api.Get(ctx, client.ObjectKeyFromObject(machine), machine))
		if child.DeletionTimestamp != nil || machine.GetDeletionTimestamp() != nil {
			t.Fatal("waiting deleted owner")
		}
	}
	// Rejected evidence and renewed workload must leave both durable ownership
	// and the removal gate unchanged. This is API admission, not eviction testing.
	must(api.Get(ctx, client.ObjectKeyFromObject(index), index))
	saved := index.Data["inventory.json"]
	must(json.Unmarshal([]byte(saved), &inventory))
	publication := inventory.Publications[a.MachineUID]
	publication.RetirementCompletion = json.RawMessage(`{}`)
	inventory.Publications[a.MachineUID] = publication
	raw, err = json.Marshal(inventory)
	must(err)
	index.Data["inventory.json"] = string(raw)
	must(api.Update(ctx, index))
	reject := func() {
		t.Helper()
		current := &v1alpha1.ProvisionedNodeGroupClaim{}
		must(api.Get(ctx, client.ObjectKeyFromObject(group), current))
		r := &Reconciler{API: api, Workload: api, MachineGVK: gvk, RetirementAPI: core}
		handled, err := r.beginReceiveRemoval(ctx, current)
		if !handled || err == nil {
			t.Fatal("unsafe removal was not rejected", handled, err)
		}
		must(api.Get(ctx, client.ObjectKeyFromObject(group), current))
		if current.Status.PendingAction.Removing {
			t.Fatal("rejected state enabled removal")
		}
		must(api.Get(ctx, client.ObjectKeyFromObject(child), child))
		must(api.Get(ctx, client.ObjectKeyFromObject(machine), machine))
		if child.DeletionTimestamp != nil || machine.GetDeletionTimestamp() != nil {
			t.Fatal("rejection deleted owner")
		}
		must(api.Get(ctx, client.ObjectKeyFromObject(mesh), mesh))
		if mesh.ResourceVersion != publishedVersion {
			t.Fatal("rejection rewrote publication")
		}
	}
	reject()
	index.Data["inventory.json"] = saved
	must(api.Update(ctx, index))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "late-retirement-workload", Namespace: group.Namespace}, Spec: corev1.PodSpec{NodeName: node.Name, Containers: []corev1.Container{{Name: "workload", Image: "example.invalid/workload:test"}}}}
	must(api.Create(ctx, pod))
	reject()

}
