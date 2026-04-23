# Traefik Webhooks (middleware plugin)

Traefik middleware that evaluates each incoming request against a list of rules and, for every rule that matches, issues an **asynchronous** `HTTP POST` to a configured webhook URL. The POST body is always JSON with this shape:

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

`statusCode` is the **upstream HTTP status** and is only set for `webhookMode: after_request`. It is omitted for `before_request` (and omitted in JSON when zero).

The implementation follows the same packaging conventions as community middleware plugins such as [traefik/plugin-rewritebody](https://github.com/traefik/plugin-rewritebody).

## When to use this

- **Audit or automation hooks** that should not block the upstream service beyond normal proxy work (webhooks are fired in a background goroutine).
- **before_request** notifications when you care about the client request as Traefik sees it (method, path, headers, body).
- **after_request** notifications when you also need the **upstream response** headers and body in the webhook payload, optionally gated by HTTP status codes.

## Matching model

Each `rules[]` entry is evaluated **independently**. If an entry matches, its `webhookUrl` receives one POST. Multiple entries may match the same request.

An entry matches when **all** of the following hold:

| Field | Behavior |
| --- | --- |
| `urlRegex` | Go [`regexp`](https://pkg.go.dev/regexp) matched against a synthesized full URL: `scheme + "://" + host + RequestURI()`. The scheme prefers `X-Forwarded-Proto` (first value) when present, then TLS. An empty pattern compiles to `""` which matches any string in Go. |
| `method` | Case-insensitive equality with the request method (`GET`, `POST`, …). Empty means “any method”. |
| `bodyRegex` | When non-empty, the **request** body must match this regex. When any rule sets `bodyRegex`, the plugin buffers the request body (see limits below) and **restores** `req.Body` so upstream handlers still see the original payload. |

`requireHttpStatus` applies only when `webhookMode` is `after_request`. When the list is **non-empty**, webhooks run only if the upstream response status is one of the listed integers. When the list is **empty**, any response status is eligible (subject to the usual rule matching).

## Webhook payload semantics

Top-level flags control what is copied into `headers` and `body`:

| `webhookMode` | `url` | `headers` (when `webhookIncludeHeaders` is `true`) | `body` (when `webhookIncludeBody` is `true`) | `statusCode` |
| --- | --- | --- | --- | --- |
| `before_request` | Client request URL (see matching) | Incoming **request** headers | Incoming **request** body bytes (UTF-8 string) | omitted |
| `after_request` | Same client request URL | **Response** headers produced by the upstream handler | **Response** body captured from the upstream handler (see compression below) | upstream **response** status (integer) |

When a boolean include flag is `false`, the field is still present but is an empty object (`headers`) or empty string (`body`).

### Compressed responses (`Content-Encoding`)

For `after_request` with `webhookIncludeBody: true`, the client still receives the **exact wire bytes** produced by the upstream handler (for example `gzip`, `deflate`, or `br`), together with the original `Content-Encoding` header.

The webhook JSON payload is different on purpose:

- If `Content-Encoding` lists encodings this plugin understands, the plugin **decompresses** a copy of those bytes for the JSON `body` string (UTF-8). Supported tokens (case-insensitive, comma-separated list order follows RFC 7231) are **`gzip` / `x-gzip`** and **`deflate` / `x-deflate`** (zlib-wrapped first, then raw DEFLATE as a fallback).
- **`br` (Brotli)** is **not** decoded for webhooks: Traefik runs community middleware plugins with [Yaegi](https://github.com/traefik/yaegi), which only resolves the **Go standard library** for third-party imports from the plugin module. A Brotli decoder would add a non-stdlib dependency and fails to load (see Traefik error: `unable to find source related to`). If the response is `br`, the webhook `body` falls back to the **raw wire bytes** (as a Go string) and `Content-Encoding` headers are left unchanged.
- On successful decode, **`Content-Encoding` and `Content-Length` are removed** from the webhook’s `headers` map so the payload stays self-consistent (plain body, no compression metadata).
- If decoding fails (unknown coding, corrupt stream, or size limit exceeded), the webhook falls back to the **raw** bytes as a Go string (same behavior as before compression support) and headers are left unchanged.

Decompressed output is capped at **10 MiB** per response to reduce zip-bomb risk.

## Operational limits and caveats

- **Request body cap for regex matching**: when any rule defines `bodyRegex`, the plugin reads at most **1 MiB** of the request body to keep memory bounded. Larger bodies return **400 Bad Request** to the client and the upstream is not called.
- **`after_request` response bodies**: when `webhookIncludeBody` is `true`, the upstream response body is fully buffered before it is written to the client (similar to [plugin-rewritebody](https://github.com/traefik/plugin-rewritebody)). When `webhookIncludeBody` is `false`, the response streams through normally and only status/headers are considered for the webhook payload. Compression handling for webhooks is described in the previous subsection.
- **Delivery guarantees**: webhook delivery is best-effort. Errors are not surfaced to the client and are not retried automatically.
- **Security**: webhook URLs are arbitrary HTTP endpoints. Misconfiguration can create **SSRF** risk toward internal networks. Headers may contain secrets (`Authorization`, cookies); keep `webhookIncludeHeaders` off unless you control the sink.

## Configuration reference

### Static configuration (Traefik v2 / v3 experimental plugins)

Adjust the module path if you fork or republish this repository.

The value of `modulename` must match **`go.mod` exactly** (including letter case). It is `github.com/JoaoVictorLouro/traefik-plugin-webhooks`, which is not the same string as the browser URL `github.com/JoaoVictorLouro/traefik-plugin-webhooks`. Using the wrong casing leads to catalog errors such as `Unknown plugin` / HTTP 404 when Traefik resolves the module.

```toml
[experimental.plugins.webhooks]
  modulename = "github.com/JoaoVictorLouro/traefik-plugin-webhooks"
  version = "v0.2.0"
```

YAML equivalents follow the Traefik file provider documentation for your edition.

### Dynamic configuration

```toml
[http.routers.my-router]
  rule = "Host(`app.example.com`)"
  middlewares = ["hooks"]
  service = "api"

[http.middlewares.hooks.plugin.webhooks]
  webhookMode = "after_request"
  webhookIncludeBody = true
  webhookIncludeHeaders = false
  requireHttpStatus = [200, 201, 204]

  [[http.middlewares.hooks.plugin.webhooks.rules]]
    urlRegex = "^https://app\\.example\\.com/api/orders/.*"
    method = "POST"
    bodyRegex = "checkout"
    webhookUrl = "https://hooks.example.com/order-events"

  [[http.middlewares.hooks.plugin.webhooks.rules]]
    urlRegex = "^https://app\\.example\\.com/admin/.*"
    method = "DELETE"
    webhookUrl = "https://hooks.example.com/security"
```

#### Fields

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `rules` | list | yes (for non-no-op behavior) | Rule entries (see below). |
| `rules[].urlRegex` | string | yes | Regex applied to the synthesized request URL. |
| `rules[].method` | string | no | HTTP method filter; empty matches all methods. |
| `rules[].bodyRegex` | string | no | Regex applied to the raw request body; omit or empty to skip body matching. |
| `rules[].webhookUrl` | string | yes | Target for successful matches. |
| `webhookIncludeBody` | bool | no | Populate JSON `body` when `true`. |
| `webhookIncludeHeaders` | bool | no | Populate JSON `headers` when `true` (note the spelling). |
| `webhookMode` | string | no | `before_request` (default) or `after_request`. |
| `requireHttpStatus` | []int | no | Only for `after_request`. When non-empty, webhooks fire only for listed response statuses. |

## Local development

```bash
make fmt       # go fmt ./...
make test      # go test -race -cover ./...
make lint      # requires golangci-lint on PATH
```

For **Traefik Yaegi** (local plugins and most hub middleware plugins), the Go **package name** must match the last segment of the module path with hyphens replaced by underscores. For `github.com/JoaoVictorLouro/traefik-plugin-webhooks` this repository uses `package traefik_plugin_webhooks`. A mismatch produces errors such as `undefined: traefik_plugin_webhooks` when Traefik evaluates `New`.

## Publishing

1. Update `go.mod` / imports if you change the module path.
2. Tag a release: `git tag v0.2.0 && git push origin v0.2.0` (semver `v…` tags are required for the Go module proxy and for Traefik’s `version` field).
3. The **Release** workflow runs tests and creates a GitHub release with generated notes.
4. Point Traefik’s static plugin stanza at the new semver tag, using the **exact** `go.mod` module path in `modulename` (see static configuration above).

### Traefik Plugin Catalog ([plugins.traefik.io](https://plugins.traefik.io/create))

Downloads for `experimental.plugins` go through the catalog, not directly from GitHub release assets. Until the repository is indexed, Traefik may report `Unknown plugin` for your module.

Requirements (from Traefik’s publishing docs):

- Public GitHub repository that is **not** a fork of another catalog plugin.
- Repository topic **`traefik-plugin`** on GitHub.
- **`.traefik.yml`** at the repository root with valid **`testData`**, and **`go.mod`** at the root.
- **Git tags** for each version you reference in Traefik.
- Non–standard-library dependencies must be **vendored** in the repository (this plugin uses the stdlib only).

The catalog refreshes on the order of **once per day**. If validation fails, a bot may open an issue in this repository; fix the problem and close the issue so indexing can resume.

You can confirm GitHub-side requirements without changing the repository, for example:

```bash
gh repo view --json isFork,repositoryTopics
gh release list
```

## License

Apache-2.0, see `LICENSE`.
