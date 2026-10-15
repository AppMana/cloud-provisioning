package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Use an actual Helm upgrade and API authorization: template-only rendering
// cannot exercise lookup or the grant reset between controller reconciles.
func TestDialerSecretAccessSurvivesHelmUpgrade(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("requires envtest API binaries")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("requires Helm")
	}
	ctx := context.Background()
	e := &envtest.Environment{}
	e.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	cfg, err := e.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := e.Stop(); err != nil {
			t.Error(err)
		}
	})
	admin, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range []string{"dialer-upgrade", "kube-system"} {
		if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	kube := clientcmdapi.Config{Clusters: map[string]*clientcmdapi.Cluster{"test": {Server: cfg.Host, CertificateAuthorityData: cfg.CAData}}, AuthInfos: map[string]*clientcmdapi.AuthInfo{"test": {ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData}}, Contexts: map[string]*clientcmdapi.Context{"test": {Cluster: "test", AuthInfo: "test"}}, CurrentContext: "test"}
	configPath := filepath.Join(dir, "kubeconfig")
	if err := clientcmd.WriteToFile(kube, configPath); err != nil {
		t.Fatal(err)
	}
	chart := filepath.Join(dir, "chart")
	if err := os.MkdirAll(filepath.Join(chart, "templates"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("apiVersion: v2\nname: rbac-upgrade\nversion: 0.1.0\nappVersion: test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"values.yaml", "templates/_helpers.tpl", "templates/rbac.yaml"} {
		body, err := os.ReadFile(filepath.Join("../../../charts/cloud-provisioning", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(chart, name), body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	helm := func(action string) {
		t.Helper()
		out, err := exec.CommandContext(ctx, "helm", action, "rbac-test", chart, "--namespace", "dialer-upgrade", "--kubeconfig", configPath).CombinedOutput()
		if err != nil {
			t.Fatalf("helm %s: %v\n%s", action, err, out)
		}
	}
	helm("install")
	key := types.NamespacedName{Namespace: "dialer-upgrade", Name: "cloud-provisioning"}
	role := &rbacv1.Role{}
	if err := admin.Get(ctx, key, role); err != nil {
		t.Fatal(err)
	}
	if len(role.Rules) != 1 || !equalStrings(role.Rules[0].ResourceNames, []string{"cloud-provisioning-peers"}) {
		t.Fatalf("unexpected initial grants: %+v", role.Rules)
	}
	role.Rules[0].ResourceNames = append(role.Rules[0].ResourceNames, "worker-a-tunnel-peers", "worker-b-tunnel-peers")
	if err := admin.Update(ctx, role); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"worker-a-tunnel-peers", "worker-b-tunnel-peers", "unrelated"} {
		if err := admin.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: key.Namespace}}); err != nil {
			t.Fatal(err)
		}
	}
	identity := rest.CopyConfig(cfg)
	identity.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:dialer-upgrade:cloud-provisioning", Groups: []string{"system:serviceaccounts", "system:serviceaccounts:dialer-upgrade", "system:authenticated"}}
	dialer, err := client.New(identity, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		for _, name := range []string{"worker-a-tunnel-peers", "worker-b-tunnel-peers"} {
			secret := &corev1.Secret{}
			if err := dialer.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: name}, secret); err != nil {
				t.Fatal("dialer access interrupted:", err)
			}
			before := secret.DeepCopy()
			secret.Annotations = map[string]string{"test": "acknowledged"}
			if err := dialer.Patch(ctx, secret, client.MergeFrom(before)); err != nil {
				t.Fatal("dialer acknowledgement denied:", err)
			}
		}
		if err := dialer.Get(ctx, types.NamespacedName{Namespace: key.Namespace, Name: "unrelated"}, &corev1.Secret{}); !apierrors.IsForbidden(err) {
			t.Fatalf("unrelated secret access: %v", err)
		}
	}
	check()
	// An unrelated rule must not be copied into the upgraded grant. The live
	// lookup is a source of named adoption Secrets, not arbitrary RBAC policy.
	role.Rules = append(role.Rules, rbacv1.PolicyRule{
		APIGroups: []string{""}, Resources: []string{"secrets"},
		Verbs:         []string{"get", "list", "patch"},
		ResourceNames: []string{"unrelated", "foreign-tunnel-peers"},
	})
	if err := admin.Update(ctx, role); err != nil {
		t.Fatal(err)
	}
	// No controller runs here to repair a transiently revoked grant.
	helm("upgrade")
	check()
	if err := admin.Get(ctx, key, role); err != nil {
		t.Fatal(err)
	}
	if len(role.Rules) != 1 || !equalStrings(role.Rules[0].ResourceNames, []string{"cloud-provisioning-peers", "worker-a-tunnel-peers", "worker-b-tunnel-peers"}) {
		t.Fatalf("upgrade changed grants: %+v", role.Rules)
	}
}
