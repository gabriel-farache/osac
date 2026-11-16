/*
Copyright (c) 2026 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except
in compliance with the License. You may obtain a copy of the License at
http://www.apache.org/licenses/LICENSE-2.0
*/

package attribution

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type assignmentServer struct {
	privatev1.UnimplementedTenantBillingAssignmentsServer
	read func(context.Context, string) (string, error)
}

func (s assignmentServer) GetActiveBillingAccount(ctx context.Context, r *privatev1.TenantBillingAssignmentsGetActiveBillingAccountRequest) (*privatev1.TenantBillingAssignmentsGetActiveBillingAccountResponse, error) {
	id, err := s.read(ctx, r.GetTenantId())
	if err != nil {
		return nil, err
	}
	return privatev1.TenantBillingAssignmentsGetActiveBillingAccountResponse_builder{ExternalBillingAccountId: id}.Build(), nil
}

func wireClient(t *testing.T, read func(context.Context, string) (string, error), readers ...func(string) ([]byte, error)) (*Client, string) {
	t.Helper()
	certServer := httptest.NewTLSServer(nil)
	cert := certServer.TLS.Certificates[0]
	certServer.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})))
	privatev1.RegisterTenantBillingAssignmentsServer(server, assignmentServer{read: read})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	dir := t.TempDir()
	caFile, tokenFile := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "token")
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: parsed.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Address: listener.Addr().String(), CAFile: caFile, TokenFile: tokenFile}
	var client *Client
	if len(readers) == 0 {
		client = NewClient(cfg)
	} else {
		client = newClient(cfg, readers[0])
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, tokenFile
}

func TestVerifiedTLSAndFileTokenRotation(t *testing.T) {
	var seen []string
	client, file := wireClient(t, func(ctx context.Context, tenant string) (string, error) {
		if tenant != "tenant-a" {
			t.Error("tenant selector changed")
		}
		seen = append(seen, metadata.ValueFromIncomingContext(ctx, "authorization")[0])
		return "external-only-in-body", nil
	})
	for _, token := range []string{"first", "rotated"} {
		if err := os.WriteFile(file+".new", []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(file+".new", file); err != nil {
			t.Fatal(err)
		}
		id, reason := client.Resolve(context.Background(), "tenant-a")
		if id != "external-only-in-body" || reason != "" {
			t.Fatalf("lookup failed: %s", reason)
		}
	}
	if len(seen) != 2 || seen[0] != "Bearer first" || seen[1] != "Bearer rotated" {
		t.Fatal("token was not reread")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if id, reason := client.Resolve(context.Background(), "tenant-a"); id != "" || reason == "" {
		t.Fatal("missing token did not omit")
	}
}

func TestLookupBudgetAndSafeReasons(t *testing.T) {
	client, _ := wireClient(t, func(ctx context.Context, _ string) (string, error) { <-ctx.Done(); return "", ctx.Err() })
	start := time.Now()
	id, reason := client.Resolve(context.Background(), "tenant-a")
	if id != "" || reason != Timeout || time.Since(start) > 750*time.Millisecond {
		t.Fatalf("deadline not enforced: %s", reason)
	}
	for _, code := range []codes.Code{codes.NotFound, codes.PermissionDenied, codes.Unavailable, codes.Internal} {
		t.Run(code.String(), func(t *testing.T) {
			client, _ := wireClient(t, func(context.Context, string) (string, error) {
				return "", status.Error(code, "sensitive-id-DO-NOT-LOG")
			})
			id, reason := client.Resolve(context.Background(), "tenant-a")
			if id != "" || reason == "" || strings.Contains(string(reason), "sensitive-id-DO-NOT-LOG") {
				t.Fatal("unsafe resolution")
			}
		})
	}
}

func TestMissingOrInvalidConfigurationOmitsWithoutConnecting(t *testing.T) {
	for _, cfg := range []Config{{}, {Address: "localhost:1"}, {Address: "localhost:1", CAFile: "/absent", TokenFile: "/absent"}} {
		client := NewClient(cfg)
		if id, reason := client.Resolve(context.Background(), "tenant-a"); id != "" || reason == "" {
			t.Fatal("configuration did not fail closed")
		}
		_ = client.Close()
	}
}

func TestRejectsUntrustedTLSAndInvalidResponses(t *testing.T) {
	empty, _ := wireClient(t, func(context.Context, string) (string, error) { return "", nil })
	if value, reason := empty.Resolve(context.Background(), "tenant-a"); value != "" || reason != InvalidResponse {
		t.Fatal("invalid success was accepted")
	}
	client, tokenFile := wireClient(t, func(context.Context, string) (string, error) { return "not-trusted", nil })
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "unrelated-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		t.Fatal(err)
	}
	untrusted := NewClient(Config{Address: client.conn.Target(), CAFile: caFile, TokenFile: tokenFile})
	defer func() { _ = untrusted.Close() }()
	if id, reason := untrusted.Resolve(context.Background(), "tenant-a"); id != "" || reason == "" {
		t.Fatal("untrusted TLS accepted")
	}
}

func TestAcceptsAuthoritativeOpaqueIDEqualToTenant(t *testing.T) {
	client, _ := wireClient(t, func(_ context.Context, tenant string) (string, error) { return tenant, nil })
	if id, reason := client.Resolve(context.Background(), "tenant-a"); id != "tenant-a" || reason != "" {
		t.Fatalf("authoritative opaque ID rejected: id=%q reason=%s", id, reason)
	}
}

// blockedTokenClient stalls inside the actual per-RPC credential reader, not
// the server. Each active read occupies one lookup worker until unblocked.
func blockedTokenClient(t *testing.T) (*Client, <-chan struct{}, func(), *atomic.Int32, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	started, release := make(chan struct{}, 16), make(chan struct{})
	var reads, active, served atomic.Int32
	client, _ := wireClient(t, func(context.Context, string) (string, error) {
		served.Add(1)
		return "fresh-authoritative-id", nil
	}, func(path string) ([]byte, error) {
		reads.Add(1)
		active.Add(1)
		defer active.Add(-1)
		started <- struct{}{}
		<-release
		return os.ReadFile(path)
	})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		deadline := time.Now().Add(2 * time.Second)
		for len(client.permits) != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if len(client.permits) != 0 || active.Load() != 0 {
			t.Error("unblocked lookup workers did not exit and release permits")
		}
	})
	return client, started, unblock, &reads, &active, &served
}

func TestBlockedTokenReadHonorsLookupAndParentDeadlines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		parent time.Duration
		limit  time.Duration
	}{
		{"total-budget", 5 * time.Second, 750 * time.Millisecond},
		{"short-parent", 20 * time.Millisecond, 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, started, _, reads, active, _ := blockedTokenClient(t)
			ctx, cancel := context.WithTimeout(context.Background(), tc.parent)
			defer cancel()
			done := make(chan struct{}, 1)
			start := time.Now()
			go func() {
				id, reason := client.Resolve(ctx, "tenant-a")
				if id != "" || reason != Timeout {
					t.Errorf("blocked token lookup returned id=%q reason=%s", id, reason)
				}
				done <- struct{}{}
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("credential read never started")
			}
			select {
			case <-done:
				if time.Since(start) > tc.limit {
					t.Fatal("blocked token read exceeded resolution deadline")
				}
			case <-time.After(time.Until(start.Add(tc.limit))):
				t.Fatal("resolution remained blocked in token I/O beyond deadline")
			}
			if reads.Load() != 1 || active.Load() != 1 || len(client.permits) != 1 {
				t.Fatal("timed-out read must retain its single worker permit")
			}
		})
	}
}

func TestBlockedTokenReadsStayBoundedUntilWorkersExit(t *testing.T) {
	client, started, unblock, reads, active, served := blockedTokenClient(t)
	const limit = 16
	done := make(chan struct{}, limit)
	for range limit {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			id, reason := client.Resolve(ctx, "tenant-a")
			if id != "" || reason != Timeout {
				t.Errorf("blocked lookup returned id=%q reason=%s", id, reason)
			}
			done <- struct{}{}
		}()
	}
	for range limit {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("expected all 16 bounded readers to start")
		}
	}
	for range limit {
		select {
		case <-done:
		case <-time.After(300 * time.Millisecond):
			t.Fatal("blocked readers prevented caller deadline completion")
		}
	}
	for range 100 {
		if id, reason := client.Resolve(context.Background(), "tenant-a"); id != "" || reason != Busy {
			t.Fatalf("saturated lookup did not return BUSY: id=%q reason=%s", id, reason)
		}
	}
	if reads.Load() != limit || active.Load() != limit || len(client.permits) != limit {
		t.Fatalf("workers grew or permits released early: reads=%d active=%d permits=%d", reads.Load(), active.Load(), len(client.permits))
	}
	unblock()
	deadline := time.Now().Add(2 * time.Second)
	for len(client.permits) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if active.Load() != 0 || len(client.permits) != 0 || served.Load() != 0 {
		t.Fatalf("late token used or workers leaked: active=%d permits=%d served=%d", active.Load(), len(client.permits), served.Load())
	}
	if id, reason := client.Resolve(context.Background(), "tenant-a"); id != "fresh-authoritative-id" || reason != "" {
		t.Fatalf("client failed to recover after workers exited: id=%q reason=%s", id, reason)
	}
	if reads.Load() != limit+1 || served.Load() != 1 {
		t.Fatal("fresh lookup did not reopen the token and perform exactly one RPC")
	}
}

func TestParentDeadlineBoundsLookup(t *testing.T) {
	client, _ := wireClient(t, func(ctx context.Context, _ string) (string, error) { <-ctx.Done(); return "", ctx.Err() })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if id, reason := client.Resolve(ctx, "tenant-a"); id != "" || reason != Timeout || time.Since(start) > 200*time.Millisecond {
		t.Fatal("short parent deadline ignored")
	}
}
