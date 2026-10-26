package join

import (
	"context"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/platform"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// nativeNetworkStub is an AWS-shaped provider whose machines run a
// provider-native pod network unless their template chooses the site CNI.
type nativeNetworkStub struct{ *stubInfraProvider }

func (nativeNetworkStub) DefaultCNI(os string) string { return "aws-vpc" }

func renderedKubeletArgs(t *testing.T, templateLabels map[string]string) string {
	t.Helper()
	machine := machineWithInfraRef("cloud-worker-0", "default", "cloud-worker-0")
	awsMachine := fakeAWSMachine("cloud-worker-0", "default", true)
	awsMachine.SetLabels(templateLabels)
	r := newFakeJoinReconciler(t, &stubJoinProvider{values: map[string]any{"joinToken": "t", "k0sVersion": "v"}}, machine, awsMachine, dialerPeerSecretFixture())
	r.InfraProviders = []InfraProvider{nativeNetworkStub{awsShapedStub()}}
	r.KubeletExtraFlags = "--max-pods=58"
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	secret := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "cloud-worker-0-bootstrap"}, secret); err != nil {
		t.Fatalf("bootstrap secret: %v", err)
	}
	rendered := secretValue(secret, "value")
	start := strings.Index(rendered, "kubeletExtraArgs=")
	end := strings.Index(rendered, " wgAddress=")
	if start < 0 || end < start {
		t.Fatalf("rendered userdata has no kubeletExtraArgs: %q", rendered)
	}
	return rendered[start+len("kubeletExtraArgs=") : end]
}

// A cloud worker registers with the labels of its node class, so the site
// can tell a provider-native network's nodes from its own before any pod
// network starts there.
func TestBootstrapRegistersTheNodeClass(t *testing.T) {
	native := renderedKubeletArgs(t, nil)
	for _, want := range []string{
		platform.CNILabel + "=aws-vpc",
		platform.RoleLabel + "=" + platform.RoleValue,
		"--register-with-taints=" + platform.TaintKey + ":NoSchedule",
		"--max-pods=58",
	} {
		if !strings.Contains(native, want) {
			t.Errorf("kubelet args %q lack %q", native, want)
		}
	}
	site := renderedKubeletArgs(t, map[string]string{platform.CNILabel: platform.SiteCNI})
	if strings.Contains(site, platform.CNILabel) {
		t.Errorf("a template choosing the site CNI still registered a native CNI label: %q", site)
	}
}
