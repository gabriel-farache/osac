/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package migrations

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/ginkgo/v2/dsl/core"
	. "github.com/onsi/gomega"
)

var _ = DescribeMigration("Create billing accounts tables", func() {
	const insert = `insert into billing_accounts (id, tenant, data) values ($1, 'shared', $2::jsonb)`

	BeforeEach(func(ctx context.Context) {
		Expect(tool.Migrate(ctx, 136)).To(Succeed())
	})

	uniqueViolation := func(err error) bool {
		var pgErr *pgconn.PgError
		return errors.As(err, &pgErr) && pgErr.Code == "23505"
	}

	It("creates the billing_accounts and archived_billing_accounts tables", func(ctx context.Context) {
		_, err := conn.Exec(ctx, insert, "a", `{"provider_key":"m360","provider_account_reference":"1"}`)
		Expect(err).ToNot(HaveOccurred())
		_, err = conn.Exec(ctx, `insert into archived_billing_accounts (id, tenant, creation_timestamp,
			deletion_timestamp, data) values ('a', 'shared', now(), now(), '{}')`)
		Expect(err).ToNot(HaveOccurred())
	})

	It("rejects two rows for the same provider account", func(ctx context.Context) {
		_, err := conn.Exec(ctx, insert, "a", `{"provider_key":"m360","provider_account_reference":"1"}`)
		Expect(err).ToNot(HaveOccurred())
		_, err = conn.Exec(ctx, insert, "b", `{"provider_key":"m360","provider_account_reference":"1"}`)
		Expect(uniqueViolation(err)).To(BeTrue())
	})

	It("allows the same native reference under another provider key", func(ctx context.Context) {
		_, err := conn.Exec(ctx, insert, "a", `{"provider_key":"m360","provider_account_reference":"1"}`)
		Expect(err).ToNot(HaveOccurred())
		_, err = conn.Exec(ctx, insert, "b", `{"provider_key":"other","provider_account_reference":"1"}`)
		Expect(err).ToNot(HaveOccurred())
	})

	It("rejects two rows with the same correlation id", func(ctx context.Context) {
		_, err := conn.Exec(ctx, insert, "a",
			`{"provider_key":"m360","provider_account_reference":"1","correlation_id":"c"}`)
		Expect(err).ToNot(HaveOccurred())
		_, err = conn.Exec(ctx, insert, "b",
			`{"provider_key":"m360","provider_account_reference":"2","correlation_id":"c"}`)
		Expect(uniqueViolation(err)).To(BeTrue())
	})

	It("allows any number of rows without a correlation id", func(ctx context.Context) {
		_, err := conn.Exec(ctx, insert, "a", `{"provider_key":"m360","provider_account_reference":"1"}`)
		Expect(err).ToNot(HaveOccurred())
		_, err = conn.Exec(ctx, insert, "b", `{"provider_key":"m360","provider_account_reference":"2"}`)
		Expect(err).ToNot(HaveOccurred())
	})
})
