package controlplane

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type tokens struct {
	minted int
	err    error
}

func (t *tokens) Token(_ context.Context, namespace, serviceAccount string, ttl time.Duration) (string, time.Time, error) {
	if t.err != nil {
		return "", time.Time{}, t.err
	}
	t.minted++
	return "token-" + string(rune('0'+t.minted)), now.Add(ttl), nil
}

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func reconciler(t *testing.T, minter *tokens) (*Reconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	icp := &v1alpha1.ImportedControlPlane{ObjectMeta: metav1.ObjectMeta{Name: "site", Namespace: "cloud-provisioning"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(icp).WithObjects(icp).Build()
	return &Reconciler{
		Client: c, Reader: c, Tokens: minter, ServiceAccount: "capi-workload", CA: []byte("ca-bundle"),
		Version: func(context.Context) (string, error) { return "v1.36.4+k0s", nil },
		TTL:     24 * time.Hour, Now: func() time.Time { return now },
	}, c
}

func reconcile(t *testing.T, r *Reconciler) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "cloud-provisioning", Name: "site"}})
}

func kubeconfigToken(t *testing.T, c client.Client) string {
	t.Helper()
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "cloud-provisioning", Name: "site-kubeconfig"}, secret); err != nil {
		t.Fatal(err)
	}
	if secret.Type != "cluster.x-k8s.io/secret" || secret.Labels["cluster.x-k8s.io/cluster-name"] != "site" {
		t.Fatalf("kubeconfig Secret type %q labels %v", secret.Type, secret.Labels)
	}
	config, err := clientcmd.Load(secret.Data["value"])
	if err != nil {
		t.Fatal(err)
	}
	ctx := config.Contexts[config.CurrentContext]
	if ctx == nil || config.Clusters[ctx.Cluster].Server != "https://kubernetes.default.svc:443" ||
		string(config.Clusters[ctx.Cluster].CertificateAuthorityData) != "ca-bundle" {
		t.Fatalf("kubeconfig does not reach this cluster's API: %+v", config)
	}
	return config.AuthInfos[ctx.AuthInfo].Token
}

// Cluster API reads the control-plane contract and reaches the cluster
// through <name>-kubeconfig with a scoped, expiring identity.
func TestAnImportedControlPlaneSatisfiesTheContract(t *testing.T) {
	r, c := reconciler(t, &tokens{})
	result, err := reconcile(t, r)
	if err != nil {
		t.Fatal(err)
	}
	if token := kubeconfigToken(t, c); token != "token-1" {
		t.Fatalf("kubeconfig token = %q", token)
	}
	icp := &v1alpha1.ImportedControlPlane{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "cloud-provisioning", Name: "site"}, icp); err != nil {
		t.Fatal(err)
	}
	s := icp.Status
	if !s.ExternalManagedControlPlane || !s.Initialization.ControlPlaneInitialized || s.Version != "v1.36.4" ||
		!apimeta.IsStatusConditionTrue(s.Conditions, "Available") {
		t.Fatalf("status = %+v", s)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > 12*time.Hour {
		t.Fatalf("requeue after %v, want by the token's half-life", result.RequeueAfter)
	}
}

// The token is replaced once half its lifetime has passed, and not before.
func TestTheWorkloadTokenRotatesAtHalfItsLifetime(t *testing.T) {
	minter := &tokens{}
	r, c := reconciler(t, minter)
	if _, err := reconcile(t, r); err != nil {
		t.Fatal(err)
	}
	r.Now = func() time.Time { return now.Add(11 * time.Hour) }
	if _, err := reconcile(t, r); err != nil {
		t.Fatal(err)
	}
	if token := kubeconfigToken(t, c); token != "token-1" {
		t.Fatalf("rotated early: %q", token)
	}
	r.Now = func() time.Time { return now.Add(13 * time.Hour) }
	if _, err := reconcile(t, r); err != nil {
		t.Fatal(err)
	}
	if token := kubeconfigToken(t, c); token != "token-2" {
		t.Fatalf("not rotated after half its lifetime: %q", token)
	}
}

// A failed token request reports the control plane unavailable rather than
// leaving an earlier success standing.
func TestATokenFailureIsReportedUnavailable(t *testing.T) {
	r, c := reconciler(t, &tokens{err: errors.New("forbidden")})
	if _, err := reconcile(t, r); err == nil {
		t.Fatal("token failure hidden")
	}
	icp := &v1alpha1.ImportedControlPlane{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "cloud-provisioning", Name: "site"}, icp); err != nil {
		t.Fatal(err)
	}
	if !apimeta.IsStatusConditionFalse(icp.Status.Conditions, "Available") {
		t.Fatalf("conditions = %+v, want Available=False", icp.Status.Conditions)
	}
}

// The token is requested for the named ServiceAccount with the asked lifetime.
func TestServiceAccountTokensRequestTheAskedLifetime(t *testing.T) {
	api := kubefake.NewSimpleClientset()
	var asked *authenticationv1.TokenRequest
	api.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create := action.(k8stesting.CreateAction)
		if create.GetSubresource() != "token" || create.GetNamespace() != "cloud-provisioning" {
			return true, nil, fmt.Errorf("unexpected %s/%s", create.GetNamespace(), create.GetSubresource())
		}
		asked = create.GetObject().(*authenticationv1.TokenRequest)
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{
			Token: "bound", ExpirationTimestamp: metav1.NewTime(now.Add(time.Hour)),
		}}, nil
	})
	token, expiry, err := ServiceAccountTokens{API: api}.Token(context.Background(), "cloud-provisioning", "capi-workload", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if token != "bound" || !expiry.Equal(now.Add(time.Hour)) || asked == nil || *asked.Spec.ExpirationSeconds != 3600 {
		t.Fatalf("token %q expiry %v request %+v", token, expiry, asked)
	}
}
