package traefik_plugin_webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNewValidation(t *testing.T) {
	t.Parallel()

	_, err := New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &Config{
		Rules: []Rule{{WebhookURL: "http://example.com", URLRegex: "("}},
	}, "t")
	if err == nil {
		t.Fatal("expected error for bad url regex")
	}

	_, err = New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &Config{
		Rules: []Rule{{WebhookURL: "http://example.com", BodyRegex: "("}},
	}, "t")
	if err == nil {
		t.Fatal("expected error for bad body regex")
	}

	_, err = New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &Config{
		Rules: []Rule{{WebhookURL: "http://example.com", HeaderRegexes: []HeaderRegexRule{{Header: "X", Regex: "("}}}},
	}, "t")
	if err == nil {
		t.Fatal("expected error for bad header regex")
	}

	_, err = New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &Config{
		Rules: []Rule{{WebhookURL: "http://example.com", HeaderRegexes: []HeaderRegexRule{{Header: "X", Regex: ""}}}},
	}, "t")
	if err == nil {
		t.Fatal("expected error when header set without regex")
	}

	_, err = New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &Config{
		Rules:       []Rule{{WebhookURL: "http://example.com"}},
		WebhookMode: "nope",
	}, "t")
	if err == nil {
		t.Fatal("expected error for bad webhook mode")
	}

	_, err = New(context.Background(), nil, &Config{
		Rules: []Rule{{WebhookURL: "http://example.com"}},
	}, "t")
	if err == nil {
		t.Fatal("expected error for nil next")
	}

	_, err = New(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), &Config{
		Rules: []Rule{{WebhookURL: ""}},
	}, "t")
	if err == nil {
		t.Fatal("expected error for empty webhook url")
	}
}

func waitForWebhook(t *testing.T, ch <-chan Payload, d time.Duration) Payload {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(d):
		t.Fatal("timed out waiting for webhook")
	}
	return Payload{}
}

func TestBeforeRequestWebhook(t *testing.T) {
	t.Parallel()

	received := make(chan Payload, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("upstream"))
	})

	h, err := New(context.Background(), next, &Config{
		Rules: []Rule{
			{
				URLRegex:   ".*/orders",
				Method:     "POST",
				BodyRegex:  "alpha",
				WebhookURL: srv.URL,
			},
		},
		WebhookIncludeBody:    true,
		WebhookIncludeHeaders: true,
		WebhookMode:           string(WebhookModeBeforeRequest),
	}, "before")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/orders?x=1", bytes.NewBufferString("body-alpha"))
	req.Host = "app.example.com"
	req.Header.Set("X-Test", "1")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("unexpected status from upstream: %d", rec.Code)
	}
	if !bytes.Equal(rec.Body.Bytes(), []byte("upstream")) {
		t.Fatalf("unexpected upstream body: %q", rec.Body.String())
	}

	p := waitForWebhook(t, received, 2*time.Second)
	if p.URL != "http://app.example.com/api/orders?x=1" {
		t.Fatalf("url: got %q", p.URL)
	}
	if p.Body != "body-alpha" {
		t.Fatalf("body: got %q", p.Body)
	}
	if p.Headers["X-Test"] != "1" {
		t.Fatalf("headers: got %#v", p.Headers)
	}
	if p.StatusCode != 0 {
		t.Fatalf("before_request webhook should omit statusCode, got %d", p.StatusCode)
	}
}

func TestHeaderRegexesMatch(t *testing.T) {
	t.Parallel()

	received := make(chan Payload, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	h, err := New(context.Background(), next, &Config{
		Rules: []Rule{
			{
				URLRegex: ".*/api",
				HeaderRegexes: []HeaderRegexRule{
					{Header: "Referer", Regex: `^https://partner\.example/`},
				},
				WebhookURL: srv.URL,
			},
		},
		WebhookMode: string(WebhookModeBeforeRequest),
	}, "hdr")
	if err != nil {
		t.Fatal(err)
	}

	noRef := httptest.NewRequest(http.MethodGet, "/api", nil)
	noRef.Host = "app.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, noRef)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	select {
	case p := <-received:
		t.Fatalf("did not expect webhook without Referer, got %#v", p)
	case <-time.After(300 * time.Millisecond):
	}

	withRef := httptest.NewRequest(http.MethodGet, "/api", nil)
	withRef.Host = "app.example.com"
	withRef.Header.Set("Referer", "https://partner.example/page")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, withRef)
	if rec2.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec2.Code)
	}
	p := waitForWebhook(t, received, 2*time.Second)
	if p.URL != "http://app.example.com/api" {
		t.Fatalf("url: got %q", p.URL)
	}
}

func TestAfterRequestWebhookRespectsStatus(t *testing.T) {
	t.Parallel()

	received := make(chan Payload, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Resp", "ok")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})

	h, err := New(context.Background(), next, &Config{
		Rules: []Rule{
			{
				URLRegex:   "/items",
				Method:     "GET",
				WebhookURL: srv.URL,
			},
		},
		WebhookIncludeBody:    true,
		WebhookIncludeHeaders: true,
		WebhookMode:           string(WebhookModeAfterRequest),
		RequireHTTPStatus:     []int{http.StatusOK},
	}, "after")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	req.Host = "svc.local"

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected created from upstream, got %d", rec.Code)
	}

	select {
	case p := <-received:
		t.Fatalf("did not expect webhook, got %#v", p)
	case <-time.After(300 * time.Millisecond):
	}

	h2, err := New(context.Background(), next, &Config{
		Rules: []Rule{
			{
				URLRegex:   "/items",
				Method:     "GET",
				WebhookURL: srv.URL,
			},
		},
		WebhookIncludeBody:    true,
		WebhookIncludeHeaders: true,
		WebhookMode:           string(WebhookModeAfterRequest),
		RequireHTTPStatus:     []int{http.StatusCreated},
	}, "after")
	if err != nil {
		t.Fatal(err)
	}

	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, req)

	p := waitForWebhook(t, received, 2*time.Second)
	if p.Body != "created" {
		t.Fatalf("body: got %q", p.Body)
	}
	if p.Headers["X-Resp"] != "ok" {
		t.Fatalf("headers: got %#v", p.Headers)
	}
	if p.StatusCode != http.StatusCreated {
		t.Fatalf("statusCode: got %d want %d", p.StatusCode, http.StatusCreated)
	}
}

func TestNoRulesPassesThrough(t *testing.T) {
	t.Parallel()

	var saw bool
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		saw = true
		w.WriteHeader(http.StatusOK)
	})

	h, err := New(context.Background(), next, &Config{}, "noop")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(rec, req)

	if !saw {
		t.Fatal("expected next handler to run")
	}
}

func TestBodyTooLarge(t *testing.T) {
	t.Parallel()

	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("next should not run when body rejected")
	})

	h, err := New(context.Background(), next, &Config{
		Rules: []Rule{
			{
				URLRegex:   ".*",
				WebhookURL: "http://example.com",
				BodyRegex:  ".",
			},
		},
	}, "big")
	if err != nil {
		t.Fatal(err)
	}

	large := bytes.Repeat([]byte("a"), (1<<20)+1)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(large))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestRequestBodyRestoredForUpstream(t *testing.T) {
	t.Parallel()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(b)
	})

	h, err := New(context.Background(), next, &Config{
		Rules: []Rule{
			{
				URLRegex:   ".*",
				BodyRegex:  "ping",
				WebhookURL: "http://127.0.0.1:9",
			},
		},
		WebhookMode: string(WebhookModeBeforeRequest),
	}, "restore")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString("ping-pong"))
	h.ServeHTTP(rec, req)

	if rec.Body.String() != "ping-pong" {
		t.Fatalf("upstream saw wrong body: %q", rec.Body.String())
	}
}

func TestAfterRequestIncludesClientRequest(t *testing.T) {
	t.Parallel()

	received := make(chan Payload, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	var upstreamBody string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		upstreamBody = string(body)
		w.Header().Set("X-Response", "response-header")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("response-body"))
	})

	h, err := New(context.Background(), next, &Config{
		Rules: []Rule{{
			URLRegex:   ".*",
			WebhookURL: srv.URL,
		}},
		WebhookIncludeBody:           true,
		WebhookIncludeHeaders:        true,
		WebhookIncludeRequestBody:    true,
		WebhookIncludeRequestHeaders: true,
		WebhookMode:                  string(WebhookModeAfterRequest),
	}, "after-request-data")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/orders", bytes.NewBufferString("request-body"))
	req.Header.Set("X-Request", "request-header")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if upstreamBody != "request-body" {
		t.Fatalf("upstream saw wrong request body: %q", upstreamBody)
	}

	p := waitForWebhook(t, received, 2*time.Second)
	if p.Body != "response-body" {
		t.Fatalf("response body: got %q", p.Body)
	}
	if p.Headers["X-Response"] != "response-header" {
		t.Fatalf("response headers: got %#v", p.Headers)
	}
	if p.RequestBody == nil || *p.RequestBody != "request-body" {
		t.Fatalf("request body: got %#v", p.RequestBody)
	}
	if p.RequestHeaders == nil || (*p.RequestHeaders)["X-Request"] != "request-header" {
		t.Fatalf("request headers: got %#v", p.RequestHeaders)
	}
}

func TestRequestFieldsOmittedWhenDisabled(t *testing.T) {
	t.Parallel()

	received := make(chan map[string]json.RawMessage, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- payload
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	h, err := New(context.Background(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), &Config{
		Rules: []Rule{{
			URLRegex:   ".*",
			WebhookURL: srv.URL,
		}},
	}, "omitted-request-data")
	if err != nil {
		t.Fatal(err)
	}

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	select {
	case payload := <-received:
		if _, ok := payload["requestBody"]; ok {
			t.Fatal("requestBody should be omitted")
		}
		if _, ok := payload["requestHeaders"]; ok {
			t.Fatal("requestHeaders should be omitted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for webhook")
	}
}

func TestIncludeRequestBodyCapturesAndLimitsBody(t *testing.T) {
	t.Parallel()

	received := make(chan Payload, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body)
	})
	config := &Config{
		Rules: []Rule{{
			URLRegex:   ".*",
			WebhookURL: srv.URL,
		}},
		WebhookIncludeRequestBody: true,
	}
	h, err := New(context.Background(), next, config, "request-body")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString("captured")))
	if rec.Body.String() != "captured" {
		t.Fatalf("upstream saw wrong request body: %q", rec.Body.String())
	}
	p := waitForWebhook(t, received, 2*time.Second)
	if p.RequestBody == nil || *p.RequestBody != "captured" {
		t.Fatalf("request body: got %#v", p.RequestBody)
	}

	upstreamCalled := false
	rejectingHandler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalled = true
	})
	rejecting, err := New(context.Background(), rejectingHandler, config, "large-request-body")
	if err != nil {
		t.Fatal(err)
	}

	large := bytes.Repeat([]byte("a"), (1<<20)+1)
	largeRec := httptest.NewRecorder()
	rejecting.ServeHTTP(largeRec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(large)))
	if largeRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for oversized request body, got %d", largeRec.Code)
	}
	if upstreamCalled {
		t.Fatal("upstream should not run for an oversized request body")
	}
}
