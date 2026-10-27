/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package servers

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	"github.com/osac-project/osac/fulfillment-service/internal/database/dao"
	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
)

// accountsAdapter is a billing.Adapter test double that serves pre-built pages keyed by page token. Only the account
// listing is implemented: calling any other method panics, which proves discovery needs nothing else.
type accountsAdapter struct {
	billing.Adapter
	pages map[string]billing.AccountPage
	err   error
	calls []billing.Page
}

func (a *accountsAdapter) ListAccounts(_ context.Context, _ billing.AccountFilter,
	page billing.Page) (billing.AccountPage, error) {
	a.calls = append(a.calls, page)
	if a.err != nil {
		return billing.AccountPage{}, a.err
	}
	return a.pages[page.Token], nil
}

func providerAccount(id, name, externalID string, state billing.AccountState) billing.Account {
	return billing.Account{
		Ref:        billing.NewAccountRef(id),
		Name:       name,
		ExternalID: externalID,
		State:      state,
	}
}

func onePage(accounts ...billing.Account) map[string]billing.AccountPage {
	return map[string]billing.AccountPage{"": {Accounts: accounts}}
}

var _ = Describe("Billing accounts server", func() {
	var (
		adapter *accountsAdapter
		logs    *bytes.Buffer
		accDAO  *dao.GenericDAO[*privatev1.BillingAccount]
	)

	build := func(billingAdapter billing.Adapter) (*BillingAccountsServer, *PrivateBillingAccountsServer) {
		logs = &bytes.Buffer{}
		testLogger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		private, err := NewPrivateBillingAccountsServer().
			SetLogger(testLogger).
			SetAttributionLogic(attribution).
			SetTenancyLogic(tenancy).
			SetBillingAdapter(billingAdapter).
			SetProviderKey("m360").
			Build()
		Expect(err).ToNot(HaveOccurred())
		public, err := NewBillingAccountsServer().
			SetLogger(testLogger).
			SetAttributionLogic(attribution).
			SetTenancyLogic(tenancy).
			SetBillingAdapter(billingAdapter).
			SetProviderKey("m360").
			Build()
		Expect(err).ToNot(HaveOccurred())
		return public, private
	}

	list := func(server *BillingAccountsServer, request *publicv1.BillingAccountsListRequest) *publicv1.BillingAccountsListResponse {
		response, err := server.List(ctx, request)
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		return response
	}

	byName := func(response *publicv1.BillingAccountsListResponse, name string) *publicv1.BillingAccount {
		for _, item := range response.GetItems() {
			if item.GetDisplayName() == name {
				return item
			}
		}
		Fail(fmt.Sprintf("account %q not listed", name))
		return nil
	}

	// stored returns the private row of the account with the given provider reference.
	stored := func(reference string) *privatev1.BillingAccount {
		response, err := accDAO.List().Do(ctx)
		ExpectWithOffset(1, err).ToNot(HaveOccurred())
		for _, item := range response.GetItems() {
			if item.GetProviderAccountReference() == reference {
				return item
			}
		}
		Fail(fmt.Sprintf("no row for reference %q", reference))
		return nil
	}

	BeforeEach(func() {
		var err error
		accDAO, err = dao.NewGenericDAO[*privatev1.BillingAccount]().
			SetLogger(logger).
			SetTenancyLogic(tenancy).
			Build()
		Expect(err).ToNot(HaveOccurred())
		adapter = &accountsAdapter{pages: onePage()}
	})

	Describe("Creation", func() {
		It("Requires a provider key when an adapter is set", func() {
			server, err := NewPrivateBillingAccountsServer().
				SetLogger(logger).
				SetAttributionLogic(attribution).
				SetTenancyLogic(tenancy).
				SetBillingAdapter(adapter).
				Build()
			Expect(err).To(MatchError("provider key is mandatory when billing is configured"))
			Expect(server).To(BeNil())
		})
	})

	Describe("Billing disabled or provider failing", func() {
		It("Fails with FAILED_PRECONDITION and BILLING_NOT_CONFIGURED when billing is disabled", func() {
			server, _ := build(nil)
			_, err := server.List(ctx, &publicv1.BillingAccountsListRequest{})
			st, ok := status.FromError(err)
			Expect(ok).To(BeTrue())
			Expect(st.Code()).To(Equal(codes.FailedPrecondition))
			Expect(st.Details()).To(HaveLen(1))
			info, ok := st.Details()[0].(*errdetails.ErrorInfo)
			Expect(ok).To(BeTrue())
			Expect(info.GetReason()).To(Equal("BILLING_NOT_CONFIGURED"))
		})

		It("Fails with UNAVAILABLE and does not serve the stale list when the provider fails", func() {
			adapter.pages = onePage(providerAccount("n1", "Acme", "", billing.AccountActive))
			server, _ := build(adapter)
			Expect(list(server, &publicv1.BillingAccountsListRequest{}).GetItems()).To(HaveLen(1))

			adapter.err = &billing.Error{Reason: billing.ReasonUnavailable, Op: "ListAccounts", Retryable: true}
			response, err := server.List(ctx, &publicv1.BillingAccountsListRequest{})
			Expect(status.Code(err)).To(Equal(codes.Unavailable))
			Expect(response).To(BeNil())
		})
	})

	Describe("Discovery", func() {
		It("Reads every page with a page size of at most 20 and upserts one row per account", func() {
			adapter.pages = map[string]billing.AccountPage{
				"":   {Accounts: []billing.Account{providerAccount("n1", "One", "", billing.AccountActive)}, Next: "p2"},
				"p2": {Next: "p3"}, // A page may be empty while Next is still set.
				"p3": {Accounts: []billing.Account{providerAccount("n2", "Two", "", billing.AccountInactive)}},
			}
			server, _ := build(adapter)

			response := list(server, &publicv1.BillingAccountsListRequest{})
			Expect(response.GetTotal()).To(BeEquivalentTo(2))
			Expect(adapter.calls).To(HaveLen(3))
			for _, call := range adapter.calls {
				Expect(call.Size).To(BeNumerically("<=", 20))
				Expect(call.Size).To(BeNumerically(">", 0))
			}

			// A second refresh updates the rows instead of adding new ones:
			response = list(server, &publicv1.BillingAccountsListRequest{})
			Expect(response.GetTotal()).To(BeEquivalentTo(2))
			Expect(response.GetItems()[0].GetId()).ToNot(BeEmpty())
			Expect(response.GetItems()[0].GetId()).ToNot(Equal("n1"))
			Expect(response.GetItems()[0].GetProviderKey()).To(Equal("m360"))
		})

		It("Stores an account that appears twice once", func() {
			adapter.pages = map[string]billing.AccountPage{
				"":   {Accounts: []billing.Account{providerAccount("n1", "One", "", billing.AccountActive)}, Next: "p2"},
				"p2": {Accounts: []billing.Account{providerAccount("n1", "One", "", billing.AccountActive)}},
			}
			server, _ := build(adapter)
			Expect(list(server, &publicv1.BillingAccountsListRequest{}).GetTotal()).To(BeEquivalentTo(1))
		})

		It("Fails with UNAVAILABLE when the provider keeps returning the same page token", func() {
			adapter.pages = map[string]billing.AccountPage{
				"":   {Next: "p2"},
				"p2": {Next: "p2"},
			}
			server, _ := build(adapter)
			_, err := server.List(ctx, &publicv1.BillingAccountsListRequest{})
			Expect(status.Code(err)).To(Equal(codes.Unavailable))
		})

		It("Maps the provider state and treats anything unmapped as UNKNOWN", func() {
			adapter.pages = onePage(
				providerAccount("n1", "Active", "", billing.AccountActive),
				providerAccount("n2", "Inactive", "", billing.AccountInactive),
				providerAccount("n3", "Unknown", "", billing.AccountUnknown),
				providerAccount("n4", "Empty", "", ""),
				providerAccount("n5", "Weird", "", billing.AccountState("ENABLED")),
			)
			server, _ := build(adapter)
			response := list(server, &publicv1.BillingAccountsListRequest{})
			Expect(byName(response, "Active").GetState()).To(Equal(publicv1.BillingAccountState_BILLING_ACCOUNT_STATE_ACTIVE))
			Expect(byName(response, "Inactive").GetState()).To(Equal(publicv1.BillingAccountState_BILLING_ACCOUNT_STATE_INACTIVE))
			for _, name := range []string{"Unknown", "Empty", "Weird"} {
				Expect(byName(response, name).GetState()).To(
					Equal(publicv1.BillingAccountState_BILLING_ACCOUNT_STATE_UNKNOWN), name)
			}
		})

		It("Refreshes the name and observation time, and keeps the name when the provider sends none", func() {
			adapter.pages = onePage(providerAccount("n1", "Before", "", billing.AccountActive))
			server, _ := build(adapter)
			first := list(server, &publicv1.BillingAccountsListRequest{}).GetItems()[0]
			Expect(first.GetStatusObservedAt().IsValid()).To(BeTrue())

			adapter.pages = onePage(providerAccount("n1", "After", "", billing.AccountInactive))
			second := list(server, &publicv1.BillingAccountsListRequest{}).GetItems()[0]
			Expect(second.GetId()).To(Equal(first.GetId()))
			Expect(second.GetDisplayName()).To(Equal("After"))
			Expect(second.GetState()).To(Equal(publicv1.BillingAccountState_BILLING_ACCOUNT_STATE_INACTIVE))
			Expect(second.GetStatusObservedAt().AsTime()).To(BeTemporally(">", first.GetStatusObservedAt().AsTime()))

			adapter.pages = onePage(providerAccount("n1", "", "", billing.AccountActive))
			third := list(server, &publicv1.BillingAccountsListRequest{}).GetItems()[0]
			Expect(third.GetDisplayName()).To(Equal("After"))
		})

		It("Never invents a currency", func() {
			adapter.pages = onePage(providerAccount("n1", "One", "", billing.AccountActive))
			server, _ := build(adapter)
			Expect(list(server, &publicv1.BillingAccountsListRequest{}).GetItems()[0].GetBaseCurrency()).To(BeEmpty())
		})

		It("Retains an account that disappears from the provider and marks it UNKNOWN", func() {
			adapter.pages = onePage(
				providerAccount("n1", "Stays", "", billing.AccountActive),
				providerAccount("n2", "Goes", "", billing.AccountActive),
			)
			server, _ := build(adapter)
			list(server, &publicv1.BillingAccountsListRequest{})

			adapter.pages = onePage(providerAccount("n1", "Stays", "", billing.AccountActive))
			response := list(server, &publicv1.BillingAccountsListRequest{})
			Expect(response.GetTotal()).To(BeEquivalentTo(2))
			Expect(byName(response, "Goes").GetState()).To(Equal(publicv1.BillingAccountState_BILLING_ACCOUNT_STATE_UNKNOWN))
			Expect(byName(response, "Stays").GetState()).To(Equal(publicv1.BillingAccountState_BILLING_ACCOUNT_STATE_ACTIVE))
		})
	})

	Describe("Correlation id", func() {
		It("Copies the provider value when the local value is null", func() {
			adapter.pages = onePage(providerAccount("n1", "One", "corr-1", billing.AccountActive))
			server, _ := build(adapter)
			item := list(server, &publicv1.BillingAccountsListRequest{}).GetItems()[0]
			Expect(item.HasCorrelationId()).To(BeTrue())
			Expect(item.GetCorrelationId()).To(Equal("corr-1"))
		})

		It("Leaves the value null when neither side has one", func() {
			adapter.pages = onePage(providerAccount("n1", "One", "", billing.AccountActive))
			server, _ := build(adapter)
			Expect(list(server, &publicv1.BillingAccountsListRequest{}).GetItems()[0].HasCorrelationId()).To(BeFalse())
		})

		Context("With a locally established value", func() {
			var server *BillingAccountsServer

			BeforeEach(func() {
				adapter.pages = onePage(providerAccount("n1", "One", "", billing.AccountActive))
				server, _ = build(adapter)
				list(server, &publicv1.BillingAccountsListRequest{})
				row := stored("n1")
				row.SetCorrelationId("local-1")
				_, err := accDAO.Update().SetObject(row).Do(ctx)
				Expect(err).ToNot(HaveOccurred())
			})

			It("Is not cleared by an empty provider value", func() {
				item := list(server, &publicv1.BillingAccountsListRequest{}).GetItems()[0]
				Expect(item.GetCorrelationId()).To(Equal("local-1"))
			})

			It("Is not overwritten by a different provider value", func() {
				adapter.pages = onePage(providerAccount("n1", "One", "provider-1", billing.AccountActive))
				item := list(server, &publicv1.BillingAccountsListRequest{}).GetItems()[0]
				Expect(item.GetCorrelationId()).To(Equal("local-1"))
			})

			It("Stays when the provider holds the same value", func() {
				adapter.pages = onePage(providerAccount("n1", "One", "local-1", billing.AccountActive))
				item := list(server, &publicv1.BillingAccountsListRequest{}).GetItems()[0]
				Expect(item.GetCorrelationId()).To(Equal("local-1"))
			})
		})

		It("Writes no value two provider accounts share", func() {
			adapter.pages = onePage(
				providerAccount("n1", "One", "shared", billing.AccountActive),
				providerAccount("n2", "Two", "shared", billing.AccountActive),
				providerAccount("n3", "Three", "own", billing.AccountActive),
			)
			server, _ := build(adapter)
			response := list(server, &publicv1.BillingAccountsListRequest{})
			Expect(response.GetTotal()).To(BeEquivalentTo(3))
			Expect(byName(response, "One").HasCorrelationId()).To(BeFalse())
			Expect(byName(response, "Two").HasCorrelationId()).To(BeFalse())
			Expect(byName(response, "Three").GetCorrelationId()).To(Equal("own"))
		})

		It("Does not copy a value another account already holds locally", func() {
			adapter.pages = onePage(
				providerAccount("n1", "One", "", billing.AccountActive),
				providerAccount("n2", "Two", "", billing.AccountActive),
			)
			server, _ := build(adapter)
			list(server, &publicv1.BillingAccountsListRequest{})
			row := stored("n1")
			row.SetCorrelationId("taken")
			_, err := accDAO.Update().SetObject(row).Do(ctx)
			Expect(err).ToNot(HaveOccurred())

			adapter.pages = onePage(
				providerAccount("n1", "One", "", billing.AccountActive),
				providerAccount("n2", "Two", "taken", billing.AccountActive),
			)
			response := list(server, &publicv1.BillingAccountsListRequest{})
			Expect(byName(response, "One").GetCorrelationId()).To(Equal("taken"))
			Expect(byName(response, "Two").HasCorrelationId()).To(BeFalse())
		})
	})

	Describe("List", func() {
		BeforeEach(func() {
			adapter.pages = onePage(
				providerAccount("n1", "Bravo", "", billing.AccountActive),
				providerAccount("n2", "Alpha", "", billing.AccountActive),
				providerAccount("n3", "Charlie", "", billing.AccountActive),
			)
		})

		// The shared list parameters accept `order` but, as for every other resource, results come back in
		// identifier order.
		It("Honors limit and offset and accepts order", func() {
			server, _ := build(adapter)
			first := list(server, &publicv1.BillingAccountsListRequest{Limit: proto.Int32(2)})
			Expect(first.GetTotal()).To(BeEquivalentTo(3))
			Expect(first.GetSize()).To(BeEquivalentTo(2))

			rest := list(server, &publicv1.BillingAccountsListRequest{
				Order:  proto.String("display_name desc"),
				Offset: proto.Int32(2),
				Limit:  proto.Int32(2),
			})
			Expect(rest.GetTotal()).To(BeEquivalentTo(3))
			Expect(rest.GetSize()).To(BeEquivalentTo(1))
			Expect(rest.GetItems()[0].GetId()).ToNot(Equal(first.GetItems()[0].GetId()))
			Expect(rest.GetItems()[0].GetId()).ToNot(Equal(first.GetItems()[1].GetId()))
		})

		It("Honors filter", func() {
			server, _ := build(adapter)
			response := list(server, &publicv1.BillingAccountsListRequest{
				Filter: proto.String(`this.display_name == "Alpha"`),
			})
			Expect(response.GetTotal()).To(BeEquivalentTo(1))
			Expect(response.GetItems()[0].GetDisplayName()).To(Equal("Alpha"))
		})

		It("Returns an empty list, not an error, when the provider has no accounts", func() {
			adapter.pages = onePage()
			server, _ := build(adapter)
			response := list(server, &publicv1.BillingAccountsListRequest{})
			Expect(response.GetItems()).To(BeEmpty())
			Expect(response.GetTotal()).To(BeZero())
		})
	})

	Describe("Provider internals", func() {
		It("Does not expose the provider account reference in the public API or the logs", func() {
			adapter.pages = onePage(providerAccount("native-secret-id", "One", "corr-secret", billing.AccountActive))
			server, _ := build(adapter)
			response := list(server, &publicv1.BillingAccountsListRequest{})

			fields := (&publicv1.BillingAccount{}).ProtoReflect().Descriptor().Fields()
			Expect(fields.ByName("provider_account_reference")).To(BeNil())
			Expect(fields.ByName("metadata")).To(BeNil())
			encoded, err := protojson.Marshal(response)
			Expect(err).ToNot(HaveOccurred())
			Expect(string(encoded)).ToNot(ContainSubstring("native-secret-id"))

			// The correlation id is returned by the method only:
			Expect(string(encoded)).To(ContainSubstring("corr-secret"))
			Expect(logs.String()).ToNot(ContainSubstring("corr-secret"))
			Expect(logs.String()).ToNot(ContainSubstring("native-secret-id"))
		})

		It("Keeps the provider account reference in the stored row", func() {
			adapter.pages = onePage(providerAccount("native-1", "One", "", billing.AccountActive))
			server, _ := build(adapter)
			list(server, &publicv1.BillingAccountsListRequest{})
			Expect(stored("native-1").GetProviderKey()).To(Equal("m360"))
		})

		It("Does not leak the correlation id or native id through errors or logs when the provider fails", func() {
			adapter.pages = onePage(providerAccount("native-secret-id", "One", "corr-secret", billing.AccountActive))
			server, _ := build(adapter)
			list(server, &publicv1.BillingAccountsListRequest{})

			adapter.err = &billing.Error{Reason: billing.ReasonUnavailable, Op: "ListAccounts"}
			_, err := server.List(ctx, &publicv1.BillingAccountsListRequest{})
			Expect(err).To(HaveOccurred())
			for _, text := range []string{err.Error(), logs.String()} {
				Expect(strings.ToLower(text)).ToNot(ContainSubstring("corr-secret"))
				Expect(text).ToNot(ContainSubstring("native-secret-id"))
			}
		})
	})
})
