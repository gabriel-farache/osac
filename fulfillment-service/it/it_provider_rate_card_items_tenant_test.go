/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package it

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	"github.com/osac-project/osac/fulfillment-service/internal/testing/billingfake"
	"github.com/osac-project/osac/fulfillment-service/internal/testing/billingstub"
	"github.com/osac-project/osac/fulfillment-service/internal/uuid"
	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
)

// Fulfillment, PostgreSQL, authentication, both gateways and the M360 adapter are real; only M360 is an HTTPS stub.
var _ = Describe("Tenant-scoped provider rates", Label("billing", "billing-configured", "tenant-rates"), Serial, func() {
	It("enforces eligibility and authorization on public/private gRPC and REST without global fallback", func(ctx context.Context) {
		listen, stubURL := os.Getenv("OSAC_IT_BILLING_STUB_LISTEN"), os.Getenv("OSAC_IT_BILLING_STUB_URL")
		if listen == "" || stubURL == "" {
			Skip("OSAC_IT_BILLING_STUB_LISTEN and OSAC_IT_BILLING_STUB_URL are not set")
		}
		scenario := billingRateCardScenario("tenant-rates-small")
		for range 2 {
			suffix := uuid.New()
			scenario.Accounts = append(scenario.Accounts, billingfake.AccountSpec{
				ID: uuid.New(), Name: "tenant-rates-" + suffix, ExternalID: "external-rates-" + suffix,
				RawStatus: "Active", ApprovalStatus: "Approved",
			})
		}
		stub := billingstub.New(scenario)
		requestCount := func() int { return len(stub.Requests()) }
		configureBillingStub(ctx, stub, listen, stubURL)
		accounts := publicv1.NewBillingAccountsClient(tool.ExternalView().AdminConn())
		var stored []*publicv1.BillingAccount
		Eventually(ctx, func(g Gomega) {
			stored = nil
			for _, fixture := range scenario.Accounts {
				response, err := accounts.List(ctx, publicv1.BillingAccountsListRequest_builder{
					Filter: proto.String(fmt.Sprintf("this.display_name == %q", fixture.Name)),
				}.Build())
				g.Expect(err).ToNot(HaveOccurred())
				for _, account := range response.GetItems() {
					if account.GetDisplayName() == fixture.Name {
						stored = append(stored, account)
					}
				}
			}
			g.Expect(stored).To(HaveLen(2))
		}, time.Minute, time.Second).Should(Succeed())
		tenants := []*privatev1.Tenant{newBillingTenant(ctx, nil, nil), newBillingTenant(ctx, nil, nil)}
		unassigned := newBillingTenant(ctx, nil, nil)
		assignments := publicv1.NewTenantBillingAssignmentsClient(tool.ExternalView().AdminConn())
		for i, tenant := range tenants {
			Expect(stored[i].GetId()).ToNot(BeElementOf(scenario.Accounts[i].ID, scenario.Accounts[i].ExternalID,
				tenant.GetMetadata().GetName()))
			created, err := assignments.Create(ctx, publicv1.TenantBillingAssignmentsCreateRequest_builder{
				Object: publicv1.TenantBillingAssignment_builder{
					Metadata: publicv1.Metadata_builder{Name: "tenant-rates-" + uuid.New()}.Build(),
					Spec: publicv1.TenantBillingAssignmentSpec_builder{
						TenantId: tenant.GetMetadata().GetName(), BillingAccountId: stored[i].GetId(),
					}.Build(),
				}.Build(),
			}.Build())
			Expect(err).ToNot(HaveOccurred())
			Expect(created.GetObject().GetStatus().GetConnectionState()).To(Equal(
				publicv1.TenantBillingAssignmentConnectionState_TENANT_BILLING_ASSIGNMENT_CONNECTION_STATE_CONNECTED))
		}

		rateStart := requestCount()
		for _, surface := range []tenantRatesSurface{{}, {private: true}, {rest: true}, {private: true, rest: true}} {
			By(fmt.Sprintf("checking tenant routing (private=%t REST=%t)", surface.private, surface.rest))
			global, err := surface.list(ctx, nil, "USD", false)
			Expect(err).ToNot(HaveOccurred())
			Expect(global.GetItems()).To(HaveLen(1), "invalid selectors must fail while global rows are available")
			encoded, err := protojson.Marshal(global)
			Expect(err).ToNot(HaveOccurred())
			for _, fixture := range scenario.Accounts {
				Expect(string(encoded)).ToNot(ContainSubstring(fixture.ID))
				Expect(string(encoded)).ToNot(ContainSubstring(fixture.ExternalID))
			}
			for _, selector := range []string{"", tenants[0].GetMetadata().GetName(), tenants[1].GetMetadata().GetName()} {
				got, err := surface.list(ctx, &selector, "USD", false)
				Expect(err).ToNot(HaveOccurred())
				Expect(proto.Equal(got, global)).To(BeTrue())
			}
			for _, selector := range []string{"missing-" + uuid.New(), " ", scenario.Accounts[0].ID,
				scenario.Accounts[0].ExternalID, stored[0].GetId(), unassigned.GetMetadata().GetName()} {
				before := requestCount()
				got, err := surface.list(ctx, &selector, "USD", false)
				Expect(got).To(BeNil())
				code, reason := codes.NotFound, "BILLING_TENANT_NOT_FOUND"
				if selector == unassigned.GetMetadata().GetName() {
					code, reason = codes.FailedPrecondition, "BILLING_ASSIGNMENT_NOT_FOUND"
				}
				expectTenantRatesError(err, code, reason)
				Expect(requestCount()).To(Equal(before), "invalid context must not call the provider")
			}
			before := requestCount()
			_, err = surface.list(ctx, proto.String(tenants[0].GetMetadata().GetName()), "USD", true)
			Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
			Expect(requestCount()).To(Equal(before), "authorization denial must precede provider work")
		}
		for _, request := range stub.Requests()[rateStart:] {
			Expect(request.Op).To(BeElementOf("FindServiceByName", "ListPriceableAttributes", "GetServiceRateCard"))
		}

		By("retaining the deployed CLI selector")
		home, err := tool.NewCLIHomeDir()
		Expect(err).ToNot(HaveOccurred())
		DeferCleanup(func() { Expect(os.RemoveAll(home)).To(Succeed()) })
		_, _, exit := tool.LoginCLI(ctx, home, adminUsername, adminsPassword)
		Expect(exit).To(BeZero())
		stdout, _, exit := tool.RunCLI(ctx, home, "get", "providerratecarditem", "--tenant-id",
			tenants[0].GetMetadata().GetName(), "--service", "compute_instance", "--currency-code", "USD", "--limit", "0", "-o", "json")
		Expect(exit).To(BeZero())
		expectBillingPricingCountOnlyCLI(NewWithT(GinkgoT()), stdout, "json", 1)

		By("checking stored account eligibility before a warm rate cache")
		scenario.Accounts[0].RawStatus = "Inactive"
		stub.SetScenario(scenario)
		// Discovery, not the Rates read, records the new last-observed account state.
		Eventually(ctx, func(g Gomega) {
			response, err := accounts.List(ctx, publicv1.BillingAccountsListRequest_builder{
				Filter: proto.String(fmt.Sprintf("this.display_name == %q", scenario.Accounts[0].Name)),
			}.Build())
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(response.GetItems()).To(HaveLen(1))
			g.Expect(response.GetItems()[0].GetState()).To(Equal(publicv1.BillingAccountState_BILLING_ACCOUNT_STATE_INACTIVE))
		}, time.Minute, time.Second).Should(Succeed())
		before := requestCount()
		for _, surface := range []tenantRatesSurface{{}, {private: true}, {rest: true}, {private: true, rest: true}} {
			_, err := surface.list(ctx, proto.String(tenants[0].GetMetadata().GetName()), "USD", false)
			expectTenantRatesError(err, codes.FailedPrecondition, "ACCOUNT_INACTIVE")
			got, err := surface.list(ctx, proto.String(tenants[1].GetMetadata().GetName()), "USD", false)
			Expect(err).ToNot(HaveOccurred())
			Expect(got.GetItems()).To(HaveLen(1))
		}
		Expect(requestCount()).To(Equal(before))

		By("retaining classified provider failure on cold currency keys, never cached USD")
		scenario.Failures = map[string]billingfake.Failure{"GetServiceRateCard": {Reason: billing.ReasonUnauthorized, Status: 401}}
		stub.SetScenario(scenario)
		for i, surface := range []tenantRatesSurface{{}, {private: true}, {rest: true}, {private: true, rest: true}} {
			before := stub.Count("GetServiceRateCard")
			_, err := surface.list(ctx, proto.String(tenants[1].GetMetadata().GetName()), fmt.Sprintf("cold-%d", i), false)
			expectTenantRatesError(err, codes.Unavailable, string(billing.ReasonUnauthorized))
			Expect(status.Convert(err).Message()).To(Equal("billing provider is unavailable"))
			Expect(stub.Count("GetServiceRateCard")).To(BeNumerically(">", before), "a cold currency must reach the provider")
		}
		By("rejecting a deleted billed tenant, retaining normal tombstone cleanup")
		cleanupBillingTenant(ctx, privatev1.NewTenantsClient(tool.InternalView().AdminConn()), tenants[0].GetId())
		before = requestCount()
		for _, surface := range []tenantRatesSurface{{}, {private: true}, {rest: true}, {private: true, rest: true}} {
			_, err := surface.list(ctx, proto.String(tenants[0].GetMetadata().GetName()), "USD", false)
			expectTenantRatesError(err, codes.NotFound, "BILLING_TENANT_NOT_FOUND")
		}
		Expect(requestCount()).To(Equal(before))
		writes := len(stub.Writes())
		Expect(writes).To(BeZero())
	})
})

type tenantRatesSurface struct{ private, rest bool }

func (s tenantRatesSurface) list(ctx context.Context, tenant *string, currency string, user bool) (*publicv1.ProviderRateCardItemsListResponse, error) {
	request := publicv1.ProviderRateCardItemsListRequest_builder{
		TenantId: tenant, CurrencyCode: proto.String(currency), Service: proto.String("compute_instance"),
	}.Build()
	view := tool.ExternalView()
	if s.private {
		view = tool.InternalView()
	}
	conn, client := view.AdminConn(), view.AdminClient()
	if user {
		conn, client = view.UserConn(), view.UserClient()
	}
	var body []byte
	if s.rest {
		prefix := "fulfillment"
		if s.private {
			prefix = "private"
		}
		query := url.Values{"currency_code": {currency}, "service": {"compute_instance"}}
		if tenant != nil {
			query.Set("tenant_id", *tenant)
		}
		data, code, err := getBillingPricingREST(ctx, client, "/api/"+prefix+"/v1/billing/provider_rate_card_items?"+query.Encode())
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			rejection := &statuspb.Status{}
			if err := protojson.Unmarshal(data, rejection); err != nil {
				return nil, err
			}
			if rejection.GetCode() == 0 {
				return nil, fmt.Errorf("unexpected REST status %d", code)
			}
			return nil, status.ErrorProto(rejection)
		}
		body = data
	} else if s.private {
		private := privatev1.ProviderRateCardItemsListRequest_builder{
			TenantId: tenant, CurrencyCode: proto.String(currency), Service: proto.String(request.GetService()),
		}.Build()
		response, err := privatev1.NewProviderRateCardItemsClient(conn).List(ctx, private)
		if err != nil {
			return nil, err
		}
		body, err = protojson.Marshal(response)
		if err != nil {
			return nil, err
		}
	} else {
		return publicv1.NewProviderRateCardItemsClient(conn).List(ctx, request)
	}
	response := &publicv1.ProviderRateCardItemsListResponse{}
	if err := protojson.Unmarshal(body, response); err != nil {
		return nil, err
	}
	return response, nil
}

func expectTenantRatesError(err error, code codes.Code, reason string) {
	GinkgoHelper()
	Expect(status.Code(err)).To(Equal(code))
	details := status.Convert(err).Details()
	Expect(details).To(HaveLen(1))
	Expect(details[0].(*errdetails.ErrorInfo).GetReason()).To(Equal(reason))
}
