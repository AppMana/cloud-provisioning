package main

import (
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const apiRequestTimeout = 10 * time.Second

func newAPIClient(config *rest.Config) (*kubernetes.Clientset, error) {
	config = rest.CopyConfig(config)
	// Match controller-runtime's default: let API Priority and Fairness budget
	// requests instead of consuming admission deadlines in a local token bucket.
	// Native phases make repeated live rechecks, not just one initial burst.
	// Honor explicit caller limits and rate limiters; keep request/phase timeouts.
	if config.QPS == 0 {
		config.QPS = -1
	}
	if config.QPS > 0 && config.Burst == 0 {
		config.Burst = 40
	}
	// API access can depend on the routes this process repairs. Bound each
	// request so an unreachable API cannot prevent cached-state reconciliation.
	if config.Timeout <= 0 || config.Timeout > apiRequestTimeout {
		config.Timeout = apiRequestTimeout
	}
	return kubernetes.NewForConfig(config)
}
