/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
*/

package auth

import (
	"context"

	"github.com/golang-jwt/jwt/v5"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/osac-project/osac/fulfillment-service/internal/serviceaccounts"
)

var _ = Describe("Metering assignment authorization", func() {
	const issuer = "https://kubernetes.example.test"
	const subject = "system:serviceaccount:osac:osac-metering"
	const audience = "osac-metering-attribution"
	const method = "/osac.private.v1.TenantBillingAssignments/GetActiveBillingAccount"

	It("grants cross-tenant read only to validated exact issuer subject and audience", func() {
		evaluator, err := NewEvaluator().SetMeteringIdentity(issuer, subject, audience).Build()
		Expect(err).NotTo(HaveOccurred())
		for _, tc := range []struct {
			name, iss, sub string
			aud            any
			valid, allowed bool
		}{
			{"dedicated", issuer, subject, []string{audience}, true, true},
			{"scalar audience", issuer, subject, audience, true, true},
			{"other issuer", "https://other.example.test", subject, audience, true, false},
			{"lookalike issuer", issuer + "/other", subject, audience, true, false},
			{"other namespace", issuer, "system:serviceaccount:other:osac-metering", audience, true, false},
			{"other SA", issuer, "system:serviceaccount:osac:adapter", audience, true, false},
			{"default audience", issuer, subject, "https://kubernetes.default.svc", true, false},
			{"missing audience", issuer, subject, nil, true, false},
			{"unvalidated", issuer, subject, audience, false, false},
			{"emergency admin", issuer, "system:serviceaccount:osac:admin", audience, true, false},
		} {
			claims := jwt.MapClaims{"iss": tc.iss, "sub": tc.sub, "aud": tc.aud, "kubernetes.io": map[string]any{}}
			token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
			token.Valid = tc.valid
			identity, extractErr := ExtractAuthContext(token)
			Expect(extractErr).NotTo(HaveOccurred())
			decision, evalErr := evaluator.Evaluate(context.Background(), identity, method)
			Expect(evalErr).NotTo(HaveOccurred(), tc.name)
			Expect(decision.Allowed).To(Equal(tc.allowed), tc.name)
			if tc.allowed {
				Expect(decision.SubjectTenants).To(Equal([]string{"*"}))
				adjacent, adjacentErr := evaluator.Evaluate(context.Background(), identity,
					"/osac.private.v1.TenantBillingAssignments/Get")
				Expect(adjacentErr).NotTo(HaveOccurred())
				Expect(adjacent.Allowed).To(BeFalse())
			}
		}
	})

	It("denies ordinary administrators and fails closed without configured identity", func() {
		for _, configured := range []bool{false, true} {
			builder := NewEvaluator().AddEmergencyServiceAccounts([]string{"admin", "osac-metering"})
			if configured {
				builder.SetMeteringIdentity(issuer, subject, audience)
			}
			evaluator, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			for _, user := range []string{"service-account-osac-admin", "service-account-osac-controller", "osac-metering"} {
				identity := &AuthContext{AuthMethod: "jwt", Username: user, Groups: []any{"admins"}}
				decision, evalErr := evaluator.Evaluate(context.Background(), identity, method)
				Expect(evalErr).NotTo(HaveOccurred())
				Expect(decision.Allowed).To(BeFalse())
				decision, evalErr = evaluator.Evaluate(context.Background(), identity, "/osac.private.v1.Clusters/List")
				Expect(evalErr).NotTo(HaveOccurred())
				Expect(decision.Allowed).To(BeTrue())
			}
		}
	})
	It("denies managed role grants before resolving RoleBindings", func() {
		called := false
		evaluator, err := NewEvaluator().SetMeteringIdentity(issuer, subject, audience).
			SetManagedServiceAccountResolver(func(context.Context, string, string) (*serviceaccounts.AuthorizationIdentity, error) {
				called = true
				return &serviceaccounts.AuthorizationIdentity{ClientID: "managed", Tenant: "tenant-a", Roles: []string{"tenant-admin", "osac-csi-driver"}}, nil
			}).
			SetTenantResolver(func(context.Context, []string) ([]string, error) { called = true; return nil, nil }).Build()
		Expect(err).NotTo(HaveOccurred())
		decision, err := evaluator.Evaluate(context.Background(), &AuthContext{
			AuthMethod: "jwt", Username: "service-account-osac-admin", Groups: []any{"admins"},
			ManagedServiceAccountCandidate: true, ManagedServiceAccountClaimsValid: true,
			ManagedServiceAccountValid: true, ManagedServiceAccountRoles: []string{"tenant-admin", "osac-csi-driver"},
		}, method)
		Expect(err).NotTo(HaveOccurred())
		Expect(decision.Allowed).To(BeFalse())
		Expect(called).To(BeFalse())
	})
})
