/*
Copyright (c) 2026 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except
in compliance with the License. You may obtain a copy of the License at
http://www.apache.org/licenses/LICENSE-2.0
*/

// Package attribution resolves optional billing metadata without gating usage.
package attribution

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"strings"
	"time"

	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const Budget = 500 * time.Millisecond

type Reason string

const (
	NotConfigured   Reason = "BILLING_ATTRIBUTION_NOT_CONFIGURED"
	Unavailable     Reason = "BILLING_ATTRIBUTION_UNAVAILABLE"
	NotFound        Reason = "BILLING_ATTRIBUTION_NOT_FOUND"
	Unauthorized    Reason = "BILLING_ATTRIBUTION_UNAUTHORIZED"
	Timeout         Reason = "BILLING_ATTRIBUTION_TIMEOUT"
	InvalidResponse Reason = "BILLING_ATTRIBUTION_INVALID_RESPONSE"
	Busy            Reason = "BILLING_ATTRIBUTION_BUSY"
)

type Resolver func(context.Context, string) (string, Reason)

type Config struct{ Address, CAFile, TokenFile string }

type Client struct {
	conn    *grpc.ClientConn
	rpc     privatev1.TenantBillingAssignmentsClient
	permits chan struct{}
	reason  Reason
}

// NewClient never connects or acquires a token. Invalid trust/configuration omits
// attribution instead of selecting insecure transport or blocking usage startup.
func NewClient(cfg Config) *Client {
	return newClient(cfg, os.ReadFile)
}

func newClient(cfg Config, readToken func(string) ([]byte, error)) *Client {
	c := &Client{reason: NotConfigured, permits: make(chan struct{}, 16)}
	if cfg.Address == "" || cfg.CAFile == "" || cfg.TokenFile == "" {
		return c
	}
	c.reason = Unavailable
	ca, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return c
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return c
	}
	conn, err := grpc.NewClient(cfg.Address,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})),
		grpc.WithPerRPCCredentials(fileToken{path: cfg.TokenFile, read: readToken}), grpc.WithDisableRetry())
	if err != nil {
		return c
	}
	c.conn, c.rpc = conn, privatev1.NewTenantBillingAssignmentsClient(conn)
	return c
}

func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// Resolve returns only fixed diagnostic reasons; transport messages may contain payloads.
func (c *Client) Resolve(ctx context.Context, tenant string) (string, Reason) {
	if c.rpc == nil {
		return "", c.reason
	}
	if tenant == "" {
		return "", InvalidResponse
	}
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	if ctx.Err() != nil {
		return "", Timeout
	}
	select {
	case c.permits <- struct{}{}:
	default:
		return "", Busy
	}
	type lookupResult struct {
		response *privatev1.TenantBillingAssignmentsGetActiveBillingAccountResponse
		err      error
	}
	result := make(chan lookupResult, 1)
	go func() {
		// Credential file I/O cannot be canceled. Keep its permit until the
		// actual RPC worker exits, even if Resolve has already timed out.
		defer func() { <-c.permits }()
		response, err := c.rpc.GetActiveBillingAccount(ctx, privatev1.TenantBillingAssignmentsGetActiveBillingAccountRequest_builder{TenantId: tenant}.Build())
		result <- lookupResult{response, err}
	}()
	var resolved lookupResult
	select {
	case <-ctx.Done():
		return "", Timeout
	case resolved = <-result:
	}
	if ctx.Err() != nil {
		return "", Timeout
	}
	if resolved.err != nil {
		switch status.Code(resolved.err) {
		case codes.NotFound:
			return "", NotFound
		case codes.Unauthenticated, codes.PermissionDenied:
			return "", Unauthorized
		case codes.Canceled, codes.DeadlineExceeded:
			return "", Timeout
		default:
			return "", Unavailable
		}
	}
	id := resolved.response.GetExternalBillingAccountId()
	if id == "" {
		return "", InvalidResponse
	}
	return id, ""
}

type fileToken struct {
	path string
	read func(string) ([]byte, error)
}

func (f fileToken) RequireTransportSecurity() bool { return true }
func (f fileToken) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	data, err := f.read(f.path)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, errors.New("attribution token unavailable")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, errors.New("attribution token unavailable")
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}
