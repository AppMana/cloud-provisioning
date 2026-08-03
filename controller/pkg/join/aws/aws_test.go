package aws

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Provider never calls the AWS API itself -- it renders specs and reads
// what CAPA wrote. These tests exercise exactly that logic against fake
// objects; they deliberately do NOT touch real AWS or CAPA (that's a
// live-cluster/E2E concern, not a unit-test one).

func TestInfraValues_ContributesArchFromInstanceType(t *testing.T) {
	for instanceType, wantArch := range map[string]string{
		"t4g.small": "arm64",
		"m7g.large": "arm64",
		"t3.medium": "amd64",
		"c5.large":  "amd64",
	} {
		m := &unstructured.Unstructured{Object: map[string]any{
			"spec": map[string]any{"instanceType": instanceType},
		}}
		values, err := Provider{}.InfraValues(context.Background(), m)
		if err != nil {
			t.Fatalf("InfraValues(%s): %v", instanceType, err)
		}
		if values["arch"] != wantArch {
			t.Errorf("InfraValues(%s)[arch] = %v, want %s", instanceType, values["arch"], wantArch)
		}
	}
}

func TestInfraValues_NoInstanceType_ReturnsEmptyNonNilMap(t *testing.T) {
	// Before CAPA (or the claim reconciler) has set spec.instanceType
	// there is nothing to derive -- must be empty, not nil, not an
	// error.
	values, err := Provider{}.InfraValues(context.Background(), &unstructured.Unstructured{Object: map[string]any{}})
	if err != nil {
		t.Fatalf("InfraValues: %v", err)
	}
	if values == nil {
		t.Error("InfraValues returned nil, want an empty-but-non-nil map")
	}
	if len(values) != 0 {
		t.Errorf("InfraValues = %v, want empty when spec.instanceType is unset", values)
	}
}

func TestResolveInstanceType_SmallestFit(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  join.NodeRequest
		want string
	}{
		{"two-cpu-four-gi-arm", join.NodeRequest{Arch: "arm64", CPUMillis: 2000, MemoryBytes: 4 << 30}, "t4g.medium"},
		{"defaults-to-arm64", join.NodeRequest{CPUMillis: 1000, MemoryBytes: 1 << 30}, "t4g.micro"},
		{"amd64", join.NodeRequest{Arch: "amd64", CPUMillis: 2000, MemoryBytes: 2 << 30}, "t3.small"},
		{"memory-dominates", join.NodeRequest{Arch: "arm64", CPUMillis: 100, MemoryBytes: 12 << 30}, "t4g.xlarge"},
	} {
		got, err := Provider{}.ResolveInstanceType(tc.req)
		if err != nil {
			t.Fatalf("%s: ResolveInstanceType: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: ResolveInstanceType = %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestResolveInstanceType_Unsatisfiable_ReturnsError(t *testing.T) {
	_, err := Provider{}.ResolveInstanceType(join.NodeRequest{Arch: "arm64", CPUMillis: 128000, MemoryBytes: 1 << 40})
	if err == nil {
		t.Fatal("expected an error for a request no catalog entry satisfies, got nil")
	}
}

func configSecretClient(t *testing.T, data map[string][]byte) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "aws-provider-config", Namespace: "wg-dialer"},
		Data:       data,
	}).Build()
}

func TestInfraMachine_RendersSpecFromProviderConfig(t *testing.T) {
	c := configSecretClient(t, map[string][]byte{
		"ami-arm64":          []byte("ami-0abc"),
		"ssh-key-name":       []byte("ben"),
		"security-group-ids": []byte("sg-1, sg-2"),
		"subnet-id":          []byte("subnet-9"),
	})
	p := Provider{ConfigNamespace: "wg-dialer", ConfigName: "aws-provider-config"}

	obj, err := p.InfraMachine(context.Background(), c, "default", "t4g.small", join.NodeRequest{Arch: "arm64"})
	if err != nil {
		t.Fatalf("InfraMachine: %v", err)
	}
	if obj.GroupVersionKind() != p.GVK() {
		t.Errorf("GVK = %v, want %v", obj.GroupVersionKind(), p.GVK())
	}
	instanceType, _, _ := unstructured.NestedString(obj.Object, "spec", "instanceType")
	if instanceType != "t4g.small" {
		t.Errorf("spec.instanceType = %q, want t4g.small", instanceType)
	}
	ami, _, _ := unstructured.NestedString(obj.Object, "spec", "ami", "id")
	if ami != "ami-0abc" {
		t.Errorf("spec.ami.id = %q, want ami-0abc", ami)
	}
	publicIP, _, _ := unstructured.NestedBool(obj.Object, "spec", "publicIP")
	if !publicIP {
		t.Error("spec.publicIP = false, want true by default (a publicly reachable node is the point)")
	}
	skipSSM, _, _ := unstructured.NestedBool(obj.Object, "spec", "cloudInit", "insecureSkipSecretsManager")
	if !skipSSM {
		t.Error("spec.cloudInit.insecureSkipSecretsManager = false, want true by default")
	}
	groups, _, _ := unstructured.NestedSlice(obj.Object, "spec", "additionalSecurityGroups")
	if len(groups) != 2 {
		t.Errorf("spec.additionalSecurityGroups has %d entries, want 2", len(groups))
	}
	subnet, _, _ := unstructured.NestedString(obj.Object, "spec", "subnet", "id")
	if subnet != "subnet-9" {
		t.Errorf("spec.subnet.id = %q, want subnet-9", subnet)
	}
}

func TestInfraMachine_MissingAMIForArch_ReturnsActionableError(t *testing.T) {
	c := configSecretClient(t, map[string][]byte{"ami-amd64": []byte("ami-0xyz")})
	p := Provider{ConfigNamespace: "wg-dialer", ConfigName: "aws-provider-config"}

	_, err := p.InfraMachine(context.Background(), c, "default", "t4g.small", join.NodeRequest{Arch: "arm64"})
	if err == nil {
		t.Fatal("expected an error when the config has no AMI for the resolved arch, got nil")
	}
	if !strings.Contains(err.Error(), "ami-arm64") {
		t.Errorf("error %q must name the missing key ami-arm64", err.Error())
	}
}

// validateFixtures builds the full Cluster -> AWSCluster ->
// AWSClusterStaticIdentity chain Validate traces, mirroring the real
// live objects (example-cluster / cloud-worker) this check
// was written against. secretNamespace lets a test place the
// credentials Secret in the wrong place, matching the actual live bug
// caught: the Secret existed only in "default", not "capa-system"
// (CAPA's own manager namespace), and CAPA retried forever with an
// opaque "Secret ... not found".
func validateFixtures(t *testing.T, secretNamespace string) (client.Client, *unstructured.Unstructured) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	scheme.AddKnownTypeWithName(clusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(awsClusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(awsClusterStaticIdentityGVK, &unstructured.Unstructured{})

	awsMachine := &unstructured.Unstructured{}
	awsMachine.SetGroupVersionKind(gvk)
	awsMachine.SetName("cloud-worker-0")
	awsMachine.SetNamespace("default")
	awsMachine.SetLabels(map[string]string{clusterNameLabel: "example-cluster"})

	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetName("example-cluster")
	cluster.SetNamespace("default")
	_ = unstructured.SetNestedField(cluster.Object, "AWSCluster", "spec", "infrastructureRef", "kind")
	_ = unstructured.SetNestedField(cluster.Object, "example-cluster", "spec", "infrastructureRef", "name")

	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(awsClusterGVK)
	awsCluster.SetName("example-cluster")
	awsCluster.SetNamespace("default")
	_ = unstructured.SetNestedField(awsCluster.Object, "AWSClusterStaticIdentity", "spec", "identityRef", "kind")
	_ = unstructured.SetNestedField(awsCluster.Object, "cloud-worker", "spec", "identityRef", "name")

	identity := &unstructured.Unstructured{}
	identity.SetGroupVersionKind(awsClusterStaticIdentityGVK)
	identity.SetName("cloud-worker")
	_ = unstructured.SetNestedField(identity.Object, "cloud-worker-credentials", "spec", "secretRef")

	objs := []client.Object{cluster, awsCluster, identity}
	if secretNamespace != "" {
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "cloud-worker-credentials", Namespace: secretNamespace},
		})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return c, awsMachine
}

func TestValidate_SecretInWrongNamespace_ReturnsActionableError(t *testing.T) {
	// The exact live bug: the credentials Secret existed only in
	// "default" (the AWSCluster's own namespace), never in
	// "capa-system" (CAPA's manager namespace) -- CAPA's own error
	// ("Secret ... not found") gave no hint about WHERE it actually
	// needed to be.
	c, awsMachine := validateFixtures(t, "default")

	err := (Provider{}).Validate(context.Background(), c, awsMachine)
	if err == nil {
		t.Fatal("expected an error when the identity secret is in the wrong namespace, got nil")
	}
	for _, want := range []string{"cloud-worker-credentials", "capa-system", "default"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message %q missing %q -- must name the secret and both namespaces so it's actually actionable", err.Error(), want)
		}
	}
}

func TestValidate_SecretInManagerNamespace_ReturnsNil(t *testing.T) {
	c, awsMachine := validateFixtures(t, managerNamespace)

	if err := (Provider{}).Validate(context.Background(), c, awsMachine); err != nil {
		t.Errorf("Validate: %v, want nil when the secret is correctly placed in %s", err, managerNamespace)
	}
}

func TestValidate_SecretMissingEntirely_ReturnsError(t *testing.T) {
	c, awsMachine := validateFixtures(t, "")

	if err := (Provider{}).Validate(context.Background(), c, awsMachine); err == nil {
		t.Fatal("expected an error when the identity secret doesn't exist anywhere, got nil")
	}
}

func TestValidate_NoClusterNameLabel_ReturnsNil(t *testing.T) {
	// A brand new AWSMachine CAPI hasn't labeled yet -- nothing to
	// trace, not an error.
	awsMachine := &unstructured.Unstructured{}
	awsMachine.SetGroupVersionKind(gvk)
	awsMachine.SetName("some-machine")
	awsMachine.SetNamespace("default")

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding clientgoscheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	if err := (Provider{}).Validate(context.Background(), c, awsMachine); err != nil {
		t.Errorf("Validate: %v, want nil when there's no cluster-name label yet", err)
	}
}

func TestGVK_IsAWSMachineV1beta2(t *testing.T) {
	// v1beta2 is the storage version confirmed live against the real
	// the cluster cluster's installed CRD (kubectl get crd
	// awsmachines.infrastructure.cluster.x-k8s.io -o
	// jsonpath={.spec.versions}), not a guess.
	gvk := Provider{}.GVK()
	if gvk.Group != "infrastructure.cluster.x-k8s.io" || gvk.Version != "v1beta2" || gvk.Kind != "AWSMachine" {
		t.Errorf("GVK = %+v, want infrastructure.cluster.x-k8s.io/v1beta2, Kind=AWSMachine", gvk)
	}
}
