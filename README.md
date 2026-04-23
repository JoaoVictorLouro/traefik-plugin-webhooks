# Traefik Webhooks (middleware plugin)

Traefik middleware that matches incoming traffic against rules (URL, method, optional request body) and sends **asynchronous** `HTTP POST` webhooks with a small JSON payload. Use it for audit trails, automation, or integrations without blocking the client on your webhook sink.

Use this to trigger webhooks on any application or service that does not support Webhook! 🎉

The plugin follows the same packaging model as community middleware plugins such as [traefik/plugin-rewritebody](https://github.com/traefik/plugin-rewritebody).

## Features

Here is a list of features: (current [x], planned [ ], and potential `?`)

* [x] Rule-based matching: URL regex, optional HTTP method, optional request-body regex
* [x] Multiple rules: each rule is evaluated independently; several can match one request
* [x] `before_request` mode: webhook sees the client request (URL, optional headers/body)
* [x] `after_request` mode: webhook can include upstream response headers, body, and status
* [x] Optional `requireHttpStatus` filter for `after_request` (only fire on selected status codes)
* [x] Best-effort delivery in a background goroutine (does not add retries or block the client for webhook I/O)
* [x] For `after_request`, optional decompression of response bodies in the webhook JSON when upstream sends `gzip` or `deflate`
* [?] Brotli (`br`): not decoded for webhooks; payload uses raw wire bytes and headers stay as sent (same constraint as other Yaegi community plugins without non-stdlib decoders)

## Configuration

### Static

Register the plugin in Traefik’s static configuration. The `moduleName` value must match **`go.mod` exactly** (including casing): `github.com/JoaoVictorLouro/traefik-plugin-webhooks`. A wrong module string leads to catalog errors such as `Unknown plugin` or HTTP 404 when Traefik resolves the module.

```yaml
experimental:
  plugins:
    webhooks:
      moduleName: "github.com/JoaoVictorLouro/traefik-plugin-webhooks"
      version: "v0.4.0"
```

If you use the file provider in TOML, the equivalent block is `[experimental.plugins.webhooks]` with `modulename` / `version` per the [Traefik documentation](https://doc.traefik.io/traefik/) for your version.

**Compatibility:** Traefik `>=2.10.0` (same as the plugin catalog entry in `.traefik.yml`).

### Dynamic

Define a [middleware](https://doc.traefik.io/traefik/middlewares/overview/) that uses the plugin, then attach it to routers that should emit webhooks.

The example below fires **after** the upstream responds, includes the response body and omits headers, and only runs for HTTP `200`, `201`, or `204`. Two rules post to different URLs when they match.

```yaml
http:
  routers:
    my-router:
      rule: "Host(`app.example.com`)"
      middlewares:
        - "hooks"
      service: "my-service"

  middlewares:
    hooks:
      plugin:
        webhooks:
          # before_request (default) or after_request
          webhookMode: after_request
          webhookIncludeBody: true
          webhookIncludeHeaders: false
          # Only used when webhookMode is after_request; empty = any status
          requireHttpStatus: [200, 201, 204]
          rules:
            - urlRegex: "^https://app\\.example\\.com/api/orders/.*"
              method: POST
              bodyRegex: "checkout"
              webhookUrl: "https://hooks.example.com/order-events"
            - urlRegex: "^https://app\\.example\\.com/admin/.*"
              method: DELETE
              webhookUrl: "https://hooks.example.com/security"

  services:
    my-service:
      loadBalancer:
        servers:
          - url: "http://127.0.0.1:8080"
```

#### Middleware fields

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `rules` | list | yes (for useful behavior) | Rule entries (see below). |
| `rules[].urlRegex` | string | no | Go [`regexp`](https://pkg.go.dev/regexp) against the synthesized request URL (see [Process](#process)). Omit or use `""` to match any URL. |
| `rules[].method` | string | no | HTTP method filter, case-insensitive; empty = any method. |
| `rules[].bodyRegex` | string | no | If set, the request body must match this regex; triggers buffering (see [Limits](#limits-and-caveats)). |
| `rules[].webhookUrl` | string | yes | URL that receives `POST` with JSON when the rule matches. |
| `webhookIncludeBody` | bool | no | When `true`, JSON includes a `body` string (see payload table below). |
| `webhookIncludeHeaders` | bool | no | When `true`, JSON includes a `headers` object. |
| `webhookMode` | string | no | `before_request` (default) or `after_request`. |
| `requireHttpStatus` | []int | no | Only for `after_request`: when non-empty, webhooks run only for these response codes. |

## How does this work?

### Process

#### Matching

Each `rules[]` entry is evaluated on its own. If it matches, **one** `POST` is sent to that rule’s `webhookUrl`. Multiple rules may match the same request.

A rule matches when **all** of the following hold:

- **`urlRegex`**: matched against a full URL built as `scheme + "://" + host + RequestURI()`. Scheme prefers `X-Forwarded-Proto` (first value) when present, then TLS. An empty pattern compiles to `""`, which in Go matches any string.
- **`method`**: case-insensitive equality with the request method, or any method if empty.
- **`bodyRegex`**: if non-empty, the **request** body must match. If **any** rule sets `bodyRegex`, the plugin buffers the request body (see [Limits](#limits-and-caveats)) and restores `req.Body` so the upstream still sees the original payload.

For `webhookMode: after_request`, `requireHttpStatus` is applied after the upstream runs: if the list is non-empty, the response status must be listed; if empty, any status is allowed (subject to rule matching).

#### Webhook payload

Every webhook is an `HTTP POST` with `Content-Type: application/json` and this shape:

```json
{
  "url": "https://app.example.com/api/orders?id=1",
  "headers": {
    "Accept": "application/json",
    "X-Request-Id": "abc"
  },
  "body": "raw-body-or-empty-string",
  "statusCode": 201
}
```

`statusCode` is the **upstream** HTTP status and appears only for `after_request`. For `before_request` it is omitted (and omitted from JSON when zero).

Top-level flags control what is copied into `headers` and `body`:

| `webhookMode` | `url` | `headers` when `webhookIncludeHeaders: true` | `body` when `webhookIncludeBody: true` | `statusCode` |
| --- | --- | --- | --- | --- |
| `before_request` | Client request URL (same as used for matching) | Incoming **request** headers | Incoming **request** body (UTF-8 string) | omitted |
| `after_request` | Same client URL | **Response** headers from upstream | **Response** body (see compression below) | upstream status |

When an include flag is `false`, the field is still present: `headers` is `{}`, `body` is `""`.

Delivery is **best-effort**: failures are not returned to the client and are not retried automatically. Each outbound webhook uses an HTTP client with a **10 second** timeout. Misconfigured `webhookUrl` values can imply **SSRF** toward internal networks; treat `webhookIncludeHeaders` carefully (`Authorization`, cookies, etc.).

#### Supported responses (`after_request` and compression)

The client always receives the **exact** bytes and headers the upstream produced (including `gzip`, `deflate`, or `br`).

For the **webhook JSON** only, when `webhookIncludeBody` is `true`:

- If `Content-Encoding` lists encodings the plugin understands, it **decompresses a copy** for the JSON `body` string. Supported tokens (case-insensitive; order follows RFC 7231) include **`gzip` / `x-gzip`** and **`deflate` / `x-deflate`** (zlib first, then raw DEFLATE as fallback).
- **`br` (Brotli)** is not decoded: the webhook `body` contains the **raw** wire bytes as a Go string, and compression-related headers stay as on the wire.
- After a successful decode, **`Content-Encoding`** and **`Content-Length`** are removed from the webhook’s `headers` map so the JSON stays self-consistent (plain body).
- If decoding fails (unknown coding, corrupt stream, or size cap), the webhook falls back to **raw** bytes and leaves headers unchanged.

Decompressed webhook body size is capped at **10 MiB** per response to limit zip-bomb style abuse.

### Limits and caveats

- **Request body for `bodyRegex`**: at most **1 MiB** is read. Larger bodies yield **400 Bad Request** to the client and the upstream is not called.
- **`after_request` with `webhookIncludeBody: true`**: the full response is buffered before streaming to the client (similar to rewrite-body style middleware). With `webhookIncludeBody: false`, the response streams through and the webhook still receives status (and headers if enabled) without buffering the body.
- **Yaegi package name**: for local plugins, the Go **package** must be `traefik_plugin_webhooks` (last segment of the module path with hyphens → underscores). A mismatch surfaces as errors like `undefined: traefik_plugin_webhooks` when Traefik loads `New`.

## Local development

```bash
make fmt       # go fmt ./...
make test      # go test -race -cover ./...
make lint      # requires golangci-lint on PATH
```

## Publishing

1. Keep `go.mod` / imports aligned with the public module path.
2. Tag releases with semver, for example `git tag v0.4.0 && git push origin v0.4.0` (required for the Go proxy and Traefik’s `version` field).
3. The **Release** workflow runs tests and creates a GitHub release.
4. Point Traefik’s static plugin stanza at the new tag using the exact `moduleName` from `go.mod`.

### Traefik Plugin Catalog ([plugins.traefik.io](https://plugins.traefik.io/create))

Catalog downloads power `experimental.plugins`; indexing can lag roughly **once per day**. Until the module is indexed, Traefik may show `Unknown plugin`.

Typical requirements:

- Public GitHub repo that is **not** a fork of another catalog plugin.
- Repository topic **`traefik-plugin`**.
- **`.traefik.yml`** at the repo root with valid **`testData`**, plus **`go.mod`** at the root.
- **Git tags** for each referenced version.
- Non–stdlib dependencies must be **vendored** (this plugin is stdlib-only).

You can sanity-check GitHub metadata without editing the repo:

```bash
gh repo view --json isFork,repositoryTopics
gh release list
```

## License

MIT, see `LICENSE`.
