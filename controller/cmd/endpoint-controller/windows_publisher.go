package main

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *meshReconciler) windowsPublisherDaemonSet() *appsv1.DaemonSet {
	name := r.dialerCloudDaemonSetName + "-windows"
	yes, no := true, false
	system := `NT AUTHORITY\SYSTEM`
	directory := corev1.HostPathDirectory
	expiration := int64(3600)
	args := []string{
		"--namespace=" + r.secretNamespace,
		`--machine-name-file=C:\cloud-provisioning\machine-name`,
		`--request-file=C:\cloud-provisioning\request.json`,
		`--receipt-file=C:\cloud-provisioning\receipt.json`,
		`--token-file=C:\publisher-credentials\token`,
		`--ca-file=C:\publisher-credentials\ca.crt`,
	}
	if r.windowsPublisherAPIServer != "" {
		args = append(args, "--api-server="+r.windowsPublisherAPIServer)
	}
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.secretNamespace, OwnerReferences: r.owners()},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					OS:                           &corev1.PodOS{Name: corev1.Windows},
					NodeSelector:                 map[string]string{corev1.LabelOSStable: "windows", corev1.LabelArchStable: "amd64", cloudWorkerRoleLabel: cloudWorkerRoleValue},
					HostNetwork:                  true,
					ServiceAccountName:           r.dialerServiceAccount,
					AutomountServiceAccountToken: &no,
					SecurityContext:              &corev1.PodSecurityContext{WindowsOptions: &corev1.WindowsSecurityContextOptions{HostProcess: &yes, RunAsUserName: &system}},
					Tolerations:                  []corev1.Toleration{{Key: cloudWorkerTaintKey, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}},
					ImagePullSecrets:             imagePullSecrets(r.dialerImagePullSecret),
					Containers: []corev1.Container{{
						Name: "peer-publisher", Image: r.windowsPublisherImage, ImagePullPolicy: corev1.PullIfNotPresent,
						Args:         args,
						VolumeMounts: []corev1.VolumeMount{{Name: "host-state", MountPath: `C:\cloud-provisioning`}, {Name: "api-credentials", MountPath: `C:\publisher-credentials`, ReadOnly: true}},
					}},
					Volumes: []corev1.Volume{
						{Name: "host-state", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: `C:\ProgramData\CloudProvisioning\` + r.ifaceName, Type: &directory}}},
						{Name: "api-credentials", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
							{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", ExpirationSeconds: &expiration}},
							{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
						}}}},
					},
				},
			},
		},
	}
}

func (r *meshReconciler) ensureWindowsPublisherDaemonSet(ctx context.Context) error {
	// Empty preserves the default until a tested Windows image is configured.
	// Do not delete an existing publisher during a configuration rollback: its
	// continued delivery keeps already joined nodes on current peer state.
	if r.windowsPublisherImage == "" {
		return nil
	}
	desired := r.windowsPublisherDaemonSet()
	existing := &appsv1.DaemonSet{}
	err := r.reader.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return fmt.Errorf("getting Windows publisher: %w", err)
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec = desired.Spec
	existing.OwnerReferences = desired.OwnerReferences
	return r.Patch(ctx, existing, patch)
}
