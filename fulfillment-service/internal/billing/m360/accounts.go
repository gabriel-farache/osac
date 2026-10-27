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
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/osac-project/osac/fulfillment-service/internal/billing"
)

const (
	maxPageSize        = 20
	healthCheckTimeout = 3 * time.Second
)

// M360 wire shapes for accounts live here so a change in the provider contract touches one place.
type account struct {
	ID             string `json:"Id"`
	Name           string `json:"Name"`
	ExternalID     string `json:"ExternalId"`
	Status         string `json:"Status"`
	ApprovalStatus string `json:"ApprovalStatus"`
}

type accountList struct {
	Items    []account `json:"Items"`
	NextPage string    `json:"NextPage"`
}

func (a account) normalize(op string) (billing.Account, error) {
	if a.ID == "" {
		return billing.Account{}, &billing.Error{Reason: billing.ReasonMalformed, Op: op}
	}
	state := billing.AccountUnknown
	switch {
	case strings.EqualFold(a.Status, "Active"):
		state = billing.AccountActive
	case strings.EqualFold(a.Status, "Inactive"):
		state = billing.AccountInactive
	}
	return billing.Account{
		Ref:            billing.NewAccountRef(a.ID),
		Name:           a.Name,
		ExternalID:     a.ExternalID,
		State:          state,
		RawStatus:      a.Status,
		ApprovalStatus: a.ApprovalStatus,
	}, nil
}

func (a *adapter) ListAccounts(ctx context.Context, filter billing.AccountFilter, page billing.Page) (billing.AccountPage, error) {
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
	var list accountList
	if err := a.client.getDiscovery(ctx, op, "/accounts", query, &list); err != nil {
		return billing.AccountPage{}, err
	}
	result := billing.AccountPage{Accounts: make([]billing.Account, 0, len(list.Items)), Next: list.NextPage}
	for _, item := range list.Items {
		acct, err := item.normalize(op)
		if err != nil {
			return billing.AccountPage{}, err
		}
		if (filter.ExternalID == "" || acct.ExternalID == filter.ExternalID) &&
			(filter.Name == "" || acct.Name == filter.Name) {
			result.Accounts = append(result.Accounts, acct)
		}
	}
	return result, nil
}

func (a *adapter) GetAccount(ctx context.Context, ref billing.AccountRef) (billing.Account, error) {
	const op = "GetAccount"
	var raw account
	if err := a.client.getDiscovery(ctx, op, "/accounts/"+url.PathEscape(ref.ID()), nil, &raw); err != nil {
		return billing.Account{}, notFoundOn404(op, err)
	}
	return raw.normalize(op)
}

func (a *adapter) CorrelationValue(ctx context.Context, ref billing.AccountRef) (string, bool, error) {
	acct, err := a.GetAccount(ctx, ref)
	if err != nil {
		return "", false, err
	}
	return acct.ExternalID, acct.ExternalID != "", nil
}

// M360 accepts externalId only when an account is created, so there is nothing to write afterwards.
func (a *adapter) SupportsCorrelationWrite() bool { return false }

func (a *adapter) SetCorrelationValue(context.Context, billing.AccountRef, string) error {
	return &billing.Error{Reason: billing.ReasonUnsupported, Op: "SetCorrelationValue"}
}

func (a *adapter) HealthCheck(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()
	query := url.Values{"pageSize": {"1"}}
	return a.client.getDiscovery(ctx, "HealthCheck", "/accounts", query, nil)
}
