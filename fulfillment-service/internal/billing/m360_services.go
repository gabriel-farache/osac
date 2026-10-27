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
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// M360 wire shapes for services live here so a change in the provider contract touches one place.
type m360Service struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

type m360ServiceList struct {
	Items    []m360Service `json:"Items"`
	NextPage string        `json:"NextPage"`
}

type m360AttributeList struct {
	Items []struct {
		Name string `json:"Name"`
	} `json:"Items"`
}

type m360RateCard struct {
	Currency string `json:"Currency"`
	Rows     []struct {
		Dimension string      `json:"Dimension"`
		Amount    json.Number `json:"Amount"`
		Unit      string      `json:"Unit"`
	} `json:"Rows"`
}

// maxServicePages bounds pagination so a provider that cycles page tokens cannot loop forever.
const maxServicePages = 50

func (m *m360) FindServiceByName(ctx context.Context, name string) (ServiceRef, error) {
	const op = "FindServiceByName"
	var matches []m360Service
	token := ""
	for page := 0; ; page++ {
		if page >= maxServicePages {
			return ServiceRef{}, &Error{Reason: ReasonMalformed, Op: op}
		}
		query := url.Values{"q": {name}, "pageSize": {strconv.Itoa(maxPageSize)}}
		if token != "" {
			query.Set("page", token)
		}
		var list m360ServiceList
		if err := m.client.get(ctx, op, "/services", query, &list); err != nil {
			return ServiceRef{}, err
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
		return ServiceRef{}, ErrServiceNotFound
	case 1:
		if matches[0].ID == "" {
			return ServiceRef{}, &Error{Reason: ReasonMalformed, Op: op}
		}
		return NewServiceRef(matches[0].ID), nil
	default:
		return ServiceRef{}, &Error{Reason: ReasonAmbiguous, Op: op}
	}
}

func (m *m360) ListPriceableAttributes(ctx context.Context, svc ServiceRef) ([]PriceableAttribute, error) {
	const op = "ListPriceableAttributes"
	var list m360AttributeList
	if err := m.client.get(ctx, op, "/services/"+url.PathEscape(svc.ID())+"/priceable-attributes", nil, &list); err != nil {
		return nil, err
	}
	attrs := make([]PriceableAttribute, 0, len(list.Items))
	for _, item := range list.Items {
		attrs = append(attrs, PriceableAttribute{Name: item.Name})
	}
	return attrs, nil
}

func (m *m360) GetServiceRateCard(ctx context.Context, svc ServiceRef, currency string) (RateCard, error) {
	const op = "GetServiceRateCard"
	if currency == "" {
		return RateCard{}, &Error{Reason: ReasonMalformed, Op: op}
	}
	var raw m360RateCard
	path := "/services/" + url.PathEscape(svc.ID()) + "/rate-cards/" + url.PathEscape(currency)
	if err := m.client.get(ctx, op, path, nil, &raw); err != nil {
		var e *Error
		if errors.As(err, &e) && e.status == http.StatusNotFound {
			return RateCard{}, ErrRateCardNotFound
		}
		return RateCard{}, err
	}
	if raw.Currency != "" && !strings.EqualFold(raw.Currency, currency) {
		return RateCard{}, &Error{Reason: ReasonMalformed, Op: op}
	}
	if len(raw.Rows) == 0 {
		return RateCard{}, ErrRateCardNotFound
	}
	card := RateCard{Currency: currency, Rows: make([]RateRow, 0, len(raw.Rows))}
	for _, row := range raw.Rows {
		if row.Amount == "" {
			return RateCard{}, &Error{Reason: ReasonMalformed, Op: op}
		}
		card.Rows = append(card.Rows, RateRow{Dimension: row.Dimension, Amount: row.Amount.String(), Unit: row.Unit})
	}
	return card, nil
}
