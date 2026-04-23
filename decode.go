package traefik_plugin_webhooks

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxWebhookDecodedBody caps decompressed bytes for webhook payloads (zip-bomb guard).
const maxWebhookDecodedBody = 10 << 20

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return http.Header{}
	}
	out := make(http.Header, len(h))
	for k, vals := range h {
		cp := make([]string, len(vals))
		copy(cp, vals)
		out[k] = cp
	}
	return out
}

func contentCodings(ce string) []string {
	parts := strings.Split(ce, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == "" || p == "identity" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func gunzipLimit(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()

	out, err := io.ReadAll(io.LimitReader(r, maxWebhookDecodedBody+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxWebhookDecodedBody {
		return nil, fmt.Errorf("gzip output exceeds limit")
	}
	return out, nil
}

func zlibOrRawDeflateLimit(data []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(data))
	if err == nil {
		defer zr.Close()
		out, err := io.ReadAll(io.LimitReader(zr, maxWebhookDecodedBody+1))
		if err != nil {
			return nil, err
		}
		if len(out) > maxWebhookDecodedBody {
			return nil, fmt.Errorf("zlib output exceeds limit")
		}
		return out, nil
	}

	fr := flate.NewReader(bytes.NewReader(data))
	defer fr.Close()

	out, err := io.ReadAll(io.LimitReader(fr, maxWebhookDecodedBody+1))
	if err != nil {
		return nil, err
	}
	if len(out) > maxWebhookDecodedBody {
		return nil, fmt.Errorf("deflate output exceeds limit")
	}
	return out, nil
}

func decompressCoding(coding string, data []byte) ([]byte, error) {
	switch coding {
	case "gzip", "x-gzip":
		return gunzipLimit(data)
	case "deflate", "x-deflate":
		return zlibOrRawDeflateLimit(data)
	case "br":
		// Brotli would require a third-party decoder. Traefik loads community middleware
		// plugins with Yaegi, which cannot import non-stdlib modules from this module path.
		return nil, fmt.Errorf("brotli content-encoding (br) is not supported in this plugin build")
	default:
		return nil, fmt.Errorf("unsupported content-encoding %q", coding)
	}
}

// decodeCompressedBodyForWebhook reverses Content-Encoding on raw (wire-format) bytes.
// On success it returns decoded bytes, a header copy with Content-Encoding and Content-Length
// removed (so payload matches plain body), and ok=true. On failure it returns ok=false.
func decodeCompressedBodyForWebhook(h http.Header, raw []byte) ([]byte, http.Header, bool) {
	if len(raw) == 0 {
		return raw, h, false
	}
	if h == nil {
		h = http.Header{}
	}

	ce := strings.TrimSpace(h.Get("Content-Encoding"))
	if ce == "" {
		return raw, h, false
	}

	codings := contentCodings(ce)
	if len(codings) == 0 {
		return raw, h, false
	}

	data := raw
	for i := len(codings) - 1; i >= 0; i-- {
		next, err := decompressCoding(codings[i], data)
		if err != nil {
			return raw, h, false
		}
		data = next
	}

	outHdr := cloneHeader(h)
	outHdr.Del("Content-Encoding")
	outHdr.Del("Content-Length")
	return data, outHdr, true
}
