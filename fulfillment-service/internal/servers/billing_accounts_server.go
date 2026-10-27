/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package servers

import (
	"context"
	"errors"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/osac-project/osac/fulfillment-service/internal/auth"
	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
)

type BillingAccountsServerBuilder struct {
	logger            *slog.Logger
	attributionLogic  auth.AttributionLogic
	tenancyLogic      auth.TenancyLogic
	metricsRegisterer prometheus.Registerer
	adapter           billing.Adapter
	providerKey       string
}

var _ publicv1.BillingAccountsServer = (*BillingAccountsServer)(nil)

type BillingAccountsServer struct {
	publicv1.UnimplementedBillingAccountsServer

	logger    *slog.Logger
	delegate  privatev1.BillingAccountsServer
	outMapper *GenericMapper[*privatev1.BillingAccount, *publicv1.BillingAccount]
}

func NewBillingAccountsServer() *BillingAccountsServerBuilder {
	return &BillingAccountsServerBuilder{}
}

func (b *BillingAccountsServerBuilder) SetLogger(value *slog.Logger) *BillingAccountsServerBuilder {
	b.logger = value
	return b
}

func (b *BillingAccountsServerBuilder) SetAttributionLogic(value auth.AttributionLogic) *BillingAccountsServerBuilder {
	b.attributionLogic = value
	return b
}

func (b *BillingAccountsServerBuilder) SetTenancyLogic(value auth.TenancyLogic) *BillingAccountsServerBuilder {
	b.tenancyLogic = value
	return b
}

func (b *BillingAccountsServerBuilder) SetMetricsRegisterer(value prometheus.Registerer) *BillingAccountsServerBuilder {
	b.metricsRegisterer = value
	return b
}

// SetBillingAdapter sets the adapter of the configured billing provider. This is optional: without it billing is
// disabled and List fails with BILLING_NOT_CONFIGURED.
func (b *BillingAccountsServerBuilder) SetBillingAdapter(value billing.Adapter) *BillingAccountsServerBuilder {
	b.adapter = value
	return b
}

// SetProviderKey sets the key stored in provider_key. It is mandatory when an adapter is set.
func (b *BillingAccountsServerBuilder) SetProviderKey(value string) *BillingAccountsServerBuilder {
	b.providerKey = value
	return b
}

func (b *BillingAccountsServerBuilder) Build() (result *BillingAccountsServer, err error) {
	if b.logger == nil {
		err = errors.New("logger is mandatory")
		return
	}

	outMapper, err := NewGenericMapper[*privatev1.BillingAccount, *publicv1.BillingAccount]().
		SetLogger(b.logger).
		SetStrict(false).
		Build()
	if err != nil {
		return
	}

	// The DAO validates filters against the public schema, so provider internals can't be filtered on:
	delegate, err := NewPrivateBillingAccountsServer().
		SetLogger(b.logger).
		SetAttributionLogic(b.attributionLogic).
		SetTenancyLogic(b.tenancyLogic).
		SetMetricsRegisterer(b.metricsRegisterer).
		SetFilterDesc((*publicv1.BillingAccount)(nil).ProtoReflect().Descriptor()).
		SetBillingAdapter(b.adapter).
		SetProviderKey(b.providerKey).
		Build()
	if err != nil {
		return
	}

	result = &BillingAccountsServer{
		logger:    b.logger,
		delegate:  delegate,
		outMapper: outMapper,
	}
	return
}

func (s *BillingAccountsServer) List(ctx context.Context,
	request *publicv1.BillingAccountsListRequest) (response *publicv1.BillingAccountsListResponse, err error) {
	privateRequest := &privatev1.BillingAccountsListRequest{}
	privateRequest.SetOffset(request.GetOffset())
	if request.HasLimit() {
		privateRequest.SetLimit(request.GetLimit())
	}
	privateRequest.SetFilter(request.GetFilter())
	privateRequest.SetOrder(request.GetOrder())

	privateResponse, err := s.delegate.List(ctx, privateRequest)
	if err != nil {
		return
	}

	privateItems := privateResponse.GetItems()
	publicItems := make([]*publicv1.BillingAccount, len(privateItems))
	for i, privateItem := range privateItems {
		publicItems[i] = &publicv1.BillingAccount{}
		err = s.outMapper.Copy(ctx, privateItem, publicItems[i])
		if err != nil {
			s.logger.ErrorContext(ctx, "Failed to map private billing account to public", slog.Any("error", err))
			err = grpcstatus.Errorf(grpccodes.Internal, "failed to process billing accounts")
			return
		}
	}
	response = &publicv1.BillingAccountsListResponse{}
	response.SetSize(privateResponse.GetSize())
	response.SetTotal(privateResponse.GetTotal())
	response.SetItems(publicItems)
	return
}
