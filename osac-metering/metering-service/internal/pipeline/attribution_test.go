/*
Copyright (c) 2026 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except
in compliance with the License. You may obtain a copy of the License at
http://www.apache.org/licenses/LICENSE-2.0
*/

package pipeline_test

import (
	"context"
	"strings"
	"testing"
	"time"

	cloudevents "github.com/cloudevents/sdk-go/v2"

	"github.com/osac-project/osac-metering/internal/attribution"
	"github.com/osac-project/osac-metering/internal/database"
	"github.com/osac-project/osac-metering/internal/heartbeat"
	"github.com/osac-project/osac-metering/internal/pipelinetest"
	"github.com/osac-project/osac-metering/internal/round"
	"github.com/osac-project/osac-metering/internal/testenv"
	"github.com/osac-project/osac-metering/internal/topics"
	"github.com/osac-project/osac-metering/schema"
	privatev1 "github.com/osac-project/osac/proto/gen/osac/private/v1"
)

func TestAttributionAcceptsOpaqueAssignmentEqualToTenant(t *testing.T) {
	const tenant = "attribution-opaque-equal"
	p := pipelinetest.NewPipeline(t, tenant, round.WithAssignmentResolver(func(_ context.Context, selector string) (string, attribution.Reason) {
		if selector != tenant {
			t.Fatal("wrong assignment selector")
		}
		// Simulates the CONNECTED projection of the opaque stored external ID.
		return tenant, ""
	}))
	events := runningVM("e1", base, "small")
	events[0].SetExternalBillingId("manual-must-not-win")
	total := testenv.ProduceEvents(t, p.Tenant, events)
	p.FoldAll(t, total)
	p.RoundAt(t, base.Add(time.Minute))
	rows := pendingRows(t, p)
	if len(rows) != 1 {
		t.Fatalf("wanted one new heartbeat, got %d", len(rows))
	}
	if id := heartbeatData(t, rows[0].Payload).ExternalBillingAccountID; id == nil || *id != tenant {
		t.Fatal("new persisted heartbeat dropped the equal-text authoritative ID")
	}
	p.StartRelay(t)
	p.WaitOutboxEmpty(t)
	beats := pipelinetest.SentHeartbeats(t, p.Tenant)
	if len(beats) != 1 || beats[0].Data.ExternalBillingAccountID == nil || *beats[0].Data.ExternalBillingAccountID != tenant {
		t.Fatal("Kafka heartbeat dropped the equal-text authoritative ID")
	}
	checkEnvelope(t, beats[0], tenant, "vm-7", schema.ResourceTypeComputeInstance, "compute", "second")
	checkQuantity(t, beats[0], 1)
}

func TestAttributionUsesCurrentAssignmentNotBackloggedManualValue(t *testing.T) {
	current, reason := "assigned-now", attribution.Reason("")
	resolver := func(context.Context, string) (string, attribution.Reason) { return current, reason }
	p := pipelinetest.NewPipeline(t, "attribution", round.WithAssignmentResolver(resolver))
	events := runningVM("e1", base, "small")
	events[0].SetExternalBillingId("different-manual-value")
	total := testenv.ProduceEvents(t, p.Tenant, events)
	p.FoldAll(t, total)
	p.RoundAt(t, base.Add(time.Minute))
	old, err := database.NextOutbox(context.Background(), p.Store.Pool(), p.Tenant, 100)
	if err != nil || len(old) == 0 {
		t.Fatal("no old outbox", err)
	}
	current = "reassigned-now"
	// Each new pipeline owns a fresh bounded snapshot; retained outbox remains on the same store.
	p = pipelinetest.NewPipelineOnStore(t, p.Tenant, p.Store, round.WithAssignmentResolver(resolver))
	p.RoundAt(t, base.Add(2*time.Minute))
	rows, err := database.NextOutbox(context.Background(), p.Store.Pool(), p.Tenant, 100)
	if err != nil || len(rows) <= len(old) {
		t.Fatal("no new usage", err)
	}
	for i, row := range rows {
		want := "reassigned-now"
		if i < len(old) {
			want = "assigned-now"
			if string(old[i].Payload) != string(row.Payload) {
				t.Fatal("persisted bytes changed")
			}
		}
		data := heartbeatData(t, row.Payload)
		if data.ExternalBillingAccountID == nil || *data.ExternalBillingAccountID != want {
			t.Fatal("wrong attribution")
		}
	}
	p.StartRelay(t)
	p.WaitOutboxEmpty(t)
	sent := testenv.ConsumeAll(t, topics.Heartbeat(p.Tenant))
	if len(sent) != len(rows) {
		t.Fatal("relay lost usage")
	}
	for i, payload := range sent {
		if string(payload) != string(rows[i].Payload) {
			t.Fatal("relay rewrote body")
		}
	}
	// A restored ledger replays the already-produced bytes without attribution reads.
	restored := pipelinetest.NewPipeline(t, p.Tenant, round.WithAssignmentResolver(func(context.Context, string) (string, attribution.Reason) {
		t.Error("replay resolved attribution")
		return "", attribution.Unavailable
	}))
	restored.FoldAll(t, total)
	restored.RoundAt(t, base.Add(2*time.Minute))
	current, reason = "", attribution.Unavailable
	p = pipelinetest.NewPipelineOnStore(t, p.Tenant, p.Store, round.WithAssignmentResolver(resolver))
	p.RoundAt(t, base.Add(3*time.Minute))
	p.WaitOutboxEmpty(t)
	all := pipelinetest.SentHeartbeats(t, p.Tenant)
	if len(all) <= len(sent) {
		t.Fatal("fulfillment failure stopped usage")
	}
	for _, beat := range all[len(sent):] {
		if beat.Data.ExternalBillingAccountID != nil {
			t.Fatal("failure fell back to manual ID")
		}
	}
}

func TestAttributionClusterAndWorkersOmitUntilConnected(t *testing.T) {
	for _, state := range []string{"NONE", "PENDING"} {
		t.Run(state, func(t *testing.T) {
			resolve := func(context.Context, string) (string, attribution.Reason) { return "", attribution.NotFound }
			p := pipelinetest.NewPipeline(t, "attribution-"+strings.ToLower(state), round.WithAssignmentResolver(resolve))
			events := []*privatev1.Event{
				testenv.ClusterEvent("c1", privatev1.EventType_EVENT_TYPE_OBJECT_CREATED, base, base, "web", "READY", base, "openshift-4-19", "4.19.0", base),
				testenv.BMIEvent("b1", privatev1.EventType_EVENT_TYPE_OBJECT_CREATED, base, base, "web", "STARTING", base, "medium", "cluster-1", "workers"),
			}
			for _, event := range events {
				event.SetExternalBillingId("manual-must-not-win")
			}
			total := testenv.ProduceEvents(t, p.Tenant, events)
			p.FoldAll(t, total)
			p.RoundAt(t, base.Add(time.Minute))
			old := pendingRows(t, p)
			if len(old) != 2 {
				t.Fatalf("wanted control plane and worker usage; got %d", len(old))
			}
			for _, row := range old {
				if heartbeatData(t, row.Payload).ExternalBillingAccountID != nil {
					t.Fatal("manual fallback with unconnected assignment")
				}
			}
			p = pipelinetest.NewPipelineOnStore(t, p.Tenant, p.Store, round.WithAssignmentResolver(func(context.Context, string) (string, attribution.Reason) { return "connected-current", "" }))
			p.RoundAt(t, base.Add(2*time.Minute))
			rows := pendingRows(t, p)
			if len(rows) != 4 {
				t.Fatal("promotion failed to continue both meters")
			}
			for i, row := range rows {
				data := heartbeatData(t, row.Payload)
				if i < 2 {
					if string(row.Payload) != string(old[i].Payload) {
						t.Fatal("promotion changed old bytes")
					}
					continue
				}
				if data.ExternalBillingAccountID == nil || *data.ExternalBillingAccountID != "connected-current" {
					t.Fatal("missing connected attribution")
				}
				checkQuantity(t, pipelinetest.SentBeat{Data: data}, 1)
			}
		})
	}
}

func TestAttributionRefreshesPositiveAndNegativeSnapshotsWithinOneSecond(t *testing.T) {
	for _, initial := range []string{"", "previous-connected"} {
		t.Run("initial-"+initial, func(t *testing.T) {
			current := initial
			resolve := func(context.Context, string) (string, attribution.Reason) {
				if current == "" {
					return "", attribution.NotFound
				}
				return current, ""
			}
			tenant := "attribution-cache-empty"
			if initial != "" {
				tenant = "attribution-cache-positive"
			}
			p := pipelinetest.NewPipeline(t, tenant, round.WithAssignmentResolver(resolve))
			total := testenv.ProduceEvents(t, p.Tenant, runningVM("e1", base, "small"))
			p.FoldAll(t, total)
			p.RoundAt(t, base.Add(time.Minute))
			old := pendingRows(t, p)
			current = "new-connected"
			// Real elapsed time is intentional here: this asserts the externally
			// observable refresh bound without introducing a production clock API.
			timer := time.NewTimer(time.Second + 20*time.Millisecond)
			<-timer.C
			p.RoundAt(t, base.Add(2*time.Minute))
			rows := pendingRows(t, p)
			if len(rows) != 2 || string(rows[0].Payload) != string(old[0].Payload) {
				t.Fatal("snapshot refresh changed old usage")
			}
			data := heartbeatData(t, rows[1].Payload)
			if data.ExternalBillingAccountID == nil || *data.ExternalBillingAccountID != "new-connected" {
				t.Fatal("snapshot exceeded one-second refresh bound")
			}
		})
	}
}

func heartbeatData(t *testing.T, payload []byte) schema.HeartbeatData {
	t.Helper()
	var event cloudevents.Event
	if err := event.UnmarshalJSON(payload); err != nil {
		t.Fatal(err)
	}
	data, err := heartbeat.Data(event)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func pendingRows(t *testing.T, p *pipelinetest.Pipeline) []database.OutboxRow {
	t.Helper()
	rows, err := database.NextOutbox(context.Background(), p.Store.Pool(), p.Tenant, 100)
	if err != nil || len(rows) == 0 {
		t.Fatal("no new usage", err)
	}
	return rows
}
