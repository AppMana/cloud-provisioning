package main

import (
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func configureReceiveSysctlPolicy(ds *appsv1.DaemonSet) {
	pod := &ds.Spec.Template.Spec
	directory := corev1.HostPathDirectoryOrCreate
	pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "sysctl-policy", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/etc/sysctl.d", Type: &directory}}})
	pod.Containers[0].VolumeMounts = append(pod.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "sysctl-policy", MountPath: tunnel.HostSysctlDirectory})
	pod.Containers[0].Args = append(pod.Containers[0].Args, "--receive-sysctl-directory="+tunnel.HostSysctlDirectory)
}
