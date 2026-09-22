package check

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestProbeObjectsUseNativePodNetworkAndPreloadedImages(t *testing.T) {
	pod, service := ProbeObjects("worker", "scenario")
	if pod.Name != "hc-worker" || pod.Namespace != "scenario" || pod.Spec.NodeSelector["kubernetes.io/hostname"] != "worker" {
		t.Fatalf("wrong native pod identity: %+v", pod)
	}
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC {
		t.Fatal("probe bypasses pod isolation")
	}
	if pod.Spec.Containers[0].ImagePullPolicy != corev1.PullNever {
		t.Fatal("probe may fetch an unprepared artifact")
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Fatal("probe lost explicit taint tolerance")
	}
	if service.Name != "svc-hc-worker" || service.Spec.Selector["app"] != pod.Labels["app"] || service.Spec.Ports[0].TargetPort.IntValue() != Port {
		t.Fatalf("service does not target its probe: %+v", service)
	}
}
