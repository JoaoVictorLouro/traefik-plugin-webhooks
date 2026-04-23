package traefik_plugin_webhooks

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAfterRequestWebhook_decodesGzipForPayload(t *testing.T) {
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

	plain := `{"hello":"world"}`
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	if _, err := gw.Write([]byte(plain)); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("X-App", "demo")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gz.Bytes())
	})

	h, err := New(context.Background(), next, &Config{
		Rules: []Rule{
			{
				URLRegex:   "/api",
				Method:     "GET",
				WebhookURL: srv.URL,
			},
		},
		WebhookIncludeBody:    true,
		WebhookIncludeHeaders: true,
		WebhookMode:           string(WebhookModeAfterRequest),
	}, "gzip-hook")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api", nil)
	req.Host = "app.local"

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !bytes.Equal(rec.Body.Bytes(), gz.Bytes()) {
		t.Fatalf("client should receive compressed bytes unchanged (len client=%d len gz=%d)", rec.Body.Len(), gz.Len())
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected Content-Encoding on client response, got %q", rec.Header().Get("Content-Encoding"))
	}

	p := waitForWebhook(t, received, 2*time.Second)
	if p.Body != plain {
		t.Fatalf("webhook body should be decoded plaintext, got %q", p.Body)
	}
	if p.Headers["Content-Encoding"] != "" {
		t.Fatalf("webhook headers should drop Content-Encoding after decode, got %q", p.Headers["Content-Encoding"])
	}
	if p.Headers["X-App"] != "demo" {
		t.Fatalf("expected other headers preserved, got %#v", p.Headers)
	}
	if p.StatusCode != http.StatusOK {
		t.Fatalf("statusCode: got %d want %d", p.StatusCode, http.StatusOK)
	}
}

func TestAfterRequestWebhook_brPayloadFallsBackToWireBytes(t *testing.T) {
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

	// Fake "brotli" wire bytes; plugin does not ship a br decoder (Traefik Yaegi cannot load it).
	wire := []byte{0xce, 0xb2, 0xcf, 0x80}

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(wire)
	})

	h, err := New(context.Background(), next, &Config{
		Rules: []Rule{
			{
				URLRegex:   "/b",
				Method:     "GET",
				WebhookURL: srv.URL,
			},
		},
		WebhookIncludeBody:    true,
		WebhookIncludeHeaders: false,
		WebhookMode:           string(WebhookModeAfterRequest),
	}, "br-hook")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/b", nil)
	req.Host = "app.local"

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !bytes.Equal(rec.Body.Bytes(), wire) {
		t.Fatal("client should receive wire bytes unchanged")
	}

	p := waitForWebhook(t, received, 2*time.Second)
	if p.Body != string(wire) {
		t.Fatalf("webhook body should fall back to raw wire string, got %q", p.Body)
	}
	if p.StatusCode != http.StatusOK {
		t.Fatalf("statusCode: got %d want %d", p.StatusCode, http.StatusOK)
	}
}
