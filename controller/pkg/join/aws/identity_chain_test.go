package aws

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func identityFixture(t *testing.T, identityRef map[string]any, secretData map[string][]byte, extra ...client.Object) (client.Client, *unstructured.Unstructured) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(clusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(awsClusterGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(awsClusterStaticIdentityGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(awsClusterRoleIdentityGVK, &unstructured.Unstructured{})
	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(gvk)
	machine.SetName("worker")
	machine.SetNamespace("default")
	machine.SetLabels(map[string]string{clusterNameLabel: "site"})
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(clusterGVK)
	cluster.SetName("site")
	cluster.SetNamespace("default")
	_ = unstructured.SetNestedField(cluster.Object, "AWSCluster", "spec", "infrastructureRef", "kind")
	_ = unstructured.SetNestedField(cluster.Object, "site", "spec", "infrastructureRef", "name")
	awsCluster := &unstructured.Unstructured{}
	awsCluster.SetGroupVersionKind(awsClusterGVK)
	awsCluster.SetName("site")
	awsCluster.SetNamespace("default")
	_ = unstructured.SetNestedMap(awsCluster.Object, identityRef, "spec", "identityRef")
	static := &unstructured.Unstructured{}
	static.SetGroupVersionKind(awsClusterStaticIdentityGVK)
	static.SetName("capa-source")
	_ = unstructured.SetNestedField(static.Object, "capa-source-keys", "spec", "secretRef")
	objects := append([]client.Object{cluster, awsCluster, static,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "capa-source-keys", Namespace: managerNamespace}, Data: secretData}}, extra...)
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), machine
}

func roleIdentity(source string) *unstructured.Unstructured {
	role := &unstructured.Unstructured{}
	role.SetGroupVersionKind(awsClusterRoleIdentityGVK)
	role.SetName("capa")
	_ = unstructured.SetNestedField(role.Object, "arn:aws:iam::123456789012:role/capa", "spec", "roleARN")
	_ = unstructured.SetNestedMap(role.Object, map[string]any{"kind": "AWSClusterStaticIdentity", "name": source}, "spec", "sourceIdentityRef")
	return role
}

var longLived = map[string][]byte{"AccessKeyID": []byte("AKIA"), "SecretAccessKey": []byte("secret")}

// CAPA's credentials must not expire unattended: a static identity holding
// an STS session stops working when the session does and nothing renews
// it. CAPA assumes and refreshes a role itself through an
// AWSClusterRoleIdentity whose source holds long-lived keys.
func TestCAPAIdentityMustNotExpireUnattended(t *testing.T) {
	session := map[string][]byte{"AccessKeyID": []byte("ASIA"), "SecretAccessKey": []byte("secret"), "SessionToken": []byte("token")}
	for name, tc := range map[string]struct {
		ref    map[string]any
		secret map[string][]byte
		extra  []client.Object
		ok     bool
	}{
		"static long-lived":       {map[string]any{"kind": "AWSClusterStaticIdentity", "name": "capa-source"}, longLived, nil, true},
		"static session":          {map[string]any{"kind": "AWSClusterStaticIdentity", "name": "capa-source"}, session, nil, false},
		"role over long-lived":    {map[string]any{"kind": "AWSClusterRoleIdentity", "name": "capa"}, longLived, []client.Object{roleIdentity("capa-source")}, true},
		"role over session":       {map[string]any{"kind": "AWSClusterRoleIdentity", "name": "capa"}, session, []client.Object{roleIdentity("capa-source")}, false},
		"role over absent source": {map[string]any{"kind": "AWSClusterRoleIdentity", "name": "capa"}, longLived, []client.Object{roleIdentity("missing")}, false},
	} {
		c, machine := identityFixture(t, tc.ref, tc.secret, tc.extra...)
		err := (Provider{}).Validate(context.Background(), c, machine)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
		if strings.Contains(name, "session") && (err == nil || !strings.Contains(err.Error(), "AWSClusterRoleIdentity")) {
			t.Errorf("%s: the refusal does not name the remedy: %v", name, err)
		}
	}
}
