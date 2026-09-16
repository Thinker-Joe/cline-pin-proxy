# cline-pin-proxy

[简体中文](README.zh-CN.md)

A Go proxy that selects inference providers for models served through the Cline API and converts Cline's wrapped JSON completions to the OpenAI response format. Provider selection supports model IDs with or without the `cline-pass/` prefix, where the model honors Cline's routing fields.

It builds into one executable, uses only the Go standard library, and supports streaming responses, configuration reloads, and an optional admin API.

```text
OpenAI-compatible client → cline-pin-proxy → Cline API → inference provider
```

## Quick start

### Docker Compose

Requires Docker with the Compose v2 plugin (`docker compose version`). Copy the files below into a deployment directory; no repository checkout or local Go installation is needed. The image supports `linux/amd64` and `linux/arm64`. The commands below use Bash.

**1. Create the deployment directory and Compose file.**

```bash
mkdir -p cline-pin-proxy/data
cd cline-pin-proxy
```

Save the following as `docker-compose.yaml`. It uses the same settings as the repository's [docker-compose.yml](docker-compose.yml):

```yaml
services:
  cline-pin-proxy:
    image: ghcr.io/thinker-joe/cline-pin-proxy:latest
    container_name: cline-pin-proxy
    restart: unless-stopped
    ports:
      - "127.0.0.1:8787:8787"
    environment:
      CLINE_PIN_UPSTREAM: ${CLINE_PIN_UPSTREAM:-}
      CLINE_PIN_API_KEY: ${CLINE_PIN_API_KEY:-}
      CLINE_PIN_FORWARD_HEADERS: ${CLINE_PIN_FORWARD_HEADERS:-}
      CLINE_PIN_LOG_LEVEL: ${CLINE_PIN_LOG_LEVEL:-info}
    volumes:
      - ./data:/etc/cline-pin-proxy
    command: ["serve", "-config", "/etc/cline-pin-proxy/config.json"]
    healthcheck:
      test: ["CMD", "/usr/local/bin/cline-pin-proxy", "healthcheck", "-url", "http://127.0.0.1:8787/healthz"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 5s
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
    security_opt:
      - no-new-privileges:true
    read_only: true
    tmpfs:
      - /tmp
```

**2. Create the configuration.**

Save the following as `data/config.json`. Keep any existing configuration if you are updating a deployment.

```json
{
  "api_key": "",
  "admin_token": ""
}
```

Omitting `rules` keeps the [default rules](#default-rules); other omitted fields use application defaults. Each client supplies its own Cline API key. To use one upstream key for all requests, fill in `api_key`. Set `admin_token` only if you need the [admin API](#admin-api).

**3. Start and check the service.**

```bash
docker compose pull
docker compose up -d --wait
docker compose ps
curl -fsS http://127.0.0.1:8787/healthz
```

`--wait` waits for the container healthcheck to pass. The endpoint returns `{"status":"ok"}`; it checks the proxy process, not upstream availability. If startup fails, inspect `docker compose logs --tail=100 cline-pin-proxy`.

**4. Connect a client.**

Use `http://127.0.0.1:8787/v1` as the Base URL and your Cline API key as the API key. See the [test request](#client-configuration) below. Port 8787 is published only on the Docker host's loopback interface. For a client in another container, use a shared Docker network and `http://cline-pin-proxy:8787/v1`; see [sub2api integration](#sub2api-integration).

**Manage the deployment.** Run these commands from the directory containing your Compose file:

| Task | Command |
|---|---|
| View status | `docker compose ps` |
| Follow logs | `docker compose logs -f --tail=100 cline-pin-proxy` |
| Stop temporarily | `docker compose stop` |
| Start again | `docker compose start` |
| Restart the process | `docker compose restart cline-pin-proxy` |
| Remove the container and Compose network | `docker compose down` |

The host directory `data/` is mounted at `/etc/cline-pin-proxy` in the container and remains after `docker compose down`. Edit `data/config.json` to change rules; most settings reload within five seconds by default. See [reloading](#reloading) for startup-only settings and environment overrides.

Validate the file and preview a rule using the running container:

```bash
docker compose exec cline-pin-proxy /usr/local/bin/cline-pin-proxy \
  check -config /etc/cline-pin-proxy/config.json -model deepseek/deepseek-v4-flash
```

To update the image, pull it and recreate the service. This briefly interrupts service and keeps `data/`:

```bash
docker compose pull
docker compose up -d --wait
```

`docker compose restart` does not switch to a newly pulled image. For a local source build, use a repository checkout, uncomment `build: .` in its `docker-compose.yml`, and run `docker compose up -d --build --wait`.

### Binary

Download a binary from [GitHub Releases](https://github.com/Thinker-Joe/cline-pin-proxy/releases), or build with Go 1.23 or later:

```bash
go build -o cline-pin-proxy ./cmd/cline-pin-proxy
cp config.example.json config.json
./cline-pin-proxy serve -config config.json
```

`-config` is explicit: the binary does not automatically load `config.json` from the working directory. Without it, configuration comes from defaults and environment variables.

### Client configuration

| Setting | Value |
|---|---|
| Base URL | `http://127.0.0.1:8787/v1` |
| API key | A Cline API key with access to the requested model |
| Example model | `cline-pass/deepseek-v4.1-flash` |

By default, the proxy forwards the client's `Authorization` header. Set `api_key` or `CLINE_PIN_API_KEY` to use a fixed upstream key instead. A fixed key replaces client credentials; it does **not** enable authentication on the proxy's public API routes.

For a test request, set `CLINE_KEY` to your Cline API key in your shell and run:

```bash
curl -i http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer $CLINE_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"cline-pass/deepseek-v4.1-flash","messages":[{"role":"user","content":"Reply with OK"}],"max_tokens":512}'
```

## Provider routing

Rules match the request's `model` value without requiring a particular namespace. The default `deepseek` rule matches both `cline-pass/deepseek-v4.1-flash` and `deepseek/deepseek-v4-flash`; the `glm-5.3-flash` rule also matches `z-ai/glm-5.3-flash`. The proxy preserves the model ID. These prefixes are part of the JSON value, not the HTTP path: all use `POST /v1/chat/completions`.

Historical tests confirmed provider switching for `deepseek/deepseek-v4-flash`: selecting `deepseek` or `novita` returned `provider: "DeepSeek"` or `"Novita"`. `z-ai/glm-5.3-flash` also passed direct-pipeline probing and an integration request with `z-ai` injected. Some `cline-pass/` models ignored provider filters. See the [verification record](docs/VERIFICATION.md#1-模型管道与上游标识).

Other model IDs can use custom rules. Effective provider selection requires account access to the model and gateway support for its routing fields; a model's presence in `/v1/models` alone does not establish that support.

Cline uses two routing pipelines. Each accepts provider preferences in a different part of the request:

| Pipeline | Backend | Routing fields | Response metadata |
|---|---|---|---|
| `planner` | Vercel AI Gateway | `providerOptions.gateway` | `choices[0].message.provider_metadata.gateway.routing` |
| `direct` | OpenRouter | Top-level `provider` | `provider` |

Cline determines the pipeline for each model. With `pipeline: "auto"`, the proxy writes both sets of fields. For example, a strict DeepSeek rule adds:

```json
{
  "providerOptions": {"gateway": {"only": ["deepseek"]}},
  "provider": {"only": ["deepseek"]}
}
```

Some models ignore these fields. Successful injection does not guarantee that Cline used the requested provider. Check the upstream routing metadata when validating a rule; wrapped responses place that metadata under `data`.

### Default rules

Rules use case-insensitive substring matching. The first match wins.

| Model substring, in order | Provider | Mode |
|---|---|---|
| `deepseek` | `deepseek` | `strict` |
| `glm-5.3-flash` | `relace` | `strict` |
| `glm-5.3` | `friendli` | `strict` |

Keep the flash rule before `glm-5.3`, which also matches flash model names. The two GLM models use different provider lists; a single broad `glm` rule can select an unsupported provider.

The GLM defaults were chosen from measurements on 2026-09-16: `friendli` returned the first stream bytes in 0.31–0.35 seconds for `glm-5.3`, and `relace` in 0.72–0.97 seconds for `glm-5.3-flash`, with four successful requests each. These are historical observations, not latency guarantees or quality comparisons. Third-party providers may use quantized models; the tests did not establish numerical precision or output quality.

Provider IDs, known filtering exceptions, and full measurements are recorded in [Verification notes (Chinese)](docs/VERIFICATION.md).

### Rule fields

| Field | Values and behavior |
|---|---|
| `name` | Label used in logs and response headers. Defaults to `rule-<index>`. |
| `model` | Required model ID or substring to match. |
| `match` | `contains` (default), `prefix`, or `exact`. All are case-insensitive. |
| `pipeline` | `auto` (default) writes both pipelines; `planner` or `direct` writes only that pipeline. |
| `mode` | `strict` (default) writes `only` with the first provider. `preferred` writes `order` and allows fallback. |
| `upstreams` | Required provider ID array. `strict` uses only the first item; `preferred` requires at least two. |
| `sort` | Optional: `cost`, `ttft`, or `tps`. On the direct pipeline these map to `price`, `latency`, and `throughput`. |

On the selected pipelines, `strict` removes an existing `order`; `preferred` removes existing `only` and `allow_fallbacks` fields. Other provider options are preserved. `sort` replaces the caller's value only when configured. Adding `sort` to a strict rule does not enable fallback or remove `only`; rules cannot express sorting alone.

A file's `rules` field replaces the entire default rule list. Use `"rules": []` to disable injection. See [config.example.json](config.example.json) for a complete configuration matching the defaults.

## Response compatibility

Cline has been observed returning non-streaming completions in this format:

```json
{"data":{"choices":[{"message":{"role":"assistant","content":"OK"}}]},"success":true}
```

Clients that expect top-level `choices` cannot read the completion. By default, the proxy returns the inner `data` object and sets `X-Cline-Pin-Unwrapped: data-envelope`.

Unwrapping applies only to responses with a JSON content type and all of these properties:

- The top level has no `choices` field.
- `data` is an object.
- `data.choices` is a non-empty array.

Other response bodies remain unchanged. JSON responses are buffered up to an 8 MiB limit, with one extra byte read to detect overflow. Larger responses are forwarded unchanged and marked `X-Cline-Pin-Unwrapped: skipped-too-large`. SSE (`text/event-stream`) responses are forwarded and flushed as chunks arrive, without JSON parsing or waiting for the complete response.

Set `"unwrap_data_envelope": false` to disable this conversion. The setting applies to JSON responses on all forwarded routes, independent of whether a provider rule matched.

## Configuration

Precedence is **non-empty environment variables > configuration file > defaults**. Whitespace-only environment values do not override the file. Command flags such as `serve -listen` and `probe -api-key` take precedence for that command.

| JSON field | Default | Environment variable |
|---|---|---|
| `listen` | `127.0.0.1:8787` | `CLINE_PIN_LISTEN` |
| `upstream` | `https://api.cline.bot/api/v1` | `CLINE_PIN_UPSTREAM` |
| `api_key` | Empty; forward client credentials | `CLINE_PIN_API_KEY` |
| `forward_headers` | `["x-client-type"]` | `CLINE_PIN_FORWARD_HEADERS` |
| `probe_headers` | `{"x-client-type":"cline-cli"}` | `CLINE_PIN_PROBE_HEADERS` |
| `max_body_bytes` | `67108864` (64 MiB) | `CLINE_PIN_MAX_BODY_BYTES` |
| `watch_seconds` | `5`; `0` disables polling | `CLINE_PIN_WATCH_SECONDS` |
| `admin_token` | Empty; admin API disabled by default | `CLINE_PIN_ADMIN_TOKEN` |
| `admin_allow_unauthenticated` | `false` | `CLINE_PIN_ADMIN_ALLOW_UNAUTHENTICATED` |
| `unwrap_data_envelope` | `true` | `CLINE_PIN_UNWRAP_DATA_ENVELOPE` |
| `rules` | [Default rules](#default-rules) | `CLINE_PIN_RULES` |

`CLINE_PIN_FORWARD_HEADERS` accepts comma-separated header names. `CLINE_PIN_PROBE_HEADERS` accepts comma-separated `name: value` pairs. `CLINE_PIN_RULES` accepts a JSON array. `CLINE_PIN_LOG_LEVEL` sets `debug`, `info`, `warn`, or `error` and has no JSON equivalent.

`upstream` must be an HTTP(S) URL with a host and no query or fragment. Configuration must be a JSON object; `null` is not valid. Invalid rule JSON and invalid numeric or boolean environment values are rejected by the normal configuration loader. The missing-file startup exception is recorded under [known implementation limits](docs/CODE_REVIEW.md#current-implementation-limits).

### Reloading

The server checks the file's modification time at the configured interval. A valid update replaces the active configuration. An invalid or missing file leaves the last valid configuration active; repeated identical errors are logged once, followed by a recovery message when loading succeeds.

`listen`, `watch_seconds`, and the log level take effect at startup. Other configuration fields can reload, including the admin token. The Docker image sets `CLINE_PIN_LISTEN=0.0.0.0:8787` so that port publishing works; this overrides the example file's loopback address inside the container.

Environment overrides still apply after reload. To manage a field through the file, remove its non-empty environment override. A container's environment changes only when it is recreated. Restarting a process rereads its file, but `docker compose up -d` alone does not restart a container merely because a mounted file changed.

When `serve -config` names a missing file, the server starts with defaults and environment settings and watches for the file to appear. An existing invalid file prevents startup. The `check` and `probe` commands require an explicitly named file to exist.

### Admin API

Set `admin_token` to enable these endpoints. Authenticate with `Authorization: Bearer <token>` or `X-Admin-Token: <token>`.

| Method | Path | Behavior |
|---|---|---|
| `GET` | `/admin/config` | Read the loaded configuration, omitting `api_key` and `admin_token`. |
| `GET` | `/admin/rules` | Read `{"rules":[...]}`. |
| `PUT` | `/admin/rules` | Replace rules in memory and attempt to save them to the file. |
| `POST` | `/admin/probe` | Probe with `{"model":"...","pipeline":"auto"}`. |
| `POST` | `/admin/reload` | Reload the file immediately. |

Without a token, these routes return 404 unless `admin_allow_unauthenticated` is explicitly enabled. When a token is set, missing or incorrect credentials return 401 even if that flag is enabled. Allow unauthenticated access only in a trusted environment.

For shell examples below, set `ADMIN_TOKEN` to the configured admin token:

```bash
curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://127.0.0.1:8787/admin/rules

# Save, edit, and submit the complete rule list.
curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://127.0.0.1:8787/admin/rules > rules.json
# Edit rules.json before running the next command.
curl -fsS -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' --data-binary @rules.json \
  http://127.0.0.1:8787/admin/rules
```

`PUT` accepts a bare array or `{"rules":[...]}`. It rejects `null`, null elements, unknown rule fields, and trailing data. Only `[]` explicitly clears the list.

A successful response includes `applied`, `persisted`, and the current `rules` array. If saving fails, it also includes `persist_error` and `hint`. Unsaved rules can be replaced by a later reload or lost on restart. `CLINE_PIN_RULES` continues to override file rules after loading; remove that override before managing rules through the API and inspect the returned array.

Saving changes only the `rules` value, preserves other JSON values and existing file permission bits, and does not copy environment secrets into the file. Atomic replacement requires a writable directory. The service runs as UID/GID 65532 in Docker; on Linux, `sudo chown -R 65532:65532 data` grants it ownership of the mounted directory. If atomic replacement is unavailable, the proxy may write in place and logs a warning. In-place writes are not atomic. Disk-full and I/O errors do not trigger that fallback.

`POST /admin/probe` uses the configured upstream key and probe headers. The admin token is not an upstream credential.

## Probe and validate

With `CLINE_PIN_API_KEY` set, inspect providers reported by the gateway:

```bash
./cline-pin-proxy probe -model cline-pass/deepseek-v4.1-flash
./cline-pin-proxy probe -model deepseek/deepseek-v4-flash -H 'x-client-type: cline-cli'
```

The probe injects `only: ["__probe__"]` and parses the routing error. Models that honor the filter reject the request before inference. Models that ignore it may generate a response and incur charges. The returned provider list can be incomplete, and a listed provider can still fail a real request.

Use `-pipeline planner` or `-pipeline direct` to restrict injection. Probe requests include `x-client-type: cline-cli` by default; some models return 403 without it. A probe prints a suggested exact-match rule, selecting the first reported provider. Review that choice before using it; replacing `rules` also replaces all existing rules.

Validate configuration and preview a match without contacting Cline:

```bash
./cline-pin-proxy check -config config.json -model cline-pass/glm-5.3-flash
```

Run `./cline-pin-proxy help` for commands and `./cline-pin-proxy <command> -h` for flags. CLI messages are currently in Chinese.

## sub2api integration

For a Cline Pass account configured as an OpenAI-compatible API-key account in sub2api, change its base URL from `https://api.cline.bot/api/v1` to the proxy's `/v1` URL.

- If both services run in containers, connect them to a shared Docker network and use `http://cline-pin-proxy:8787/v1`. A container's `127.0.0.1` refers to itself.
- Keep the Cline Pass key on the account unless the proxy has a fixed upstream key.
- If the account sets `x-client-type: cline-cli`, the default proxy configuration forwards it. Normal proxy requests do not add that header themselves.
- Requests to `/v1/responses` and other supported paths are forwarded without provider injection, allowing sub2api to check upstream protocol support.

## Diagnostics

| Header | Meaning |
|---|---|
| `X-Cline-Pin-Rule` | Rule applied to a chat completion request; `none` if no rule was applied. |
| `X-Cline-Pin-Upstreams` | Configured provider list, joined with `>`. A strict rule still uses only the first entry. |
| `X-Cline-Pin-Mode` | `strict` or `preferred`. |
| `X-Cline-Pin-Note` | Why injection was skipped: no match, unreadable model, or injection failure. |
| `X-Cline-Pin-Unwrapped` | `data-envelope` when converted; `skipped-too-large` when over the buffer limit. |

Pinning headers report proxy decisions, not the provider Cline actually used. They are not added to ordinary passthrough routes. Upstream `x-*` headers are forwarded when present; the proxy does not generate an actual-provider header.

Logs use Go's `slog` text format on stderr. A pinned request includes `model`, `rule`, `upstreams`, `mode`, and `pipeline`:

```text
level=INFO msg="pinned request" model=cline-pass/deepseek-v4.1-flash rule=deepseek upstreams=deepseek mode=strict pipeline=auto
```

If a non-streaming completion appears empty, check `X-Cline-Pin-Unwrapped` and the response's `choices` location. Also allow enough output tokens: a recorded test with `max_tokens: 24` produced `empty response content` errors on five of six reasoning models; all six succeeded with 512. This is separate from response wrapping.

## Behavior and security

- Provider fields are injected only for `POST /v1/chat/completions`, `/chat/completions`, and `/api/v1/chat/completions`. Other requests under `/v1/` and `/api/v1/` are forwarded without request-body rewriting. `OPTIONS` is handled locally.
- Unmatched or unparseable chat requests retain their original body. Injection failures also fall back to the original body and are marked in the response headers.
- Upstream status codes are preserved. Redirects are returned with `Location` and are not followed. Response bodies are preserved except for the JSON envelope conversion described above; HTTP framing and compression may be handled by Go's transport.
- SSE is copied with a fixed-size buffer and flushed after each read. The proxy has no overall HTTP client timeout; dial and TLS timeouts are 10 seconds each, and the response-header timeout is 120 seconds.
- The normal streaming path aborts the downstream connection if the upstream body ends with a read error. Buffered JSON reads do the same. The oversized JSON fallback has a separate [known limitation](docs/CODE_REVIEW.md#current-implementation-limits).
- Each forwarded request uses one configuration snapshot. Injection preserves JSON number precision and does not HTML-escape `<`, `>`, or `&`.
- Request bodies are limited to 64 MiB by default, including on passthrough routes. Over-limit reads return 413; unknown-length bodies may already have been partially sent upstream.
- API paths are validated before forwarding. Encoded paths and traversal segments are rejected by the proxy path validator. Requests outside supported routes return 404.
- Request headers are limited to `Content-Type`, `Accept`, `Authorization`, `User-Agent`, and `forward_headers`. Response headers are limited to `Content-Type`, `Content-Encoding`, `Cache-Control`, `Retry-After`, `Location`, and `x-*`; upstream `Content-Length` is not copied. Buffered JSON responses get a computed length.
- The default listener and Compose host port use loopback. Public API routes have no separate client authentication. Protect access with network controls or an authenticated reverse proxy if exposing them beyond trusted clients.
- Keys stored in configuration files are plaintext. `config.json` and `data/` are git-ignored. `/admin/config` omits the two key fields but returns `probe_headers`; do not treat it as a general secret-redaction endpoint.

## Development and project notes

See [CONTRIBUTING.md](CONTRIBUTING.md) for the source layout, checks, and release workflow. Historical gateway tests are in [docs/VERIFICATION.md](docs/VERIFICATION.md); the review baseline, fixes, and current limitations are in [docs/CODE_REVIEW.md](docs/CODE_REVIEW.md). These detailed records are in Chinese.

Routing knowledge came from public records in [cline-pass-switcher](https://github.com/munmunjaklin458-afk/cline-pass-switcher), [cpagw-gateway](https://github.com/Stabilize7440/cpagw-gateway), and [dsh-cline-pass](https://github.com/yhshzh/dsh-cline-pass). No source code was copied from those projects. See [NOTICE](NOTICE).

The project focuses on provider routing and response compatibility. It does not include an account pool or web UI.

## License

[MIT](LICENSE).
