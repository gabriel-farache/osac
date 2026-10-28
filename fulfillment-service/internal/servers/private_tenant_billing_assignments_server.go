/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package servers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"buf.build/go/protovalidate"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/osac-project/osac/fulfillment-service/internal/auth"
	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	"github.com/osac-project/osac/fulfillment-service/internal/database"
	"github.com/osac-project/osac/fulfillment-service/internal/uuid"
	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
)

// Stable connection_state column values; see TenantBillingAssignmentConnectionState in the proto.
const (
	tbaStatePending   = "PENDING"
	tbaStateConnected = "CONNECTED"
	tbaStateFailed    = "FAILED"
)

// tbaAccountAlreadyBoundReason and tbaTenantAlreadyBoundReason are the stable reasons Create/Update report when
// the account or the tenant already has an active assignment. Neither appears anywhere in the OSAC-3784 design
// document; they come from this story's acceptance criteria ("ACCOUNT_ALREADY_BOUND (or the tenant-side
// equivalent)"), which name only the account-side reason. tbaTenantAlreadyBoundReason is this implementation's
// choice for the unnamed tenant-side equivalent.
const (
	tbaAccountAlreadyBoundReason = "ACCOUNT_ALREADY_BOUND"
	tbaTenantAlreadyBoundReason  = "TENANT_ALREADY_BOUND"
	tbaAccountInactiveReason     = "ACCOUNT_INACTIVE"
	tbaAccountStateUnknownReason = "ACCOUNT_STATE_UNKNOWN"
)

// tbaMaxUniquenessPages bounds how many pages the post-resolution provider listing reads, mirroring
// correlation.go's own bound for the same reason: a provider with more pages than this stops being scanned
// rather than looping forever.
const tbaMaxUniquenessPages = 50

// PrivateTenantBillingAssignmentsServerBuilder builds a PrivateTenantBillingAssignmentsServer.
type PrivateTenantBillingAssignmentsServerBuilder struct {
	logger   *slog.Logger
	provider *billing.Provider
}

var _ privatev1.TenantBillingAssignmentsServer = (*PrivateTenantBillingAssignmentsServer)(nil)

// PrivateTenantBillingAssignmentsServer reads and changes tenant/billing-account assignments.
type PrivateTenantBillingAssignmentsServer struct {
	privatev1.UnimplementedTenantBillingAssignmentsServer
	logger    *slog.Logger
	provider  *billing.Provider
	validator protovalidate.Validator
}

func NewPrivateTenantBillingAssignmentsServer() *PrivateTenantBillingAssignmentsServerBuilder {
	return &PrivateTenantBillingAssignmentsServerBuilder{}
}

// SetLogger sets the logger to use. This is mandatory.
func (b *PrivateTenantBillingAssignmentsServerBuilder) SetLogger(
	value *slog.Logger) *PrivateTenantBillingAssignmentsServerBuilder {
	b.logger = value
	return b
}

// SetProvider sets the billing provider. This is mandatory.
func (b *PrivateTenantBillingAssignmentsServerBuilder) SetProvider(
	value *billing.Provider) *PrivateTenantBillingAssignmentsServerBuilder {
	b.provider = value
	return b
}

func (b *PrivateTenantBillingAssignmentsServerBuilder) Build() (
	result *PrivateTenantBillingAssignmentsServer, err error) {
	if b.logger == nil {
		err = errors.New("logger is mandatory")
		return
	}
	if b.provider == nil {
		err = errors.New("provider is mandatory")
		return
	}
	validator, err := protovalidate.New()
	if err != nil {
		err = fmt.Errorf("failed to create protovalidate validator: %w", err)
		return
	}
	result = &PrivateTenantBillingAssignmentsServer{logger: b.logger, provider: b.provider, validator: validator}
	return
}

// List returns every assignment matching the request's filter/order/pagination. Only Cloud Provider Admins reach
// this method (design.md §4.3/§4.7; enforced by authz.rego's is_admin catch-all, no tenant self-service path).
func (s *PrivateTenantBillingAssignmentsServer) List(ctx context.Context,
	request *privatev1.TenantBillingAssignmentsListRequest) (
	response *privatev1.TenantBillingAssignmentsListResponse, err error) {
	if err = requireBilling(s.provider); err != nil {
		return nil, err
	}

	tx, err := database.TxFromContext(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.ReportError(&err)

	limit := int32(math.MaxInt32)
	if request.HasLimit() {
		limit = request.GetLimit()
	}
	rows, total, err := database.ListTenantBillingAssignments(
		ctx, tx, request.GetOffset(), limit, request.GetFilter(), request.GetOrder())
	if err != nil {
		var invalid *database.ErrInvalidTenantBillingAssignmentsQuery
		if errors.As(err, &invalid) {
			return nil, status.Errorf(codes.InvalidArgument, "%v", err)
		}
		s.logger.ErrorContext(ctx, "Failed to list tenant billing assignments", slog.Any("error", err))
		return nil, status.Errorf(codes.Internal, "failed to list tenant billing assignments")
	}
	return buildTenantBillingAssignmentsListResponse(rows, total), nil
}

// Get returns one assignment interval. Only Cloud Provider Admins reach this method (design.md §4.3/§4.7;
// enforced by authz.rego's is_admin catch-all, no tenant self-service path).
func (s *PrivateTenantBillingAssignmentsServer) Get(ctx context.Context,
	request *privatev1.TenantBillingAssignmentsGetRequest) (
	response *privatev1.TenantBillingAssignmentsGetResponse, err error) {
	if err = requireBilling(s.provider); err != nil {
		return nil, err
	}

	tx, err := database.TxFromContext(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.ReportError(&err)

	row, err := database.GetTenantBillingAssignment(ctx, tx, request.GetId())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "tenant billing assignment %q not found", request.GetId())
		}
		s.logger.ErrorContext(ctx, "Failed to get tenant billing assignment", slog.Any("error", err))
		return nil, status.Errorf(codes.Internal, "failed to get tenant billing assignment")
	}

	return privatev1.TenantBillingAssignmentsGetResponse_builder{
		Object: toTenantBillingAssignmentProto(row),
	}.Build(), nil
}

// Create assigns a tenant to a billing account for the first time.
func (s *PrivateTenantBillingAssignmentsServer) Create(ctx context.Context,
	request *privatev1.TenantBillingAssignmentsCreateRequest) (
	response *privatev1.TenantBillingAssignmentsCreateResponse, err error) {
	if err = requireBilling(s.provider); err != nil {
		return nil, err
	}

	object := request.GetObject()
	if object.GetSpec() == nil {
		return nil, status.Errorf(codes.InvalidArgument, "spec is required")
	}
	if err = s.validator.Validate(object); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	tx, err := database.TxFromContext(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.ReportError(&err)

	tenantID := object.GetSpec().GetTenantId()
	accountID := object.GetSpec().GetBillingAccountId()

	if bindErr := s.rejectIfAlreadyBound(ctx, tx, tenantID, accountID); bindErr != nil {
		return nil, bindErr
	}

	account, err := database.GetBillingAccountForAssignment(ctx, tx, accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Errorf(codes.InvalidArgument, "billing account %q does not exist", accountID)
		}
		s.logger.ErrorContext(ctx, "Failed to read billing account", slog.Any("error", err))
		return nil, status.Errorf(codes.Internal, "failed to read billing account")
	}
	if stateErr := requireActiveBillingAccount(account); stateErr != nil {
		return nil, stateErr
	}

	adapter, err := s.provider.Adapter()
	if err != nil {
		return nil, mapBillingProviderBuildError(err)
	}

	row, err := s.assign(ctx, tx, adapter, "", tenantBillingAssignmentAssignInput{
		Name: object.GetMetadata().GetName(), TenantID: tenantID, Creator: auth.SubjectFromContext(ctx).User,
	}, account)
	if err != nil {
		return nil, mapTenantBillingAssignmentError(err)
	}

	return privatev1.TenantBillingAssignmentsCreateResponse_builder{
		Object: toTenantBillingAssignmentProto(row),
	}.Build(), nil
}

// Update replaces the tenant's active account, or re-validates the current one. Closes the previous interval and
// opens a new one in one transaction, except: when the account is unchanged and the current interval is already
// CONNECTED, it is a no-op that returns the current interval.
func (s *PrivateTenantBillingAssignmentsServer) Update(ctx context.Context,
	request *privatev1.TenantBillingAssignmentsUpdateRequest) (
	response *privatev1.TenantBillingAssignmentsUpdateResponse, err error) {
	if err = requireBilling(s.provider); err != nil {
		return nil, err
	}

	object := request.GetObject()
	if object.GetSpec() == nil {
		return nil, status.Errorf(codes.InvalidArgument, "spec is required")
	}
	if err = s.validator.Validate(object); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	id := object.GetId()
	if id == "" {
		return nil, status.Errorf(codes.InvalidArgument, "id is required")
	}

	tx, err := database.TxFromContext(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.ReportError(&err)

	existing, err := database.GetTenantBillingAssignment(ctx, tx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "tenant billing assignment %q not found", id)
		}
		s.logger.ErrorContext(ctx, "Failed to get tenant billing assignment", slog.Any("error", err))
		return nil, status.Errorf(codes.Internal, "failed to get tenant billing assignment")
	}
	if existing.EffectiveTo != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"tenant billing assignment %q is no longer the current interval", id)
	}

	// spec.tenant_id is a required, non-empty field (enforced by validator.Validate above), so this is a real
	// mismatch whenever it triggers, never an "omitted" case.
	if object.GetSpec().GetTenantId() != existing.TenantID {
		return nil, status.Errorf(codes.InvalidArgument, "spec.tenant_id is immutable and cannot be changed by Update")
	}

	accountID := object.GetSpec().GetBillingAccountId()

	if accountID == existing.BillingAccountID {
		if existing.ConnectionState == tbaStateConnected {
			return privatev1.TenantBillingAssignmentsUpdateResponse_builder{
				Object: toTenantBillingAssignmentProto(existing),
			}.Build(), nil
		}
	} else if bindErr := s.rejectIfAccountAlreadyBound(ctx, tx, accountID); bindErr != nil {
		return nil, bindErr
	}

	account, err := database.GetBillingAccountForAssignment(ctx, tx, accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Errorf(codes.InvalidArgument, "billing account %q does not exist", accountID)
		}
		s.logger.ErrorContext(ctx, "Failed to read billing account", slog.Any("error", err))
		return nil, status.Errorf(codes.Internal, "failed to read billing account")
	}
	if stateErr := requireActiveBillingAccount(account); stateErr != nil {
		return nil, stateErr
	}

	adapter, err := s.provider.Adapter()
	if err != nil {
		return nil, mapBillingProviderBuildError(err)
	}

	row, err := s.assign(ctx, tx, adapter, existing.ID, tenantBillingAssignmentAssignInput{
		Name: existing.Name, TenantID: existing.TenantID, Creator: existing.Creator,
	}, account)
	if err != nil {
		return nil, mapTenantBillingAssignmentError(err)
	}

	return privatev1.TenantBillingAssignmentsUpdateResponse_builder{
		Object: toTenantBillingAssignmentProto(row),
	}.Build(), nil
}

// rejectIfAlreadyBound fails with ACCOUNT_ALREADY_BOUND or TENANT_ALREADY_BOUND when tenantID or accountID
// already has an active assignment. Used only by Create: Update's "account changing" path only needs the
// account-side check, since the tenant side is the assignment being updated.
func (s *PrivateTenantBillingAssignmentsServer) rejectIfAlreadyBound(
	ctx context.Context, tx database.Tx, tenantID, accountID string) error {
	if _, err := database.GetActiveTenantBillingAssignmentByTenant(ctx, tx, tenantID); err == nil {
		return failedPreconditionWithReason(tbaTenantAlreadyBoundReason,
			"tenant %q already has an active billing assignment", tenantID)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		s.logger.ErrorContext(ctx, "Failed to check tenant billing assignment", slog.Any("error", err))
		return status.Errorf(codes.Internal, "failed to check tenant billing assignment")
	}
	return s.rejectIfAccountAlreadyBound(ctx, tx, accountID)
}

func (s *PrivateTenantBillingAssignmentsServer) rejectIfAccountAlreadyBound(
	ctx context.Context, tx database.Tx, accountID string) error {
	if _, err := database.GetActiveTenantBillingAssignmentByAccount(ctx, tx, accountID); err == nil {
		return failedPreconditionWithReason(tbaAccountAlreadyBoundReason,
			"billing account %q is already linked to a tenant", accountID)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		s.logger.ErrorContext(ctx, "Failed to check billing account assignment", slog.Any("error", err))
		return status.Errorf(codes.Internal, "failed to check billing account assignment")
	}
	return nil
}

func requireActiveBillingAccount(account database.BillingAccountForAssignment) error {
	switch billing.AccountState(account.State) {
	case billing.AccountActive:
		return nil
	case billing.AccountInactive:
		return failedPreconditionWithReason(tbaAccountInactiveReason, "billing account %q is inactive", account.ID)
	default:
		return failedPreconditionWithReason(tbaAccountStateUnknownReason,
			"billing account %q has an unknown state", account.ID)
	}
}

// tenantBillingAssignmentAssignInput carries the fields of the assignment interval assign is about to open.
type tenantBillingAssignmentAssignInput struct {
	Name     string
	TenantID string
	Creator  string
}

// assign is the correlation-resolution critical section shared by Create and Update. It runs entirely on the
// caller's ambient per-request transaction tx: it acquires a per-account lock before calling the OSAC-6885
// resolver, so the lock is held through the final write and the request's eventual commit. The resolver's
// provider I/O (CorrelationValue, SetCorrelationValue, its own internal uniqueness check) and the post-resolution
// provider listing below never touch tx - only billing.AccountCorrelationInput.LocalDuplicate does, by design:
// correlation.go's own doc comment calls for the local-uniqueness check to "participate in the same lock/commit
// scope as the eventual write", i.e. tx, not a separate transaction.
func (s *PrivateTenantBillingAssignmentsServer) assign(
	ctx context.Context, tx database.Tx, adapter billing.Adapter, closeID string,
	in tenantBillingAssignmentAssignInput, account database.BillingAccountForAssignment,
) (database.TenantBillingAssignmentRow, error) {
	if err := database.LockBillingAccountForAssignment(ctx, tx, account.ID); err != nil {
		return database.TenantBillingAssignmentRow{}, err
	}

	ref := billing.NewAccountRef(account.ProviderAccountReference)
	resolution, err := billing.ResolveAccountCorrelationID(ctx, billing.AccountCorrelationInput{
		Adapter: adapter, AccountID: account.ID, Ref: ref, LocalValue: account.CorrelationID,
		LocalDuplicate: func(dupCtx context.Context, value, excludeAccountID string) (bool, error) {
			return database.BillingAccountCorrelationConflict(dupCtx, tx, value, excludeAccountID)
		},
	})
	if err != nil {
		return database.TenantBillingAssignmentRow{}, err
	}

	matches, err := countTenantBillingAssignmentProviderMatches(ctx, adapter, resolution.Value)
	if err != nil {
		return database.TenantBillingAssignmentRow{}, err
	}

	var state, message string
	switch matches {
	case 1:
		state = tbaStateConnected
	case 0:
		state = tbaStatePending
		if resolution.ProviderWriteSkippedReason != "" {
			message = resolution.ProviderWriteSkippedReason +
				": set externalId on the account in the provider to the correlation_id shown by BillingAccounts.List"
		} else {
			message = "no provider account currently carries the resolved correlation id; set it on the " +
				"account in the provider to the value shown by BillingAccounts.List"
		}
	default:
		return database.TenantBillingAssignmentRow{}, &billing.CorrelationResolutionError{
			Reason: billing.ReasonAccountAmbiguous,
		}
	}

	if err := database.SetBillingAccountCorrelationID(ctx, tx, account.ID, resolution.Value); err != nil {
		return database.TenantBillingAssignmentRow{}, err
	}
	newID, err := database.CloseAndInsertTenantBillingAssignment(ctx, tx, closeID,
		database.TenantBillingAssignmentInsert{
			ID: uuid.New(), Name: in.Name, TenantID: in.TenantID, BillingAccountID: account.ID,
			ConnectionState: state, Message: message, Creator: in.Creator,
		})
	if err != nil {
		return database.TenantBillingAssignmentRow{}, err
	}
	return database.GetTenantBillingAssignment(ctx, tx, newID)
}

// countTenantBillingAssignmentProviderMatches lists provider accounts carrying value, paging like
// correlation.go's own checkProviderUnique, and returns the total count (not merely whether it exceeds one): the
// caller distinguishes zero (PENDING), one (CONNECTED) and more than one (ACCOUNT_AMBIGUOUS). This is a
// deliberate re-run of a similar check the resolver already performs internally before any provider write - see
// ResolveAccountCorrelationID's doc comment - because this one is the connection-state decision over the final,
// post-write value, not a re-validation of the resolver's own success.
func countTenantBillingAssignmentProviderMatches(ctx context.Context, adapter billing.Adapter, value string) (
	int, error) {
	count := 0
	page := billing.Page{}
	for i := 0; i < tbaMaxUniquenessPages; i++ {
		result, err := adapter.ListAccounts(ctx, billing.AccountFilter{ExternalID: value}, page)
		if err != nil {
			return 0, err
		}
		count += len(result.Accounts)
		if result.Next == "" {
			return count, nil
		}
		page.Token = result.Next
	}
	return count, nil
}

func buildTenantBillingAssignmentsListResponse(
	rows []database.TenantBillingAssignmentRow, total int32) *privatev1.TenantBillingAssignmentsListResponse {
	items := make([]*privatev1.TenantBillingAssignment, 0, len(rows))
	for _, row := range rows {
		items = append(items, toTenantBillingAssignmentProto(row))
	}
	return privatev1.TenantBillingAssignmentsListResponse_builder{
		Size:  int32(len(items)), // #nosec G115 -- bounded by page size
		Total: total,
		Items: items,
	}.Build()
}

func toTenantBillingAssignmentProto(row database.TenantBillingAssignmentRow) *privatev1.TenantBillingAssignment {
	statusBuilder := privatev1.TenantBillingAssignmentStatus_builder{
		ConnectionState: mapTenantBillingAssignmentConnectionState(row.ConnectionState),
		EffectiveFrom:   timestamppb.New(row.EffectiveFrom),
		Message:         row.Message,
	}
	if row.EffectiveTo != nil {
		statusBuilder.EffectiveTo = timestamppb.New(*row.EffectiveTo)
	}
	return privatev1.TenantBillingAssignment_builder{
		Id: row.ID,
		Metadata: privatev1.Metadata_builder{
			Name:   row.Name,
			Tenant: row.TenantID,
		}.Build(),
		Spec: privatev1.TenantBillingAssignmentSpec_builder{
			TenantId:         row.TenantID,
			BillingAccountId: row.BillingAccountID,
		}.Build(),
		Status: statusBuilder.Build(),
	}.Build()
}

func mapTenantBillingAssignmentConnectionState(state string) privatev1.TenantBillingAssignmentConnectionState {
	switch state {
	case tbaStatePending:
		return privatev1.TenantBillingAssignmentConnectionState_TENANT_BILLING_ASSIGNMENT_CONNECTION_STATE_PENDING
	case tbaStateConnected:
		return privatev1.TenantBillingAssignmentConnectionState_TENANT_BILLING_ASSIGNMENT_CONNECTION_STATE_CONNECTED
	case tbaStateFailed:
		return privatev1.TenantBillingAssignmentConnectionState_TENANT_BILLING_ASSIGNMENT_CONNECTION_STATE_FAILED
	default:
		return privatev1.TenantBillingAssignmentConnectionState_TENANT_BILLING_ASSIGNMENT_CONNECTION_STATE_UNSPECIFIED
	}
}

// failedPreconditionWithReason builds a FAILED_PRECONDITION status carrying reason in its ErrorInfo detail, the
// shape every billing failure in this story uses.
func failedPreconditionWithReason(reason, format string, args ...any) error {
	result := status.New(codes.FailedPrecondition, fmt.Sprintf(format, args...))
	if detailed, err := result.WithDetails(&errdetails.ErrorInfo{Reason: reason}); err == nil {
		result = detailed
	}
	return result.Err()
}

// mapBillingProviderBuildError maps a configured-but-unbuildable provider (e.g. an unreadable credential Secret)
// the same way private_billing_accounts_server.go maps a provider that cannot be reached: UNAVAILABLE, never a
// silently empty result.
func mapBillingProviderBuildError(err error) error {
	var provErr *billing.Error
	if errors.As(err, &provErr) {
		return mapTenantBillingAssignmentProviderError(provErr)
	}
	result := status.New(codes.Unavailable, "billing provider account API is unavailable")
	if detailed, derr := result.WithDetails(&errdetails.ErrorInfo{Reason: billingProviderUnavailableReason}); derr == nil {
		result = detailed
	}
	return result.Err()
}

// mapTenantBillingAssignmentProviderError classifies a *billing.Error from provider I/O: UNAVAILABLE/TIMEOUT map
// to gRPC UNAVAILABLE, everything else surfaces the provider's own reason as FAILED_PRECONDITION detail.
func mapTenantBillingAssignmentProviderError(err *billing.Error) error {
	if err.Reason == billing.ReasonUnavailable || err.Reason == billing.ReasonTimeout {
		result := status.New(codes.Unavailable, fmt.Sprintf("billing provider is unavailable: %s", err.Reason))
		if detailed, derr := result.WithDetails(&errdetails.ErrorInfo{Reason: string(err.Reason)}); derr == nil {
			result = detailed
		}
		return result.Err()
	}
	return failedPreconditionWithReason(string(err.Reason), "billing provider error: %s", err.Reason)
}

// mapTenantBillingAssignmentError maps every error assign can return to a gRPC status: a resolution-specific
// *billing.CorrelationResolutionError, the DAO's correlation-conflict sentinel, a classified provider *billing.Error,
// a race on the 1:1 partial unique indexes (the early rejectIfAlreadyBound checks are a fast path, not the guard;
// a concurrent request can still lose the race at this final insert), or an infrastructure error.
func mapTenantBillingAssignmentError(err error) error {
	var resErr *billing.CorrelationResolutionError
	if errors.As(err, &resErr) {
		return failedPreconditionWithReason(resErr.Reason, "billing correlation resolution failed: %s", resErr.Reason)
	}
	if errors.Is(err, database.ErrBillingAccountCorrelationConflict) {
		return failedPreconditionWithReason(billing.ReasonAccountCorrelationConflict,
			"billing account correlation id is already in use by another account")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.ConstraintName {
		case "tenant_billing_assignments_unique_active_tenant":
			return failedPreconditionWithReason(tbaTenantAlreadyBoundReason,
				"tenant already has an active billing assignment")
		case "tenant_billing_assignments_unique_active_account":
			return failedPreconditionWithReason(tbaAccountAlreadyBoundReason,
				"billing account is already linked to a tenant")
		}
	}
	var provErr *billing.Error
	if errors.As(err, &provErr) {
		return mapTenantBillingAssignmentProviderError(provErr)
	}
	return status.Errorf(codes.Internal, "failed to assign billing account: %v", err)
}
