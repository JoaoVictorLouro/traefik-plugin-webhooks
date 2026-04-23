package traefik_plugin_webhooks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAfterRequestWithoutBodyBuffering(t *testing.T) {
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

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Resp", "stream")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("chunk"))
	})

	h, err := New(context.Background(), next, &Config{
		Rules: []Rule{
			{
				URLRegex:   "/stream",
				Method:     "GET",
				WebhookURL: srv.URL,
			},
		},
		WebhookIncludeBody:    false,
		WebhookIncludeHeaders: true,
		WebhookMode:           string(WebhookModeAfterRequest),
		RequireHTTPStatus:     []int{http.StatusAccepted},
	}, "after-stream")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	req.Host = "app.local"

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	if rec.Body.String() != "chunk" {
		t.Fatalf("unexpected body: %q", rec.Body.String())
	}

	p := waitForWebhook(t, received, 2*time.Second)
	if p.Body != "" {
		t.Fatalf("expected empty webhook body, got %q", p.Body)
	}
	if p.Headers["X-Resp"] != "stream" {
		t.Fatalf("expected response header in payload, got %#v", p.Headers)
	}
	if p.StatusCode != http.StatusAccepted {
		t.Fatalf("statusCode: got %d want %d", p.StatusCode, http.StatusAccepted)
	}
}
