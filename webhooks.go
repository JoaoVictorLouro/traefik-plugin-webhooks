// Package traefik_plugin_webhooks implements a Traefik middleware that POSTs JSON payloads to
// configured webhooks when incoming requests match optional URL, method, and body rules.
//
// The package name must match Traefik’s Yaegi convention: the last path segment of the Go
// module, with hyphens replaced by underscores (traefik-plugin-webhooks → traefik_plugin_webhooks).
package traefik_plugin_webhooks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"regexp"
	"strings"
	"time"
)

// WebhookMode selects when matched webhooks are invoked relative to the upstream handler.
type WebhookMode string

const (
	// WebhookModeBeforeRequest invokes webhooks before the next handler runs.
	WebhookModeBeforeRequest WebhookMode = "before_request"
	// WebhookModeAfterRequest invokes webhooks after the next handler completes.
	WebhookModeAfterRequest WebhookMode = "after_request"
)

// Rule describes one match expression and its webhook target.
type Rule struct {
	// URLRegex is matched against the synthesized request URL (scheme, host, path, and raw query).
	// An empty string compiles to a pattern that matches any URL.
	URLRegex string `json:"urlRegex,omitempty"`
	// Method is matched case-insensitively against the request method (for example GET or POST).
	// An empty string matches any method.
	Method string `json:"method,omitempty"`
	// BodyRegex, when non-empty, must match the request body for the rule to trigger.
	// Request bodies are buffered in memory when any rule defines BodyRegex.
	BodyRegex string `json:"bodyRegex,omitempty"`
	// WebhookURL receives an HTTP POST with a JSON payload when this rule matches.
	WebhookURL string `json:"webhookUrl,omitempty"`
}

// Config is the dynamic configuration for the middleware.
type Config struct {
	Rules []Rule `json:"rules,omitempty"`

	// WebhookIncludeBody controls whether the JSON payload includes a non-empty body field.
	WebhookIncludeBody bool `json:"webhookIncludeBody,omitempty"`
	// WebhookIncludeHeaders controls whether the JSON payload includes response or request headers.
	WebhookIncludeHeaders bool `json:"webhookIncludeHeaders,omitempty"`

	// WebhookMode must be before_request or after_request (default: before_request).
	WebhookMode string `json:"webhookMode,omitempty"`

	// RequireHTTPStatus limits after_request delivery: when non-empty, webhooks run only if the
	// response status code is listed. Ignored for before_request.
	RequireHTTPStatus []int `json:"requireHttpStatus,omitempty"`
}

// CreateConfig returns a default configuration instance used by Traefik.
func CreateConfig() *Config {
	return &Config{}
}

type compiledRule struct {
	urlRegex   *regexp.Regexp
	method     string
	bodyRegex  *regexp.Regexp
	webhookURL string
}

type webhookMiddleware struct {
	name    string
	next    http.Handler
	rules   []compiledRule
	mode    WebhookMode
	include struct {
		body    bool
		headers bool
	}
	statusAllow map[int]struct{}

	client *http.Client

	needsRequestBody bool
}

// New validates configuration and builds the middleware.
func New(_ context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	if next == nil {
		return nil, fmt.Errorf("next handler is nil")
	}

	mode := WebhookMode(strings.TrimSpace(config.WebhookMode))
	switch mode {
	case "":
		mode = WebhookModeBeforeRequest
	case WebhookModeBeforeRequest, WebhookModeAfterRequest:
	default:
		return nil, fmt.Errorf("invalid webhookMode %q: use %q or %q", config.WebhookMode, WebhookModeBeforeRequest, WebhookModeAfterRequest)
	}

	rules := make([]compiledRule, 0, len(config.Rules))
	needsBody := false

	for i, r := range config.Rules {
		if strings.TrimSpace(r.WebhookURL) == "" {
			return nil, fmt.Errorf("rules[%d].webhookUrl is required", i)
		}

		urlRe, err := regexp.Compile(r.URLRegex)
		if err != nil {
			return nil, fmt.Errorf("rules[%d].urlRegex: %w", i, err)
		}

		var bodyRe *regexp.Regexp
		if strings.TrimSpace(r.BodyRegex) != "" {
			bodyRe, err = regexp.Compile(r.BodyRegex)
			if err != nil {
				return nil, fmt.Errorf("rules[%d].bodyRegex: %w", i, err)
			}
			needsBody = true
		}

		rules = append(rules, compiledRule{
			urlRegex:   urlRe,
			method:     strings.ToUpper(strings.TrimSpace(r.Method)),
			bodyRegex:  bodyRe,
			webhookURL: r.WebhookURL,
		})
	}

	statusAllow := make(map[int]struct{}, len(config.RequireHTTPStatus))
	for _, code := range config.RequireHTTPStatus {
		statusAllow[code] = struct{}{}
	}

	return &webhookMiddleware{
		name:  name,
		next:  next,
		rules: rules,
		mode:  mode,
		include: struct {
			body    bool
			headers bool
		}{
			body:    config.WebhookIncludeBody,
			headers: config.WebhookIncludeHeaders,
		},
		statusAllow:      statusAllow,
		needsRequestBody: needsBody,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}, nil
}

// Payload is the JSON body sent to each webhook URL.
type Payload struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
	// StatusCode is the upstream HTTP status when webhookMode is after_request; omitted for before_request.
	StatusCode int `json:"statusCode,omitempty"`
}

func (m *webhookMiddleware) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if len(m.rules) == 0 {
		m.next.ServeHTTP(rw, req)
		return
	}

	bodyBytes, err := m.captureRequestBody(req)
	if err != nil {
		http.Error(rw, "Bad Request", http.StatusBadRequest)
		return
	}

	switch m.mode {
	case WebhookModeBeforeRequest:
		m.runBefore(rw, req, bodyBytes)
	default:
		m.runAfter(rw, req, bodyBytes)
	}
}

func (m *webhookMiddleware) captureRequestBody(req *http.Request) ([]byte, error) {
	if !m.needsRequestBody {
		return nil, nil
	}

	if req.Body == nil {
		return []byte{}, nil
	}

	const maxBody = 1 << 20 // 1 MiB cap to protect memory; document in README
	limited := io.LimitReader(req.Body, maxBody+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(data) > maxBody {
		return nil, fmt.Errorf("request body exceeds %d bytes", maxBody)
	}
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

func (m *webhookMiddleware) runBefore(rw http.ResponseWriter, req *http.Request, body []byte) {
	urlStr := requestURL(req)
	for _, rule := range m.rules {
		if !ruleMatches(rule, req, urlStr, body) {
			continue
		}
		payload := m.buildRequestPayload(req, urlStr, body)
		m.dispatch(rule.webhookURL, payload)
	}
	m.next.ServeHTTP(rw, req)
}

func (m *webhookMiddleware) runAfter(rw http.ResponseWriter, req *http.Request, body []byte) {
	if m.include.body {
		wrapped := &bufferedResponseWriter{ResponseWriter: rw}
		m.next.ServeHTTP(wrapped, req)
		m.finishAfterWebhook(req, body, wrapped.statusCode, wrapped.Header(), wrapped.body.Bytes())
		_ = wrapped.flush()
		return
	}

	tw := &trackingResponseWriter{ResponseWriter: rw}
	m.next.ServeHTTP(tw, req)
	status := tw.statusCode
	if status == 0 {
		status = http.StatusOK
	}
	m.finishAfterWebhook(req, body, status, tw.Header(), nil)
}

func (m *webhookMiddleware) finishAfterWebhook(req *http.Request, body []byte, status int, respHeader http.Header, respBody []byte) {
	if status == 0 {
		status = http.StatusOK
	}

	if len(m.statusAllow) > 0 {
		if _, ok := m.statusAllow[status]; !ok {
			return
		}
	}

	urlStr := requestURL(req)
	for _, rule := range m.rules {
		if !ruleMatches(rule, req, urlStr, body) {
			continue
		}
		payload := m.buildAfterPayloadFromResponse(urlStr, status, respHeader, respBody)
		m.dispatch(rule.webhookURL, payload)
	}
}

func ruleMatches(rule compiledRule, req *http.Request, urlStr string, body []byte) bool {
	if !rule.urlRegex.MatchString(urlStr) {
		return false
	}

	if rule.method != "" && !strings.EqualFold(req.Method, rule.method) {
		return false
	}

	if rule.bodyRegex != nil && !rule.bodyRegex.Match(body) {
		return false
	}

	return true
}

func (m *webhookMiddleware) buildRequestPayload(req *http.Request, urlStr string, body []byte) Payload {
	p := Payload{
		URL:     urlStr,
		Headers: map[string]string{},
		Body:    "",
	}
	if m.include.headers {
		p.Headers = flattenHeaders(req.Header)
	}
	if m.include.body {
		p.Body = string(body)
	}
	return p
}

func (m *webhookMiddleware) buildAfterPayloadFromResponse(urlStr string, statusCode int, respHeader http.Header, respBody []byte) Payload {
	p := Payload{
		URL:        urlStr,
		StatusCode: statusCode,
		Headers:    map[string]string{},
		Body:       "",
	}

	headerSource := respHeader
	bodyBytes := respBody
	if m.include.body && len(respBody) > 0 {
		decoded, newHdr, ok := decodeCompressedBodyForWebhook(respHeader, respBody)
		if ok {
			bodyBytes = decoded
			headerSource = newHdr
		}
	}

	if m.include.headers {
		p.Headers = flattenHeaders(headerSource)
	}
	if m.include.body {
		p.Body = string(bodyBytes)
	}
	return p
}

func (m *webhookMiddleware) dispatch(url string, payload Payload) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), m.client.Timeout)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", fmt.Sprintf("traefik-plugin-webhooks (%s)", m.name))

		resp, err := m.client.Do(req)
		if err != nil {
			return
		}
		_ = resp.Body.Close()
	}()
}

type bufferedResponseWriter struct {
	http.ResponseWriter

	statusCode  int
	wroteHeader bool
	body        bytes.Buffer
}

func (b *bufferedResponseWriter) WriteHeader(code int) {
	if b.wroteHeader {
		return
	}
	b.statusCode = code
	b.wroteHeader = true
	b.ResponseWriter.Header().Del("Content-Length")
	b.ResponseWriter.WriteHeader(code)
}

func (b *bufferedResponseWriter) Write(p []byte) (int, error) {
	if !b.wroteHeader {
		b.WriteHeader(http.StatusOK)
	}
	return b.body.Write(p)
}

func (b *bufferedResponseWriter) flush() error {
	_, err := io.Copy(b.ResponseWriter, bytes.NewReader(b.body.Bytes()))
	return err
}

type trackingResponseWriter struct {
	http.ResponseWriter

	statusCode  int
	wroteHeader bool
}

func (t *trackingResponseWriter) WriteHeader(code int) {
	if t.wroteHeader {
		return
	}
	t.statusCode = code
	t.wroteHeader = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingResponseWriter) Write(p []byte) (int, error) {
	if !t.wroteHeader {
		t.WriteHeader(http.StatusOK)
	}
	return t.ResponseWriter.Write(p)
}

func (t *trackingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := t.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("%T is not a http.Hijacker", t.ResponseWriter)
	}

	return hijacker.Hijack()
}

func (t *trackingResponseWriter) Flush() {
	if flusher, ok := t.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (b *bufferedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := b.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("%T is not a http.Hijacker", b.ResponseWriter)
	}

	return hijacker.Hijack()
}

func (b *bufferedResponseWriter) Flush() {
	if flusher, ok := b.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func requestURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = strings.Split(proto, ",")[0]
		scheme = strings.TrimSpace(scheme)
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	return scheme + "://" + host + r.URL.RequestURI()
}

func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vals := range h {
		if len(vals) == 0 {
			continue
		}
		canonical := textproto.CanonicalMIMEHeaderKey(k)
		out[canonical] = strings.Join(vals, ", ")
	}
	return out
}
