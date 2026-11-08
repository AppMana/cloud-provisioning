package nodegroup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	attachmentaws "github.com/appmana/cloud-provisioning/controller/pkg/attachment/aws"
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
	var groups []*v1alpha1.ProvisionedNodeGroupClaim
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
			g.UID = types.UID("uid-" + g.Name)
			g.Generation = 1
			initObjs = append(initObjs, &g)
			groups = append(groups, &g)
		default:
			initObjs = append(initObjs, &u)
		}
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 ProvisionedNodeGroupClaims (IPv6 pool + IPv4 gateway) in examples/node-group.yaml, got %d", len(groups))
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
	for _, group := range groups {
		req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: group.Namespace, Name: group.Name}}
		for step := 0; step < 15; step++ {
			if _, err := ngReconciler.Reconcile(ctx, req); err != nil {
				t.Fatalf("nodegroup Reconcile(%s) step %d: %v", group.Name, step, err)
			}
		}
	}

	var ipv6Group, ipv4Group v1alpha1.ProvisionedNodeGroupClaim
	if err := c.Get(ctx, types.NamespacedName{Namespace: "cloud-provisioning", Name: "aws-worker-pool"}, &ipv6Group); err != nil {
		t.Fatal(err)
	}
	if ipv6Group.Status.Replicas != 3 || ipv6Group.Status.Selector != "app=batch-worker" {
		t.Fatalf("aws-worker-pool Status = %+v, want Replicas=3 Selector=app=batch-worker", ipv6Group.Status)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "cloud-provisioning", Name: "aws-ipv4-gateway"}, &ipv4Group); err != nil {
		t.Fatal(err)
	}
	if ipv4Group.Status.Replicas != 1 {
		t.Fatalf("aws-ipv4-gateway Status.Replicas = %d, want 1", ipv4Group.Status.Replicas)
	}

	var children v1alpha1.ProvisionedNodeClaimList
	if err := c.List(ctx, &children, client.InNamespace("cloud-provisioning")); err != nil {
		t.Fatal(err)
	}
	if len(children.Items) != 4 {
		t.Fatalf("child ProvisionedNodeClaim count = %d, want 4 (3 IPv6 + 1 IPv4)", len(children.Items))
	}

	awsProv := joinaws.Provider{
		EC2: func(context.Context, string) (attachmentaws.API, error) {
			return exampleEC2{}, nil
		},
	}
	claimReconciler := &claim.Reconciler{
		Client:                    c,
		Reader:                    c,
		Provisioners:              []join.MachineProvisioner{awsProv},
		RoleLabel:                 "cloud-provisioning.appmana.com/role",
		RoleValue:                 "cloud-worker",
		BootstrapSecretNameFormat: "%s-bootstrap",
		TunnelInterface:           "cldt0",
	}
	ipv6Count := 0
	ipv4Count := 0
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
		sgs, _, _ := unstructured.NestedSlice(awsMachine.Object, "spec", "additionalSecurityGroups")
		if len(sgs) != 1 {
			t.Fatalf("%s additionalSecurityGroups = %v, want 1 auto-provisioned security group", ch.Name, sgs)
		}
		pubIP, _, _ := unstructured.NestedBool(awsMachine.Object, "spec", "publicIP")
		assignV6, _, _ := unstructured.NestedString(awsMachine.Object, "spec", "assignPrimaryIPv6")
		if strings.HasPrefix(ch.Name, "aws-worker-pool-") {
			if pubIP || assignV6 != "enabled" {
				t.Fatalf("%s: publicIP=%v assignPrimaryIPv6=%q, want publicIP=false assignPrimaryIPv6=enabled", ch.Name, pubIP, assignV6)
			}
			ipv6Count++
		} else if strings.HasPrefix(ch.Name, "aws-ipv4-gateway-") {
			if !pubIP {
				t.Fatalf("%s: publicIP=%v, want true for explicit IPv4 group", ch.Name, pubIP)
			}
			ipv4Count++
		}
	}
	if ipv6Count != 3 || ipv4Count != 1 {
		t.Fatalf("ipv6Count=%d ipv4Count=%d, want 3 and 1", ipv6Count, ipv4Count)
	}
}

type exampleEC2 struct{}

func (exampleEC2) Call(_ context.Context, _, op string, input map[string]any) (json.RawMessage, error) {
	switch op {
	case "describe-vpcs":
		return json.Marshal(map[string]any{
			"Vpcs": []map[string]any{{"VpcId": "vpc-0default", "IsDefault": true, "State": "available"}},
		})
	case "describe-subnets":
		return json.Marshal(map[string]any{
			"Subnets": []map[string]any{{"SubnetId": "subnet-0public", "VpcId": "vpc-0default", "MapPublicIpOnLaunch": true}},
		})
	case "create-security-group":
		return json.Marshal(map[string]any{"GroupId": "sg-auto-site-aws"})
	case "describe-security-groups":
		if _, byID := input["GroupIds"]; byID {
			return json.Marshal(map[string]any{
				"SecurityGroups": []map[string]any{
					{
						"GroupId": "sg-auto-site-aws",
						"IpPermissions": []map[string]any{
							{"IpProtocol": "-1", "UserIdGroupPairs": []map[string]any{{"GroupId": "sg-auto-site-aws"}}},
						},
					},
				},
			})
		}
		return json.Marshal(map[string]any{"SecurityGroups": []any{}})
	default:
		return json.Marshal(map[string]any{})
	}
}
