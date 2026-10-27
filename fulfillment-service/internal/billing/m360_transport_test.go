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
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2/dsl/core"
	. "github.com/onsi/ginkgo/v2/dsl/table"
	. "github.com/onsi/gomega"
)

const testToken = "super-secret-token"

type fakeM360 struct {
	*httptest.Server
	hits    atomic.Int32
	methods chan string
}

func newFakeM360(h http.HandlerFunc) *fakeM360 {
	f := &fakeM360{methods: make(chan string, 1000)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.methods <- r.Method
		h(w, r)
	}))
	return f
}

func testClient(url string) *m360Client {
	c := newM360Client(Config{BaseURL: url, APIVersion: "v2", Credential: testToken}, logger)
	c.backoffBase = time.Millisecond
	c.backoffMax = 2 * time.Millisecond
	return c
}

func reasonOf(err error) Reason {
	var e *Error
	ExpectWithOffset(1, errors.As(err, &e)).To(BeTrue(), "error was %v", err)
	return e.Reason
}

var _ = Describe("M360 transport", func() {
	var fake *fakeM360

	AfterEach(func() {
		fake.Close()
		close(fake.methods)
		for m := range fake.methods {
			Expect(m).To(Equal(http.MethodGet), "only GET may ever be sent")
		}
	})

	It("sends bearer token and addresses the configured API version", func() {
		var auth, path string
		fake = newFakeM360(func(w http.ResponseWriter, r *http.Request) {
			auth, path = r.Header.Get("Authorization"), r.URL.Path
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
		var out struct{ OK bool }
		Expect(testClient(fake.URL).get(context.Background(), "op", "/accounts", nil, &out)).To(Succeed())
		Expect(auth).To(Equal("Bearer " + testToken))
		Expect(path).To(Equal("/api/v2/accounts"))
		Expect(out.OK).To(BeTrue())
	})

	DescribeTable("classifies non-2xx responses",
		func(status int, reason Reason, attempts int32) {
			fake = newFakeM360(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
			err := testClient(fake.URL).get(context.Background(), "op", "/x", nil, nil)
			Expect(reasonOf(err)).To(Equal(reason))
			Expect(fake.hits.Load()).To(Equal(attempts))
		},
		Entry("401 not retried", 401, ReasonUnauthorized, int32(1)),
		Entry("403 not retried", 403, ReasonUnauthorized, int32(1)),
		Entry("409 not retried", 409, ReasonConflict, int32(1)),
		Entry("400 not retried", 400, ReasonMalformed, int32(1)),
		Entry("404 not retried", 404, ReasonMalformed, int32(1)),
		Entry("500 retried to 4 attempts", 500, ReasonUnavailable, int32(4)),
		Entry("503 retried to 4 attempts", 503, ReasonUnavailable, int32(4)),
		Entry("429 retried to 4 attempts", 429, ReasonUnavailable, int32(4)),
		Entry("408 retried to 4 attempts", 408, ReasonTimeout, int32(4)),
	)

	It("recovers when a retry succeeds", func() {
		var n atomic.Int32
		fake = newFakeM360(func(w http.ResponseWriter, _ *http.Request) {
			if n.Add(1) < 3 {
				w.WriteHeader(502)
				return
			}
			_, _ = w.Write([]byte(`{}`))
		})
		Expect(testClient(fake.URL).get(context.Background(), "op", "/x", nil, &struct{}{})).To(Succeed())
		Expect(fake.hits.Load()).To(Equal(int32(3)))
	})

	It("times out a slow attempt and retries it", func() {
		fake = newFakeM360(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		c := testClient(fake.URL)
		c.attemptTimeout = 20 * time.Millisecond
		err := c.get(context.Background(), "op", "/x", nil, nil)
		Expect(reasonOf(err)).To(Equal(ReasonTimeout))
		Expect(fake.hits.Load()).To(Equal(int32(4)))
	})

	It("reports network failure as UNAVAILABLE", func() {
		fake = newFakeM360(func(http.ResponseWriter, *http.Request) {})
		c := testClient(fake.URL)
		fake.CloseClientConnections()
		fake.Close()
		Expect(reasonOf(c.get(context.Background(), "op", "/x", nil, nil))).To(Equal(ReasonUnavailable))
	})

	It("stops retrying once the caller's context is done", func() {
		fake = newFakeM360(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		Expect(testClient(fake.URL).get(ctx, "op", "/x", nil, nil)).To(HaveOccurred())
		Expect(fake.hits.Load()).To(BeNumerically("<=", 1))
	})

	DescribeTable("treats bad 2xx bodies as MALFORMED, never as empty data",
		func(body string) {
			fake = newFakeM360(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })
			var out struct{ Items []string }
			err := testClient(fake.URL).get(context.Background(), "op", "/x", nil, &out)
			Expect(reasonOf(err)).To(Equal(ReasonMalformed))
			Expect(fake.hits.Load()).To(Equal(int32(1)))
		},
		Entry("not json", "<html>"),
		Entry("empty body", ""),
		Entry("wrong shape", `{"Items":"nope"}`),
		Entry("error envelope", `{"error":{"code":"X","message":"boom"}}`),
	)

	Describe("pricing breaker", func() {
		It("opens after 5 consecutive exhausted failures, then probes and recovers", func() {
			var fail atomic.Bool
			fail.Store(true)
			fake = newFakeM360(func(w http.ResponseWriter, _ *http.Request) {
				if fail.Load() {
					w.WriteHeader(500)
					return
				}
				_, _ = w.Write([]byte(`{}`))
			})
			now := time.Now()
			c := testClient(fake.URL)
			c.breaker.now = func() time.Time { return now }
			for range 5 {
				Expect(reasonOf(c.get(context.Background(), "op", "/x", nil, nil))).To(Equal(ReasonUnavailable))
			}
			before := fake.hits.Load()
			err := c.get(context.Background(), "op", "/x", nil, nil)
			Expect(reasonOf(err)).To(Equal(ReasonUnavailable))
			Expect(fake.hits.Load()).To(Equal(before), "open breaker must not call the provider")

			now = now.Add(31 * time.Second)
			fail.Store(false)
			Expect(c.get(context.Background(), "op", "/x", nil, &struct{}{})).To(Succeed())
			Expect(c.get(context.Background(), "op", "/x", nil, &struct{}{})).To(Succeed())
		})

		It("is neither tripped by nor blocking account discovery", func() {
			var fail atomic.Bool
			fail.Store(true)
			fake = newFakeM360(func(w http.ResponseWriter, _ *http.Request) {
				if fail.Load() {
					w.WriteHeader(500)
					return
				}
				_, _ = w.Write([]byte(`{}`))
			})
			c := testClient(fake.URL)
			for range 6 {
				Expect(reasonOf(c.getDiscovery(context.Background(), "op", "/accounts", nil, nil))).To(Equal(ReasonUnavailable))
			}
			fail.Store(false)
			Expect(c.get(context.Background(), "op", "/services", nil, &struct{}{})).To(Succeed(), "pricing breaker must still be closed")

			fail.Store(true)
			for range 5 {
				Expect(c.get(context.Background(), "op", "/services", nil, nil)).To(HaveOccurred())
			}
			fail.Store(false)
			Expect(reasonOf(c.get(context.Background(), "op", "/services", nil, nil))).To(Equal(ReasonUnavailable))
			Expect(c.getDiscovery(context.Background(), "op", "/accounts", nil, &struct{}{})).To(Succeed(), "open pricing breaker must not block accounts")
		})

		It("is not tripped by non-retryable client errors", func() {
			fake = newFakeM360(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) })
			c := testClient(fake.URL)
			for range 10 {
				Expect(reasonOf(c.get(context.Background(), "op", "/x", nil, nil))).To(Equal(ReasonUnauthorized))
			}
			Expect(fake.hits.Load()).To(Equal(int32(10)))
		})
	})
})

var _ = Describe("M360 redaction", func() {
	It("keeps bodies, tokens and ids out of errors and logs", func() {
		const leak = "acct-native-9999"
		fake := newFakeM360(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"error":{"code":"E","message":"` + leak + ` ` + testToken + `"}}`))
		})
		defer fake.Close()
		var logs strings.Builder
		c := testClient(fake.URL)
		c.logger = newCaptureLogger(&logs)
		err := c.get(context.Background(), "GetAccount", "/accounts/"+leak, nil, nil)
		Expect(err).To(HaveOccurred())
		for _, s := range []string{err.Error(), logs.String()} {
			Expect(s).ToNot(ContainSubstring(leak))
			Expect(s).ToNot(ContainSubstring(testToken))
			Expect(s).ToNot(ContainSubstring("boom"))
		}
		Expect(logs.String()).To(ContainSubstring("GetAccount"))
	})
})

func newCaptureLogger(w *strings.Builder) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
