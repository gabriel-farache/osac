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

import "log/slog"

const redacted = "[redacted]"

// AccountRef identifies a provider account. The native id is only reachable through ID so that accidental
// formatting or logging cannot leak it.
type AccountRef struct{ id string }

func NewAccountRef(id string) AccountRef { return AccountRef{id: id} }
func (r AccountRef) ID() string          { return r.id }
func (AccountRef) String() string        { return redacted }
func (AccountRef) GoString() string      { return redacted }
func (AccountRef) LogValue() slog.Value  { return slog.StringValue(redacted) }

// ServiceRef identifies a provider service. It follows the same redaction rules as AccountRef.
type ServiceRef struct{ id string }

func NewServiceRef(id string) ServiceRef { return ServiceRef{id: id} }
func (r ServiceRef) ID() string          { return r.id }
func (ServiceRef) String() string        { return redacted }
func (ServiceRef) GoString() string      { return redacted }
func (ServiceRef) LogValue() slog.Value  { return slog.StringValue(redacted) }

type AccountState string

const (
	AccountActive   AccountState = "ACTIVE"
	AccountInactive AccountState = "INACTIVE"
	AccountUnknown  AccountState = "UNKNOWN"
)

// Account keeps the raw provider status values next to the normalized state.
type Account struct {
	Ref            AccountRef
	ExternalID     string
	State          AccountState
	RawStatus      string
	ApprovalStatus string
}

type Page struct {
	Token string
	Size  int
}

// AccountPage.Next is empty on the last page.
type AccountPage struct {
	Accounts []Account
	Next     string
}

type PriceableAttribute struct{ Name string }

type RateRow struct {
	Dimension string
	Amount    string // decimal exactly as received from the provider
	Unit      string // empty when the provider advertises none
}

type RateCard struct {
	Currency string
	Rows     []RateRow
}
