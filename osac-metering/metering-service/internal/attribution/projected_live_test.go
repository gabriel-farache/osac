/*
Copyright (c) 2026 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except
in compliance with the License. You may obtain a copy of the License at
http://www.apache.org/licenses/LICENSE-2.0
*/

package attribution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProjectedLiveFulfillmentClient is run only by the installed billing-projected-client
// spec, in a probe based on the final metering image with its actual projected volume.
// The default local skip is not runtime coverage. This is not a usage-producer test.
func TestProjectedLiveFulfillmentClient(t *testing.T) {
	live := os.Getenv("OSAC_IT_METERING_PROJECTED_LIVE")
	if live == "" {
		t.Skip("requires an explicitly enabled in-Pod projected-client probe; not runtime coverage")
	}
	if live != "1" || os.Getenv("KUBERNETES_SERVICE_HOST") == "" {
		t.Fatal("projected-client probe requires the live flag and Kubernetes Pod environment")
	}
	cfg := Config{
		Address:   os.Getenv("FULFILLMENT_ADDRESS"),
		CAFile:    os.Getenv("FULFILLMENT_CA_FILE"),
		TokenFile: os.Getenv("FULFILLMENT_TOKEN_FILE"),
	}
	tenant := os.Getenv("OSAC_IT_METERING_TENANT")
	expected := os.Getenv("OSAC_IT_METERING_EXPECTED_EXTERNAL_ID")
	if cfg.Address == "" || !filepath.IsAbs(cfg.CAFile) || !filepath.IsAbs(cfg.TokenFile) ||
		tenant == "" || !strings.HasPrefix(expected, "projected-client-dummy-") || expected == tenant {
		t.Fatal("missing or invalid projected-client fixture configuration")
	}
	// Inspect metadata only: credential bytes are read exclusively by the production client.
	// Kubelet's projected directory exposes the token through an atomic-writer symlink.
	info, err := os.Lstat(cfg.TokenFile)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("projected token path is not a mounted atomic-writer entry")
	}
	client := NewClient(cfg)
	t.Cleanup(func() {
		if client.Close() != nil {
			t.Error("closing projected-client connection failed")
		}
	})
	id, reason := client.Resolve(context.Background(), tenant)
	if reason != "" {
		t.Fatalf("production projected-client lookup failed: %s", reason)
	}
	if id != expected {
		t.Fatal("production projected-client result did not match the dummy assignment")
	}
}
