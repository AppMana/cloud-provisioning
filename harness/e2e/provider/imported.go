package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"k8s.io/client-go/tools/clientcmd"
)

// ImportControlPlane observes the existing site's API and supplies the CAPI
// control-plane contract. It owns only ImportedControlPlane and its connection
// Secret; the real CAPI controllers own Cluster status and Machine nodeRef.
func (c *Controller) ImportControlPlane(ctx context.Context, namespace, name string) error {
	if _, err := c.Kube.Server(ctx); err != nil {
		return fmt.Errorf("observing imported control plane: %w", err)
	}
	raw, err := c.Kube.Bastion.Exec(ctx, "kubectl", "config", "view", "--raw", "--minify", "-o", "json")
	if err != nil {
		return fmt.Errorf("reading the site's connection credentials: %w", err)
	}
	connection, err := selfHostedConnection(raw)
	if err != nil {
		return err
	}
	secret, err := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "cluster.x-k8s.io/secret",
		"metadata": map[string]any{"name": name + "-kubeconfig", "namespace": namespace,
			"labels": map[string]string{"cluster.x-k8s.io/cluster-name": name}},
		"data": map[string][]byte{"value": connection},
	})
	if err != nil {
		return err
	}
	if err := c.Kube.Apply(ctx, secret); err != nil {
		// Do not echo a rejected credential-bearing manifest.
		return fmt.Errorf("publishing the imported cluster connection failed")
	}
	status, _ := json.Marshal(map[string]any{"status": map[string]any{
		// These control-plane nodes are managed outside CAPI and therefore
		// have no control-plane Machines. Without this contract field, CAPI
		// skips worker drain and pre-drain hooks as if no control plane exists.
		"externalManagedControlPlane": true,
		"initialization":              map[string]bool{"controlPlaneInitialized": true},
		"conditions": []map[string]any{{"type": "Available", "status": "True", "reason": "APIObserved",
			"lastTransitionTime": time.Now().UTC().Format(time.RFC3339)}},
	}})
	_, err = c.Kube.Run(ctx, "-n", namespace, "patch", "importedcontrolplane", name, "--subresource=status", "--type=merge", "-p", string(status))
	return err
}

// CAPI runs inside the imported cluster. Its standard Kubernetes Service gives
// it the API's existing HA endpoint without adding a route from the lab host.
func selfHostedConnection(raw []byte) ([]byte, error) {
	config, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid imported kubeconfig")
	}
	current := config.Contexts[config.CurrentContext]
	if current == nil || config.Clusters[current.Cluster] == nil || config.AuthInfos[current.AuthInfo] == nil {
		return nil, fmt.Errorf("imported kubeconfig has no complete current context")
	}
	config.Clusters[current.Cluster].Server = "https://kubernetes.default.svc:443"
	return clientcmd.Write(*config)
}

// WaitAssociation delegates the provider-independent CAPI identity gate.
func (c *Controller) WaitAssociation(ctx context.Context, namespace, machine, node string) error {
	return c.Kube.WaitMachineAssociation(ctx, namespace, machine, node)
}
