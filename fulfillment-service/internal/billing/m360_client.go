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

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	maxRetries         = 3
	maxResponseBytes   = 4 << 20
	breakerThreshold   = 5
	breakerCooldown    = 30 * time.Second
	defaultAttemptTime = 10 * time.Second
)

// m360Client issues GET requests only. Response bodies are decoded or discarded, never logged or put in errors.
type m360Client struct {
	cfg            Config
	http           *http.Client
	logger         *slog.Logger
	breaker        *breaker // pricing/coverage only; see get and getDiscovery
	attemptTimeout time.Duration
	backoffBase    time.Duration
	backoffMax     time.Duration
}

func newM360Client(cfg Config, logger *slog.Logger) *m360Client {
	return &m360Client{
		cfg:            cfg,
		http:           &http.Client{},
		logger:         logger,
		breaker:        newBreaker(breakerThreshold, breakerCooldown),
		attemptTimeout: defaultAttemptTime,
		backoffBase:    100 * time.Millisecond,
		backoffMax:     time.Second,
	}
}

// get is for pricing and coverage reads (services, attributes, rate cards): it goes through the pricing breaker.
// It decodes a 2xx JSON response into out (which may be nil to ignore the body).
func (c *m360Client) get(ctx context.Context, op, path string, query url.Values, out any) error {
	return c.do(ctx, c.breaker, op, path, query, out)
}

// getDiscovery is for account reads and the health check. Story 7.01 keeps account discovery out of the pricing
// breaker, so these calls neither trip it nor are blocked by it.
func (c *m360Client) getDiscovery(ctx context.Context, op, path string, query url.Values, out any) error {
	return c.do(ctx, nil, op, path, query, out)
}

func (c *m360Client) do(ctx context.Context, b *breaker, op, path string, query url.Values, out any) error {
	if !b.allow() {
		return &Error{Reason: ReasonUnavailable, Op: op, Retryable: true}
	}
	var err *Error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 && !c.sleep(ctx, c.backoff(attempt)) {
			b.abort()
			return ctxError(op, ctx)
		}
		err = c.attempt(ctx, op, path, query, out)
		if err == nil {
			b.success()
			return nil
		}
		c.logger.DebugContext(ctx, "billing request failed", "op", op, "reason", err.Reason, "attempt", attempt+1)
		if ctx.Err() != nil {
			b.abort()
			return ctxError(op, ctx)
		}
		if !err.Retryable {
			b.success()
			return err
		}
	}
	b.failure()
	return err
}

func (c *m360Client) attempt(ctx context.Context, op, path string, query url.Values, out any) *Error {
	ctx, cancel := context.WithTimeout(ctx, c.attemptTimeout)
	defer cancel()

	u := strings.TrimSuffix(c.cfg.BaseURL, "/") + "/api/" + c.cfg.APIVersion + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return &Error{Reason: ReasonMalformed, Op: op}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.Credential)

	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return &Error{Reason: ReasonTimeout, Op: op, Retryable: true}
		}
		return &Error{Reason: ReasonUnavailable, Op: op, Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return classifyStatus(op, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return &Error{Reason: ReasonUnavailable, Op: op, Retryable: true, status: resp.StatusCode}
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Error != nil {
		return &Error{Reason: ReasonMalformed, Op: op, status: resp.StatusCode}
	}
	if out != nil && json.Unmarshal(body, out) != nil {
		return &Error{Reason: ReasonMalformed, Op: op, status: resp.StatusCode}
	}
	return nil
}

// notFoundOn404 reclassifies a 404 as NOT_FOUND. Only reads of a resource addressed by id call it: a 404 on a
// collection route means a wrong base URL or API version and stays MALFORMED.
func notFoundOn404(err error) error {
	var e *Error
	if errors.As(err, &e) && e.status == http.StatusNotFound {
		return &Error{Reason: ReasonNotFound, Op: e.Op, status: e.status}
	}
	return err
}

func classifyStatus(op string, status int) *Error {
	e := &Error{Op: op, status: status}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Reason = ReasonUnauthorized
	case status == http.StatusConflict:
		e.Reason = ReasonConflict
	case status == http.StatusRequestTimeout:
		e.Reason, e.Retryable = ReasonTimeout, true
	case status == http.StatusTooManyRequests || status >= 500:
		e.Reason, e.Retryable = ReasonUnavailable, true
	default:
		e.Reason = ReasonMalformed
	}
	return e
}

func ctxError(op string, ctx context.Context) *Error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &Error{Reason: ReasonTimeout, Op: op}
	}
	return &Error{Reason: ReasonUnavailable, Op: op}
}

func (c *m360Client) backoff(attempt int) time.Duration {
	return min(c.backoffBase<<(attempt-1), c.backoffMax)
}

func (c *m360Client) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
