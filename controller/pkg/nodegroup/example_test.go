package nodegroup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	"github.com/appmana/cloud-provisioning/controller/pkg/claim"
	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	joinaws "github.com/appmana/cloud-provisioning/controller/pkg/join/aws"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

func TestExamplesNodeGroupManifest(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "examples", "node-group.yaml"))
	if err != nil {
		t.Fatalf("reading examples/node-group.yaml: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	capiGV := schema.GroupVersion{Group: "cluster.x-k8s.io", Version: "v1beta2"}
	for _, kind := range []string{"Cluster", "Machine"} {
		scheme.AddKnownTypeWithName(capiGV.WithKind(kind), &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(capiGV.WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	infraGV := schema.GroupVersion{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2"}
	for _, kind := range []string{"AWSCluster", "AWSMachine", "AWSMachineTemplate", "AWSClusterStaticIdentity", "AWSClusterRoleIdentity"} {
		scheme.AddKnownTypeWithName(infraGV.WithKind(kind), &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(infraGV.WithKind(kind+"List"), &unstructured.UnstructuredList{})
	}
	mapper := apimeta.NewDefaultRESTMapper(scheme.PrioritizedVersionsAllGroups())
	for gvk := range scheme.AllKnownTypes() {
		scope := apimeta.RESTScopeNamespace
		if gvk.Kind == "Node" || gvk.Kind == "Namespace" || gvk.Kind == "AWSClusterStaticIdentity" || gvk.Kind == "AWSClusterRoleIdentity" {
			scope = apimeta.RESTScopeRoot
		}
		mapper.Add(gvk, scope)
	}

	var initObjs []client.Object
	var group *v1alpha1.ProvisionedNodeGroupClaim
	for _, doc := range strings.Split(string(raw), "\n---") {
		var u unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &u.Object); err != nil {
			t.Fatalf("unmarshaling YAML doc: %v", err)
		}
		if len(u.Object) == 0 || u.GetKind() == "" {
			continue
		}
		switch u.GetKind() {
		case "ScaledObject":
			continue
		case "Secret":
			var s corev1.Secret
			if err := yaml.Unmarshal([]byte(doc), &s); err != nil {
				t.Fatalf("unmarshaling Secret: %v", err)
			}
			if s.Data == nil && len(s.StringData) > 0 {
				s.Data = map[string][]byte{}
				for k, v := range s.StringData {
					s.Data[k] = []byte(v)
				}
			}
			initObjs = append(initObjs, &s)
		case "ProvisionedNodeGroupClaim":
			var g v1alpha1.ProvisionedNodeGroupClaim
			if err := yaml.Unmarshal([]byte(doc), &g); err != nil {
				t.Fatalf("unmarshaling ProvisionedNodeGroupClaim: %v", err)
			}
			g.UID = "uid-aws-worker-pool"
			g.Generation = 1
			initObjs = append(initObjs, &g)
			group = &g
		default:
			initObjs = append(initObjs, &u)
		}
	}
	if group == nil {
		t.Fatal("expected ProvisionedNodeGroupClaim in examples/node-group.yaml")
	}

	initObjs = append(initObjs, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "onprem-0"},
		Status:     corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: "v1.36.2+k0s"}},
	})

	uCluster := &unstructured.Unstructured{}
	uCluster.SetGroupVersionKind(capiGV.WithKind("Cluster"))
	uAWSCluster := &unstructured.Unstructured{}
	uAWSCluster.SetGroupVersionKind(infraGV.WithKind("AWSCluster"))

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRESTMapper(mapper).
		WithStatusSubresource(&v1alpha1.ProvisionedNodeGroupClaim{}, &v1alpha1.ProvisionedNodeClaim{}, uCluster, uAWSCluster).
		WithObjects(initObjs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					obj.SetUID(types.UID("uid-" + obj.GetName()))
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).
		Build()

	ngReconciler := &Reconciler{
		API:        c,
		Workload:   c,
		MachineGVK: capiGV.WithKind("Machine"),
		MeshName:   "tunnel-peers",
	}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: group.Namespace, Name: group.Name}}
	for step := 0; step < 15; step++ {
		if _, err := ngReconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("nodegroup Reconcile step %d: %v", step, err)
		}
		var children v1alpha1.ProvisionedNodeClaimList
		if err := c.List(ctx, &children, client.InNamespace(group.Namespace)); err != nil {
			t.Fatal(err)
		}
		for i := range children.Items {
			ch := &children.Items[i]
			if ch.UID == "" {
				ch.UID = types.UID("uid-" + ch.Name)
				if err := c.Update(ctx, ch); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	var updatedGroup v1alpha1.ProvisionedNodeGroupClaim
	if err := c.Get(ctx, req.NamespacedName, &updatedGroup); err != nil {
		t.Fatal(err)
	}
	if updatedGroup.Status.Replicas != 3 {
		t.Fatalf("Status.Replicas = %d, want 3", updatedGroup.Status.Replicas)
	}
	if updatedGroup.Status.Selector != "app=batch-worker" {
		t.Fatalf("Status.Selector = %q, want \"app=batch-worker\"", updatedGroup.Status.Selector)
	}

	var children v1alpha1.ProvisionedNodeClaimList
	if err := c.List(ctx, &children, client.InNamespace(group.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(children.Items) != 3 {
		t.Fatalf("child ProvisionedNodeClaim count = %d, want 3", len(children.Items))
	}

	awsProv := joinaws.Provider{}
	claimReconciler := &claim.Reconciler{
		Client:                    c,
		Reader:                    c,
		Provisioners:              []join.MachineProvisioner{awsProv},
		RoleLabel:                 "cloud-provisioning.appmana.com/role",
		RoleValue:                 "cloud-worker",
		BootstrapSecretNameFormat: "%s-bootstrap",
		TunnelInterface:           "cldt0",
	}
	for i := range children.Items {
		ch := &children.Items[i]
		if _, err := claimReconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ch)}); err != nil {
			t.Fatalf("claim Reconcile(%s): %v", ch.Name, err)
		}
		awsMachine := &unstructured.Unstructured{}
		awsMachine.SetGroupVersionKind(awsProv.GVK())
		if err := c.Get(ctx, client.ObjectKeyFromObject(ch), awsMachine); err != nil {
			t.Fatalf("expected AWSMachine for %s: %v", ch.Name, err)
		}
		if err := awsProv.Validate(ctx, c, awsMachine); err != nil {
			t.Fatalf("aws.Provider.Validate(%s): %v", ch.Name, err)
		}
	}
}
