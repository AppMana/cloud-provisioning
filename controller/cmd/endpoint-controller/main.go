// Command wg-dialer-endpoint-controller watches core Cluster API Machine
// objects and mirrors their external address into the wg-dialer DaemonSet's
// peer Secret, so the cluster-side dialer always has an up to date endpoint
// for the node it's dialing -- without anything having to know AWSMachine's
// schema exists.
//
// Cluster API's own Machine controller copies the address up from
// whatever infrastructure provider sits underneath (AWSMachine today,
// anything else later) into Machine.status.addresses automatically. That
// is the one thing this controller depends on; it never reads AWSMachine
// (or any other provider-specific type) directly.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

var machineGVK = schema.GroupVersionKind{
	Group:   "cluster.x-k8s.io",
	Version: "v1beta1",
	Kind:    "Machine",
}

// reconciler mirrors one Machine's external address into a Secret key.
// It never watches or reads the Secret it writes -- the dialer DaemonSet
// (manifests/wg-dialer/dialer.yaml) polls the mounted file itself and
// re-applies `wg set` when it changes, so there is nothing for this
// controller to coordinate beyond a plain patch.
type reconciler struct {
	client.Client
	secretNamespace string
	secretName      string
	secretKey       string
	port            string
}

func (r *reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)
	if err := r.Get(ctx, req.NamespacedName, machine); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	addresses, found, err := unstructured.NestedSlice(machine.Object, "status", "addresses")
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reading status.addresses: %w", err)
	}
	if !found {
		log.V(1).Info("no status.addresses yet, waiting for the infrastructure provider")
		return ctrl.Result{}, nil
	}

	var externalIP string
	for _, entry := range addresses {
		address, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		if address["type"] != "ExternalIP" {
			continue
		}
		if ip, ok := address["address"].(string); ok && ip != "" {
			externalIP = ip
			break
		}
	}
	if externalIP == "" {
		log.V(1).Info("no ExternalIP in status.addresses yet, waiting")
		return ctrl.Result{}, nil
	}

	endpoint := fmt.Sprintf("%s:%s", externalIP, r.port)

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: r.secretNamespace, Name: r.secretName}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return ctrl.Result{}, fmt.Errorf("getting secret %s: %w", secretKey, err)
	}

	if string(secret.Data[r.secretKey]) == endpoint {
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(secret.DeepCopy())
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[r.secretKey] = []byte(endpoint)
	if err := r.Patch(ctx, secret, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patching secret %s: %w", secretKey, err)
	}
	log.Info("updated dialer peer endpoint", "endpoint", endpoint, "machine", req.NamespacedName)
	return ctrl.Result{}, nil
}

func main() {
	var (
		machineSelector string
		secretNamespace string
		secretName      string
		secretKey       string
		port            string
		metricsAddr     string
	)
	flag.StringVar(&machineSelector, "machine-selector", "cloud-provisioning.appmana.com/role=cloud-worker",
		"label selector identifying the Machine(s) whose external address drives the dialer's endpoint")
	flag.StringVar(&secretNamespace, "secret-namespace", "wg-dialer", "namespace of the dialer peer Secret")
	flag.StringVar(&secretName, "secret-name", "wg-dialer-peer", "name of the dialer peer Secret")
	flag.StringVar(&secretKey, "secret-key", "peer-endpoint", "key within the Secret to write the endpoint into")
	flag.StringVar(&port, "port", "51820", "WireGuard listen port on the joining node")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "metrics endpoint address (0 disables it)")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	selector, err := labels.Parse(machineSelector)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid --machine-selector: %v\n", err)
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: metricsAddr},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to start manager: %v\n", err)
		os.Exit(1)
	}

	machine := &unstructured.Unstructured{}
	machine.SetGroupVersionKind(machineGVK)

	err = ctrl.NewControllerManagedBy(mgr).
		For(machine, builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return selector.Matches(labels.Set(obj.GetLabels()))
		}))).
		Complete(&reconciler{
			Client:          mgr.GetClient(),
			secretNamespace: secretNamespace,
			secretName:      secretName,
			secretKey:       secretKey,
			port:            port,
		})
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to create controller: %v\n", err)
		os.Exit(1)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		fmt.Fprintf(os.Stderr, "problem running manager: %v\n", err)
		os.Exit(1)
	}
}
