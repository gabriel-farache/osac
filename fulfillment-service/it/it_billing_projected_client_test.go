/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
*/

package it

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/osac-project/osac/fulfillment-service/internal/auth"
	"github.com/osac-project/osac/fulfillment-service/internal/testing/billingfake"
	"github.com/osac-project/osac/fulfillment-service/internal/testing/billingstub"
	"github.com/osac-project/osac/fulfillment-service/internal/uuid"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
)

// DEV installed-boundary evidence only: real kubelet projection -> production Go client ->
// deployed TLS/authn/OPA/Fulfillment/PostgreSQL. Provider discovery uses the existing HTTPS
// stub. No usage component, heartbeat, broker, VM, or provider journey is created or claimed.
var _ = Describe("Billing projected-file production client", Label("billing-projected-client"), Serial, func() {
	It("resolves a CONNECTED dummy assignment from the actual projected token mount", func(ctx context.Context) {
		image := os.Getenv("OSAC_IT_METERING_PROBE_IMAGE")
		if image == "" {
			Skip("requires prebuilt OSAC_IT_METERING_PROBE_IMAGE; skip is not projected-client coverage")
		}
		Expect(strings.TrimSpace(image) == image && !strings.ContainsAny(image, " \t\r\n")).To(BeTrue(), "invalid probe image")
		listen, stubURL := os.Getenv("OSAC_IT_BILLING_STUB_LISTEN"), os.Getenv("OSAC_IT_BILLING_STUB_URL")
		if listen == "" || stubURL == "" {
			Skip("requires OSAC_IT_BILLING_STUB_LISTEN and OSAC_IT_BILLING_STUB_URL")
		}
		kube := tool.KubeClientSet()
		deployments, err := kube.AppsV1().Deployments(billingStubNamespace).List(ctx,
			metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=metering"})
		Expect(err).NotTo(HaveOccurred())
		// Scalar counts keep full Pod specs and fixture IDs out of failure diagnostics.
		deploymentCount := len(deployments.Items)
		Expect(deploymentCount).To(Equal(1), "requires exactly one final metering Deployment")
		deployment := deployments.Items[0]
		Expect(deployment.DeletionTimestamp).To(BeNil())
		Expect(deployment.Spec.Replicas).To(Equal(new(int32(1))))
		Expect(deployment.Status.ObservedGeneration).To(Equal(deployment.Generation))
		Expect(deployment.Status.UpdatedReplicas).To(Equal(int32(1)))
		Expect(deployment.Status.AvailableReplicas).To(Equal(int32(1)))
		selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
		Expect(err).NotTo(HaveOccurred())
		Expect(selector.Empty()).To(BeFalse())
		pods, err := kube.CoreV1().Pods(billingStubNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		Expect(err).NotTo(HaveOccurred())
		podCount := len(pods.Items)
		Expect(podCount).To(Equal(1), "requires exactly one final producer Pod")
		producer := pods.Items[0]
		Expect(producer.DeletionTimestamp).To(BeNil())
		Expect(producer.Status.Phase).To(Equal(corev1.PodRunning))
		containerCount := len(producer.Spec.Containers)
		Expect(containerCount).To(Equal(1), "requires the producer container only")
		Expect(producer.Status.ContainerStatuses).To(HaveLen(1))
		Expect(producer.Status.ContainerStatuses[0].Ready).To(BeTrue())
		templateContainerCount := len(deployment.Spec.Template.Spec.Containers)
		Expect(templateContainerCount).To(Equal(1), "requires one rendered producer container")
		container := producer.Spec.Containers[0]
		template := deployment.Spec.Template.Spec.Containers[0]
		Expect(container.Image).To(Equal(template.Image))
		Expect(producer.Spec.ServiceAccountName).NotTo(BeEmpty())
		Expect(producer.Spec.ServiceAccountName).To(Equal(deployment.Spec.Template.Spec.ServiceAccountName))
		Expect(container.EnvFrom).To(BeEmpty())

		// Construct from scratch, never copying controller metadata, application labels,
		// init containers, DB/Kafka credentials, or the default-audience token mount.
		probe := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: billingStubNamespace, Name: "billing-projected-client-" + uuid.New(),
				Labels: map[string]string{"testing.osac.openshift.io/billing-projected-client": uuid.New()},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: producer.Spec.ServiceAccountName, AutomountServiceAccountToken: new(false),
				SecurityContext: producer.Spec.SecurityContext.DeepCopy(), RestartPolicy: corev1.RestartPolicyNever,
				ActiveDeadlineSeconds: new(int64(90)),
				Containers: []corev1.Container{{
					Name: "projected-client", Image: image, ImagePullPolicy: container.ImagePullPolicy,
					SecurityContext: container.SecurityContext.DeepCopy(),
					Command:         []string{"/projected-client.test"},
					Args:            []string{"-test.run=^TestProjectedLiveFulfillmentClient$", "-test.v", "-test.count=1", "-test.timeout=20s"},
				}},
			},
		}
		Expect(selector.Matches(labels.Set(probe.Labels))).To(BeFalse(), "probe must not match the producer selector")
		probeContainer := &probe.Spec.Containers[0]
		for _, name := range []string{"FULFILLMENT_ADDRESS", "FULFILLMENT_CA_FILE", "FULFILLMENT_TOKEN_FILE"} {
			var matched []corev1.EnvVar
			for _, env := range container.Env {
				if env.Name == name {
					matched = append(matched, env)
				}
			}
			Expect(matched).To(HaveLen(1), "requires exactly one rendered client setting")
			env := matched[0]
			Expect(env.ValueFrom).To(BeNil())
			Expect(env.Value).NotTo(BeEmpty())
			Expect(template.Env).To(ContainElement(env), "live client setting must match the final Deployment")
			probeContainer.Env = append(probeContainer.Env, env)
			if name == "FULFILLMENT_ADDRESS" {
				continue
			}
			Expect(path.IsAbs(env.Value) && path.Clean(env.Value) == env.Value).To(BeTrue(), "requires an absolute mounted file path")
			var mounts []corev1.VolumeMount
			for _, mount := range container.VolumeMounts {
				if mount.MountPath == path.Dir(env.Value) {
					mounts = append(mounts, mount)
				}
			}
			Expect(mounts).To(HaveLen(1))
			mount := mounts[0]
			Expect(mount.ReadOnly).To(BeTrue())
			Expect(mount.SubPath).To(BeEmpty())
			Expect(mount.SubPathExpr).To(BeEmpty())
			Expect(template.VolumeMounts).To(ContainElement(mount))
			var volumes []corev1.Volume
			for _, volume := range producer.Spec.Volumes {
				if volume.Name == mount.Name {
					volumes = append(volumes, volume)
				}
			}
			Expect(volumes).To(HaveLen(1))
			volume := volumes[0]
			Expect(deployment.Spec.Template.Spec.Volumes).To(ContainElement(volume), "live client volume must match the final Deployment")
			if name == "FULFILLMENT_TOKEN_FILE" {
				Expect(volume.Projected).NotTo(BeNil())
				Expect(volume.Projected.Sources).To(HaveLen(1))
				source := volume.Projected.Sources[0]
				Expect(source.ServiceAccountToken).NotTo(BeNil())
				Expect(source).To(Equal(corev1.VolumeProjection{ServiceAccountToken: source.ServiceAccountToken}))
				Expect(source.ServiceAccountToken.Audience).To(Equal(auth.MeteringAudience))
				Expect(source.ServiceAccountToken.ExpirationSeconds).To(Equal(new(int64(600))))
				Expect(source.ServiceAccountToken.Path).To(Equal(path.Base(env.Value)))
			} else {
				Expect(volume.ConfigMap).NotTo(BeNil(), "CA must come from the deployed trust ConfigMap, not credentials")
				Expect(volume.ConfigMap.Items).To(HaveLen(1))
				Expect(volume.ConfigMap.Items[0].Path).To(Equal(path.Base(env.Value)))
			}
			// Copy actual API-defaulted modes and effective admission-assigned UID/fsGroup.
			probe.Spec.Volumes = append(probe.Spec.Volumes, *volume.DeepCopy())
			probeContainer.VolumeMounts = append(probeContainer.VolumeMounts, mount)
		}
		Expect(probe.Spec.Volumes).To(HaveLen(2))

		started := metav1.Now()
		account := billingfake.AccountSpec{ID: uuid.New(), Name: "projected-client-" + uuid.New(),
			ExternalID: "projected-client-dummy-" + uuid.New(), RawStatus: "Active", ApprovalStatus: "Approved"}
		stub := billingstub.New(billingfake.Scenario{Accounts: []billingfake.AccountSpec{account}})
		configureBillingStub(ctx, stub, listen, stubURL)
		tenant := newBillingTenant(ctx, nil, nil)
		accounts, err := publicv1.NewBillingAccountsClient(tool.ExternalView().AdminConn()).List(ctx,
			publicv1.BillingAccountsListRequest_builder{Filter: new(fmt.Sprintf("this.display_name == %q", account.Name))}.Build())
		Expect(err).NotTo(HaveOccurred())
		accountCount := len(accounts.GetItems())
		Expect(accountCount).To(Equal(1), "requires exactly one dummy account")
		assigned, err := publicv1.NewTenantBillingAssignmentsClient(tool.ExternalView().AdminConn()).Create(ctx,
			publicv1.TenantBillingAssignmentsCreateRequest_builder{Object: publicv1.TenantBillingAssignment_builder{
				Metadata: publicv1.Metadata_builder{Name: "projected-client-" + uuid.New()}.Build(),
				Spec: publicv1.TenantBillingAssignmentSpec_builder{
					TenantId: tenant.GetMetadata().GetName(), BillingAccountId: accounts.GetItems()[0].GetId(),
				}.Build(),
			}.Build()}.Build())
		Expect(err).NotTo(HaveOccurred())
		Expect(assigned.GetObject().GetStatus().GetConnectionState()).To(Equal(
			publicv1.TenantBillingAssignmentConnectionState_TENANT_BILLING_ASSIGNMENT_CONNECTION_STATE_CONNECTED))
		Expect(assigned.GetObject().GetStatus().GetEffectiveTo()).To(BeNil())
		probeContainer.Env = append(probeContainer.Env,
			corev1.EnvVar{Name: "OSAC_IT_METERING_PROJECTED_LIVE", Value: "1"},
			corev1.EnvVar{Name: "OSAC_IT_METERING_TENANT", Value: tenant.GetMetadata().GetName()},
			corev1.EnvVar{Name: "OSAC_IT_METERING_EXPECTED_EXTERNAL_ID", Value: account.ExternalID})
		before := len(stub.Requests())
		created, err := kube.CoreV1().Pods(billingStubNamespace).Create(ctx, probe, metav1.CreateOptions{})
		if err != nil {
			Fail("creating the owned projected-client probe failed; inspect only redacted diagnostics")
		}
		DeferCleanup(func(ctx context.Context) {
			err := kube.CoreV1().Pods(billingStubNamespace).Delete(ctx, created.Name,
				metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &created.UID}})
			Expect(err == nil || apierrors.IsNotFound(err)).To(BeTrue(), "deleting the owned probe failed")
			Eventually(ctx, func() bool {
				_, err := kube.CoreV1().Pods(billingStubNamespace).Get(ctx, created.Name, metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}, time.Minute, time.Second).Should(BeTrue(), "owned probe must be removed")
		})
		Expect(created.Spec.SecurityContext).To(Equal(probe.Spec.SecurityContext))
		createdContainerCount := len(created.Spec.Containers)
		Expect(createdContainerCount).To(Equal(1), "admission must not add another probe container")
		Expect(created.Spec.Containers[0].SecurityContext).To(Equal(container.SecurityContext))
		Expect(created.Spec.Containers[0].VolumeMounts).To(Equal(probeContainer.VolumeMounts))
		Expect(created.Spec.Volumes).To(Equal(probe.Spec.Volumes), "admission must preserve the two client volumes and their modes")
		Eventually(ctx, func(g Gomega) {
			current, err := kube.CoreV1().Pods(billingStubNamespace).Get(ctx, created.Name, metav1.GetOptions{})
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(current.UID).To(Equal(created.UID), "probe identity changed")
			g.Expect(current.Status.Phase).NotTo(Equal(corev1.PodFailed), "projected-client probe failed; inspect only redacted diagnostics")
			g.Expect(current.Status.Phase).To(Equal(corev1.PodSucceeded))
		}, 2*time.Minute, time.Second).Should(Succeed())
		raw, err := kube.CoreV1().Pods(billingStubNamespace).GetLogs(created.Name,
			&corev1.PodLogOptions{Container: probeContainer.Name}).DoRaw(ctx)
		Expect(err).NotTo(HaveOccurred())
		// Boolean assertions deliberately avoid printing either logs or the dummy external ID.
		Expect(bytes.Contains(raw, []byte(account.ExternalID))).To(BeFalse(), "dummy account leaked into probe output")
		output := string(raw)
		Expect(strings.Count(output, "=== RUN   TestProjectedLiveFulfillmentClient\n") == 1 &&
			strings.Count(output, "=== RUN") == 1 &&
			strings.Count(output, "--- PASS: TestProjectedLiveFulfillmentClient (") == 1 &&
			strings.HasSuffix(output, "\nPASS\n") && !strings.Contains(output, "SKIP") && !strings.Contains(output, "FAIL")).
			To(BeTrue(), "probe must execute exactly the live Go test and PASS, without a skip")
		after := len(stub.Requests())
		Expect(after).To(Equal(before), "the projected private read must not call the billing provider")

		// Check receiver and producer logs during the fixture/probe without dumping them.
		receiver, err := kube.AppsV1().Deployments(billingStubNamespace).Get(ctx, billingStubDeployment, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		receiverSelector, err := metav1.LabelSelectorAsSelector(receiver.Spec.Selector)
		Expect(err).NotTo(HaveOccurred())
		receivers, err := kube.CoreV1().Pods(billingStubNamespace).List(ctx, metav1.ListOptions{LabelSelector: receiverSelector.String()})
		Expect(err).NotTo(HaveOccurred())
		checked := 0
		for _, pod := range append(receivers.Items, producer) {
			if pod.DeletionTimestamp != nil {
				continue
			}
			name := billingStubContainer
			if pod.UID == producer.UID {
				name = container.Name
			}
			logs, err := kube.CoreV1().Pods(billingStubNamespace).GetLogs(pod.Name,
				&corev1.PodLogOptions{Container: name, SinceTime: &started}).DoRaw(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(bytes.Contains(logs, []byte(account.ExternalID))).To(BeFalse(), "dummy account leaked into component logs")
			checked++
		}
		Expect(checked).To(BeNumerically(">=", 2), "must inspect receiver and producer logs")
	}, SpecTimeout(6*time.Minute))
})
