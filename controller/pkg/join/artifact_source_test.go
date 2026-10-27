package join

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// An s3:// dialer source is signed by the controller when it renders a
// bootstrap document, so the URL a machine boots with is always fresh and
// nothing outside the product renews it.
func TestS3DialerSourcesAreSignedAtRender(t *testing.T) {
	machine := machineWithInfraRef("signed-worker", "default", "signed-worker")
	r := newFakeJoinReconciler(t, &stubJoinProvider{values: map[string]any{"joinToken": "fake", "k0sVersion": "test"}}, machine, fakeAWSMachine("signed-worker", "default", false), dialerPeerSecretFixture())
	r.DialerBinaryURLARM64 = "s3://assets/downloads/wg-dialer-linux-arm64"
	var signed []string
	r.SignArtifact = func(_ context.Context, source string) (string, error) {
		signed = append(signed, source)
		return fmt.Sprintf("https://assets.s3.us-west-2.amazonaws.com/downloads/wg-dialer-linux-arm64?X-Amz-Signature=fresh%d", len(signed)), nil
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)}); err != nil {
		t.Fatal(err)
	}
	if len(signed) != 1 || signed[0] != "s3://assets/downloads/wg-dialer-linux-arm64" {
		t.Fatalf("signed %v", signed)
	}
	secret := &corev1.Secret{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "signed-worker-bootstrap"}, secret); err != nil {
		t.Fatal(err)
	}
	if body := secret.StringData["value"]; !strings.Contains(body, "X-Amz-Signature=fresh1") || strings.Contains(body, "s3://") {
		t.Fatal("the bootstrap does not carry the signed URL")
	}
}

// An s3:// source with no signer cannot be fetched by a machine, so nothing
// is rendered.
func TestS3SourcesNeedASigner(t *testing.T) {
	r := &Reconciler{DialerBinaryURLARM64: "s3://assets/downloads/wg-dialer-linux-arm64", DialerBinarySHA256ARM64: strings.Repeat("0", 64)}
	if _, err := r.artifactURL(context.Background(), r.DialerBinaryURLARM64); err == nil {
		t.Fatal("an s3 source was rendered without a signer")
	}
}
