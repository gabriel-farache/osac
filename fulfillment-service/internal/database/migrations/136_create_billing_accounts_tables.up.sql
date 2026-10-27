--
-- Copyright (c) 2026 Red Hat Inc.
--
-- Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with
-- the License. You may obtain a copy of the License at
--
--   http://www.apache.org/licenses/LICENSE-2.0
--
-- Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
-- "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
-- specific language governing permissions and limitations under the License.
--

-- Create the billing_accounts tables following the generic schema pattern.
-- A billing account is provider-global: it is discovered from the configured billing provider and carries the
-- reserved 'shared' tenant only because the generic schema requires one. The account fields (provider_key,
-- provider_account_reference, display_name, base_currency, state, status_observed_at, correlation_id) live in the
-- data column.

create table billing_accounts (
  id text not null primary key,
  name text not null default '',
  creation_timestamp timestamp with time zone not null default now(),
  deletion_timestamp timestamp with time zone not null default 'epoch',
  finalizers text[] not null default '{}',
  creator text not null default '',
  tenant text not null default '',
  project ltree not null default ''::ltree,
  labels jsonb not null default '{}'::jsonb,
  annotations jsonb not null default '{}'::jsonb,
  data jsonb not null,
  version integer not null default 0
);

create table archived_billing_accounts (
  id text not null,
  name text not null default '',
  creation_timestamp timestamp with time zone not null,
  deletion_timestamp timestamp with time zone not null,
  archival_timestamp timestamp with time zone not null default now(),
  creator text not null default '',
  tenant text not null default '',
  project ltree not null default ''::ltree,
  labels jsonb not null default '{}'::jsonb,
  annotations jsonb not null default '{}'::jsonb,
  data jsonb not null,
  version integer not null default 0
);

-- One row per provider account.
create unique index billing_accounts_unique_provider_account
  on billing_accounts ((data->>'provider_key'), (data->>'provider_account_reference'));

-- Two accounts can never store the same correlation id. Accounts without one are not constrained.
create unique index billing_accounts_unique_correlation_id
  on billing_accounts ((data->>'correlation_id'))
  where data->>'correlation_id' is not null;

-- Tenant foreign key referencing the tenants table.
alter table billing_accounts
  add constraint billing_accounts_tenant_fk
  foreign key (tenant) references tenants (name);

-- Enforce immutability of id, name, tenant, and project columns at the database level.
create trigger check_immutable_columns
  before update on billing_accounts
  for each row
  execute function check_immutable_columns('id', 'name', 'tenant', 'project');

-- Active companion table for billing_accounts (see migration 97 for the pattern).
create table active_billing_accounts (
  id text not null primary key references billing_accounts (id) on delete cascade
);

create trigger materialize_active_objects
  after insert or update of deletion_timestamp on billing_accounts
  for each row execute function materialize_active_objects();

create trigger enqueue_change
  after insert or update or delete on billing_accounts
  for each row execute function enqueue_change();
