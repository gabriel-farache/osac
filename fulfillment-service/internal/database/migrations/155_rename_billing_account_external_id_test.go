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
	"os"

	. "github.com/onsi/ginkgo/v2/dsl/core"
	. "github.com/onsi/gomega"
)

var _ = DescribeMigration("Rename billing account external id", func() {
	BeforeEach(func(ctx context.Context) {
		_, err := conn.Exec(ctx, `
			insert into tenants (id, name, tenant, data) values ('tenant', 'tenant', 'tenant', '{}');
			insert into billing_accounts (id, provider_key, provider_account_reference, correlation_id) values
				('present', 'm360', 'native-present', ' ext-1 '),
				('absent', 'm360', 'native-absent', null),
				('empty', 'm360', 'native-empty', '');
			insert into tenant_billing_assignments (id, name, tenant_id, billing_account_id, connection_state)
				values ('assignment', 'assignment', 'tenant', 'present', 'CONNECTED');
		`)
		Expect(err).ToNot(HaveOccurred())
	})

	assignment := func(ctx context.Context) string {
		GinkgoHelper()
		var result string
		Expect(conn.QueryRow(ctx, `select row_to_json(t)::text from tenant_billing_assignments t`).Scan(&result)).To(Succeed())
		return result
	}

	It("preserves stored values, nulls, empty strings and assignment links on upgrade and rollback", func(ctx context.Context) {
		before := assignment(ctx)
		Expect(tool.Migrate(ctx, 155)).To(Succeed())
		var present, empty string
		var absent *string
		Expect(conn.QueryRow(ctx, `select external_billing_account_id from billing_accounts where id = 'present'`).Scan(&present)).To(Succeed())
		Expect(conn.QueryRow(ctx, `select external_billing_account_id from billing_accounts where id = 'absent'`).Scan(&absent)).To(Succeed())
		Expect(conn.QueryRow(ctx, `select external_billing_account_id from billing_accounts where id = 'empty'`).Scan(&empty)).To(Succeed())
		Expect(present).To(Equal(" ext-1 "))
		Expect(absent).To(BeNil())
		Expect(empty).To(BeEmpty())
		Expect(assignment(ctx)).To(Equal(before))
		script, err := os.ReadFile("rollback/155_rename_billing_account_external_id.down.sql")
		Expect(err).ToNot(HaveOccurred())
		_, err = conn.Exec(ctx, string(script))
		Expect(err).ToNot(HaveOccurred())
		Expect(conn.QueryRow(ctx, `select correlation_id from billing_accounts where id = 'present'`).Scan(&present)).To(Succeed())
		Expect(conn.QueryRow(ctx, `select correlation_id from billing_accounts where id = 'absent'`).Scan(&absent)).To(Succeed())
		Expect(conn.QueryRow(ctx, `select correlation_id from billing_accounts where id = 'empty'`).Scan(&empty)).To(Succeed())
		Expect(present).To(Equal(" ext-1 "))
		Expect(absent).To(BeNil())
		Expect(empty).To(BeEmpty())
		Expect(assignment(ctx)).To(Equal(before))
	})

	It("retains uniqueness and permits multiple null identifiers after upgrade", func(ctx context.Context) {
		Expect(tool.Migrate(ctx, 155)).To(Succeed())
		_, err := conn.Exec(ctx, `insert into billing_accounts
			(id, provider_key, provider_account_reference, external_billing_account_id)
			values ('duplicate', 'm360', 'native-duplicate', ' ext-1 ')`)
		Expect(err).To(MatchError(ContainSubstring("billing_accounts_unique_correlation_id")))
		_, err = conn.Exec(ctx, `insert into billing_accounts (id, provider_key, provider_account_reference)
			values ('another-null', 'm360', 'native-null')`)
		Expect(err).ToNot(HaveOccurred())
	})
})
