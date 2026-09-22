// Package kube retains cloud-provisioning's import path for the shared
// Kubernetes fixture client. Product-specific CAPI assertions remain here.
package kube

import shared "github.com/appmana/labcontainers/pkg/kubernetes/kube"

type Client = shared.Client
