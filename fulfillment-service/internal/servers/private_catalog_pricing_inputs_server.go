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

	"github.com/google/cel-go/cel"
	"github.com/prometheus/client_golang/prometheus"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/osac-project/osac/fulfillment-service/internal/auth"
	"github.com/osac-project/osac/fulfillment-service/internal/database/dao"
	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
)

// fetchAllPagesLimit is the page size used while fetching every catalog item for aggregation, equal to the DAO's
// default max limit.
const fetchAllPagesLimit = 1000

// catalogPricingInputFilterEnv is the CEL environment used to evaluate the optional 'filter' request field against
// each aggregated (service, dimension, value) row, following the 'this' convention documented in docs/FILTER.md.
// Built once at package scope: the message type is fixed, so there's nothing per-request to configure. This mirrors
// the in-memory CEL evaluation EventsServer already uses for its Watch filter, applied here to a derived,
// non-DAO-backed list instead of a DAO List().
var catalogPricingInputFilterEnv = func() *cel.Env {
	var model *privatev1.CatalogPricingInput
	desc := model.ProtoReflect().Descriptor()
	env, err := cel.NewEnv(
		cel.Types(model),
		cel.Variable("this", cel.ObjectType(string(desc.FullName()))),
	)
	if err != nil {
		panic(fmt.Sprintf("failed to create catalog pricing input filter CEL environment: %v", err))
	}
	return env
}()

// filterCatalogPricingInputs returns the subset of items for which the CEL filter expression evaluates to true. An
// empty filter matches every item.
func filterCatalogPricingInputs(items []*privatev1.CatalogPricingInput,
	filter string) ([]*privatev1.CatalogPricingInput, error) {
	if filter == "" {
		return items, nil
	}
	tree, issues := catalogPricingInputFilterEnv.Compile(filter)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	program, err := catalogPricingInputFilterEnv.Program(tree)
	if err != nil {
		return nil, err
	}
	result := make([]*privatev1.CatalogPricingInput, 0, len(items))
	for _, item := range items {
		value, _, err := program.Eval(map[string]any{"this": item})
		if err != nil {
			return nil, err
		}
		accepted, ok := value.Value().(bool)
		if !ok {
			return nil, fmt.Errorf("filter result must be a boolean, but it is of type '%T'", value.Value())
		}
		if accepted {
			result = append(result, item)
		}
	}
	return result, nil
}

// PrivateCatalogPricingInputsServerBuilder contains the data and logic needed to create a new private catalog
// pricing inputs server.
type PrivateCatalogPricingInputsServerBuilder struct {
	logger            *slog.Logger
	tenancyLogic      auth.TenancyLogic
	metricsRegisterer prometheus.Registerer
}

var _ privatev1.CatalogPricingInputsServer = (*PrivateCatalogPricingInputsServer)(nil)

// PrivateCatalogPricingInputsServer lists the distinct metered (service, dimension, value) inventory derived from
// the VMaaS and CaaS catalog item DAOs. It never calls the billing provider: it has no dependency capable of doing
// so.
type PrivateCatalogPricingInputsServer struct {
	privatev1.UnimplementedCatalogPricingInputsServer
	computeInstanceCatalogItemsDao *dao.GenericDAO[*privatev1.ComputeInstanceCatalogItem]
	clusterCatalogItemsDao         *dao.GenericDAO[*privatev1.ClusterCatalogItem]
}

// NewPrivateCatalogPricingInputsServer starts building the private catalog pricing inputs server.
func NewPrivateCatalogPricingInputsServer() *PrivateCatalogPricingInputsServerBuilder {
	return &PrivateCatalogPricingInputsServerBuilder{}
}

// SetLogger sets the logger to use. This is mandatory.
func (b *PrivateCatalogPricingInputsServerBuilder) SetLogger(value *slog.Logger) *PrivateCatalogPricingInputsServerBuilder {
	b.logger = value
	return b
}

// SetTenancyLogic sets the tenancy logic used to build the underlying catalog item DAOs. This is mandatory.
func (b *PrivateCatalogPricingInputsServerBuilder) SetTenancyLogic(value auth.TenancyLogic) *PrivateCatalogPricingInputsServerBuilder {
	b.tenancyLogic = value
	return b
}

// SetMetricsRegisterer sets the Prometheus registerer used to register DAO metrics.
func (b *PrivateCatalogPricingInputsServerBuilder) SetMetricsRegisterer(value prometheus.Registerer) *PrivateCatalogPricingInputsServerBuilder {
	b.metricsRegisterer = value
	return b
}

// Build fails when the logger or tenancy logic is missing.
func (b *PrivateCatalogPricingInputsServerBuilder) Build() (result *PrivateCatalogPricingInputsServer, err error) {
	if b.logger == nil {
		err = errors.New("logger is mandatory")
		return
	}
	if b.tenancyLogic == nil {
		err = errors.New("tenancy logic is mandatory")
		return
	}
	computeInstanceCatalogItemsDao, err := dao.NewGenericDAO[*privatev1.ComputeInstanceCatalogItem]().
		SetLogger(b.logger).
		SetTenancyLogic(b.tenancyLogic).
		SetMetricsRegisterer(b.metricsRegisterer).
		Build()
	if err != nil {
		return
	}
	clusterCatalogItemsDao, err := dao.NewGenericDAO[*privatev1.ClusterCatalogItem]().
		SetLogger(b.logger).
		SetTenancyLogic(b.tenancyLogic).
		SetMetricsRegisterer(b.metricsRegisterer).
		Build()
	if err != nil {
		return
	}
	result = &PrivateCatalogPricingInputsServer{
		computeInstanceCatalogItemsDao: computeInstanceCatalogItemsDao,
		clusterCatalogItemsDao:         clusterCatalogItemsDao,
	}
	return
}

// List fetches every draft and published VMaaS and CaaS catalog item, aggregates the distinct (service, dimension,
// value) inventory, applies the request's optional CEL filter, and applies the offset/limit to the resulting
// deterministic list. 'order' is accepted for parity with other list requests but, like everywhere else in this
// API, is not yet implemented (see docs/API.md).
func (s *PrivateCatalogPricingInputsServer) List(ctx context.Context,
	request *privatev1.CatalogPricingInputsListRequest) (*privatev1.CatalogPricingInputsListResponse, error) {
	computeItems, err := fetchAllPages(ctx, s.computeInstanceCatalogItemsDao)
	if err != nil {
		return nil, err
	}
	clusterItems, err := fetchAllPages(ctx, s.clusterCatalogItemsDao)
	if err != nil {
		return nil, err
	}
	all := aggregateCatalogPricingInputs(computeItems, clusterItems)

	all, err = filterCatalogPricingInputs(all, request.GetFilter())
	if err != nil {
		return nil, grpcstatus.Errorf(grpccodes.InvalidArgument, "invalid filter: %s", err)
	}

	total := int32(len(all)) // #nosec G115 -- bounded by catalog size
	offset := max(request.GetOffset(), 0)
	limit := request.GetLimit()
	if limit <= 0 {
		limit = total
	}
	start := min(offset, total)
	end := min(start+limit, total)
	page := all[start:end]

	return privatev1.CatalogPricingInputsListResponse_builder{
		Size:  int32(len(page)), // #nosec G115 -- bounded by catalog size
		Total: total,
		Items: page,
	}.Build(), nil
}

// fetchAllPages pages through a catalog item DAO until every item, published and draft, has been fetched.
func fetchAllPages[O dao.Object](ctx context.Context, d *dao.GenericDAO[O]) ([]O, error) {
	var all []O
	var offset int32
	for {
		response, err := d.List().SetOffset(offset).SetLimit(fetchAllPagesLimit).Do(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, response.GetItems()...)
		offset += response.GetSize()
		if response.GetSize() == 0 || offset >= response.GetTotal() {
			break
		}
	}
	return all, nil
}
