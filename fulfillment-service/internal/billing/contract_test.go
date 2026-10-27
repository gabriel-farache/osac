/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package billing

import (
	"context"
	"net/http"
	"strings"

	. "github.com/onsi/ginkgo/v2/dsl/core"
	. "github.com/onsi/gomega"
)

// Both adapters are seeded with the same logical data: one active account "acct-1" correlated to "ext-1", and a
// service "GPU Hours" priced at 0.25 USD for gpu_hours.
func seededFake() *fakeAdapter {
	return &fakeAdapter{
		accounts: map[string]*fakeNativeAccount{"acct-1": {uuid: "acct-1", state: "enabled", approval: "Approved", tag: "ext-1"}},
		order:    []string{"acct-1"},
		services: map[string]string{"svc-1": "GPU Hours", "svc-2": "GPU Hours Extra"},
		rates:    map[string]map[string]string{"svc-1": {"USD": "0.25"}},
	}
}

func seededM360() (*fakeM360, Adapter) {
	fake := newFakeM360(func(w http.ResponseWriter, r *http.Request) {
		switch p := strings.TrimPrefix(r.URL.Path, "/api/v2"); p {
		case "/accounts":
			_, _ = w.Write([]byte(`{"Items":[{"Id":"acct-1","ExternalId":"ext-1","Status":"Active","ApprovalStatus":"Approved"}]}`))
		case "/accounts/acct-1":
			_, _ = w.Write([]byte(`{"Id":"acct-1","ExternalId":"ext-1","Status":"Active","ApprovalStatus":"Approved"}`))
		case "/services":
			_, _ = w.Write([]byte(`{"Items":[{"Id":"svc-1","Name":"GPU Hours"},{"Id":"svc-2","Name":"GPU Hours Extra"}]}`))
		case "/services/svc-1/priceable-attributes":
			_, _ = w.Write([]byte(`{"Items":[{"Name":"gpu_hours"}]}`))
		case "/services/svc-1/rate-cards/USD":
			_, _ = w.Write([]byte(`{"Currency":"USD","Rows":[{"Dimension":"gpu_hours","Amount":"0.25"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	adapter, err := NewM360(Config{BaseURL: fake.URL, APIVersion: "v2", Credential: testToken}).SetLogger(logger).Build()
	Expect(err).ToNot(HaveOccurred())
	return fake, adapter
}

var _ = Describe("Adapter contract", func() {
	type provider struct {
		name      string
		build     func() Adapter
		canWrite  bool
		afterEach func()
	}
	var m360Server *fakeM360

	providers := []provider{
		{
			name: "M360",
			build: func() Adapter {
				var a Adapter
				m360Server, a = seededM360()
				return a
			},
			afterEach: func() {
				m360Server.Close()
				close(m360Server.methods)
				for m := range m360Server.methods {
					Expect(m).To(Equal(http.MethodGet), "M360 must never receive a write")
				}
			},
		},
		{name: "write-capable fake", build: func() Adapter { return seededFake() }, canWrite: true},
	}

	for _, p := range providers {
		Describe(p.name, func() {
			var adapter Adapter
			ctx := context.Background()

			BeforeEach(func() { adapter = p.build() })
			AfterEach(func() {
				if p.afterEach != nil {
					p.afterEach()
				}
			})

			It("lists accounts in the normalized shape", func() {
				page, err := adapter.ListAccounts(ctx, Page{})
				Expect(err).ToNot(HaveOccurred())
				Expect(page.Accounts).To(HaveLen(1))
				a := page.Accounts[0]
				Expect(a.Ref.ID()).To(Equal("acct-1"))
				Expect(a.State).To(Equal(AccountActive))
				Expect(a.ExternalID).To(Equal("ext-1"))
				Expect(a.ApprovalStatus).To(Equal("Approved"))
			})

			It("reads the correlation value of an account", func() {
				v, ok, err := adapter.CorrelationValue(ctx, NewAccountRef("acct-1"))
				Expect(err).ToNot(HaveOccurred())
				Expect(ok).To(BeTrue())
				Expect(v).To(Equal("ext-1"))
			})

			It("resolves a service by exact name and reads its rate card", func() {
				svc, err := adapter.FindServiceByName(ctx, "GPU Hours")
				Expect(err).ToNot(HaveOccurred())
				attrs, err := adapter.ListPriceableAttributes(ctx, svc)
				Expect(err).ToNot(HaveOccurred())
				Expect(attrs).To(Equal([]PriceableAttribute{{Name: "gpu_hours"}}))
				card, err := adapter.GetServiceRateCard(ctx, svc, "USD")
				Expect(err).ToNot(HaveOccurred())
				Expect(card).To(Equal(RateCard{Currency: "USD", Rows: []RateRow{{Dimension: "gpu_hours", Amount: "0.25"}}}))
			})

			It("reports an unknown service as not-found", func() {
				_, err := adapter.FindServiceByName(ctx, "Nope")
				Expect(err).To(MatchError(ErrServiceNotFound))
			})

			It("reports a missing rate card as not-found", func() {
				_, err := adapter.GetServiceRateCard(ctx, NewServiceRef("svc-1"), "EUR")
				Expect(err).To(MatchError(ErrRateCardNotFound))
			})

			It("honours the correlation write capability it advertises", func() {
				Expect(adapter.SupportsCorrelationWrite()).To(Equal(p.canWrite))
				err := adapter.SetCorrelationValue(ctx, NewAccountRef("acct-1"), "ext-2")
				if !p.canWrite {
					Expect(reasonOf(err)).To(Equal(ReasonUnsupported))
					return
				}
				Expect(err).ToNot(HaveOccurred())
				v, _, err := adapter.CorrelationValue(ctx, NewAccountRef("acct-1"))
				Expect(err).ToNot(HaveOccurred())
				Expect(v).To(Equal("ext-2"))
			})
		})
	}
})
