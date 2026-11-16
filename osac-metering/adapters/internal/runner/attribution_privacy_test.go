/*
Copyright (c) 2026 Red Hat, Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except
in compliance with the License. You may obtain a copy of the License at
http://www.apache.org/licenses/LICENSE-2.0
*/

package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/IBM/sarama"
	"github.com/IBM/sarama/mocks"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/osac-project/osac-metering/adapters"
)

var _ = Describe("Attribution diagnostic privacy", func() {
	DescribeTable("keeps echoed body out of diagnostics and headers while retaining original payload", func(nonRetryable, dlqFails bool) {
		const marker = "sensitive-external-billing-account-marker"
		var diagnostic bytes.Buffer
		logger := funcr.New(func(prefix, args string) { diagnostic.WriteString(prefix + args) }, funcr.Options{Verbosity: 10})
		adapter := &mockAdapter{name: "test", submitFn: func(event adapters.MeteringEvent) error {
			err := errors.New(string(event.CloudEvent.Data()))
			if nonRetryable {
				return &adapters.NonRetryableError{Err: err}
			}
			return err
		}}
		producer := mocks.NewSyncProducer(GinkgoT(), nil)
		DeferCleanup(producer.Close)
		ce := cloudevents.NewEvent()
		ce.SetID("stable-event-id")
		ce.SetType("osac.resource.heartbeat.v1")
		ce.SetSource("osac-metering")
		Expect(ce.SetData(cloudevents.ApplicationJSON, map[string]string{"external_billing_account_id": marker})).To(Succeed())
		payload, err := json.Marshal(ce)
		Expect(err).NotTo(HaveOccurred())
		checker := func(message *sarama.ProducerMessage) error {
			value, err := message.Value.Encode()
			Expect(err).NotTo(HaveOccurred())
			Expect(value).To(Equal(payload))
			for _, header := range message.Headers {
				Expect(string(header.Value)).NotTo(ContainSubstring(marker))
			}
			return nil
		}
		if dlqFails {
			producer.ExpectSendMessageWithMessageCheckerFunctionAndFail(checker, errors.New(marker))
		} else {
			producer.ExpectSendMessageWithMessageCheckerFunctionAndSucceed(checker)
		}
		runner := NewRunner(adapter, RunnerConfig{MaxRetries: 1, DedupTTL: time.Minute, FlushInterval: time.Second}, logger,
			WithDLQ(NewDLQProducerFromSyncProducer(producer, "osac.metering.dlq.test")))
		session := &mockSession{ctx: context.Background()}
		Expect(runner.Setup(session)).To(Succeed())
		claim := &mockClaim{topic: testTopic, messages: make(chan *sarama.ConsumerMessage, 1)}
		claim.messages <- &sarama.ConsumerMessage{Topic: testTopic, Offset: 1, Value: payload}
		close(claim.messages)
		_ = runner.ConsumeClaim(session, claim)
		Expect(diagnostic.String()).NotTo(ContainSubstring(marker))
	}, Entry("nonretryable", true, false), Entry("exhausted", false, false), Entry("DLQ failure", true, true))
})
