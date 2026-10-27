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
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/osac-project/osac/fulfillment-service/internal/billing"
)

// M360 wire shapes for services live here so a change in the provider contract touches one place.
type service struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

type serviceList struct {
	Items    []service `json:"Items"`
	NextPage string    `json:"NextPage"`
}

type attributeList struct {
	Items []struct {
		Name string `json:"Name"`
	} `json:"Items"`
}

type rateCard struct {
	Currency string `json:"Currency"`
	Rows     []struct {
		Dimension string      `json:"Dimension"`
		Amount    json.Number `json:"Amount"`
		Unit      string      `json:"Unit"`
	} `json:"Rows"`
}

// maxServicePages bounds pagination so a provider that cycles page tokens cannot loop forever.
const maxServicePages = 50

func (a *adapter) FindServiceByName(ctx context.Context, name string) (billing.ServiceRef, error) {
	const op = "FindServiceByName"
	var matches []service
	token := ""
	for page := 0; ; page++ {
		if page >= maxServicePages {
			return billing.ServiceRef{}, &billing.Error{Reason: billing.ReasonMalformed, Op: op}
		}
		query := url.Values{"q": {name}, "pageSize": {strconv.Itoa(maxPageSize)}}
		if token != "" {
			query.Set("page", token)
		}
		var list serviceList
		if err := a.client.get(ctx, op, "/services", query, &list); err != nil {
			return billing.ServiceRef{}, err
		}
		for _, item := range list.Items {
			if item.Name == name {
				matches = append(matches, item)
			}
		}
		if list.NextPage == "" || list.NextPage == token {
			break
		}
		token = list.NextPage
	}
	switch len(matches) {
	case 0:
		return billing.ServiceRef{}, billing.ErrServiceNotFound
	case 1:
		if matches[0].ID == "" {
			return billing.ServiceRef{}, &billing.Error{Reason: billing.ReasonMalformed, Op: op}
		}
		return billing.NewServiceRef(matches[0].ID), nil
	default:
		return billing.ServiceRef{}, &billing.Error{Reason: billing.ReasonAmbiguous, Op: op}
	}
}

func (a *adapter) ListPriceableAttributes(ctx context.Context, svc billing.ServiceRef) ([]billing.PriceableAttribute, error) {
	const op = "ListPriceableAttributes"
	var list attributeList
	if err := a.client.get(ctx, op, "/services/"+url.PathEscape(svc.ID())+"/priceable-attributes", nil, &list); err != nil {
		return nil, notFoundOn404(err)
	}
	attrs := make([]billing.PriceableAttribute, 0, len(list.Items))
	for _, item := range list.Items {
		attrs = append(attrs, billing.PriceableAttribute{Name: item.Name})
	}
	return attrs, nil
}

func (a *adapter) GetServiceRateCard(ctx context.Context, svc billing.ServiceRef, currency string) (billing.RateCard, error) {
	const op = "GetServiceRateCard"
	if currency == "" {
		return billing.RateCard{}, &billing.Error{Reason: billing.ReasonMalformed, Op: op}
	}
	var raw rateCard
	path := "/services/" + url.PathEscape(svc.ID()) + "/rate-cards/" + url.PathEscape(currency)
	if err := a.client.get(ctx, op, path, nil, &raw); err != nil {
		var e *billing.Error
		if errors.As(err, &e) && e.HTTPStatus() == http.StatusNotFound {
			return billing.RateCard{}, billing.ErrRateCardNotFound
		}
		return billing.RateCard{}, err
	}
	if raw.Currency != "" && !strings.EqualFold(raw.Currency, currency) {
		return billing.RateCard{}, &billing.Error{Reason: billing.ReasonMalformed, Op: op}
	}
	if len(raw.Rows) == 0 {
		return billing.RateCard{}, billing.ErrRateCardNotFound
	}
	card := billing.RateCard{Currency: currency, Rows: make([]billing.RateRow, 0, len(raw.Rows))}
	for _, row := range raw.Rows {
		if row.Amount == "" {
			return billing.RateCard{}, &billing.Error{Reason: billing.ReasonMalformed, Op: op}
		}
		card.Rows = append(card.Rows, billing.RateRow{Dimension: row.Dimension, Amount: row.Amount.String(), Unit: row.Unit})
	}
	return card, nil
}
