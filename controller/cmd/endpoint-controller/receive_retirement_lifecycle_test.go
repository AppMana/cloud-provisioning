package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reuse the preparation fixture's completed journal in separate real API
// storage. Owner/consumer objects, TokenReview answers and native observations
// are synthetic. This checks controller dispatch and write-side ownership
// fencing, not provider termination, token cryptography or native continuity.
func verifyNormalRetirementControllerLifecycle(t *testing.T, r *meshReconciler, owners client.Client, mesh *corev1.Secret, store handover.Store, plan tunnel.ReceiveTransitionPlan) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(v1alpha1.AddToScheme(owners.Scheme()))
	r.dialerServiceAccount, r.dialerDaemonSetName = "normal-agent", "normal-agent"
	r.dialerCloudDaemonSetName = "normal-agent"
	r.dialerImage = "test.invalid/agent@sha256:" + strings.Repeat("a", 64)
	policy := receiveReceiptPolicy{BindAddress: ":9444", CertFile: "/tls/cert", KeyFile: "/tls/key",
		ImageIDs: map[string]string{"amd64": r.dialerImage}, Endpoint: "https://receipts.invalid/v1/receipts", TrustSecret: "trust",
		FinalizeRetirements: true, ReleaseRetirements: true}
	must(policy.validate())
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: r.dialerServiceAccount, Namespace: mesh.Namespace, UID: "normal-account"}}
	daemon := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: r.dialerDaemonSetName, Namespace: mesh.Namespace, UID: "normal-daemon"},
		Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dialer", Image: r.dialerImage}}}}}}
	objects := []runtime.Object{mesh.DeepCopy(), sa, daemon}
	resolver := attachment.MeshConsumerResolver{Reader: owners, Namespace: mesh.Namespace, SecretName: mesh.Name, SecretUID: string(mesh.UID)}
	consumers, err := resolver.PublishedConsumers(ctx, mesh)
	must(err)
	entries := []handover.PodReceipt{}
	reviews := map[string]authenticationv1.TokenReviewStatus{}
	for i, consumer := range consumers {
		node := &corev1.Node{}
		must(owners.Get(ctx, client.ObjectKey{Name: consumer.NodeName}, node))
		node.Status.NodeInfo = corev1.NodeSystemInfo{Architecture: "amd64", OperatingSystem: "linux", BootID: "normal-boot"}
		node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
		objects = append(objects, node)
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("normal-agent-%d", i), Namespace: mesh.Namespace, UID: types.UID(fmt.Sprintf("normal-pod-%d", i)), OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(daemon, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))}},
			Spec:   corev1.PodSpec{NodeName: node.Name, ServiceAccountName: sa.Name, Containers: []corev1.Container{{Name: "dialer", Image: r.dialerImage}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "dialer", Ready: true, ImageID: r.dialerImage, ContainerID: "containerd://normal", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-time.Minute))}}}}}}
		objects = append(objects, pod)
		token := fmt.Sprintf("synthetic-normal-token-%d", i)
		reviews[token] = authenticationv1.TokenReviewStatus{Authenticated: true, Audiences: []string{receiveReceiptAudience}, User: authenticationv1.UserInfo{Username: "system:serviceaccount:" + mesh.Namespace + ":" + sa.Name, UID: string(sa.UID), Extra: map[string]authenticationv1.ExtraValue{
			"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)},
			"authentication.kubernetes.io/node-name": {node.Name}, "authentication.kubernetes.io/node-uid": {string(node.UID)},
		}}}
		entries = append(entries, handover.PodReceipt{BearerToken: token, BootID: node.Status.NodeInfo.BootID, Receipt: handover.Receipt{NodeUID: string(node.UID), ObservedBootID: node.Status.NodeInfo.BootID, AgentPodName: pod.Name, AgentPodUID: string(pod.UID), AgentContainerName: "dialer", AgentContainerID: "containerd://normal"}})
	}
	api := kubefake.NewClientset(objects...)
	api.PrependReactor("create", "tokenreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		request := action.(clienttesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		return true, &authenticationv1.TokenReview{Status: reviews[request.Spec.Token]}, nil
	})
	read := func() (tunnel.ReceiveInventory, string) {
		inventory, index, err := tunnel.ReadReceiveInventory(ctx, store.ConfigMaps, store.ClusterUID, mesh.Namespace, mesh.Name, string(mesh.UID))
		must(err)
		return inventory, index.ResourceVersion
	}
	collection := func(p receiveReceiptPolicy) handover.ReceiptCollection {
		c, err := r.resolveReceiveReceiptCollection(ctx, api, p, plan.MachineUID)
		must(err)
		for i := range entries {
			entries[i].Receipt.RoundHash, entries[i].Receipt.ObservedAt = c.Digest, time.Now()
		}
		return c
	}
	complete := func(c handover.ReceiptCollection) {
		must(c.Observe(ctx, entries))
		must(c.Complete(ctx, entries))
	}
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	machine.SetName(plan.MachineName)
	machine.SetNamespace(mesh.Namespace)
	machine.SetUID(types.UID(plan.MachineUID))
	retiredOwners := []client.Object{
		&v1alpha1.ProvisionedNodeClaim{ObjectMeta: metav1.ObjectMeta{Name: plan.MachineName, Namespace: mesh.Namespace, UID: "normal-claim"}},
		machine,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: plan.CloudNodeName, UID: types.UID(plan.CloudNodeUID)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: plan.AdoptionName, Namespace: mesh.Namespace, UID: types.UID(plan.AdoptionUID)}},
	}
	for _, owner := range retiredOwners {
		must(owners.Create(ctx, owner))
		complete(collection(policy))
		inventory, _ := read()
		pub := inventory.Publications[plan.MachineUID]
		if len(pub.RetirementCompletion) == 0 || len(pub.RetirementFinalization) != 0 || len(inventory.Released) != 0 {
			t.Fatal("existing owner did not retain renewable completion")
		}
		must(owners.Delete(ctx, owner))
	}
	t.Run("retire-captured-adoption", func(t *testing.T) {
		verifyRetiredAdoptionCleanup(t, r, owners, api, mesh, store, plan, policy, retiredOwners, func() { complete(collection(policy)) })
	})
	// Existing orphan-only opt-in must not silently expand to normal deletion.
	orphanOnly := policy
	orphanOnly.FinalizeRetirements, orphanOnly.ReleaseRetirements = false, false
	orphanOnly.FinalizeOrphanRetirements, orphanOnly.ReleaseOrphanRetirements = true, true
	complete(collection(orphanOnly))
	inventory, _ := read()
	if len(inventory.Publications[plan.MachineUID].RetirementFinalization) != 0 {
		t.Fatal("orphan-only policy finalized a normal withdrawal")
	}
	r.reader = retirementUnavailableReader{Reader: owners}
	_, beforeUnavailable := read()
	if _, err := r.resolveReceiveReceiptCollection(ctx, api, policy, plan.MachineUID); err == nil {
		t.Fatal("failed owner read admitted retirement")
	}
	_, afterUnavailable := read()
	if beforeUnavailable != afterUnavailable {
		t.Fatal("failed owner read mutated retirement")
	}
	r.reader = owners
	rejectReplacement := func(c handover.ReceiptCollection) {
		for _, owner := range retiredOwners {
			replacement := owner.DeepCopyObject().(client.Object)
			replacement.SetResourceVersion("")
			replacement.SetUID("replacement-owner")
			_, before := read()
			must(owners.Create(ctx, replacement))
			if err := c.Observe(ctx, entries); err == nil {
				t.Fatal("replacement owner admitted an observation")
			}
			if err := c.Complete(ctx, entries); err == nil {
				t.Fatal("replacement owner authorized mutation")
			}
			_, after := read()
			if before != after {
				t.Fatal("rejected owner race changed retirement storage")
			}
			must(owners.Delete(ctx, replacement))
		}
	}
	rejectReplacement(collection(policy))
	complete(collection(policy))
	inventory, _ = read()
	if len(inventory.Publications[plan.MachineUID].RetirementFinalization) == 0 || len(inventory.Released) != 0 {
		t.Fatal("normal finalization did not retain its reservation")
	}
	rejectReplacement(collection(policy))
	release := collection(policy)
	complete(release)
	inventory, _ = read()
	if len(inventory.MachineUIDs) != 0 || len(inventory.Publications) != 0 || len(inventory.Released) != 1 {
		t.Fatal("normal lifecycle did not release its finalized reservation")
	}
	if err := release.Complete(ctx, entries); err == nil {
		t.Fatal("consumed release collection replayed")
	}
}

type retirementUnavailableReader struct{ client.Reader }

func (r retirementUnavailableReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*v1alpha1.ProvisionedNodeClaim); ok {
		return fmt.Errorf("injected claim read failure")
	}
	return r.Reader.Get(ctx, key, object, options...)
}
