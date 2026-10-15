package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/appmana/cloud-provisioning/harness/e2e/kube"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
)

// ImportControlPlane observes the existing site's API and supplies the CAPI
// control-plane contract, connection Secret and scoped CAPA read access.
// The real CAPI controllers own Cluster status and Machine nodeRef.
func (c *Controller) ImportControlPlane(ctx context.Context, namespace, name string) error {
	if _, err := c.Kube.Server(ctx); err != nil {
		return fmt.Errorf("observing imported control plane: %w", err)
	}
	role, binding := importedCAPAReader(namespace, name)
	if err := c.Kube.ApplyObjects(ctx, role, binding); err != nil {
		return fmt.Errorf("granting scoped imported control-plane read access: %w", err)
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
	_, err = c.Kube.Run(ctx, "-n", namespace, "patch", "labimportedcontrolplane", name, "--subresource=status", "--type=merge", "-p", string(status))
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
	return kube.WaitMachineAssociation(ctx, c.Kube, namespace, machine, node)
}

// CAPA reads the control-plane contract while reconciling workers. It does not
// need the connection Secret or permission to mutate the imported resource.
func importedCAPAReader(namespace, name string) (*rbacv1.Role, *rbacv1.RoleBinding) {
	roleName := name + "-capa-control-plane-reader"
	role := &rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"}, ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: namespace}, Rules: []rbacv1.PolicyRule{{APIGroups: []string{"infrastructure.labcontainers.appmana.com"}, Resources: []string{"labimportedcontrolplanes"}, ResourceNames: []string{name}, Verbs: []string{"get"}}}}
	binding := &rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: namespace}, RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: roleName}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "capa-controller-manager", Namespace: "capa-system"}}}
	return role, binding
}
