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
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/osac-project/osac/fulfillment-service/internal/auth"
	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	"github.com/osac-project/osac/fulfillment-service/internal/database/dao"
	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
)

const (
	// billingDiscoveryPageSize is the largest page the provider adapters accept.
	billingDiscoveryPageSize = 20

	// billingRowsPageSize is the page size used to read the stored accounts back; it matches the DAO maximum.
	billingRowsPageSize = 1000

	billingErrorDomain = "osac.openshift.io"
)

type PrivateBillingAccountsServerBuilder struct {
	logger            *slog.Logger
	attributionLogic  auth.AttributionLogic
	tenancyLogic      auth.TenancyLogic
	metricsRegisterer prometheus.Registerer
	filterDesc        protoreflect.MessageDescriptor
	adapter           billing.Adapter
	providerKey       string
}

var _ privatev1.BillingAccountsServer = (*PrivateBillingAccountsServer)(nil)

type PrivateBillingAccountsServer struct {
	privatev1.UnimplementedBillingAccountsServer

	logger           *slog.Logger
	attributionLogic auth.AttributionLogic
	dao              *dao.GenericDAO[*privatev1.BillingAccount]
	adapter          billing.Adapter
	providerKey      string

	// refreshLock serializes discovery inside this process. shortcut: replicas can still race, in which case the
	// unique indexes make the loser fail and the caller retry; upgrade to a database advisory lock if that hurts.
	refreshLock sync.Mutex
}

func NewPrivateBillingAccountsServer() *PrivateBillingAccountsServerBuilder {
	return &PrivateBillingAccountsServerBuilder{}
}

func (b *PrivateBillingAccountsServerBuilder) SetLogger(value *slog.Logger) *PrivateBillingAccountsServerBuilder {
	b.logger = value
	return b
}

func (b *PrivateBillingAccountsServerBuilder) SetAttributionLogic(
	value auth.AttributionLogic) *PrivateBillingAccountsServerBuilder {
	b.attributionLogic = value
	return b
}

func (b *PrivateBillingAccountsServerBuilder) SetTenancyLogic(
	value auth.TenancyLogic) *PrivateBillingAccountsServerBuilder {
	b.tenancyLogic = value
	return b
}

func (b *PrivateBillingAccountsServerBuilder) SetMetricsRegisterer(
	value prometheus.Registerer) *PrivateBillingAccountsServerBuilder {
	b.metricsRegisterer = value
	return b
}

// SetFilterDesc sets the descriptor used to validate and translate CEL filters. This is optional; the private
// message is used when it isn't set.
func (b *PrivateBillingAccountsServerBuilder) SetFilterDesc(
	value protoreflect.MessageDescriptor) *PrivateBillingAccountsServerBuilder {
	b.filterDesc = value
	return b
}

// SetBillingAdapter sets the adapter of the configured billing provider. This is optional: without it billing is
// disabled and List fails with BILLING_NOT_CONFIGURED.
func (b *PrivateBillingAccountsServerBuilder) SetBillingAdapter(
	value billing.Adapter) *PrivateBillingAccountsServerBuilder {
	b.adapter = value
	return b
}

// SetProviderKey sets the key stored in provider_key, for example `m360`. It is mandatory when an adapter is set.
func (b *PrivateBillingAccountsServerBuilder) SetProviderKey(value string) *PrivateBillingAccountsServerBuilder {
	b.providerKey = value
	return b
}

func (b *PrivateBillingAccountsServerBuilder) Build() (result *PrivateBillingAccountsServer, err error) {
	if b.logger == nil {
		err = errors.New("logger is mandatory")
		return
	}
	if b.attributionLogic == nil {
		err = errors.New("attribution logic is mandatory")
		return
	}
	if b.tenancyLogic == nil {
		err = errors.New("tenancy logic is mandatory")
		return
	}
	if b.adapter != nil && b.providerKey == "" {
		err = errors.New("provider key is mandatory when billing is configured")
		return
	}
	accountsDAO, err := dao.NewGenericDAO[*privatev1.BillingAccount]().
		SetLogger(b.logger).
		SetTenancyLogic(b.tenancyLogic).
		SetMetricsRegisterer(b.metricsRegisterer).
		SetFilterDesc(b.filterDesc).
		Build()
	if err != nil {
		return
	}
	result = &PrivateBillingAccountsServer{
		logger:           b.logger,
		attributionLogic: b.attributionLogic,
		dao:              accountsDAO,
		adapter:          b.adapter,
		providerKey:      b.providerKey,
	}
	return
}

// List reads the accounts of the billing provider, upserts them and returns the stored accounts. A provider failure
// fails the call: a list that could not be refreshed is never served as current.
func (s *PrivateBillingAccountsServer) List(ctx context.Context,
	request *privatev1.BillingAccountsListRequest) (response *privatev1.BillingAccountsListResponse, err error) {
	if s.adapter == nil {
		err = billingStatus(grpccodes.FailedPrecondition, "BILLING_NOT_CONFIGURED", "billing is not configured")
		return
	}
	err = s.refresh(ctx)
	if err != nil {
		return
	}
	listRequest := s.dao.List().SetFilter(request.GetFilter()).SetOffset(request.GetOffset())
	if request.HasLimit() {
		listRequest.SetLimit(request.GetLimit())
	}
	daoResponse, err := listRequest.Do(ctx)
	if err != nil {
		err = ConvertDAOErrorToGRPC(err, "list", "")
		return
	}
	response = &privatev1.BillingAccountsListResponse{}
	response.SetSize(daoResponse.GetSize())
	response.SetTotal(daoResponse.GetTotal())
	response.SetItems(daoResponse.GetItems())
	return
}

func billingStatus(code grpccodes.Code, reason, message string) error {
	result, err := grpcstatus.New(code, message).WithDetails(&errdetails.ErrorInfo{
		Reason: reason,
		Domain: billingErrorDomain,
	})
	if err != nil {
		return grpcstatus.Error(code, message)
	}
	return result.Err()
}

func errBillingProviderUnavailable() error {
	return billingStatus(grpccodes.Unavailable, "BILLING_PROVIDER_UNAVAILABLE", "the billing provider is unavailable")
}

// refresh brings the stored accounts in line with the provider. Provider values are never logged.
func (s *PrivateBillingAccountsServer) refresh(ctx context.Context) error {
	s.refreshLock.Lock()
	defer s.refreshLock.Unlock()

	accounts, err := s.providerAccounts(ctx)
	if err != nil {
		return err
	}
	rows, err := s.storedAccounts(ctx)
	if err != nil {
		return err
	}

	byReference := map[string]*privatev1.BillingAccount{}
	takenCorrelation := map[string]bool{}
	for _, row := range rows {
		if row.GetProviderKey() == s.providerKey {
			byReference[row.GetProviderAccountReference()] = row
		}
		if row.HasCorrelationId() {
			takenCorrelation[row.GetCorrelationId()] = true
		}
	}
	providerCorrelation := map[string]int{}
	for _, account := range accounts {
		if account.ExternalID != "" {
			providerCorrelation[account.ExternalID]++
		}
	}
	creator, err := s.attributionLogic.DetermineAssignedCreator(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "Failed to determine assigned creator", slog.Any("error", err))
		return grpcstatus.Errorf(grpccodes.Internal, "failed to determine assigned creator")
	}

	now := timestamppb.Now()
	seen := map[string]bool{}
	for _, account := range accounts {
		reference := account.Ref.ID()
		seen[reference] = true
		row := byReference[reference]
		isNew := row == nil
		if isNew {
			row = privatev1.BillingAccount_builder{
				Metadata: privatev1.Metadata_builder{
					Tenant:  auth.SharedTenant,
					Creator: creator,
				}.Build(),
				ProviderKey:              s.providerKey,
				ProviderAccountReference: reference,
			}.Build()
		}
		if account.Name != "" {
			row.SetDisplayName(account.Name)
		}
		row.SetState(billingAccountState(account.State))
		row.SetStatusObservedAt(now)

		// The provider value is copied only into a null column, and only when it is unambiguous: it must not be
		// shared by two provider accounts nor already held by another row, or the unique index would reject it.
		// A differing value is left for the assignment to report as a conflict.
		value := account.ExternalID
		if value != "" && !row.HasCorrelationId() && providerCorrelation[value] == 1 && !takenCorrelation[value] {
			row.SetCorrelationId(value)
			takenCorrelation[value] = true
		}
		if err := s.save(ctx, row, isNew); err != nil {
			return err
		}
	}

	// An account the provider no longer lists keeps its row, but its state can no longer be vouched for.
	for reference, row := range byReference {
		if seen[reference] || row.GetState() == privatev1.BillingAccountState_BILLING_ACCOUNT_STATE_UNKNOWN {
			continue
		}
		row.SetState(privatev1.BillingAccountState_BILLING_ACCOUNT_STATE_UNKNOWN)
		if err := s.save(ctx, row, false); err != nil {
			return err
		}
	}
	return nil
}

func (s *PrivateBillingAccountsServer) save(ctx context.Context, row *privatev1.BillingAccount, isNew bool) error {
	var err error
	if isNew {
		_, err = s.dao.Create().SetObject(row).Do(ctx)
	} else {
		_, err = s.dao.Update().SetObject(row).Do(ctx)
	}
	if err != nil {
		return ConvertDAOErrorToGRPC(err, "save", "")
	}
	return nil
}

// providerAccounts reads every page of the provider account list and drops repeated accounts. Any provider failure,
// or a page token that repeats, is reported as UNAVAILABLE; the details stay out of the response and the logs.
func (s *PrivateBillingAccountsServer) providerAccounts(ctx context.Context) ([]billing.Account, error) {
	var (
		result []billing.Account
		page   = billing.Page{Size: billingDiscoveryPageSize}
		seen   = map[string]bool{}
		tokens = map[string]bool{}
	)
	for {
		accountPage, err := s.adapter.ListAccounts(ctx, billing.AccountFilter{}, page)
		if err != nil {
			var billingErr *billing.Error
			reason := "unknown"
			if errors.As(err, &billingErr) {
				reason = string(billingErr.Reason)
			}
			s.logger.WarnContext(ctx, "Failed to list the billing provider accounts", slog.String("reason", reason))
			return nil, errBillingProviderUnavailable()
		}
		for _, account := range accountPage.Accounts {
			if !seen[account.Ref.ID()] {
				seen[account.Ref.ID()] = true
				result = append(result, account)
			}
		}
		if accountPage.Next == "" {
			return result, nil
		}
		if tokens[accountPage.Next] {
			s.logger.WarnContext(ctx, "The billing provider repeated a page token")
			return nil, errBillingProviderUnavailable()
		}
		tokens[accountPage.Next] = true
		page.Token = accountPage.Next
	}
}

// storedAccounts reads every stored account, whatever its provider.
func (s *PrivateBillingAccountsServer) storedAccounts(ctx context.Context) ([]*privatev1.BillingAccount, error) {
	var result []*privatev1.BillingAccount
	for offset := int32(0); ; offset += billingRowsPageSize {
		response, err := s.dao.List().SetOffset(offset).SetLimit(billingRowsPageSize).Do(ctx)
		if err != nil {
			return nil, ConvertDAOErrorToGRPC(err, "list", "")
		}
		result = append(result, response.GetItems()...)
		if len(result) >= int(response.GetTotal()) || len(response.GetItems()) == 0 {
			return result, nil
		}
	}
}

// billingAccountState maps the state the adapter normalized. Anything but the two known states, including an empty
// value, is UNKNOWN: it is never promoted to ACTIVE.
func billingAccountState(state billing.AccountState) privatev1.BillingAccountState {
	switch state {
	case billing.AccountActive:
		return privatev1.BillingAccountState_BILLING_ACCOUNT_STATE_ACTIVE
	case billing.AccountInactive:
		return privatev1.BillingAccountState_BILLING_ACCOUNT_STATE_INACTIVE
	default:
		return privatev1.BillingAccountState_BILLING_ACCOUNT_STATE_UNKNOWN
	}
}
