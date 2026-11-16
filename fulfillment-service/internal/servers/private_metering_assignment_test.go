/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
*/

package servers

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"github.com/golang-jwt/jwt/v5"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/osac-project/osac/fulfillment-service/internal/auth"
	"github.com/osac-project/osac/fulfillment-service/internal/billing"
	"github.com/osac-project/osac/fulfillment-service/internal/collections"
	"github.com/osac-project/osac/fulfillment-service/internal/database"
	kubefiles "github.com/osac-project/osac/fulfillment-service/internal/kubernetes/files"
	test "github.com/osac-project/osac/fulfillment-service/internal/testing"
	"github.com/osac-project/osac/fulfillment-service/internal/trust"
	"github.com/osac-project/osac/fulfillment-service/internal/uuid"
	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
	publicv1 "github.com/osac-project/osac/proto/gen/osac/public/v1"
)

var _ = Describe("Private metering assignment read", func() {
	var s *PrivateTenantBillingAssignmentsServer
	BeforeEach(func() {
		var err error
		s, err = NewPrivateTenantBillingAssignmentsServer().SetLogger(logger).SetProvider(billing.NewProvider("", nil, nil)).Build()
		Expect(err).NotTo(HaveOccurred())
		ctx = auth.ContextWithSubject(ctx, &auth.Subject{User: "osac-metering", Tenants: auth.AllTenants})
	})
	seed := func(tenant, externalID, state string) string {
		_, err := suiteTx.Exec(ctx, `insert into tenants(id,name,tenant,creator,data) values ($1,$1,$1,'system','{}')
			on conflict do nothing`, tenant)
		Expect(err).NotTo(HaveOccurred())
		id := uuid.New()
		_, err = database.UpsertBillingAccount(ctx, suiteTx, database.BillingAccountUpsert{
			ID: id, ProviderKey: "m360", ProviderAccountReference: "native-" + id,
			ExternalBillingAccountID: externalID, State: "INACTIVE", StatusObservedAt: time.Now(),
		})
		Expect(err).NotTo(HaveOccurred())
		assignment, err := database.CloseAndInsertTenantBillingAssignment(ctx, suiteTx, "", database.TenantBillingAssignmentInsert{
			ID: uuid.New(), Name: "assignment-" + tenant, TenantID: tenant, BillingAccountID: id, ConnectionState: state, Creator: "system",
		})
		Expect(err).NotTo(HaveOccurred())
		return assignment
	}
	read := func(tenant string) (*privatev1.TenantBillingAssignmentsGetActiveBillingAccountResponse, error) {
		return s.GetActiveBillingAccount(ctx, privatev1.TenantBillingAssignmentsGetActiveBillingAccountRequest_builder{TenantId: tenant}.Build())
	}
	It("projects only each tenant's connected stored ID with billing off inactive accounts and no rates", func() {
		seed(testTenant, "external-a", "CONNECTED")
		seed("other-tenant", "external-b", "CONNECTED")
		for tenant, want := range map[string]string{testTenant: "external-a", "other-tenant": "external-b"} {
			response, err := read(tenant)
			Expect(err).NotTo(HaveOccurred())
			Expect(response.GetExternalBillingAccountId()).To(Equal(want))
			Expect(response.ProtoReflect().Descriptor().Fields().Len()).To(Equal(1))
		}
	})
	DescribeTable("omits unavailable assignment data", func(state, id, lifecycle string) {
		assignment := seed(testTenant, id, state)
		switch lifecycle {
		case "closed":
			_, err := suiteTx.Exec(ctx, `update tenant_billing_assignments set effective_to=now() where id=$1`, assignment)
			Expect(err).NotTo(HaveOccurred())
		case "deleted":
			_, err := suiteTx.Exec(ctx, `update tenants set deletion_timestamp=now() where name=$1`, testTenant)
			Expect(err).NotTo(HaveOccurred())
		}
		response, err := read(testTenant)
		Expect(response).To(BeNil())
		Expect(status.Code(err)).To(Equal(codes.NotFound))
		Expect(status.Convert(err).Message()).NotTo(ContainSubstring(testTenant))
	}, Entry("pending", "PENDING", "secret-id", ""), Entry("failed", "FAILED", "secret-id", ""),
		Entry("empty ID", "CONNECTED", "", ""), Entry("closed", "CONNECTED", "secret-id", "closed"),
		Entry("deleted tenant", "CONNECTED", "secret-id", "deleted"))
	It("rejects invalid selectors and reports missing tenant generically", func() {
		for _, tenant := range []string{"", "UPPER", "a/b", "*"} {
			_, err := read(tenant)
			Expect(status.Code(err)).To(Equal(codes.InvalidArgument))
		}
		for _, tenant := range []string{"missing", "shared", "system"} {
			_, err := read(tenant)
			Expect(status.Code(err)).To(Equal(codes.NotFound))
		}
	})
	It("has no public method or messages", func() {
		file := publicv1.File_osac_public_v1_tenant_billing_assignments_service_proto
		Expect(file.Services().ByName("TenantBillingAssignments").Methods().ByName("GetActiveBillingAccount")).To(BeNil())
		for _, name := range []protoreflect.Name{"TenantBillingAssignmentsGetActiveBillingAccountRequest", "TenantBillingAssignmentsGetActiveBillingAccountResponse"} {
			Expect(file.Messages().ByName(name)).To(BeNil())
		}
		method := privatev1.File_osac_private_v1_tenant_billing_assignments_service_proto.Services().ByName("TenantBillingAssignments").Methods().ByName("GetActiveBillingAccount")
		Expect(method.Input().Fields().Len()).To(Equal(1))
		Expect(method.Output().Fields().Len()).To(Equal(1))
		Expect(proto.HasExtension(method.Options(), annotations.E_Http)).To(BeFalse())
	})
	It("sanitizes storage failure", func() {
		_, err := suiteTx.Exec(ctx, `alter table billing_accounts rename column external_billing_account_id to unavailable`)
		Expect(err).NotTo(HaveOccurred())
		_, err = read(testTenant)
		Expect(status.Code(err)).To(Equal(codes.Internal))
		Expect(status.Convert(err).Message()).To(Equal("billing attribution unavailable"))
	})
	It("fails closed instead of choosing an arbitrary account on ambiguous active intervals", func() {
		seed(testTenant, "first-external", "CONNECTED")
		// Fault injection is confined to this spec's disposable PostgreSQL database.
		_, err := suiteTx.Exec(ctx, `drop index tenant_billing_assignments_unique_active_tenant`)
		Expect(err).NotTo(HaveOccurred())
		seed(testTenant, "second-external", "CONNECTED")
		response, err := read(testTenant)
		Expect(response).To(BeNil())
		Expect(status.Code(err)).To(Equal(codes.Internal))
		Expect(status.Convert(err).Message()).To(Equal("billing attribution unavailable"))
	})
	It("rejects a scope denial before accessing storage", func() {
		ctx = auth.ContextWithSubject(context.Background(), &auth.Subject{User: "unprivileged", Tenants: collections.NewSet("other-tenant")})
		_, err := read(testTenant)
		Expect(status.Code(err)).To(Equal(codes.PermissionDenied))
	})
	It("preserves cancellation status", func() {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := s.GetActiveBillingAccount(cancelled, privatev1.TenantBillingAssignmentsGetActiveBillingAccountRequest_builder{TenantId: testTenant}.Build())
		Expect(status.Code(err)).To(Equal(codes.Canceled))
	})
	It("enforces signed JWT JWKS issuer subject audience and admin denial through real TLS authn OPA and PostgreSQL", func() {
		seed(testTenant, "private-external-value", "CONNECTED")
		Expect(suiteTx.End(ctx)).To(Succeed())
		// Subsequent RPC transactions must observe the committed assignment.
		var err error
		suiteTx, err = tm.Begin(ctx)
		Expect(err).NotTo(HaveOccurred())
		ctx = database.TxIntoContext(ctx, suiteTx)
		var issuerURL string
		issuer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/keys" {
				_ = json.NewEncoder(w).Encode(test.MakeJwksObject())
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": issuerURL + "/keys"})
		}))
		DeferCleanup(issuer.Close)
		issuerURL = issuer.URL
		root := GinkgoT().TempDir()
		file := filepath.Join(root, kubefiles.ServiceAccountToken)
		Expect(os.MkdirAll(filepath.Dir(file), 0700)).To(Succeed())
		Expect(os.WriteFile(file, []byte(test.MakeTokenString(issuerURL, "Bearer", time.Minute)), 0600)).To(Succeed())
		pool, err := trust.NewCertPool().SetLogger(logger).AddCertificate(issuer.Certificate()).Build()
		Expect(err).NotTo(HaveOccurred())
		keys, err := auth.NewJwksCache().SetLogger(logger).SetCaPool(pool).SetRoot(root).
			AddKubernetesIssuer(true).AddIssuer(issuerURL + "/other").Build()
		Expect(err).NotTo(HaveOccurred())
		Expect(auth.KubernetesIssuer(keys)).To(Equal(issuerURL))
		validator, err := auth.NewJwtValidator().SetLogger(logger).SetJwksCache(keys).Build()
		Expect(err).NotTo(HaveOccurred())
		authn, err := auth.NewGrpcAuthnInterceptor().SetLogger(logger).SetJwtValidator(validator).Build()
		Expect(err).NotTo(HaveOccurred())
		const subject = "system:serviceaccount:osac:osac-metering"
		evaluator, err := auth.NewEvaluator().SetMeteringIdentity(auth.KubernetesIssuer(keys), subject, auth.MeteringAudience).
			AddEmergencyServiceAccounts([]string{"admin", "osac-metering"}).SetTenantResolver(database.ResolveLiveTenantNames).Build()
		Expect(err).NotTo(HaveOccurred())
		authz, err := auth.NewGrpcAuthzInterceptor().SetLogger(logger).SetEvaluator(evaluator).Build()
		Expect(err).NotTo(HaveOccurred())
		txs, err := database.NewTxInterceptor().SetLogger(logger).SetManager(tm).Build()
		Expect(err).NotTo(HaveOccurred())
		cert := test.LocalhostCertificate()
		wire := test.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})),
			grpc.ChainUnaryInterceptor(txs.UnaryServer, authn.UnaryServer, authz.UnaryServer))
		privatev1.RegisterTenantBillingAssignmentsServer(wire.Registrar(), s)
		public, err := NewTenantBillingAssignmentsServer().SetLogger(logger).SetDelegate(s).Build()
		Expect(err).NotTo(HaveOccurred())
		publicv1.RegisterTenantBillingAssignmentsServer(wire.Registrar(), public)
		wire.Start()
		DeferCleanup(wire.Stop)
		certPool := x509.NewCertPool()
		parsed, err := x509.ParseCertificate(cert.Certificate[0])
		Expect(err).NotTo(HaveOccurred())
		certPool.AddCert(parsed)
		conn, err := grpc.NewClient(wire.Address(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: certPool, MinVersion: tls.VersionTLS12})))
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(conn.Close)
		client := privatev1.NewTenantBillingAssignmentsClient(conn)
		for _, tc := range []struct {
			name, iss, sub, aud           string
			expired, unsigned, oauthAdmin bool
			code                          codes.Code
		}{
			{name: "metering", iss: issuerURL, sub: subject, aud: auth.MeteringAudience, code: codes.OK},
			{name: "other trusted issuer", iss: issuerURL + "/other", sub: subject, aud: auth.MeteringAudience, code: codes.PermissionDenied},
			{name: "wrong issuer", iss: issuerURL + "/untrusted", sub: subject, aud: auth.MeteringAudience, code: codes.Unauthenticated},
			{name: "wrong audience", iss: issuerURL, sub: subject, aud: "kubernetes.default.svc", code: codes.PermissionDenied},
			{name: "wrong namespace", iss: issuerURL, sub: "system:serviceaccount:elsewhere:osac-metering", aud: auth.MeteringAudience, code: codes.PermissionDenied},
			{name: "other service account", iss: issuerURL, sub: "system:serviceaccount:osac:adapter", aud: auth.MeteringAudience, code: codes.PermissionDenied},
			{name: "ordinary administrator", iss: issuerURL, sub: "admin-subject", aud: auth.MeteringAudience, oauthAdmin: true, code: codes.PermissionDenied},
			{name: "emergency admin", iss: issuerURL, sub: "system:serviceaccount:osac:admin", aud: auth.MeteringAudience, code: codes.PermissionDenied},
			{name: "expired", iss: issuerURL, sub: subject, aud: auth.MeteringAudience, expired: true, code: codes.Unauthenticated},
			{name: "unsigned", iss: issuerURL, sub: subject, aud: auth.MeteringAudience, unsigned: true, code: codes.Unauthenticated},
		} {
			claims := jwt.MapClaims{"iss": tc.iss, "sub": tc.sub, "aud": []string{tc.aud}, "kubernetes.io": map[string]any{},
				"iat": time.Now().Add(-time.Second).Unix(), "exp": time.Now().Add(time.Minute).Unix()}
			if tc.oauthAdmin {
				delete(claims, "kubernetes.io")
				claims["preferred_username"] = "service-account-osac-admin"
				claims["groups"] = []string{"admins"}
			}
			if tc.expired {
				claims["exp"] = time.Now().Add(-time.Minute).Unix()
			}
			token := test.MakeTokenObject(nil, claims).Raw
			if tc.unsigned {
				unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
				unsigned.Header["kid"] = "123"
				token, err = unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
				Expect(err).NotTo(HaveOccurred())
			}
			call, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token)), 3*time.Second)
			response, err := client.GetActiveBillingAccount(call, privatev1.TenantBillingAssignmentsGetActiveBillingAccountRequest_builder{TenantId: testTenant}.Build())
			cancel()
			Expect(status.Code(err)).To(Equal(tc.code), tc.name)
			if tc.code == codes.OK {
				Expect(response.GetExternalBillingAccountId()).To(Equal("private-external-value"))
				publicCall, publicCancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token)), time.Second)
				var publicResponse privatev1.TenantBillingAssignmentsGetActiveBillingAccountResponse
				publicErr := conn.Invoke(publicCall, "/osac.public.v1.TenantBillingAssignments/GetActiveBillingAccount",
					privatev1.TenantBillingAssignmentsGetActiveBillingAccountRequest_builder{TenantId: testTenant}.Build(), &publicResponse)
				publicCancel()
				Expect(status.Code(publicErr)).To(Equal(codes.Unimplemented))
			}
		}
	})
})
