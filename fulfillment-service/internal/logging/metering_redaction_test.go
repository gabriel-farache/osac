/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
*/

package logging

import (
	"bytes"
	"context"
	"log/slog"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
	"google.golang.org/grpc"
)

var _ = Describe("Metering attribution body privacy", func() {
	It("unconditionally hides private request and response without mutating either", func() {
		for _, redact := range []bool{true, false} {
			var buffer bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
			interceptor, err := NewInterceptor().SetLogger(logger).SetBodies(true).SetRedact(redact).Build()
			Expect(err).NotTo(HaveOccurred())
			request := privatev1.TenantBillingAssignmentsGetActiveBillingAccountRequest_builder{TenantId: "private-tenant-marker"}.Build()
			response := privatev1.TenantBillingAssignmentsGetActiveBillingAccountResponse_builder{ExternalBillingAccountId: "sensitive-account-marker"}.Build()
			got, err := interceptor.UnaryServer(context.Background(), request, &grpc.UnaryServerInfo{
				FullMethod: "/osac.private.v1.TenantBillingAssignments/GetActiveBillingAccount",
			}, func(context.Context, any) (any, error) { return response, nil })
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(BeIdenticalTo(response))
			Expect(request.GetTenantId()).To(Equal("private-tenant-marker"))
			Expect(response.GetExternalBillingAccountId()).To(Equal("sensitive-account-marker"))
			Expect(buffer.String()).NotTo(ContainSubstring("private-tenant-marker"))
			Expect(buffer.String()).NotTo(ContainSubstring("sensitive-account-marker"))
		}
	})
})
