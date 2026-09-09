package join

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestObservedExpiredBootstrapDownload(t *testing.T) {
	// Native failed Linux boot: signed 2026-09-07 05:17:27 UTC for four
	// hours; cloud-init fetched it on September 8 and received HTTP 403.
	// Object host and signature are synthetic; no live credential is a fixture.
	now := time.Date(2026, 9, 8, 1, 53, 14, 0, time.UTC)
	expired := "https://example.invalid/dialer?X-Amz-Date=20260907T051727Z&X-Amz-Expires=14400&X-Amz-Signature=do-not-log"
	if err := validateDownloadExpiry(expired, now); err == nil || strings.Contains(err.Error(), "do-not-log") || strings.Contains(err.Error(), "example.invalid") {
		t.Fatalf("expired download was accepted or leaked: %v", err)
	}
	for _, raw := range []string{"https://example.invalid/dialer", "https://example.invalid/dialer?X-Amz-Date=20260908T010000Z&X-Amz-Expires=14400"} {
		if err := validateDownloadExpiry(raw, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{
		"X-Amz-Date=bad&X-Amz-Expires=1", "X-Amz-Date=20260908T010000Z",
		"X-Amz-Expires=1", "X-Amz-Date=20260908T010000Z&X-Amz-Expires=-1",
		"X-Amz-Date=20260908T010000Z&X-Amz-Expires=99999999999999999",
		"X-Amz-Date=20260908T010000Z&X-Amz-Expires=10&X-Amz-Expires=20",
	} {
		if err := validateDownloadExpiry("https://example.invalid/?"+query, now); err == nil {
			t.Fatalf("accepted %s", query)
		}
	}
	deadline := time.Date(2026, 9, 7, 9, 17, 27, 0, time.UTC)
	if err := validateDownloadExpiry(expired, deadline); err == nil {
		t.Fatal("accepted exact expiration boundary")
	}
}

func TestExpiredDownloadDoesNotPublishCAPABootstrap(t *testing.T) {
	machine := machineWithInfraRef("expired-worker", "default", "expired-worker")
	r := newFakeJoinReconciler(t, &stubJoinProvider{values: map[string]any{"joinToken": "fake", "k0sVersion": "test"}}, machine, fakeAWSMachine("expired-worker", "default", false), dialerPeerSecretFixture())
	r.DialerBinaryURLARM64 = "https://example.invalid/dialer?X-Amz-Date=20000101T000000Z&X-Amz-Expires=14400"
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expiry failure, got %v", err)
	}
	var secret corev1.Secret
	if err := r.Client.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "expired-worker-bootstrap"}, &secret); !apierrors.IsNotFound(err) {
		t.Fatalf("bootstrap Secret was published: %v", err)
	}
}
