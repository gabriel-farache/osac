/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
*/

package it

import (
	"context"
	"fmt"
	"os"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/osac-project/osac/fulfillment-service/internal/auth"
	"github.com/osac-project/osac/fulfillment-service/internal/testing/billingfake"
	"github.com/osac-project/osac/fulfillment-service/internal/testing/billingstub"
	"github.com/osac-project/osac/fulfillment-service/internal/uuid"
	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
)

// This installed-boundary spec needs the rendered producer, real Kubernetes
// TokenRequest signing, Fulfillment and PostgreSQL. It does not contact a provider.
var _ = Describe("Metering projected-token attribution", Label("metering-attribution"), Serial, func() {
	It("binds the existing producer pod identity and dedicated audience without granting administrators", func(ctx context.Context) {
		pods, err := tool.kubeClientSet.CoreV1().Pods("osac").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=metering"})
		Expect(err).NotTo(HaveOccurred())
		Expect(pods.Items).To(HaveLen(1))
		pod := pods.Items[0]
		Expect(pod.Spec.ServiceAccountName).To(Equal("osac-metering"))
		var projected bool
		for _, volume := range pod.Spec.Volumes {
			if volume.Name != "attribution-token" {
				continue
			}
			Expect(volume.Projected).NotTo(BeNil())
			for _, source := range volume.Projected.Sources {
				if token := source.ServiceAccountToken; token != nil {
					Expect(token.Audience).To(Equal(auth.MeteringAudience))
					Expect(token.Path).To(Equal("token"))
					projected = true
				}
			}
		}
		Expect(projected).To(BeTrue())
		for _, container := range pod.Spec.InitContainers {
			for _, mount := range container.VolumeMounts {
				Expect(mount.Name).NotTo(Equal("attribution-token"))
			}
		}
		Expect(pod.Spec.Containers).To(HaveLen(1))
		Expect(pod.Spec.Containers[0].VolumeMounts).To(ContainElement(HaveField("Name", "attribution-token")))
		client := privatev1.NewTenantBillingAssignmentsClient(tool.InternalView().AnonymousConn())
		request := privatev1.TenantBillingAssignmentsGetActiveBillingAccountRequest_builder{TenantId: "metering-unassigned"}.Build()
		for _, audience := range []string{auth.MeteringAudience, ""} {
			call := meteringPodCall(ctx, pod, audience)
			response, err := client.GetActiveBillingAccount(call, request)
			Expect(response).To(BeNil())
			want := codes.PermissionDenied
			if audience == auth.MeteringAudience {
				want = codes.NotFound
			}
			Expect(status.Code(err)).To(Equal(want))
		}
		_, err = privatev1.NewTenantBillingAssignmentsClient(tool.InternalView().AdminConn()).GetActiveBillingAccount(ctx, request)
		Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
		_, err = privatev1.NewTenantBillingAssignmentsClient(tool.InternalView().EmergencyConn()).GetActiveBillingAccount(ctx, request)
		Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
	})

	It("reads a connected assignment through deployed Fulfillment and PostgreSQL without provider calls", func(ctx context.Context) {
		listen, stubURL := os.Getenv("OSAC_IT_BILLING_STUB_LISTEN"), os.Getenv("OSAC_IT_BILLING_STUB_URL")
		if listen == "" || stubURL == "" {
			Skip("requires OSAC_IT_BILLING_STUB_LISTEN and OSAC_IT_BILLING_STUB_URL")
		}
		account := billingfake.AccountSpec{ID: uuid.New(), Name: "metering-" + uuid.New(), ExternalID: "metering-external-" + uuid.New(), RawStatus: "Active", ApprovalStatus: "Approved"}
		stub := billingstub.New(billingfake.Scenario{Accounts: []billingfake.AccountSpec{account}})
		configureBillingStub(ctx, stub, listen, stubURL)
		tenant := newBillingTenant(ctx, nil, nil)
		accounts, err := publicv1.NewBillingAccountsClient(tool.ExternalView().AdminConn()).List(ctx,
			publicv1.BillingAccountsListRequest_builder{Filter: new(fmt.Sprintf("this.display_name == %q", account.Name))}.Build())
		Expect(err).NotTo(HaveOccurred())
		Expect(accounts.GetItems()).To(HaveLen(1))
		_, err = publicv1.NewTenantBillingAssignmentsClient(tool.ExternalView().AdminConn()).Create(ctx,
			publicv1.TenantBillingAssignmentsCreateRequest_builder{Object: publicv1.TenantBillingAssignment_builder{
				Metadata: publicv1.Metadata_builder{Name: "metering-" + uuid.New()}.Build(),
				Spec:     publicv1.TenantBillingAssignmentSpec_builder{TenantId: tenant.GetMetadata().GetName(), BillingAccountId: accounts.GetItems()[0].GetId()}.Build(),
			}.Build()}.Build())
		Expect(err).NotTo(HaveOccurred())
		pods, err := tool.kubeClientSet.CoreV1().Pods("osac").List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=metering"})
		Expect(err).NotTo(HaveOccurred())
		Expect(pods.Items).To(HaveLen(1))
		before := len(stub.Requests())
		response, err := privatev1.NewTenantBillingAssignmentsClient(tool.InternalView().AnonymousConn()).GetActiveBillingAccount(
			meteringPodCall(ctx, pods.Items[0], auth.MeteringAudience),
			privatev1.TenantBillingAssignmentsGetActiveBillingAccountRequest_builder{TenantId: tenant.GetMetadata().GetName()}.Build())
		Expect(err).NotTo(HaveOccurred())
		Expect(response.GetExternalBillingAccountId()).To(Equal(account.ExternalID))
		Expect(stub.Requests()).To(HaveLen(before))
	})
})

func meteringPodCall(ctx context.Context, pod corev1.Pod, audience string) context.Context {
	GinkgoHelper()
	// This uses the projection's real pod-bound TokenRequest mechanism, not its
	// mounted credential bytes, so no token needs to leave a workload through exec.
	var audiences []string
	if audience != "" {
		audiences = []string{audience}
	}
	token, err := tool.kubeClientSet.CoreV1().ServiceAccounts("osac").CreateToken(ctx, pod.Spec.ServiceAccountName,
		&authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
			Audiences: audiences, ExpirationSeconds: new(int64(600)),
			BoundObjectRef: &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID},
		}}, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+token.Status.Token))
}
