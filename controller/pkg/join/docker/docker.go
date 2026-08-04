// Package docker implements join.InfraProvider and
// join.MachineProvisioner for CAPD (the Cluster API Docker provider,
// cluster-api's own development/test infrastructure). AWS is one
// implementation of machine fulfillment; this is another over the same
// seams, and it makes a plain kind cluster a complete local e2e of
// the single-resource flow: a ProvisionedNodeClaim against a DockerCluster
// resolves here, CAPD launches the "cloud" node as a container and
// executes the rendered cloud-config bootstrap, and the node joins the
// kind control plane for real. No QEMU, no bespoke cluster rig.
package docker

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/appmana/cloud-provisioning/controller/pkg/join"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Provider renders unstructured CAPD objects, matching how the AWS
// provider avoids depending on CAPA's own types. DockerMachine's
// actual reconciliation (docker run, exec'ing the bootstrap) is
// entirely CAPD's job.
//
// ConfigNamespace/ConfigName point at this provider's cluster-level
// config Secret (the aws-provider-config pattern). Keys:
//
//	node-image   required: the kind node image to launch (e.g.
//	                kindest/node:v1.33.1). It has to match the control
//	                plane's version closely enough for kubeadm's skew
//	                policy.
//	extra-mounts optional comma-separated hostPath:containerPath
//	                read-only mounts. This is how a local e2e delivers
//	                the dialer binary deterministically (a file:// URL
//	                into a mounted directory) instead of standing up a
//	                download server with all its lifecycle problems.
//	preload-images optional comma-separated image references CAPD
//	                loads into the node from the host's own daemon.
//	                A real cloud node pulls from a registry; a local
//	                container has no credentials and no registry, so
//	                this is how locally-built images (the dialer
//	                DaemonSet's) reach it.
type Provider struct {
	ConfigNamespace string
	ConfigName      string
}

var (
	// v1beta2, matching the cluster.x-k8s.io era this module verifies
	// against live; the e2e that installs CAPD asserts the installed
	// CRD actually serves this version.
	gvk              = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "DockerMachine"}
	dockerClusterGVK = schema.GroupVersionKind{Group: "infrastructure.cluster.x-k8s.io", Version: "v1beta2", Kind: "DockerCluster"}
)

// GVK implements join.InfraProvider.
func (Provider) GVK() schema.GroupVersionKind { return gvk }

// ClusterGVK implements join.MachineProvisioner.
func (Provider) ClusterGVK() schema.GroupVersionKind { return dockerClusterGVK }

// InfraValues implements join.InfraProvider: a container runs whatever
// the host's architecture is.
func (Provider) InfraValues(ctx context.Context, machine *unstructured.Unstructured) (map[string]any, error) {
	return map[string]any{"arch": runtime.GOARCH}, nil
}

// InfraMachine implements join.MachineProvisioner.
func (p Provider) InfraMachine(ctx context.Context, c client.Reader, namespace, instanceType string, req join.NodeRequest) (*unstructured.Unstructured, error) {
	cfg := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: p.ConfigNamespace, Name: p.ConfigName}, cfg); err != nil {
		return nil, fmt.Errorf("reading docker provider config %s/%s: %w", p.ConfigNamespace, p.ConfigName, err)
	}
	nodeImage := strings.TrimSpace(string(cfg.Data["node-image"]))
	if nodeImage == "" {
		return nil, fmt.Errorf("docker provider config %s/%s has no node-image", p.ConfigNamespace, p.ConfigName)
	}

	spec := map[string]any{
		"customImage": nodeImage,
	}
	if raw := strings.TrimSpace(string(cfg.Data["extra-mounts"])); raw != "" {
		var mounts []any
		for _, m := range strings.Split(raw, ",") {
			parts := strings.SplitN(strings.TrimSpace(m), ":", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, fmt.Errorf("docker provider config extra-mounts entry %q is not hostPath:containerPath", m)
			}
			mounts = append(mounts, map[string]any{
				"hostPath":      parts[0],
				"containerPath": parts[1],
				"readOnly":      true,
			})
		}
		spec["extraMounts"] = mounts
	}
	if raw := strings.TrimSpace(string(cfg.Data["preload-images"])); raw != "" {
		var images []any
		for _, img := range strings.Split(raw, ",") {
			if img = strings.TrimSpace(img); img != "" {
				images = append(images, img)
			}
		}
		spec["preLoadImages"] = images
	}

	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	return obj, nil
}
