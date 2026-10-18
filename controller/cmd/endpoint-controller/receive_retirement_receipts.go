package main

import (
	"context"
	"fmt"
	"maps"

	"github.com/appmana/cloud-provisioning/controller/pkg/attachment"
	"github.com/appmana/cloud-provisioning/controller/pkg/handover"
	"github.com/appmana/cloud-provisioning/controller/pkg/tunnel"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Dispatch from persisted inventory state, not an agent-selected operation.
// Domain-separated receipt digests prevent reuse across handover and deletion.
func (r *meshReconciler) resolveReceiveReceiptCollection(ctx context.Context, api kubernetes.Interface, policy receiveReceiptPolicy, machineUID string) (handover.ReceiptCollection, error) {
	mesh := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
		return handover.ReceiptCollection{}, err
	}
	inventory, _, err := tunnel.ReadReceiveInventory(ctx, r.receiveJournal.ConfigMaps, r.receiveJournal.ClusterUID, r.secretNamespace, r.secretName, string(mesh.UID))
	if err != nil {
		return handover.ReceiptCollection{}, err
	}
	if len(inventory.Publications[machineUID].Retirement) != 0 {
		return r.resolveReceiveRetirementReceipts(ctx, api, policy, machineUID)
	}
	store, policies, err := r.resolveReceiveReceipts(ctx, api, policy, machineUID)
	if err != nil {
		return handover.ReceiptCollection{}, err
	}
	return handover.NewHandoverReceiptCollection(ctx, store, policies)
}

// Resolve retirement independently of the departed Machine, Node and adoption
// Secret. Every retained consumer still needs its current published identity.
func (r *meshReconciler) resolveReceiveRetirementReceipts(ctx context.Context, api kubernetes.Interface, policy receiveReceiptPolicy, machineUID string) (handover.ReceiptCollection, error) {
	fail := func(err error) (handover.ReceiptCollection, error) { return handover.ReceiptCollection{}, err }
	if api == nil || r.receivePreparation == nil || r.receivePreparation.Journal == nil || r.dialerCloudImage != "" {
		return fail(fmt.Errorf("retirement receipts require journal policy and image-entrypoint agents"))
	}
	mesh := &corev1.Secret{}
	if err := r.reader.Get(ctx, client.ObjectKey{Namespace: r.secretNamespace, Name: r.secretName}, mesh); err != nil {
		return fail(err)
	}
	store := r.receiveJournal
	store.Name = tunnel.ReceiveTransitionName(machineUID)
	source, err := attachment.ReadReceiveRetirementSet(ctx, api, store, r.secretNamespace, r.secretName, string(mesh.UID), machineUID)
	if err != nil {
		return fail(err)
	}
	snapshot := source.Journals[machineUID]
	store.ExpectedUID = string(snapshot.Metadata().UID)
	policies, err := r.retirementConsumerPolicies(ctx, api, policy, source.Mesh, source.Withdrawal.Consumers)
	if err != nil {
		return fail(err)
	}
	recheck := func() error {
		current, err := r.retirementConsumerPolicies(ctx, api, policy, source.Mesh, source.Withdrawal.Consumers)
		if err != nil {
			return err
		}
		if !maps.Equal(current, policies) {
			return fmt.Errorf("retirement consumer policy changed during authentication")
		}
		return source.Recheck(ctx, api, store)
	}
	return attachment.NewReceiveRetirementReceiptCollection(ctx, api, store, string(source.Metadata.UID), source.Mesh, snapshot, policies, r.receivePreparation.Journal.MaxReceiptAge.Duration, recheck)
}

// Reuse published-consumer discovery, including CAPI placement and adoption
// identity, without filtering unavailable participants or replacing saved UIDs.
func (r *meshReconciler) retirementConsumerPolicies(ctx context.Context, api kubernetes.Interface, policy receiveReceiptPolicy, mesh *corev1.Secret, retained []attachment.ConsumerTarget) (map[string]handover.PodReceiptAuthenticator, error) {
	if api == nil || mesh == nil || len(retained) == 0 || r.dialerCloudImage != "" {
		return nil, fmt.Errorf("retirement requires retained consumers and image-entrypoint agents")
	}
	resolver := attachment.MeshConsumerResolver{Reader: r.reader, Namespace: r.secretNamespace, SecretName: r.secretName, SecretUID: string(mesh.UID)}
	current, err := resolver.PublishedConsumers(ctx, mesh)
	if err != nil {
		return nil, err
	}
	expected := map[string]attachment.ConsumerTarget{}
	for _, consumer := range retained {
		if consumer.NodeUID == "" {
			return nil, fmt.Errorf("retained consumer has no identity")
		}
		if _, found := expected[consumer.NodeUID]; found {
			return nil, fmt.Errorf("duplicate retained consumer")
		}
		expected[consumer.NodeUID] = consumer
	}
	if len(current) != len(expected) {
		return nil, fmt.Errorf("retirement consumer membership changed")
	}
	for _, consumer := range current {
		if expected[consumer.NodeUID] != consumer {
			return nil, fmt.Errorf("retirement consumer identity changed")
		}
	}
	sa, err := api.CoreV1().ServiceAccounts(r.secretNamespace).Get(ctx, r.dialerServiceAccount, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if sa.UID == "" || sa.DeletionTimestamp != nil {
		return nil, fmt.Errorf("receipt agent account is not live")
	}
	policies := map[string]handover.PodReceiptAuthenticator{}
	for _, consumer := range retained {
		node, err := api.CoreV1().Nodes().Get(ctx, consumer.NodeName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if string(node.UID) != consumer.NodeUID || node.DeletionTimestamp != nil || node.Status.NodeInfo.OperatingSystem != "linux" {
			return nil, fmt.Errorf("retirement participant identity or platform changed")
		}
		imageID := policy.ImageIDs[node.Status.NodeInfo.Architecture]
		if imageID == "" {
			return nil, fmt.Errorf("retirement participant architecture has no admitted release")
		}
		daemonName := r.dialerDaemonSetName
		if !consumer.Site {
			daemonName = r.dialerCloudDaemonSetName
		}
		daemon, err := api.AppsV1().DaemonSets(r.secretNamespace).Get(ctx, daemonName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		if daemon.UID == "" || daemon.DeletionTimestamp != nil {
			return nil, fmt.Errorf("receipt agent daemon is not live")
		}
		policies[consumer.NodeUID] = handover.PodReceiptAuthenticator{API: api, Audience: receiveReceiptAudience, Namespace: r.secretNamespace, ServiceAccountName: sa.Name, ServiceAccountUID: string(sa.UID), DaemonSetName: daemon.Name, DaemonSetUID: string(daemon.UID), ContainerName: "dialer", Image: r.dialerImage, ImageID: imageID}
	}
	return policies, nil
}
