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
	"net/url"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2/dsl/core"
	. "github.com/onsi/ginkgo/v2/dsl/table"
	. "github.com/onsi/gomega"
)

var _ = Describe("M360 accounts", func() {
	var (
		fake    *fakeM360
		adapter Adapter
		queries chan url.Values
	)

	build := func(h http.HandlerFunc) {
		queries = make(chan url.Values, 100)
		fake = newFakeM360(func(w http.ResponseWriter, r *http.Request) {
			queries <- r.URL.Query()
			h(w, r)
		})
		var err error
		adapter, err = NewM360(Config{BaseURL: fake.URL, APIVersion: "v2", Credential: testToken}).
			SetLogger(logger).Build()
		Expect(err).ToNot(HaveOccurred())
	}

	AfterEach(func() {
		fake.Close()
		close(fake.methods)
		for m := range fake.methods {
			Expect(m).To(Equal(http.MethodGet))
		}
	})

	Describe("ListAccounts", func() {
		It("keeps raw status fields and returns the next page token", func() {
			build(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"Items":[
					{"Id":"a1","ExternalId":"ext-1","Status":"Active","ApprovalStatus":"Approved"},
					{"Id":"a2","Status":"Weird","ApprovalStatus":"Pending"}],"NextPage":"p2"}`))
			})
			page, err := adapter.ListAccounts(context.Background(), Page{Size: 5, Token: "p1"})
			Expect(err).ToNot(HaveOccurred())
			Expect(page.Next).To(Equal("p2"))
			Expect(page.Accounts).To(HaveLen(2))
			Expect(page.Accounts[0].ExternalID).To(Equal("ext-1"))
			Expect(page.Accounts[0].RawStatus).To(Equal("Active"))
			Expect(page.Accounts[0].ApprovalStatus).To(Equal("Approved"))
			Expect(page.Accounts[0].Ref.ID()).To(Equal("a1"))
			Expect(page.Accounts[1].ExternalID).To(BeEmpty())
			q := <-queries
			Expect(q.Get("pageSize")).To(Equal("5"))
			Expect(q.Get("page")).To(Equal("p1"))
		})

		DescribeTable("clamps the page size to the provider maximum of 20",
			func(requested int, sent int) {
				build(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"Items":[]}`)) })
				_, err := adapter.ListAccounts(context.Background(), Page{Size: requested})
				Expect(err).ToNot(HaveOccurred())
				Expect((<-queries).Get("pageSize")).To(Equal(strconv.Itoa(sent)))
			},
			Entry("zero means max", 0, 20),
			Entry("negative means max", -3, 20),
			Entry("over max", 500, 20),
			Entry("within bounds", 7, 7),
		)

		It("returns an empty page without error when the provider has no accounts", func() {
			build(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"Items":[]}`)) })
			page, err := adapter.ListAccounts(context.Background(), Page{})
			Expect(err).ToNot(HaveOccurred())
			Expect(page.Accounts).To(BeEmpty())
			Expect(page.Next).To(BeEmpty())
		})

		It("passes duplicate accounts through unchanged", func() {
			build(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"Items":[{"Id":"a1","Status":"Active"},{"Id":"a1","Status":"Active"}]}`))
			})
			page, err := adapter.ListAccounts(context.Background(), Page{})
			Expect(err).ToNot(HaveOccurred())
			Expect(page.Accounts).To(HaveLen(2))
		})

		It("fails instead of dropping an account that has no id", func() {
			build(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"Items":[{"Status":"Active"}]}`))
			})
			_, err := adapter.ListAccounts(context.Background(), Page{})
			Expect(reasonOf(err)).To(Equal(ReasonMalformed))
		})

		It("surfaces provider failure rather than an empty list", func() {
			build(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) })
			page, err := adapter.ListAccounts(context.Background(), Page{})
			Expect(reasonOf(err)).To(Equal(ReasonUnauthorized))
			Expect(page.Accounts).To(BeNil())
		})
	})

	Describe("GetAccount and CorrelationValue", func() {
		It("reads one account by its native id", func() {
			var path string
			build(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.EscapedPath()
				_, _ = w.Write([]byte(`{"Id":"a/1","ExternalId":"ext-1","Status":"Inactive","ApprovalStatus":"Approved"}`))
			})
			acct, err := adapter.GetAccount(context.Background(), NewAccountRef("a/1"))
			Expect(err).ToNot(HaveOccurred())
			Expect(path).To(Equal("/api/v2/accounts/a%2F1"))
			Expect(acct.State).To(Equal(AccountInactive))
			Expect(acct.RawStatus).To(Equal("Inactive"))
		})

		It("reports the correlation value as present when ExternalId is set", func() {
			build(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"Id":"a1","ExternalId":"ext-1","Status":"Active"}`))
			})
			v, ok, err := adapter.CorrelationValue(context.Background(), NewAccountRef("a1"))
			Expect(err).ToNot(HaveOccurred())
			Expect(ok).To(BeTrue())
			Expect(v).To(Equal("ext-1"))
		})

		It("reports the correlation value as absent when ExternalId is missing", func() {
			build(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"Id":"a1","Status":"Active"}`)) })
			v, ok, err := adapter.CorrelationValue(context.Background(), NewAccountRef("a1"))
			Expect(err).ToNot(HaveOccurred())
			Expect(ok).To(BeFalse())
			Expect(v).To(BeEmpty())
		})

		It("propagates lookup failure from CorrelationValue", func() {
			build(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(409) })
			_, _, err := adapter.CorrelationValue(context.Background(), NewAccountRef("a1"))
			Expect(reasonOf(err)).To(Equal(ReasonConflict))
		})
	})

	DescribeTable("normalizes provider status to a closed state set",
		func(raw string, want AccountState) {
			build(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"Id":"a1","Status":"` + raw + `"}`))
			})
			acct, err := adapter.GetAccount(context.Background(), NewAccountRef("a1"))
			Expect(err).ToNot(HaveOccurred())
			Expect(acct.State).To(Equal(want))
			Expect(acct.RawStatus).To(Equal(raw))
		},
		Entry("Active", "Active", AccountActive),
		Entry("case-insensitive", "ACTIVE", AccountActive),
		Entry("Inactive", "Inactive", AccountInactive),
		Entry("anything else", "Suspended", AccountUnknown),
		Entry("empty", "", AccountUnknown),
	)

	Describe("correlation write", func() {
		It("is unsupported and never contacts the provider", func() {
			build(func(http.ResponseWriter, *http.Request) {})
			Expect(adapter.SupportsCorrelationWrite()).To(BeFalse())
			err := adapter.SetCorrelationValue(context.Background(), NewAccountRef("a1"), "v")
			Expect(reasonOf(err)).To(Equal(ReasonUnsupported))
			Expect(fake.hits.Load()).To(BeZero())
		})
	})

	Describe("HealthCheck", func() {
		It("succeeds when the account API answers", func() {
			build(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"Items":[]}`)) })
			Expect(adapter.HealthCheck(context.Background())).To(Succeed())
			Expect((<-queries).Get("pageSize")).To(Equal("1"))
		})

		It("fails when the account API rejects the credential", func() {
			build(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) })
			Expect(reasonOf(adapter.HealthCheck(context.Background()))).To(Equal(ReasonUnauthorized))
		})

		It("gives up quickly on a hung provider", func() {
			build(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			Expect(adapter.HealthCheck(ctx)).To(HaveOccurred())
		})
	})
})

var _ = It("M360 refuses to build without complete config", func() {
	_, err := NewM360(Config{}).Build()
	Expect(reasonOf(err)).To(Equal(ReasonNotConfigured))
})
