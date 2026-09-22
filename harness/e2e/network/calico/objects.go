package calico

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"sort"

	"github.com/appmana/labcontainers/pkg/artifact"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// ReadObjects reads a content-pinned native Kubernetes List, prepared from the
// aligned fork. Go tests can pass runtime.Objects directly instead. It neither
// generates manifests nor fetches a default release.
func ReadObjects(ctx context.Context, path, sha256 string) ([]runtime.Object, error) {
	body, err := artifact.ReadFile(ctx, path, sha256)
	if err != nil {
		return nil, err
	}
	var list unstructured.UnstructuredList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	if list.GetAPIVersion() != "v1" || list.GetKind() != "List" || len(list.Items) == 0 {
		return nil, fmt.Errorf("prepared Calico objects must be a nonempty Kubernetes v1 List in JSON")
	}
	objects := make([]runtime.Object, len(list.Items))
	for i := range list.Items {
		objects[i] = list.Items[i].DeepCopy()
	}
	return objects, nil
}

var digestImage = regexp.MustCompile(`^.+@sha256:[0-9a-fA-F]{64}$`)

func prepareObjects(input []runtime.Object, podCIDR string) ([]runtime.Object, []string, error) {
	if len(input) == 0 {
		return nil, nil, fmt.Errorf("explicit Calico objects from the aligned fork are required; no upstream release is downloaded")
	}
	if _, err := netip.ParsePrefix(podCIDR); err != nil {
		return nil, nil, fmt.Errorf("invalid pod CIDR: %w", err)
	}
	objects := make([]runtime.Object, 0, len(input))
	images := map[string]bool{}
	found := 0
	for _, original := range input {
		if original == nil {
			return nil, nil, fmt.Errorf("nil Calico object")
		}
		// Convert through upstream codecs so callers can use native generated
		// types or unstructured custom resources without a second object model.
		body, err := json.Marshal(original)
		if err != nil {
			return nil, nil, err
		}
		var object unstructured.Unstructured
		if err := json.Unmarshal(body, &object); err != nil {
			return nil, nil, err
		}
		if object.GetAPIVersion() == "" || object.GetKind() == "" || object.GetName() == "" {
			return nil, nil, fmt.Errorf("Calico object requires apiVersion, kind and metadata.name")
		}
		var pod *corev1.PodSpec
		var prepared runtime.Object = &object
		switch object.GroupVersionKind().String() {
		case "apps/v1, Kind=DaemonSet":
			ds := &appsv1.DaemonSet{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, ds); err != nil {
				return nil, nil, err
			}
			pod, prepared = &ds.Spec.Template.Spec, ds
			if ds.Name == "calico-node" {
				if ds.Namespace != "kube-system" {
					return nil, nil, fmt.Errorf("this standalone fixture requires calico-node in kube-system")
				}
				for i := range pod.Containers {
					container := &pod.Containers[i]
					if container.Name != "calico-node" {
						continue
					}
					found++
					env := corev1.EnvVar{Name: "CALICO_IPV4POOL_CIDR", Value: podCIDR}
					var updated []corev1.EnvVar
					for _, current := range container.Env {
						if current.Name != env.Name {
							updated = append(updated, current)
						}
					}
					container.Env = append(updated, env)
				}
			}
		case "apps/v1, Kind=Deployment":
			deployment := &appsv1.Deployment{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, deployment); err != nil {
				return nil, nil, err
			}
			pod, prepared = &deployment.Spec.Template.Spec, deployment
		default:
			switch object.GetKind() {
			case "Pod", "Job", "CronJob", "StatefulSet", "ReplicaSet", "ReplicationController", "DaemonSet", "Deployment", "List":
				return nil, nil, fmt.Errorf("unsupported Calico workload %s; this fixture handles apps/v1 DaemonSet and Deployment", object.GroupVersionKind())
			}
		}
		if pod != nil {
			for _, containers := range [][]corev1.Container{pod.InitContainers, pod.Containers} {
				for i := range containers {
					if !digestImage.MatchString(containers[i].Image) {
						return nil, nil, fmt.Errorf("Calico container %s requires a prepared image reference pinned by SHA256", containers[i].Name)
					}
					images[containers[i].Image] = true
					containers[i].ImagePullPolicy = corev1.PullNever
				}
			}
		}
		objects = append(objects, prepared)
	}
	if found != 1 {
		return nil, nil, fmt.Errorf("expected exactly one calico-node container in its DaemonSet, found %d", found)
	}
	refs := make([]string, 0, len(images))
	for image := range images {
		refs = append(refs, image)
	}
	sort.Strings(refs)
	return objects, refs, nil
}
