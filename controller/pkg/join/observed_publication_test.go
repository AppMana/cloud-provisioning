package join

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type interruptedPublication struct {
	client.Client
	stage  string
	failed bool
}

func (c *interruptedPublication) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	stage := "peer"
	if obj.GetObjectKind().GroupVersionKind() == machineGVK {
		stage = "machine"
	}
	if !c.failed && c.stage == stage {
		c.failed = true
		return apierrors.NewInternalError(fmt.Errorf("observed CAPI admission webhook EOF"))
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *interruptedPublication) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if !c.failed && c.stage == "bootstrap" && strings.HasSuffix(obj.GetName(), "-bootstrap") {
		c.failed = true
		return apierrors.NewInternalError(fmt.Errorf("bootstrap write unavailable"))
	}
	return c.Client.Create(ctx, obj, opts...)
}

// The real k0s/Calico run observed a webhook EOF while annotating remote2.
// Its bootstrap Secret already existed, so retries skipped the missing address
// reservation. Recreating remote1 then reused remote2's live tunnel address.
func TestObservedBootstrapPublicationInterruption(t *testing.T) {
	for _, stage := range []string{"machine", "peer", "bootstrap"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			a := machineWithInfraRef("remote-a", "default", "remote-a")
			b := machineWithInfraRef("remote-b", "default", "remote-b")
			r := newFakeJoinReconciler(t, &stubJoinProvider{values: map[string]any{"joinToken": "fake-token", "k0sVersion": "v1.36.2+k0s"}}, a, b,
				fakeAWSMachine("remote-a", "default", true), fakeAWSMachine("remote-b", "default", true), dialerPeerSecretFixture())
			r.Client = &interruptedPublication{Client: r.Client, stage: stage}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(a)}
			if _, err := r.Reconcile(ctx, req); err == nil {
				t.Fatal("expected interrupted publication")
			}
			secret := &corev1.Secret{}
			key := client.ObjectKey{Namespace: "default", Name: "remote-a-bootstrap"}
			if err := r.Reader.Get(ctx, key, secret); !apierrors.IsNotFound(err) {
				t.Fatalf("infrastructure can boot an incomplete publication: secret lookup=%v", err)
			}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			if err := r.Reader.Get(ctx, key, secret); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(secretValue(secret, "value"), "wgAddress=10.100.0.128/24") {
				t.Fatal("retry abandoned its address reservation")
			}
			_, encoded, ok := strings.Cut(secretValue(secret, "value"), "peersFileJSON=")
			if !ok {
				t.Fatal("missing rendered peer document")
			}
			var document struct {
				PrivateKey string `json:"privateKey"`
			}
			if err := json.Unmarshal([]byte(encoded), &document); err != nil {
				t.Fatal(err)
			}
			keyPair, err := wgtypes.ParseKey(document.PrivateKey)
			if err != nil {
				t.Fatal("invalid rendered private key")
			}
			published := &corev1.Secret{}
			if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: "wg-dialer", Name: "wg-dialer-peer"}, published); err != nil {
				t.Fatal(err)
			}
			if string(published.Data["peer-public-key-remote-a"]) != keyPair.PublicKey().String() {
				t.Fatal("retry published a peer that cannot authenticate the final bootstrap")
			}
			if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(a), a); err != nil {
				t.Fatal(err)
			}
			if a.GetAnnotations()[WireGuardAddrAnnotation] != "10.100.0.128/24" {
				t.Fatal("published bootstrap has no matching reservation")
			}
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}); err != nil {
				t.Fatal(err)
			}
			if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(b), b); err != nil {
				t.Fatal(err)
			}
			if b.GetAnnotations()[WireGuardAddrAnnotation] != "10.100.0.129/24" {
				t.Fatal("second Machine did not receive a distinct address")
			}
		})
	}
}

func TestObservedPublishedPeerReservesAddressWithoutMachineAnnotation(t *testing.T) {
	ctx := context.Background()
	old := machineWithInfraRef("remote2", "default", "remote2")
	newMachine := machineWithInfraRef("remote1", "default", "remote1")
	peers := dialerPeerSecretFixture()
	peers.Data["peer-route-hosts-remote2"] = []byte("10.100.0.129,192.0.2.10")
	peers.Data["retired-tunnel-addresses"] = []byte("10.100.0.128")
	r := newFakeJoinReconciler(t, &stubJoinProvider{values: map[string]any{"joinToken": "fake-token", "k0sVersion": "v1.36.2+k0s"}}, old, newMachine,
		fakeAWSMachine("remote1", "default", true), peers)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(newMachine)}); err != nil {
		t.Fatal(err)
	}
	if err := r.Reader.Get(ctx, client.ObjectKeyFromObject(newMachine), newMachine); err != nil {
		t.Fatal(err)
	}
	if newMachine.GetAnnotations()[WireGuardAddrAnnotation] != "10.100.0.130/24" {
		t.Fatalf("reused the observed live peer address: %s", newMachine.GetAnnotations()[WireGuardAddrAnnotation])
	}
}
