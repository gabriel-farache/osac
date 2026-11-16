/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the
specific language governing permissions and limitations under the License.
*/

package restgateway

import (
	"context"
	"net/http"
	"net/http/httptest"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
)

type publicBillingAccountsClient struct {
	publicv1.BillingAccountsClient
	account *publicv1.BillingAccount
}

func (c publicBillingAccountsClient) List(context.Context, *publicv1.BillingAccountsListRequest,
	...grpc.CallOption) (*publicv1.BillingAccountsListResponse, error) {
	return publicv1.BillingAccountsListResponse_builder{Items: []*publicv1.BillingAccount{c.account}, Size: 1, Total: 1}.Build(), nil
}

type privateBillingAccountsClient struct {
	privatev1.BillingAccountsClient
	account *privatev1.BillingAccount
}

func (c privateBillingAccountsClient) List(context.Context, *privatev1.BillingAccountsListRequest,
	...grpc.CallOption) (*privatev1.BillingAccountsListResponse, error) {
	return privatev1.BillingAccountsListResponse_builder{Items: []*privatev1.BillingAccount{c.account}, Size: 1, Total: 1}.Build(), nil
}

var _ = Describe("Billing account identifier contract", func() {
	DescribeTable("preserves binary tag 7 and publishes only the normalized JSON name", func(account proto.Message) {
		wire := []byte{0x3a, 0x05, 'e', 'x', 't', '-', '1'}
		Expect(proto.Unmarshal(wire, account)).To(Succeed())
		field := account.ProtoReflect().Descriptor().Fields().ByNumber(7)
		Expect(field.Name()).To(Equal(protoreflect.Name("external_billing_account_id")))
		Expect(field.JSONName()).To(Equal("externalBillingAccountId"))
		Expect(field.Kind()).To(Equal(protoreflect.StringKind))
		encoded, err := proto.Marshal(account)
		Expect(err).ToNot(HaveOccurred())
		Expect(encoded).To(Equal(wire))
		encoded, err = protojson.Marshal(account)
		Expect(err).ToNot(HaveOccurred())
		Expect(encoded).To(MatchJSON(`{"externalBillingAccountId":"ext-1"}`))
		Expect(protojson.Unmarshal([]byte(`{"correlation_id":"old"}`), account)).To(HaveOccurred())
	}, Entry("public", &publicv1.BillingAccount{}), Entry("private", &privatev1.BillingAccount{}))

	It("serves the normalized snake-case name through both generated REST routes", func() {
		ctx := context.Background()
		mux := runtime.NewServeMux(runtime.WithMarshalerOption(runtime.MIMEWildcard,
			&runtime.JSONPb{MarshalOptions: protojson.MarshalOptions{UseProtoNames: true}}))
		public := &publicv1.BillingAccount{}
		private := &privatev1.BillingAccount{}
		wire := []byte{0x3a, 0x05, 'e', 'x', 't', '-', '1'}
		Expect(proto.Unmarshal(wire, public)).To(Succeed())
		Expect(proto.Unmarshal(wire, private)).To(Succeed())
		Expect(publicv1.RegisterBillingAccountsHandlerClient(ctx, mux,
			publicBillingAccountsClient{account: public})).To(Succeed())
		Expect(privatev1.RegisterBillingAccountsHandlerClient(ctx, mux,
			privateBillingAccountsClient{account: private})).To(Succeed())
		for _, layer := range []string{"fulfillment", "private"} {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/"+layer+"/v1/billing/accounts", nil))
			Expect(response.Code).To(Equal(http.StatusOK))
			Expect(response.Body.String()).To(MatchJSON(`{
				"items":[{"external_billing_account_id":"ext-1"}],"size":1,"total":1
			}`))
		}
	})
})
