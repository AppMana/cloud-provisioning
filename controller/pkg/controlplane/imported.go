// Package controlplane presents the cluster this controller runs in to
// Cluster API as an imported control plane, so CAPI can manage cloud workers
// joining a cluster it did not create.
package controlplane

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	v1alpha1 "github.com/appmana/cloud-provisioning/controller/api/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// Server is where Cluster API, running in this cluster, reaches its API:
// the standard Service, which already spans every control plane.
const Server = "https://kubernetes.default.svc:443"

const expiryAnnotation = "cloud-provisioning.appmana.com/token-expires"

// TokenRequester mints a bound token for a ServiceAccount.
type TokenRequester interface {
	Token(ctx context.Context, namespace, serviceAccount string, ttl time.Duration) (string, time.Time, error)
}

// Reconciler keeps an ImportedControlPlane's contract status and the
// <name>-kubeconfig Secret Cluster API uses to reach the cluster.
type Reconciler struct {
	client.Client
	// Reader reads the kubeconfig Secret without a cluster-wide Secret
	// cache, which this identity is not granted.
	Reader client.Reader
	Tokens TokenRequester
	// ServiceAccount is the scoped identity in the control plane's namespace
	// that Cluster API acts as on this cluster.
	ServiceAccount string
	CA             []byte
	// Version reads the API server's Kubernetes version.
	Version func(context.Context) (string, error)
	TTL     time.Duration
	Now     func() time.Time
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	icp := &v1alpha1.ImportedControlPlane{}
	if err := r.Get(ctx, req.NamespacedName, icp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	refreshAt, err := r.ensureKubeconfig(ctx, icp)
	if err == nil {
		var version string
		if version, err = r.Version(ctx); err == nil {
			// Machine and control-plane versions are plain semver.
			icp.Status.Version, _, _ = strings.Cut(version, "+")
		}
	}
	icp.Status.ExternalManagedControlPlane = true
	icp.Status.Initialization.ControlPlaneInitialized = err == nil || icp.Status.Initialization.ControlPlaneInitialized
	available := metav1.Condition{Type: "Available", Status: metav1.ConditionTrue, Reason: "APIServing", ObservedGeneration: icp.Generation}
	if err != nil {
		available.Status, available.Reason, available.Message = metav1.ConditionFalse, "ConnectionUnavailable", err.Error()
	}
	apimeta.SetStatusCondition(&icp.Status.Conditions, available)
	if statusErr := r.Status().Update(ctx, icp); statusErr != nil && !apierrors.IsConflict(statusErr) {
		return ctrl.Result{}, fmt.Errorf("updating imported control plane status: %w", statusErr)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: refreshAt.Sub(r.Now())}, nil
}

// ensureKubeconfig keeps a current token in <name>-kubeconfig, replacing it
// once half its lifetime has passed, and returns when to look again.
func (r *Reconciler) ensureKubeconfig(ctx context.Context, icp *v1alpha1.ImportedControlPlane) (time.Time, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: icp.Name + "-kubeconfig", Namespace: icp.Namespace}}
	err := r.Reader.Get(ctx, client.ObjectKeyFromObject(secret), secret)
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return time.Time{}, err
	}
	if expiry, perr := time.Parse(time.RFC3339, secret.Annotations[expiryAnnotation]); exists && perr == nil {
		if refresh := expiry.Add(-r.TTL / 2); r.Now().Before(refresh) {
			return refresh, nil
		}
	}
	token, expiry, err := r.Tokens.Token(ctx, icp.Namespace, r.ServiceAccount, r.TTL)
	if err != nil {
		return time.Time{}, fmt.Errorf("minting the Cluster API workload token: %w", err)
	}
	value, err := clientcmd.Write(clientcmdapi.Config{
		Clusters:       map[string]*clientcmdapi.Cluster{icp.Name: {Server: Server, CertificateAuthorityData: r.CA}},
		AuthInfos:      map[string]*clientcmdapi.AuthInfo{r.ServiceAccount: {Token: token}},
		Contexts:       map[string]*clientcmdapi.Context{icp.Name: {Cluster: icp.Name, AuthInfo: r.ServiceAccount}},
		CurrentContext: icp.Name,
	})
	if err != nil {
		return time.Time{}, err
	}
	secret.Type = "cluster.x-k8s.io/secret"
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	secret.Labels["cluster.x-k8s.io/cluster-name"] = icp.Name
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[expiryAnnotation] = expiry.UTC().Format(time.RFC3339)
	secret.Data = map[string][]byte{"value": value}
	if err := controllerutil.SetControllerReference(icp, secret, r.Scheme()); err != nil {
		return time.Time{}, err
	}
	if exists {
		err = r.Update(ctx, secret)
	} else {
		err = r.Create(ctx, secret)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("writing %s: %w", secret.Name, err)
	}
	return expiry.Add(-r.TTL / 2), nil
}

// ServiceAccountTokens mints bound tokens through the TokenRequest API.
type ServiceAccountTokens struct{ API kubernetes.Interface }

func (s ServiceAccountTokens) Token(ctx context.Context, namespace, serviceAccount string, ttl time.Duration) (string, time.Time, error) {
	seconds := int64(ttl / time.Second)
	req := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}}
	got, err := s.API.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, serviceAccount, req, metav1.CreateOptions{})
	if err != nil {
		return "", time.Time{}, err
	}
	return got.Status.Token, got.Status.ExpirationTimestamp.Time, nil
}

// Register reconciles the ImportedControlPlanes in namespace, issuing Cluster
// API the scoped serviceAccount's identity on this cluster.
func Register(mgr ctrl.Manager, namespace, serviceAccount string) error {
	config := mgr.GetConfig()
	ca := config.CAData
	if len(ca) == 0 && config.CAFile != "" {
		var err error
		if ca, err = os.ReadFile(config.CAFile); err != nil {
			return fmt.Errorf("reading the cluster CA: %w", err)
		}
	}
	api, err := kubernetes.NewForConfig(config)
	if err != nil {
		return err
	}
	r := &Reconciler{
		Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Tokens: ServiceAccountTokens{API: api}, ServiceAccount: serviceAccount, CA: ca,
		Version: func(context.Context) (string, error) {
			info, err := api.Discovery().ServerVersion()
			if err != nil {
				return "", err
			}
			return info.GitVersion, nil
		},
		TTL: 24 * time.Hour, Now: time.Now,
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("imported-control-plane").
		For(&v1alpha1.ImportedControlPlane{}, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetNamespace() == namespace
		}))).
		Complete(r)
}
