/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package servers

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/mock/gomock"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/osac-project/osac/fulfillment-service/internal/auth"
	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	"github.com/osac-project/osac/fulfillment-service/internal/collections"
	"github.com/osac-project/osac/fulfillment-service/internal/database"
	"github.com/osac-project/osac/fulfillment-service/internal/testing/billingfake"
	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
)

type tenantRatesRow func(...any) error

func (r tenantRatesRow) Scan(dest ...any) error { return r(dest...) }

var _ = Describe("Provider rate card items tenant context", func() {
	var server *ProviderRateCardItemsServer
	var fake *billingfake.Fake
	const tenantA, tenantB = "rates-tenant-a", "rates-tenant-b"
	const accountA, accountB = "rates-account-a", "rates-account-b"
	request := func(tenant string) *publicv1.ProviderRateCardItemsListRequest {
		return publicv1.ProviderRateCardItemsListRequest_builder{
			TenantId: proto.String(tenant), Service: proto.String("widget_service"), CurrencyCode: proto.String("USD"),
		}.Build()
	}
	build := func(scenario billingfake.Scenario, configured bool) {
		fake = billingfake.New(scenario)
		provider := fake.Provider()
		if !configured {
			provider = billing.NewProvider("", fake, nil)
		}
		private, err := NewPrivateProviderRateCardItemsServer().SetLogger(logger).SetProvider(provider).
			SetTenancyLogic(tenancy).SetMetricsRegisterer(prometheus.NewRegistry()).Build()
		Expect(err).ToNot(HaveOccurred())
		server, err = NewProviderRateCardItemsServer().SetLogger(logger).SetDelegate(private).Build()
		Expect(err).ToNot(HaveOccurred())
	}
	reject := func(callCtx context.Context, tenant string, code codes.Code, reason string) {
		GinkgoHelper()
		before := len(fake.Calls())
		response, err := server.List(callCtx, request(tenant))
		Expect(response).To(BeNil())
		Expect(status.Code(err)).To(Equal(code))
		if reason != "" {
			details := status.Convert(err).Details()
			Expect(details).To(HaveLen(1))
			Expect(details[0].(*errdetails.ErrorInfo).GetReason()).To(Equal(reason))
		}
		calls := len(fake.Calls())
		Expect(calls).To(Equal(before), "invalid context must not reach the provider or fall back")
	}
	BeforeEach(func() {
		Expect(suiteTx.End(ctx)).To(Succeed())
		suiteTx = nil
		ctx = auth.ContextWithSubject(database.TxManagerIntoContext(context.Background(), tm),
			&auth.Subject{User: "admin", Tenants: auth.AllTenants})
		build(widgetScenario(), true)
	})

	Describe("PostgreSQL eligibility", func() {
		BeforeEach(func() {
			for _, fixture := range []struct{ tenant, account string }{{tenantA, accountA}, {tenantB, accountB}} {
				_, err := testPool.Exec(ctx, `insert into tenants (id, name, tenant, creator, data)
					values ($1, $1, $1, 'system', '{}')`, fixture.tenant)
				Expect(err).ToNot(HaveOccurred())
				_, err = database.WithNewTx(ctx, func(txCtx context.Context) (struct{}, error) {
					tx, err := database.TxFromContext(txCtx)
					if err != nil {
						return struct{}{}, err
					}
					_, err = database.UpsertBillingAccount(txCtx, tx, database.BillingAccountUpsert{
						ID: fixture.account, ProviderKey: billingfake.Key, ProviderAccountReference: "native-" + fixture.account,
						ExternalBillingAccountID: "external-" + fixture.account, State: "ACTIVE", BaseCurrency: "EUR",
					})
					if err != nil {
						return struct{}{}, err
					}
					_, err = database.CloseAndInsertTenantBillingAssignment(txCtx, tx, "", database.TenantBillingAssignmentInsert{
						ID: "assignment-" + fixture.tenant, Name: "assignment-" + fixture.tenant,
						TenantID: fixture.tenant, BillingAccountID: fixture.account, ConnectionState: "CONNECTED", Creator: "admin",
					})
					return struct{}{}, err
				})
				Expect(err).ToNot(HaveOccurred())
			}
		})

		It("uses tenant names and distinct linked accounts for the same global rates without provider identity lookups", func() {
			global, err := server.List(ctx, request(""))
			Expect(err).ToNot(HaveOccurred())
			Expect(global.GetItems()).To(HaveLen(1))
			for _, tenant := range []string{tenantA, tenantB} {
				finite := auth.ContextWithSubject(ctx, &auth.Subject{User: "admin", Tenants: collections.NewSet(tenant)})
				got, err := server.List(finite, request(tenant))
				Expect(err).ToNot(HaveOccurred())
				Expect(proto.Equal(got, global)).To(BeTrue())
			}
			Expect(fake.Count("GetServiceRateCard")).To(Equal(1))
			private, err := server.private.List(ctx, privatev1.ProviderRateCardItemsListRequest_builder{
				TenantId: proto.String(tenantA), Service: proto.String("widget_service"), CurrencyCode: proto.String("USD"),
			}.Build())
			Expect(err).ToNot(HaveOccurred())
			Expect(private.GetTotal()).To(Equal(global.GetTotal()))
			for _, call := range fake.Calls() {
				Expect(call.Op).To(BeElementOf("FindServiceByName", "ListPriceableAttributes", "GetServiceRateCard"))
				Expect(call.Account).To(BeEmpty())
			}
		})

		DescribeTable("never substitutes another identity for the tenant selector", func(selector string) {
			reject(ctx, selector, codes.NotFound, "BILLING_TENANT_NOT_FOUND")
		}, Entry("unknown", "missing"), Entry("whitespace", " "), Entry("unrelated UUID", "e7df3a86-5dad-49cd-9c0d-7654a289e853"),
			Entry("opaque account", accountA), Entry("native reference", "native-"+accountA),
			Entry("external billing account ID", "external-"+accountA))

		DescribeTable("rechecks stored eligibility even with warm shared caches", func(sql, reason string, code codes.Code) {
			_, err := server.List(ctx, request(tenantA))
			Expect(err).ToNot(HaveOccurred())
			_, err = testPool.Exec(ctx, sql)
			Expect(err).ToNot(HaveOccurred())
			reject(ctx, tenantA, code, reason)
			other, err := server.List(ctx, request(tenantB))
			Expect(err).ToNot(HaveOccurred())
			Expect(other.GetItems()).To(HaveLen(1))
		},
			Entry("closed history only", `update tenant_billing_assignments set effective_to=now() where tenant_id='rates-tenant-a'`, "BILLING_ASSIGNMENT_NOT_FOUND", codes.FailedPrecondition),
			Entry("pending", `update tenant_billing_assignments set connection_state='PENDING' where tenant_id='rates-tenant-a'`, "BILLING_ASSIGNMENT_NOT_CONNECTED", codes.FailedPrecondition),
			Entry("failed", `update tenant_billing_assignments set connection_state='FAILED' where tenant_id='rates-tenant-a'`, "BILLING_ASSIGNMENT_NOT_CONNECTED", codes.FailedPrecondition),
			Entry("inactive account", `update billing_accounts set state='INACTIVE' where id='rates-account-a'`, "ACCOUNT_INACTIVE", codes.FailedPrecondition),
			Entry("unknown account", `update billing_accounts set state='UNKNOWN' where id='rates-account-a'`, "ACCOUNT_STATE_UNKNOWN", codes.FailedPrecondition),
			Entry("deletion-marked tenant", `update tenants set deletion_timestamp=now() where name='rates-tenant-a'`, "BILLING_TENANT_NOT_FOUND", codes.NotFound),
		)

		It("rejects a live unassigned tenant while global rows are available", func() {
			reject(ctx, testTenant, codes.FailedPrecondition, "BILLING_ASSIGNMENT_NOT_FOUND")
			got, err := server.List(ctx, request(""))
			Expect(err).ToNot(HaveOccurred())
			Expect(got.GetTotal()).To(Equal(int32(1)))
		})

		It("uses the current assignment rather than a failed closed interval", func() {
			_, err := testPool.Exec(ctx, `insert into tenant_billing_assignments
				(id,name,tenant_id,billing_account_id,effective_to,connection_state,creator)
				values ('history','history',$1,$2,now(),'FAILED','admin')`, tenantA, accountB)
			Expect(err).ToNot(HaveOccurred())
			got, err := server.List(ctx, request(tenantA))
			Expect(err).ToNot(HaveOccurred())
			Expect(got.GetTotal()).To(Equal(int32(1)))
		})

		It("retains all mapped services when service is omitted in tenant mode", func() {
			scenario := widgetScenario()
			first, second := scenario.Services[0], widgetScenario().Services[0]
			first.Name, second.Name, second.ID = serviceComputeInstance, serviceCluster, "svc-2"
			scenario.Services = []billingfake.ServiceSpec{first, second}
			build(scenario, true)
			r := request(tenantA)
			r.ClearService()
			got, err := server.List(ctx, r)
			Expect(err).ToNot(HaveOccurred())
			Expect(got.GetItems()).To(HaveLen(2))
			Expect(fake.Count("GetServiceRateCard")).To(Equal(2))
		})

		It("preserves exact currency, CEL, pagination and count-only with tenant context", func() {
			scenario := widgetScenario()
			scenario.Services[0].Rates = map[string]billingfake.RateCardSpec{" provider-credit ": {Rows: []billing.RateRow{
				{DimensionID: "dim-1", PriceMethod: "QUANTITY", UnitRate: "1"},
				{DimensionID: "dim-1", PriceMethod: "QUANTITY", UnitRate: "2"},
				{DimensionID: "dim-1", PriceMethod: "QUANTITY", UnitRate: "3"},
			}}}
			build(scenario, true)
			r := request(tenantA)
			r.SetCurrencyCode(" provider-credit ")
			r.SetFilter(`this.rate.amount != "1"`)
			r.SetOffset(1)
			r.SetLimit(1)
			got, err := server.List(ctx, r)
			Expect(err).ToNot(HaveOccurred())
			Expect(got.GetTotal()).To(Equal(int32(2)))
			Expect(got.GetItems()).To(HaveLen(1))
			Expect(got.GetItems()[0].GetRate().GetAmount()).To(Equal("3"))
			Expect(got.GetItems()[0].GetCurrencyCode()).To(Equal(" provider-credit "))
			r.SetLimit(0)
			got, err = server.List(ctx, r)
			Expect(err).ToNot(HaveOccurred())
			Expect(got.GetTotal()).To(Equal(int32(2)))
			Expect(got.GetItems()).To(BeEmpty())
		})

		DescribeTable("keeps provider failures classified and sanitized", func(reason billing.Reason) {
			scenario := widgetScenario()
			scenario.Failures = map[string]billingfake.Failure{"GetServiceRateCard": {Reason: reason}}
			build(scenario, true)
			response, err := server.List(ctx, request(tenantA))
			Expect(response).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.Unavailable))
			Expect(status.Convert(err).Message()).To(Equal("billing provider is unavailable"))
			Expect(status.Convert(err).Details()[0].(*errdetails.ErrorInfo).GetReason()).To(Equal(string(reason)))
		}, Entry("timeout", billing.ReasonTimeout), Entry("unauthorized", billing.ReasonUnauthorized))
	})

	Describe("mocked database boundary", func() {
		It("denies trusted scope before accessing storage", func() {
			limited := auth.ContextWithSubject(context.Background(), &auth.Subject{Tenants: collections.NewSet(tenantB)})
			reject(limited, tenantA, codes.PermissionDenied, "")
		})
		It("denies an empty trusted scope before storage", func() {
			reject(auth.ContextWithSubject(context.Background(), &auth.Subject{}), tenantA, codes.PermissionDenied, "")
		})
		It("retains the required configured-provider guard with a tenant", func() {
			build(widgetScenario(), false)
			reject(ctx, tenantA, codes.FailedPrecondition, "BILLING_NOT_CONFIGURED")
		})
		DescribeTable("sanitizes storage faults and preserves context status", func(stage string, done bool, want codes.Code) {
			manager := database.NewMockTxManager(ctrl)
			fault := errors.New("database credential/native-reference must stay private")
			callCtx := database.TxManagerIntoContext(ctx, manager)
			if done {
				var cancel context.CancelFunc
				callCtx, cancel = context.WithDeadline(callCtx, time.Now().Add(-time.Second))
				defer cancel()
			}
			if stage == "begin" {
				manager.EXPECT().Begin(gomock.Any()).Return(nil, fault)
			} else {
				tx := database.NewMockTx(ctrl)
				manager.EXPECT().Begin(gomock.Any()).Return(tx, nil)
				tx.EXPECT().QueryRow(gomock.Any(), gomock.Any(), tenantA).Return(tenantRatesRow(func(...any) error { return fault }))
				tx.EXPECT().ReportError(gomock.Any())
				tx.EXPECT().End(gomock.Any()).Return(nil)
			}
			response, err := server.List(callCtx, request(tenantA))
			Expect(response).To(BeNil())
			Expect(status.Code(err)).To(Equal(want))
			Expect(err.Error()).ToNot(ContainSubstring(fault.Error()))
			Expect(fake.Calls()).To(BeEmpty())
		}, Entry("begin fault", "begin", false, codes.Internal), Entry("read fault", "query", false, codes.Internal),
			Entry("expired read", "query", true, codes.DeadlineExceeded))

		DescribeTable("fails closed on assignment, account and transaction completion faults", func(stage string) {
			manager, tx := database.NewMockTxManager(ctrl), database.NewMockTx(ctrl)
			fault := errors.New("private storage fault")
			manager.EXPECT().Begin(gomock.Any()).Return(tx, nil)
			tx.EXPECT().QueryRow(gomock.Any(), gomock.Any(), tenantA).Return(tenantRatesRow(func(dest ...any) error {
				*dest[0].(*bool) = true
				return nil
			}))
			assignment := tx.EXPECT().QueryRow(gomock.Any(), gomock.Any(), tenantA).Return(tenantRatesRow(func(dest ...any) error {
				if stage == "assignment" {
					return fault
				}
				*dest[3].(*string) = accountA
				*dest[6].(*string) = "CONNECTED"
				return nil
			}))
			if stage != "assignment" {
				tx.EXPECT().QueryRow(gomock.Any(), gomock.Any(), accountA).After(assignment).Return(tenantRatesRow(func(dest ...any) error {
					if stage == "missing account" {
						return pgx.ErrNoRows
					}
					if stage == "account" {
						return fault
					}
					*dest[0].(*string), *dest[3].(*string) = accountA, "ACTIVE"
					return nil
				}))
			}
			tx.EXPECT().ReportError(gomock.Any())
			var endErr error
			if stage == "commit" {
				endErr = fault
			}
			tx.EXPECT().End(gomock.Any()).Return(endErr)
			callCtx := database.TxManagerIntoContext(ctx, manager)
			response, err := server.List(callCtx, request(tenantA))
			Expect(response).To(BeNil())
			Expect(status.Code(err)).To(Equal(codes.Internal))
			Expect(status.Convert(err).Message()).To(Equal("failed to validate billing tenant context"))
			Expect(fake.Calls()).To(BeEmpty())
		}, Entry("assignment read", "assignment"), Entry("account read", "account"),
			Entry("broken account reference", "missing account"), Entry("commit before provider work", "commit"))

		It("preserves cancellation rather than returning a storage fault", func() {
			manager := database.NewMockTxManager(ctrl)
			manager.EXPECT().Begin(gomock.Any()).Return(nil, context.Canceled)
			callCtx, cancel := context.WithCancel(database.TxManagerIntoContext(ctx, manager))
			cancel()
			reject(callCtx, tenantA, codes.Canceled, "")
		})
	})
})
