package main

import (
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const apiRequestTimeout = 10 * time.Second

func newAPIClient(config *rest.Config) (*kubernetes.Clientset, error) {
	config = rest.CopyConfig(config)
	// A phase performs multiple complete admission reads before and after native
	// work. The client-go default (5 QPS, burst 10) can consume the phase's
	// revalidation deadline in local throttling even when the API is responsive.
	// Keep a bounded budget and honor explicit caller limits or rate limiters.
	if config.QPS == 0 {
		config.QPS = 20
	}
	if config.Burst == 0 {
		config.Burst = 40
	}
	// API access can depend on the routes this process repairs. Bound each
	// request so an unreachable API cannot prevent cached-state reconciliation.
	if config.Timeout <= 0 || config.Timeout > apiRequestTimeout {
		config.Timeout = apiRequestTimeout
	}
	return kubernetes.NewForConfig(config)
}
