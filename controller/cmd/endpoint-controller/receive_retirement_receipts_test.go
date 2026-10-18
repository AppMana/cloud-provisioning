package main

import (
	"context"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Fake API identity/discovery tests only: include keyed and keyless sites plus
// a surviving cloud consumer. No departed worker objects exist. Unready nodes
// remain required; live pod authentication, native cleanup, journal admission
// and completion storage are tested separately, not established by this fixture.
func TestRetirementConsumerPolicies(t *testing.T) {
	ctx := context.Background()
	for _, fault := range []string{"none", "omitted", "duplicate", "replaced-node", "adoption", "provider", "architecture", "platform", "account", "daemon", "cloud-command", "mesh-revision"} {
		t.Run(fault, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			gv := schema.GroupVersion{Group: "cluster.x-k8s.io", Version: "v1beta2"}
			scheme.AddKnownTypeWithName(gv.WithKind("Machine"), &unstructured.Unstructured{})
			scheme.AddKnownTypeWithName(gv.WithKind("MachineList"), &unstructured.UnstructuredList{})
			mesh := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mesh", Namespace: "test", UID: "mesh", ResourceVersion: "1"}, Data: map[string][]byte{
				tunnel.NodePublicKeyPrefix + "site": []byte("site-key"), tunnel.SiteAddressesPrefix + "follower": []byte("10.0.0.3"), tunnel.PeerPublicKeyPrefix + "survivor": []byte("cloud-key"),
			}}
			machine := &unstructured.Unstructured{Object: map[string]any{"apiVersion": gv.String(), "kind": "Machine", "metadata": map[string]any{"name": "survivor", "namespace": "test", "uid": "surviving-machine", "annotations": map[string]any{"cloud-provisioning.appmana.com/wireguard-addr4": "10.100.0.2/24"}}, "spec": map[string]any{"providerID": "test://survivor"}, "status": map[string]any{"nodeRef": map[string]any{"name": "cloud"}}}}
			objects := []client.Object{mesh, machine, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tunnel.AdoptionSecretName("survivor"), Namespace: "test", UID: "adoption"}}}
			apiObjects := []runtime.Object{}
			for _, name := range []string{"site", "follower", "cloud"} {
				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name)}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{OperatingSystem: "linux", Architecture: "amd64"}}}
				if name == "cloud" {
					node.Spec.ProviderID = "test://survivor"
					node.Status.NodeInfo.Architecture = "arm64"
				}
				objects = append(objects, node.DeepCopy())
				if name == "cloud" {
					switch fault {
					case "replaced-node":
						node.UID = "replacement"
					case "architecture":
						node.Status.NodeInfo.Architecture = "unknown"
					case "platform":
						node.Status.NodeInfo.OperatingSystem = "windows"
					}
				}
				apiObjects = append(apiObjects, node)
			}
			if fault != "account" {
				apiObjects = append(apiObjects, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "test", UID: "account"}})
			}
			for _, name := range []string{"site-agent", "cloud-agent"} {
				if fault == "daemon" && name == "cloud-agent" {
					continue
				}
				apiObjects = append(apiObjects, &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test", UID: types.UID(name)}})
			}
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			r := &meshReconciler{reader: reader, secretNamespace: "test", secretName: "mesh", dialerServiceAccount: "agent", dialerDaemonSetName: "site-agent", dialerCloudDaemonSetName: "cloud-agent", dialerImage: "release-image"}
			resolver := attachment.MeshConsumerResolver{Reader: reader, Namespace: "test", SecretName: "mesh", SecretUID: "mesh"}
			retained, err := resolver.PublishedConsumers(ctx, mesh)
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "omitted":
				retained = retained[:2]
			case "duplicate":
				retained = append(retained, retained[0])
			case "adoption":
				secret := &corev1.Secret{}
				if err := reader.Get(ctx, client.ObjectKey{Namespace: "test", Name: tunnel.AdoptionSecretName("survivor")}, secret); err != nil {
					t.Fatal(err)
				}
				secret.UID = "replacement"
				if err := reader.Update(ctx, secret); err != nil {
					t.Fatal(err)
				}
			case "provider":
				if err := reader.Get(ctx, client.ObjectKeyFromObject(machine), machine); err != nil {
					t.Fatal(err)
				}
				if err := unstructured.SetNestedField(machine.Object, "test://different", "spec", "providerID"); err != nil {
					t.Fatal(err)
				}
				if err := reader.Update(ctx, machine); err != nil {
					t.Fatal(err)
				}
			case "cloud-command":
				r.dialerCloudImage = "override"
			case "mesh-revision":
				mesh.ResourceVersion = "stale"
			}
			policy := receiveReceiptPolicy{ImageIDs: map[string]string{"amd64": "amd-release", "arm64": "arm-release"}}
			policies, err := r.retirementConsumerPolicies(ctx, kubefake.NewClientset(apiObjects...), policy, mesh, retained)
			if fault != "none" {
				if err == nil {
					t.Fatal("changed consumer or policy admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(policies) != 3 || policies["follower"].DaemonSetName != "site-agent" || policies["cloud"].DaemonSetName != "cloud-agent" || policies["cloud"].ImageID != "arm-release" || policies["site"].ImageID != "amd-release" {
				t.Fatal("lost participant or architecture policy")
			}
		})
	}
}
