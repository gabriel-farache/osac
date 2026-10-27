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
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxPageSize        = 20
	healthCheckTimeout = 3 * time.Second
)

// M360 wire shapes for accounts live here so a change in the provider contract touches one place.
type m360Account struct {
	ID             string `json:"Id"`
	Name           string `json:"Name"`
	ExternalID     string `json:"ExternalId"`
	Status         string `json:"Status"`
	ApprovalStatus string `json:"ApprovalStatus"`
}

type m360AccountList struct {
	Items    []m360Account `json:"Items"`
	NextPage string        `json:"NextPage"`
}

func (a m360Account) normalize(op string) (Account, error) {
	if a.ID == "" {
		return Account{}, &Error{Reason: ReasonMalformed, Op: op}
	}
	state := AccountUnknown
	switch {
	case strings.EqualFold(a.Status, "Active"):
		state = AccountActive
	case strings.EqualFold(a.Status, "Inactive"):
		state = AccountInactive
	}
	return Account{
		Ref:            NewAccountRef(a.ID),
		Name:           a.Name,
		ExternalID:     a.ExternalID,
		State:          state,
		RawStatus:      a.Status,
		ApprovalStatus: a.ApprovalStatus,
	}, nil
}

func (m *m360) ListAccounts(ctx context.Context, filter AccountFilter, page Page) (AccountPage, error) {
	const op = "ListAccounts"
	size := page.Size
	if size <= 0 || size > maxPageSize {
		size = maxPageSize
	}
	query := url.Values{"pageSize": {strconv.Itoa(size)}}
	if page.Token != "" {
		query.Set("page", page.Token)
	}
	// q is a case-insensitive substring search over name, number and externalId, so it only narrows the
	// candidates; the exact match below is what decides.
	switch {
	case filter.ExternalID != "":
		query.Set("q", filter.ExternalID)
	case filter.Name != "":
		query.Set("q", filter.Name)
	}
	var list m360AccountList
	if err := m.client.get(ctx, op, "/accounts", query, &list); err != nil {
		return AccountPage{}, err
	}
	result := AccountPage{Accounts: make([]Account, 0, len(list.Items)), Next: list.NextPage}
	for _, item := range list.Items {
		account, err := item.normalize(op)
		if err != nil {
			return AccountPage{}, err
		}
		if (filter.ExternalID == "" || account.ExternalID == filter.ExternalID) &&
			(filter.Name == "" || account.Name == filter.Name) {
			result.Accounts = append(result.Accounts, account)
		}
	}
	return result, nil
}

func (m *m360) GetAccount(ctx context.Context, ref AccountRef) (Account, error) {
	const op = "GetAccount"
	var raw m360Account
	if err := m.client.get(ctx, op, "/accounts/"+url.PathEscape(ref.ID()), nil, &raw); err != nil {
		return Account{}, notFoundOn404(err)
	}
	return raw.normalize(op)
}

func (m *m360) CorrelationValue(ctx context.Context, ref AccountRef) (string, bool, error) {
	account, err := m.GetAccount(ctx, ref)
	if err != nil {
		return "", false, err
	}
	return account.ExternalID, account.ExternalID != "", nil
}

// M360 accepts externalId only when an account is created, so there is nothing to write afterwards.
func (m *m360) SupportsCorrelationWrite() bool { return false }

func (m *m360) SetCorrelationValue(context.Context, AccountRef, string) error {
	return &Error{Reason: ReasonUnsupported, Op: "SetCorrelationValue"}
}

func (m *m360) HealthCheck(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	query := url.Values{"pageSize": {"1"}}
	return m.client.get(ctx, "HealthCheck", "/accounts", query, nil)
}
