package main

import (
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const apiRequestTimeout = 10 * time.Second

func newAPIClient(config *rest.Config) (*kubernetes.Clientset, error) {
	config = rest.CopyConfig(config)
	// API access can depend on the routes this process repairs. Bound each
	// request so an unreachable API cannot prevent cached-state reconciliation.
	if config.Timeout <= 0 || config.Timeout > apiRequestTimeout {
		config.Timeout = apiRequestTimeout
	}
	return kubernetes.NewForConfig(config)
}
