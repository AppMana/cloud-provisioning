package claim

import (
	"context"
	"encoding/json"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
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
// must stop compute deletion. A separate valid-history fixture covers real pod
// authentication and the CAPI deletion request. Native observations and agent
// status are synthetic; no live CAPI, interruption bound or orphan recovery is
// established by this test.
func verifyStandaloneReceiveDeletion(t *testing.T, api client.Client, core kubernetes.Interface, complete bool) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	name := "reserved-standalone"
	if complete {
		name += "-completion"
	}
	claim := fakeClaim(name)
	claim.Finalizers = []string{Finalizer}
	must(api.Create(ctx, claim))
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: claim.Name}, Spec: corev1.NodeSpec{ProviderID: "test:///standalone"}}
	must(api.Create(ctx, node))
	site := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name + "-site"}}
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
	key := func(b byte) string { var k wgtypes.Key; k[0] = b; return k.String() }
	root, siteKey := key(2), key(1)
	mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name + "-mesh", Namespace: claim.Namespace}, Data: map[string][]byte{tunnel.PeerPublicKeyPrefix + claim.Name: []byte(root), tunnel.NodePublicKeyPrefix + site.Name: []byte(siteKey), tunnel.PeerEndpointPrefix + claim.Name: []byte("198.51.100.1:51820"), tunnel.PeerAllowedIPsPrefix + claim.Name: []byte("172.29.0.1/32"), tunnel.PeerRouteHostsPrefix + claim.Name: []byte("172.29.0.1")}}
	must(api.Create(ctx, mesh))
	cluster, err := core.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	must(err)
	inventory := tunnel.ReceiveInventory{Version: 1, ClusterUID: string(cluster.UID), Namespace: mesh.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), MachineUIDs: []string{string(machine.GetUID())}, Publications: map[string]tunnel.ReceiveJournalPublication{string(machine.GetUID()): {JournalUID: "missing-journal"}}}
	var store handover.Store
	if complete {
		store = standaloneRetirementJournal(t, api, core, string(cluster.UID), mesh, machine, node, site, adoption, root, siteKey, key(3))
		inventory.Publications[string(machine.GetUID())] = tunnel.ReceiveJournalPublication{JournalUID: store.ExpectedUID}
	}
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
	if complete {
		verifyStandalonePendingTransition(t, api, core, claim, machine, mesh, index, store)
		verifyStandaloneCompletion(t, api, core, claim, machine, mesh, index, store, site, step)
		return
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
	if string(mesh.Data[tunnel.PeerPublicKeyPrefix+claim.Name]) != root {
		t.Fatal("failed retirement removed peer")
	}
}

// Synthetic unfinished histories exercise the real deletion reconciler and API
// persistence. Pending work must use its fixed retry without freezing a journal,
// withdrawing publication or requesting compute deletion. This does not prove
// native transition progress or recovery after an actual participant outage.
func verifyStandalonePendingTransition(t *testing.T, api client.Client, core kubernetes.Interface, claim *v1alpha1.ProvisionedNodeClaim, machine *unstructured.Unstructured, mesh *corev1.Secret, index *corev1.ConfigMap, store handover.Store) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Load(ctx)
	must(err)
	plan, err := tunnel.DecodeReceiveTransition(snapshot, store.ClusterUID)
	must(err)
	journal, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
	must(err)
	spec, err := tunnel.ReceiveTransitionSpec(plan, store.ClusterUID, tunnel.HashPeerList([]byte(journal.Data["plan.json"])))
	must(err)
	completed := journal.Data["transition.json"]
	defer func() {
		current, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
		must(err)
		current.Data["transition.json"] = completed
		_, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
		must(err)
	}()
	for _, scenario := range []string{"prepare", "switch", "drain", "retire", "abort", "expired-prepare", "malformed"} {
		t.Run("pending-transition/"+scenario, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			at := time.Now()
			if scenario == "expired-prepare" {
				at = at.Add(-time.Hour)
			}
			tr, err := handover.NewTransition(spec, at, handover.Policy{RoundLifetime: time.Minute, MaxReceiptAge: 30 * time.Second, DrainInterval: time.Second})
			must(err)
			if scenario == "abort" {
				tr, err = tr.Rollback(at.Add(time.Second))
				must(err)
			}
			for advances := 0; scenario == "switch" || scenario == "drain" || scenario == "retire"; advances++ {
				view, err := tr.View()
				must(err)
				if string(view.Round.Phase) == scenario {
					break
				}
				if advances > 5 || view.Complete {
					t.Fatal("fixture did not reach pending phase", scenario)
				}
				at = view.Round.IssuedAt.Add(time.Second)
				if at.Before(view.DrainUntil) {
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
			raw, err := tr.Bytes()
			must(err)
			if scenario == "malformed" {
				raw = []byte(`{"invalid":true}`)
			}
			current, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
			must(err)
			current.Data["transition.json"] = string(raw)
			current, err = store.ConfigMaps.Update(ctx, current, metav1.UpdateOptions{})
			must(err)
			must(api.Get(ctx, client.ObjectKeyFromObject(claim), claim))
			must(api.Get(ctx, client.ObjectKeyFromObject(machine), machine))
			must(api.Get(ctx, client.ObjectKeyFromObject(mesh), mesh))
			must(api.Get(ctx, client.ObjectKeyFromObject(index), index))
			versions := []string{claim.ResourceVersion, machine.GetResourceVersion(), mesh.ResourceVersion, index.ResourceVersion}
			r := &Reconciler{Client: api, Reader: api, Workload: api, RetirementAPI: core, MeshName: mesh.Name}
			for attempt := 0; attempt < 2; attempt++ {
				result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(claim)})
				if scenario == "malformed" {
					if err == nil {
						t.Fatal("malformed transition treated as pending")
					}
				} else if err != nil || result.RequeueAfter != 5*time.Second {
					t.Fatalf("unfinished transition must retry in five seconds without error: %+v, %v", result, err)
				}
				after, err := store.ConfigMaps.Get(ctx, store.Name, metav1.GetOptions{})
				must(err)
				if after.ResourceVersion != current.ResourceVersion || (after.Immutable != nil && *after.Immutable) {
					t.Fatal("pending deletion mutated or froze the journal")
				}
				must(api.Get(ctx, client.ObjectKeyFromObject(claim), claim))
				must(api.Get(ctx, client.ObjectKeyFromObject(machine), machine))
				must(api.Get(ctx, client.ObjectKeyFromObject(mesh), mesh))
				must(api.Get(ctx, client.ObjectKeyFromObject(index), index))
				if claim.ResourceVersion != versions[0] || machine.GetResourceVersion() != versions[1] || mesh.ResourceVersion != versions[2] || index.ResourceVersion != versions[3] || machine.GetDeletionTimestamp() != nil {
					t.Fatal("pending deletion changed authority, publication or compute")
				}
			}
		})
	}
}

// Construct synthetic completed transport history; no transport receipts here
// come from native agents. Retirement receipts below use real TokenReview.
func standaloneRetirementJournal(t *testing.T, api client.Client, core kubernetes.Interface, clusterUID string, mesh *corev1.Secret, machine *unstructured.Unstructured, node, site *corev1.Node, adoption *corev1.Secret, root, siteKey, receiverKey string) handover.Store {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	peers := tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: siteKey, WGAllowedIPs: []string{"10.90.0.0/24"}}}}
	base, err := json.Marshal(peers)
	must(err)
	plan := tunnel.ReceiveTransitionPlan{Version: 1, Namespace: mesh.Namespace, MeshName: mesh.Name, MeshUID: string(mesh.UID), MachineName: machine.GetName(), MachineUID: string(machine.GetUID()), AdoptionName: tunnel.AdoptionSecretName(machine.GetName()), AdoptionUID: string(adoption.UID), CloudNodeName: node.Name, CloudNodeUID: string(node.UID), RootPublicKey: root, Base: base, Desired: peers, Native: []netip.Prefix{}, Ports: map[string]int{}, Bindings: map[string]tunnel.ReceiveBinding{}, Sites: map[string]tunnel.ReceiveTransitionSite{}}
	plan.SiteDesired = tunnel.PeerListDoc{Peers: []tunnel.PeerSpec{{PublicKey: root, Remote: true, Endpoint: "198.51.100.1:51820", WGAllowedIPs: []string{"172.29.0.1/32"}}}}
	prep, err := tunnel.PlanReceiveHandover(plan.MeshUID, peers, peers, nil)
	must(err)
	for _, path := range prep.Receivers {
		binding := tunnel.ReceiveBinding{Interface: path.Interface, PeerPublicKey: siteKey, PublicKey: receiverKey, ListenPort: 51821}
		plan.Bindings[siteKey] = binding
		plan.Ports[siteKey] = binding.ListenPort
		receiver, err := tunnel.PlanSiteReceiveClient(plan.MeshUID, siteKey, root, binding, plan.SiteDesired, plan.SiteDesired)
		must(err)
		plan.Sites[siteKey] = tunnel.ReceiveTransitionSite{NodeName: site.Name, NodeUID: string(site.UID), Client: receiver}
	}
	raw, err := json.Marshal(plan)
	must(err)
	spec, err := tunnel.ReceiveTransitionSpec(plan, clusterUID, tunnel.HashPeerList(raw))
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
	journal := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: tunnel.ReceiveTransitionName(string(machine.GetUID())), Namespace: mesh.Namespace}, Data: map[string]string{"plan.json": string(raw), "transition.json": string(history)}}
	must(api.Create(ctx, journal))
	return handover.Store{ConfigMaps: core.CoreV1().ConfigMaps(mesh.Namespace), ClusterUID: clusterUID, Name: journal.Name, ExpectedUID: string(journal.UID)}
}

func verifyStandaloneCompletion(t *testing.T, api client.Client, core kubernetes.Interface, claim *v1alpha1.ProvisionedNodeClaim, machine *unstructured.Unstructured, mesh *corev1.Secret, index *corev1.ConfigMap, store handover.Store, site *corev1.Node, step func() error) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	// Metadata changes between durable stages must not invalidate captured
	// network data or require recapturing the withdrawal's survivor identities.
	mesh.Annotations = map[string]string{"test/metadata": "after-capture"}
	must(api.Update(ctx, mesh))
	for stage := 0; stage < 4; stage++ {
		must(step())
		snapshot, err := store.Load(ctx)
		must(err)
		if !snapshot.Frozen() {
			t.Fatal("standalone journal not frozen")
		}
		inventory, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
		must(err)
		publication := inventory.Publications[string(machine.GetUID())]
		if (len(publication.Retirement) > 0) != (stage >= 1) {
			t.Fatal("incorrect standalone retirement persistence stage", stage)
		}
		if len(publication.RetirementCompletion) > 0 {
			t.Fatal("standalone fabricated cleanup completion")
		}
		must(api.Get(ctx, client.ObjectKeyFromObject(mesh), mesh))
		if (len(mesh.Data[tunnel.PeerPublicKeyPrefix+claim.Name]) == 0) != (stage >= 2) {
			t.Fatal("incorrect standalone publication stage", stage)
		}
		var record claimReceiveRemoval
		must(json.Unmarshal([]byte(claim.Annotations[receiveRemovalAnnotation]), &record))
		if record.Removing {
			t.Fatal("missing receipts committed standalone removal")
		}
	}
	const image = "example.invalid/dialer@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sa, err := core.CoreV1().ServiceAccounts(claim.Namespace).Create(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "retirement-agent"}}, metav1.CreateOptions{})
	must(err)
	labels := map[string]string{"app": "retirement-agent"}
	daemon, err := core.AppsV1().DaemonSets(claim.Namespace).Create(ctx, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "retirement-agent"}, Spec: appsv1.DaemonSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{ServiceAccountName: sa.Name, Containers: []corev1.Container{{Name: "dialer", Image: image}}}}}}, metav1.CreateOptions{})
	must(err)
	site.Status.NodeInfo.BootID = "retirement-boot"
	site.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	site, err = core.CoreV1().Nodes().UpdateStatus(ctx, site, metav1.UpdateOptions{})
	must(err)
	agent, err := core.CoreV1().Pods(claim.Namespace).Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "retirement-agent", Labels: labels, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(daemon, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}}, Spec: corev1.PodSpec{NodeName: site.Name, ServiceAccountName: sa.Name, Containers: []corev1.Container{{Name: "dialer", Image: image}}}}, metav1.CreateOptions{})
	must(err)
	agent.Status.Phase = corev1.PodRunning
	agent.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	agent.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "dialer", Ready: true, ContainerID: "containerd://retirement-agent", Image: image, ImageID: image, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}}}
	agent, err = core.CoreV1().Pods(claim.Namespace).UpdateStatus(ctx, agent, metav1.UpdateOptions{})
	must(err)
	policy := handover.PodReceiptAuthenticator{API: core, Audience: "cloud-provisioning-receipts", Namespace: claim.Namespace, ServiceAccountName: sa.Name, ServiceAccountUID: string(sa.UID), DaemonSetName: daemon.Name, DaemonSetUID: string(daemon.UID), ContainerName: "dialer", Image: image, ImageID: image}
	token, err := core.CoreV1().ServiceAccounts(claim.Namespace).CreateToken(ctx, sa.Name, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: []string{policy.Audience}, BoundObjectRef: &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: agent.Name, UID: agent.UID}}}, metav1.CreateOptions{})
	must(err)
	submit := func(bearer string) error {
		source, err := attachment.ReadReceiveRetirementSet(ctx, core, store, claim.Namespace, mesh.Name, string(mesh.UID), string(machine.GetUID()))
		if err != nil {
			return err
		}
		snapshot, err := store.Load(ctx)
		if err != nil {
			return err
		}
		scope, err := attachment.NewReceiveRetirementReceiptScope(ctx, store.ConfigMaps, store.ClusterUID, string(index.UID), snapshot, mesh)
		if err != nil {
			return err
		}
		entry := handover.PodReceipt{BearerToken: bearer, BootID: site.Status.NodeInfo.BootID, Receipt: handover.Receipt{NodeUID: string(site.UID), RoundHash: scope.Digest(), AgentContainerID: agent.Status.ContainerStatuses[0].ContainerID, ObservedAt: time.Now()}}
		return attachment.RecordReceiveRetirementWithPodReceipts(ctx, core, store, string(index.UID), scope.Digest(), mesh, snapshot, []handover.PodReceipt{entry}, map[string]handover.PodReceiptAuthenticator{string(site.UID): policy}, time.Minute, func() error { return source.Recheck(ctx, core, store) })
	}
	if err := submit("invalid-token"); err == nil {
		t.Fatal("invalid token persisted completion")
	}
	stored, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, claim.Namespace, mesh.Name, string(mesh.UID))
	must(err)
	if len(stored.Publications[string(machine.GetUID())].RetirementCompletion) != 0 {
		t.Fatal("failed authentication fabricated completion")
	}
	must(submit(token.Status.Token))
	// A boot change after successful authentication invalidates stored evidence.
	// Fresh authenticated observations may refresh it without releasing ownership.
	site.Status.NodeInfo.BootID = "retirement-reboot"
	site, err = core.CoreV1().Nodes().UpdateStatus(ctx, site, metav1.UpdateOptions{})
	must(err)
	if err := step(); err == nil {
		t.Fatal("stale boot evidence authorized standalone deletion")
	}
	must(submit(token.Status.Token))

	must(step())
	var record claimReceiveRemoval
	must(json.Unmarshal([]byte(claim.Annotations[receiveRemovalAnnotation]), &record))
	if !record.Removing {
		t.Fatal("authenticated cleanup did not commit standalone removal")
	}
	r := &Reconciler{Client: api, Reader: api, Workload: api, RetirementAPI: core, MeshName: mesh.Name}
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(claim)})
	must(err)
	must(api.Get(ctx, client.ObjectKeyFromObject(machine), machine))
	if machine.GetDeletionTimestamp() == nil {
		t.Fatal("committed standalone removal did not reach CAPI deletion")
	}
	inventory, _, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
	must(err)
	publication := inventory.Publications[string(machine.GetUID())]
	if len(inventory.MachineUIDs) != 1 || publication.JournalUID != store.ExpectedUID || len(publication.RetirementCompletion) == 0 {
		t.Fatal("standalone deletion lost cleanup proof or reservation")
	}
	raw, err := json.Marshal(inventory)
	must(err)
	if strings.Contains(string(raw), token.Status.Token) || strings.Contains(claim.Annotations[receiveRemovalAnnotation], token.Status.Token) {
		t.Fatal("retirement persisted bearer credential")
	}
}
