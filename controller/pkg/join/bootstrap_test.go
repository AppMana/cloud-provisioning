package join

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/appmana/cloud-provisioning/controller/pkg/bootstrap"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconcile_UnsupportedGuestHasNoSideEffects(t *testing.T) {
	machine := machineWithInfraRef("windows-worker", "default", "windows-worker")
	infra := fakeAWSMachine("windows-worker", "default", false)
	infra.SetLabels(map[string]string{corev1.LabelOSStable: "windows"})
	peers := dialerPeerSecretFixture()
	provider := &stubJoinProvider{}
	r := newFakeJoinReconciler(t, provider, machine, infra, peers)
	before := &corev1.SecretList{}
	if err := r.List(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	if err == nil || !strings.Contains(err.Error(), `guest OS "windows"`) {
		t.Fatalf("expected unsupported Windows error, got %v", err)
	}
	if provider.calls != 0 {
		t.Fatal("minted join credentials for unsupported guest")
	}
	after := &corev1.SecretList{}
	if err := r.List(context.Background(), after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Items, after.Items) {
		t.Fatal("changed Secrets for unsupported guest")
	}
	actual := machine.DeepCopy()
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(machine), actual); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(machine.Object, actual.Object) {
		t.Fatal("changed Machine for unsupported guest")
	}
}

func TestReconcile_NativeGuestBootstrap(t *testing.T) {
	for _, guest := range []string{"", "linux", "windows"} {
		t.Run("guest="+guest, func(t *testing.T) {
			machine := machineWithInfraRef("worker", "default", "worker")
			infra := fakeAWSMachine("worker", "default", false)
			if guest != "" {
				infra.SetLabels(map[string]string{corev1.LabelOSStable: guest})
			}
			provider := &stubJoinProvider{values: map[string]any{"joinToken": "fake-token", "k0sVersion": "v1.36.2+k0s.0"}}
			r := newFakeJoinReconciler(t, provider, machine, infra, dialerPeerSecretFixture())
			script := "$ErrorActionPreference = 'Stop'\r\n# Ω & < >\r\n$token = '{{.joinToken}}'\r\n"
			path := filepath.Join(t.TempDir(), "windows.ps1.tmpl")
			if err := os.WriteFile(path, []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			r.BootstrapRenderers["windows"] = PatternBootstrap{Path: path, Format: bootstrap.PowerShell}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)}); err != nil {
				t.Fatal(err)
			}
			secret := &corev1.Secret{}
			if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "worker-bootstrap"}, secret); err != nil {
				t.Fatal(err)
			}
			wantFormat := bootstrap.CloudConfig
			if guest == "windows" {
				wantFormat = bootstrap.PowerShell
				if secretValue(secret, "value") != strings.ReplaceAll(script, "{{.joinToken}}", "fake-token") {
					t.Fatal("raw PowerShell changed before reaching infrastructure transport")
				}
			}
			if got := secretValue(secret, "format"); got != wantFormat {
				t.Fatalf("format = %q, want %q", got, wantFormat)
			}
		})
	}
}

type fixedBootstrap struct{ data bootstrap.Data }

func (f fixedBootstrap) Render(map[string]any) (bootstrap.Data, error) { return f.data, nil }

func TestReconcile_InvalidRendererCannotPublish(t *testing.T) {
	for _, data := range []bootstrap.Data{{Format: bootstrap.PowerShell}, {Format: "unknown", Value: []byte("payload")}} {
		t.Run(data.Format, func(t *testing.T) {
			machine := machineWithInfraRef("worker", "default", "worker")
			r := newFakeJoinReconciler(t, &stubJoinProvider{}, machine, fakeAWSMachine("worker", "default", false), dialerPeerSecretFixture())
			r.BootstrapRenderers["linux"] = fixedBootstrap{data: data}
			before := &corev1.SecretList{}
			if err := r.List(context.Background(), before); err != nil {
				t.Fatal(err)
			}
			_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
			if err == nil || !strings.Contains(err.Error(), "invalid rendered bootstrap") {
				t.Fatalf("expected invalid payload error, got %v", err)
			}
			after := &corev1.SecretList{}
			if err := r.List(context.Background(), after); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.Items, after.Items) {
				t.Fatal("published invalid bootstrap or modified peers")
			}
		})
	}
}
