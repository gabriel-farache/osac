/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package billingaccount

import (
	"bytes"
	"context"
	"encoding/json"

	. "github.com/onsi/ginkgo/v2/dsl/core"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/osac-project/osac/fulfillment-service/internal/terminal"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
)

func render(format string, accounts ...*publicv1.BillingAccount) (string, error) {
	buffer := &bytes.Buffer{}
	console, err := terminal.NewConsole().
		SetLogger(logger).
		SetStdout(buffer).
		Build()
	Expect(err).ToNot(HaveOccurred())
	err = renderAccounts(context.Background(), console, accounts, format)
	return buffer.String(), err
}

var _ = Describe("renderAccounts", func() {
	var full *publicv1.BillingAccount

	BeforeEach(func() {
		full = publicv1.BillingAccount_builder{
			Id:               "ba-1",
			ProviderKey:      "m360",
			DisplayName:      "Acme production",
			BaseCurrency:     "USD",
			State:            publicv1.BillingAccountState_BILLING_ACCOUNT_STATE_ACTIVE,
			StatusObservedAt: timestamppb.New(timestamppb.Now().AsTime().UTC().Truncate(1e9)),
			CorrelationId:    proto.String("corr-1"),
		}.Build()
	})

	Describe("Table", func() {
		It("renders every field of a complete account without enum prefixes", func() {
			output, err := render("table", full)
			Expect(err).ToNot(HaveOccurred())
			for _, header := range []string{"ID", "PROVIDER", "NAME", "CURRENCY", "STATE", "OBSERVED", "CORRELATION ID"} {
				Expect(output).To(ContainSubstring(header))
			}
			Expect(output).To(MatchRegexp(`ba-1\s+m360\s+Acme production\s+USD\s+ACTIVE\s+\S+\s+corr-1`))
			Expect(output).ToNot(ContainSubstring("BILLING_ACCOUNT_STATE_"))
		})

		It("shows an explicit unknown currency, UNKNOWN state and unset correlation id", func() {
			output, err := render("table", publicv1.BillingAccount_builder{
				Id:          "ba-2",
				ProviderKey: "m360",
				DisplayName: "No currency",
				State:       publicv1.BillingAccountState_BILLING_ACCOUNT_STATE_UNKNOWN,
			}.Build())
			Expect(err).ToNot(HaveOccurred())
			Expect(output).To(MatchRegexp(`ba-2\s+m360\s+No currency\s+\(unknown\)\s+UNKNOWN\s+-\s+-`))
		})

		It("shows an unspecified state as UNKNOWN", func() {
			output, err := render("table", publicv1.BillingAccount_builder{Id: "ba-3"}.Build())
			Expect(err).ToNot(HaveOccurred())
			Expect(output).To(ContainSubstring("UNKNOWN"))
			Expect(output).ToNot(ContainSubstring("UNSPECIFIED"))
		})

		It("tells the user when there are no accounts", func() {
			output, err := render("table")
			Expect(err).ToNot(HaveOccurred())
			Expect(output).To(ContainSubstring("No billing accounts found"))
		})
	})

	Describe("Machine-readable", func() {
		It("renders JSON with the API field names and enum names", func() {
			output, err := render("json", full)
			Expect(err).ToNot(HaveOccurred())
			var decoded []map[string]any
			Expect(json.Unmarshal([]byte(output), &decoded)).To(Succeed())
			Expect(decoded).To(HaveLen(1))
			Expect(decoded[0]).To(HaveKeyWithValue("id", "ba-1"))
			Expect(decoded[0]).To(HaveKeyWithValue("provider_key", "m360"))
			Expect(decoded[0]).To(HaveKeyWithValue("state", "BILLING_ACCOUNT_STATE_ACTIVE"))
			Expect(decoded[0]).To(HaveKeyWithValue("correlation_id", "corr-1"))
		})

		It("renders an empty JSON list when there are no accounts", func() {
			output, err := render("json")
			Expect(err).ToNot(HaveOccurred())
			Expect(output).To(MatchRegexp(`^\[\s*\]\s*$`))
		})

		It("renders YAML", func() {
			output, err := render("yaml", full)
			Expect(err).ToNot(HaveOccurred())
			Expect(output).To(ContainSubstring("provider_key: m360"))
			Expect(output).To(ContainSubstring("display_name: Acme production"))
		})

		It("rejects an unknown format", func() {
			_, err := render("xml", full)
			Expect(err).To(MatchError(ContainSubstring("unknown output format 'xml'")))
		})
	})
})
