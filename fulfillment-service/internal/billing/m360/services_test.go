/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package m360

import (
	"context"
	"net/http"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2/dsl/core"
	. "github.com/onsi/gomega"

	"github.com/osac-project/osac/fulfillment-service/internal/billing"
)

var _ = Describe("M360 services", func() {
	var (
		fake    *fakeServer
		adapter billing.Adapter
		mu      sync.Mutex
		paths   []string
	)

	build := func(h http.HandlerFunc) {
		paths = nil
		fake = newFakeServer(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.EscapedPath())
			mu.Unlock()
			h(w, r)
		})
		var err error
		adapter, err = New(billing.Config{BaseURL: fake.URL, APIVersion: "v2", Credential: testToken}).
			SetLogger(logger).Build()
		Expect(err).ToNot(HaveOccurred())
	}

	AfterEach(func() {
		fake.Close()
		close(fake.methods)
		for m := range fake.methods {
			Expect(m).To(Equal(http.MethodGet))
		}
		mu.Lock()
		defer mu.Unlock()
		for _, p := range paths {
			Expect(p).ToNot(ContainSubstring("contract"))
		}
	})

	servicesBody := func(names ...string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			items := make([]string, 0, len(names))
			for i, n := range names {
				items = append(items, `{"Id":"svc-`+string(rune('a'+i))+`","Name":"`+n+`"}`)
			}
			_, _ = w.Write([]byte(`{"Items":[` + strings.Join(items, ",") + `]}`))
		}
	}

	Describe("FindServiceByName", func() {
		It("returns the one exact match and searches with q", func() {
			var q string
			build(func(w http.ResponseWriter, r *http.Request) {
				q = r.URL.Query().Get("q")
				servicesBody("GPU Hours Extra", "GPU Hours")(w, r)
			})
			ref, err := adapter.FindServiceByName(context.Background(), "GPU Hours")
			Expect(err).ToNot(HaveOccurred())
			Expect(ref.ID()).To(Equal("svc-b"), "must pick the exact match, not the first hit")
			Expect(q).To(Equal("GPU Hours"))
		})

		It("reports not-found when the provider returns no hits", func() {
			build(servicesBody())
			_, err := adapter.FindServiceByName(context.Background(), "GPU Hours")
			Expect(err).To(MatchError(billing.ErrServiceNotFound))
		})

		It("reports not-found when only near-matches exist", func() {
			build(servicesBody("GPU Hours Extra", "gpu hours"))
			_, err := adapter.FindServiceByName(context.Background(), "GPU Hours")
			Expect(err).To(MatchError(billing.ErrServiceNotFound))
		})

		It("gives up as malformed when the provider cycles page tokens", func() {
			var n int
			build(func(w http.ResponseWriter, r *http.Request) {
				n++
				tok := "A"
				if n%2 == 0 {
					tok = "B"
				}
				_, _ = w.Write([]byte(`{"Items":[],"NextPage":"` + tok + `"}`))
			})
			_, err := adapter.FindServiceByName(context.Background(), "GPU Hours")
			Expect(reasonOf(err)).To(Equal(billing.ReasonMalformed))
		})

		It("tolerates a trailing slash on the configured base URL", func() {
			var path string
			build(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				servicesBody("GPU Hours")(w, r)
			})
			a, err := New(billing.Config{BaseURL: fake.URL + "/", APIVersion: "v2", Credential: testToken}).
				SetLogger(logger).Build()
			Expect(err).ToNot(HaveOccurred())
			_, err = a.FindServiceByName(context.Background(), "GPU Hours")
			Expect(err).ToNot(HaveOccurred())
			Expect(path).To(Equal("/api/v2/services"))
		})

		It("reports ambiguity when two services match exactly", func() {
			build(servicesBody("GPU Hours", "GPU Hours"))
			_, err := adapter.FindServiceByName(context.Background(), "GPU Hours")
			Expect(reasonOf(err)).To(Equal(billing.ReasonAmbiguous))
		})

		It("finds an exact match on a later result page", func() {
			build(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "" {
					_, _ = w.Write([]byte(`{"Items":[{"Id":"s1","Name":"Other"}],"NextPage":"2"}`))
					return
				}
				_, _ = w.Write([]byte(`{"Items":[{"Id":"s2","Name":"GPU Hours"}]}`))
			})
			ref, err := adapter.FindServiceByName(context.Background(), "GPU Hours")
			Expect(err).ToNot(HaveOccurred())
			Expect(ref.ID()).To(Equal("s2"))
		})

		It("surfaces provider failure instead of not-found", func() {
			build(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) })
			_, err := adapter.FindServiceByName(context.Background(), "GPU Hours")
			Expect(reasonOf(err)).To(Equal(billing.ReasonUnauthorized))
		})
	})

	Describe("ListPriceableAttributes", func() {
		It("reports NOT_FOUND when the service does not exist", func() {
			build(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
			_, err := adapter.ListPriceableAttributes(context.Background(), billing.NewServiceRef("s"))
			Expect(reasonOf(err)).To(Equal(billing.ReasonNotFound))
		})

		It("returns the attribute names", func() {
			build(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"Items":[{"Name":"gpu_hours"},{"Name":"storage_gb"}]}`))
			})
			attrs, err := adapter.ListPriceableAttributes(context.Background(), billing.NewServiceRef("svc 1"))
			Expect(err).ToNot(HaveOccurred())
			Expect(attrs).To(Equal([]billing.PriceableAttribute{{Name: "gpu_hours"}, {Name: "storage_gb"}}))
			Expect(paths).To(ConsistOf("/api/v2/services/svc%201/priceable-attributes"))
		})

		It("surfaces provider failure instead of an empty list", func() {
			build(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(403) })
			attrs, err := adapter.ListPriceableAttributes(context.Background(), billing.NewServiceRef("s"))
			Expect(reasonOf(err)).To(Equal(billing.ReasonUnauthorized))
			Expect(attrs).To(BeNil())
		})
	})

	Describe("GetServiceRateCard", func() {
		It("reads the global rate card and keeps amounts as exact decimals", func() {
			build(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"Currency":"USD","Rows":[
					{"Dimension":"gpu_hours","Amount":0.1000000000000000055,"Unit":"hour"},
					{"Dimension":"storage_gb","Amount":"12.50"}]}`))
			})
			card, err := adapter.GetServiceRateCard(context.Background(), billing.NewServiceRef("s1"), "USD")
			Expect(err).ToNot(HaveOccurred())
			Expect(paths).To(ConsistOf("/api/v2/services/s1/rate-cards/USD"))
			Expect(card.Currency).To(Equal("USD"))
			Expect(card.Rows).To(Equal([]billing.RateRow{
				{Dimension: "gpu_hours", Amount: "0.1000000000000000055", Unit: "hour"},
				{Dimension: "storage_gb", Amount: "12.50"},
			}))
		})

		It("reports a missing card as not-found, never as a zero amount", func() {
			build(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
			card, err := adapter.GetServiceRateCard(context.Background(), billing.NewServiceRef("s1"), "EUR")
			Expect(err).To(MatchError(billing.ErrRateCardNotFound))
			Expect(card.Rows).To(BeNil())
		})

		It("treats a card without rows as not-found", func() {
			build(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"Currency":"USD","Rows":[]}`)) })
			_, err := adapter.GetServiceRateCard(context.Background(), billing.NewServiceRef("s1"), "USD")
			Expect(err).To(MatchError(billing.ErrRateCardNotFound))
		})

		It("rejects a row without an amount", func() {
			build(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"Currency":"USD","Rows":[{"Dimension":"gpu_hours"}]}`))
			})
			_, err := adapter.GetServiceRateCard(context.Background(), billing.NewServiceRef("s1"), "USD")
			Expect(reasonOf(err)).To(Equal(billing.ReasonMalformed))
		})

		It("rejects a card in a different currency than requested", func() {
			build(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"Currency":"EUR","Rows":[{"Dimension":"d","Amount":1}]}`))
			})
			_, err := adapter.GetServiceRateCard(context.Background(), billing.NewServiceRef("s1"), "USD")
			Expect(reasonOf(err)).To(Equal(billing.ReasonMalformed))
		})

		It("rejects an empty currency without contacting the provider", func() {
			build(func(http.ResponseWriter, *http.Request) {})
			_, err := adapter.GetServiceRateCard(context.Background(), billing.NewServiceRef("s1"), "")
			Expect(reasonOf(err)).To(Equal(billing.ReasonMalformed))
			Expect(fake.hits.Load()).To(BeZero())
		})

		It("surfaces provider outage as an error", func() {
			build(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
			_, err := adapter.GetServiceRateCard(context.Background(), billing.NewServiceRef("s1"), "USD")
			Expect(reasonOf(err)).To(Equal(billing.ReasonUnavailable))
		})
	})
})
