package cni

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CalicoPodMTU is the MTU Calico gives pod interfaces, read from the CNI
// configuration its calico-node DaemonSet installs, and a description of
// where it was read from.
func CalicoPodMTU(ctx context.Context, c client.Reader, ds *appsv1.DaemonSet) (int, string, error) {
	for _, container := range ds.Spec.Template.Spec.InitContainers {
		env := map[string]corev1.EnvVar{}
		for _, e := range container.Env {
			env[e.Name] = e
		}
		config, ok := env["CNI_NETWORK_CONFIG"]
		if !ok {
			continue
		}
		var doc struct {
			Plugins []struct {
				Type string          `json:"type"`
				MTU  json.RawMessage `json:"mtu"`
			} `json:"plugins"`
		}
		// The template's placeholders are strings, so it parses as JSON.
		if err := json.Unmarshal([]byte(config.Value), &doc); err != nil {
			return 0, "", fmt.Errorf("parsing %s's CNI_NETWORK_CONFIG: %w", container.Name, err)
		}
		for _, p := range doc.Plugins {
			if p.Type != "calico" {
				continue
			}
			var literal int
			if json.Unmarshal(p.MTU, &literal) == nil {
				return literal, "the calico plugin's mtu in " + container.Name + "'s CNI configuration", nil
			}
			var placeholder string
			if json.Unmarshal(p.MTU, &placeholder) != nil || placeholder != "__CNI_MTU__" {
				return 0, "", fmt.Errorf("the calico plugin's mtu %s is neither a number nor __CNI_MTU__", p.MTU)
			}
			mtuVar, ok := env["CNI_MTU"]
			if !ok {
				return 0, "", fmt.Errorf("the CNI configuration takes its MTU from CNI_MTU, which %s does not set", container.Name)
			}
			value := mtuVar.Value
			if ref := mtuVar.ValueFrom; ref != nil && ref.ConfigMapKeyRef != nil {
				cm := &corev1.ConfigMap{}
				if err := c.Get(ctx, types.NamespacedName{Namespace: ds.Namespace, Name: ref.ConfigMapKeyRef.Name}, cm); err != nil {
					return 0, "", fmt.Errorf("reading the CNI MTU from ConfigMap %s: %w", ref.ConfigMapKeyRef.Name, err)
				}
				value = cm.Data[ref.ConfigMapKeyRef.Key]
			}
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return 0, "", fmt.Errorf("CNI_MTU %q is not a number", value)
			}
			return n, "CNI_MTU", nil
		}
	}
	return 0, "", fmt.Errorf("calico-node carries no CNI configuration to read the pod MTU from")
}
