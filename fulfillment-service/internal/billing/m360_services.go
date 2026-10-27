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

import "context"

func (m *m360) FindServiceByName(context.Context, string) (ServiceRef, error) {
	return ServiceRef{}, &Error{Reason: ReasonUnsupported, Op: "FindServiceByName"}
}

func (m *m360) ListPriceableAttributes(context.Context, ServiceRef) ([]PriceableAttribute, error) {
	return nil, &Error{Reason: ReasonUnsupported, Op: "ListPriceableAttributes"}
}

func (m *m360) GetServiceRateCard(context.Context, ServiceRef, string) (RateCard, error) {
	return RateCard{}, &Error{Reason: ReasonUnsupported, Op: "GetServiceRateCard"}
}
