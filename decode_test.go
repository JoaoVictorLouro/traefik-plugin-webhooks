package traefik_plugin_webhooks

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"net/http"
	"testing"
)

func TestContentCodings(t *testing.T) {
	t.Parallel()

	got := contentCodings(" gzip , identity , br ")
	want := []string{"gzip", "br"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("idx %d got %q want %q", i, got[i], want[i])
		}
	}
}

func TestDecodeCompressedBodyForWebhook_gzip(t *testing.T) {
	t.Parallel()

	plain := []byte(`{"ok":true}`)
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}

	h := http.Header{}
	h.Set("Content-Encoding", "gzip")
	h.Set("Content-Length", "999")

	dec, outH, ok := decodeCompressedBodyForWebhook(h, buf.Bytes())
	if !ok {
		t.Fatal("expected decode ok")
	}
	if !bytes.Equal(dec, plain) {
		t.Fatalf("body got %q want %q", dec, plain)
	}
	if outH.Get("Content-Encoding") != "" || outH.Get("Content-Length") != "" {
		t.Fatalf("expected encoding headers stripped, got %#v", outH)
	}
}

func TestDecodeCompressedBodyForWebhook_brNotSupported(t *testing.T) {
	t.Parallel()

	// Any non-empty bytes behave as opaque brotli payload; decoder must refuse (Yaegi-safe build).
	h := http.Header{}
	h.Set("Content-Encoding", "br")

	raw := []byte{0x0b, 0x00} // not valid brotli, but we do not attempt decode anyway
	_, _, ok := decodeCompressedBodyForWebhook(h, raw)
	if ok {
		t.Fatal("brotli must not decode in stdlib-only plugin build")
	}
}

func TestDecodeCompressedBodyForWebhook_deflate_zlib(t *testing.T) {
	t.Parallel()

	plain := []byte("zlib-payload")
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	h := http.Header{}
	h.Set("Content-Encoding", "deflate")

	dec, _, ok := decodeCompressedBodyForWebhook(h, buf.Bytes())
	if !ok {
		t.Fatal("expected decode ok")
	}
	if !bytes.Equal(dec, plain) {
		t.Fatalf("body got %q want %q", dec, plain)
	}
}

func TestDecodeCompressedBodyForWebhook_unknown(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	h.Set("Content-Encoding", "lz4")

	raw := []byte("nope")
	_, _, ok := decodeCompressedBodyForWebhook(h, raw)
	if ok {
		t.Fatal("expected decode failure for unknown coding")
	}
}
