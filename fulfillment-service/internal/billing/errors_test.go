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
	"errors"
	"fmt"
	"log/slog"
	"strings"

	. "github.com/onsi/ginkgo/v2/dsl/core"
	. "github.com/onsi/ginkgo/v2/dsl/table"
	. "github.com/onsi/gomega"
)

var _ = Describe("Error", func() {
	It("renders reason and operation without any provider detail", func() {
		err := &Error{Reason: ReasonUnavailable, Op: "GetAccount", Retryable: true}
		Expect(err.Error()).To(Equal("billing GetAccount: UNAVAILABLE"))
	})

	It("is matchable with errors.As through wrapping", func() {
		wrapped := fmt.Errorf("outer: %w", &Error{Reason: ReasonConflict, Op: "x"})
		var target *Error
		Expect(errors.As(wrapped, &target)).To(BeTrue())
		Expect(target.Reason).To(Equal(ReasonConflict))
	})
})

var _ = Describe("Opaque refs", func() {
	const native = "native-id-12345"

	DescribeTable("never leak the provider id through formatting or logging",
		func(ref any) {
			var buf strings.Builder
			l := slog.New(slog.NewTextHandler(&buf, nil))
			l.Info("x", "ref", ref)
			for _, s := range []string{
				fmt.Sprint(ref), fmt.Sprintf("%v", ref), fmt.Sprintf("%+v", ref),
				fmt.Sprintf("%#v", ref), fmt.Sprintf("%s", ref), buf.String(),
			} {
				Expect(s).ToNot(ContainSubstring(native))
			}
		},
		Entry("account ref", NewAccountRef(native)),
		Entry("service ref", NewServiceRef(native)),
	)

	It("round-trips the id for the adapter that issued it", func() {
		Expect(NewAccountRef(native).ID()).To(Equal(native))
		Expect(NewServiceRef(native).ID()).To(Equal(native))
	})
})

var _ = Describe("Config", func() {
	DescribeTable("validation",
		func(cfg Config, ok bool) {
			err := cfg.validate()
			if ok {
				Expect(err).ToNot(HaveOccurred())
				return
			}
			var e *Error
			Expect(errors.As(err, &e)).To(BeTrue())
			Expect(e.Reason).To(Equal(ReasonNotConfigured))
		},
		Entry("complete", Config{BaseURL: "https://x", APIVersion: "v1", Credential: "t"}, true),
		Entry("missing url", Config{APIVersion: "v1", Credential: "t"}, false),
		Entry("missing version", Config{BaseURL: "https://x", Credential: "t"}, false),
		Entry("missing credential", Config{BaseURL: "https://x", APIVersion: "v1"}, false),
	)
})
