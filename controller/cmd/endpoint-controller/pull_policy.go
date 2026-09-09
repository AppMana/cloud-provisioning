package main

import (
	"fmt"
	corev1 "k8s.io/api/core/v1"
)

func parseDialerPullPolicy(value string) (corev1.PullPolicy, error) {
	policy := corev1.PullPolicy(value)
	switch policy {
	case corev1.PullAlways, corev1.PullIfNotPresent, corev1.PullNever:
		return policy, nil
	default:
		return "", fmt.Errorf("invalid dialer image pull policy %q", value)
	}
}

func effectiveDialerPullPolicy(policy corev1.PullPolicy) corev1.PullPolicy {
	if policy == "" {
		return corev1.PullIfNotPresent
	}
	return policy
}
