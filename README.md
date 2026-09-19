# cline-pin-proxy

[简体中文](README.zh-CN.md)

A Go proxy that makes the Cline API use a chosen inference provider for a given model, and converts Cline's `data`-wrapped JSON completions into the OpenAI response format. Rules match model IDs with or without the `cline-pass/` prefix; whether routing takes effect depends on the model honoring Cline's routing fields.

It builds into one executable, uses only the Go standard library, and supports streaming responses, configuration reloads, and an optional admin API.

```text
OpenAI-compatible client → cline-pin-proxy → Cline API → inference provider
```

## Quick start

The image needs Docker with the Compose v2 plugin (`docker compose version`) — no source checkout and no Go toolchain. It supports `linux/amd64` and `linux/arm64`. The commands below use Bash.

**1. Save this as `docker-compose.yaml`.**

```yaml
services:
  cline-pin-proxy:
    image: ghcr.io/thinker-joe/cline-pin-proxy:latest
    restart: unless-stopped
    ports:
      - "127.0.0.1:8787:8787"
    # environment:
    #   CLINE_PIN_API_KEY: "sk-..."   # optional: one shared upstream key for all requests
```

No configuration file is required: the [default rules](#default-rules) apply, and each client sends its own Cline API key. The image sets `CLINE_PIN_LISTEN=0.0.0.0:8787` so that port publishing works, and includes a healthcheck.

**2. Start and check it.**

```bash
docker compose up -d --wait
curl -fsS http://127.0.0.1:8787/healthz
```

`--wait` returns once the container healthcheck passes. The endpoint returns `{"status":"ok"}` and checks the proxy process, not upstream availability. If startup fails, run `docker compose logs --tail=100 cline-pin-proxy`.

**3. Point a client at it.**

Any client that lets you set an OpenAI-compatible base URL can use the proxy: replace Cline's API base URL `https://api.cline.bot/api/v1` with the proxy URL and change nothing else — the API key and model names stay as they are.

| Setting | Value |
|---|---|
| Base URL | `http://127.0.0.1:8787/v1` (replacing `https://api.cline.bot/api/v1`) |
| API key | Your Cline API key; any non-empty value when `CLINE_PIN_API_KEY` is set |
| Model | `cline-pass/deepseek-v4.1-flash` |

Model IDs are preserved and models without a matching rule are forwarded unchanged, so existing model names keep working. Port 8787 is published only on the Docker host's loopback interface. For a client in another container, connect both to a shared Docker network and use `http://cline-pin-proxy:8787/v1`; see [sub2api integration](#sub2api-integration).

Test request:

```bash
export CLINE_KEY='<your Cline API key>'
curl -i http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer $CLINE_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"cline-pass/deepseek-v4.1-flash","messages":[{"role":"user","content":"Reply with OK"}],"max_tokens":512}'
```

By default the proxy forwards the client's `Authorization` header. A fixed `api_key`/`CLINE_PIN_API_KEY` replaces client credentials; it does **not** add authentication to the proxy's own API routes.

**Managing the container.** Run these from the directory containing your Compose file:

| Task | Command |
|---|---|
| View status | `docker compose ps` |
| Follow logs | `docker compose logs -f --tail=100 cline-pin-proxy` |
| Stop / start | `docker compose stop` / `docker compose start` |
| Remove container and Compose network | `docker compose down` |

To update the image, pull it and recreate the service. This briefly interrupts service:

```bash
docker compose pull
docker compose up -d --wait
```

`docker compose restart` does not switch to a newly pulled image. To build from local source instead, use a repository checkout, uncomment `build: .` in its `docker-compose.yml`, and run `docker compose up -d --build --wait`.

### Persistent configuration

Add this only for custom rules, the admin API, or settings you want to keep across container recreation. The repository's [docker-compose.yml](docker-compose.yml) is the hardened version (data mount, read-only root filesystem, log rotation); its mount and command are:

```yaml
    volumes:
      - ./data:/etc/cline-pin-proxy
    command: ["serve", "-config", "/etc/cline-pin-proxy/config.json"]
```

```bash
mkdir -p data && cp config.example.json data/config.json
docker compose up -d --wait
```

[config.example.json](config.example.json) matches the built-in defaults. Omitting `rules` keeps the [default rules](#default-rules); other omitted fields use application defaults. The file is polled, and most settings reload within five seconds ([Reloading](#reloading)). The `data/` directory survives `docker compose down`.

Validate the file and preview a rule using the running container:

```bash
docker compose exec cline-pin-proxy /usr/local/bin/cline-pin-proxy \
  check -config /etc/cline-pin-proxy/config.json -model deepseek/deepseek-v4-flash
```

### Binary

Download a binary from [GitHub Releases](https://github.com/Thinker-Joe/cline-pin-proxy/releases), or build with Go 1.23 or later:

```bash
go build -o cline-pin-proxy ./cmd/cline-pin-proxy
./cline-pin-proxy serve -config config.json
```

`-config` is explicit: the binary does not automatically load `config.json` from the working directory. Without it, configuration comes from defaults and environment variables.

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

`CLINE_PIN_FORWARD_HEADERS` takes comma-separated header names, `CLINE_PIN_PROBE_HEADERS` comma-separated `name: value` pairs, and `CLINE_PIN_RULES` a JSON array. `CLINE_PIN_LOG_LEVEL` sets `debug`, `info`, `warn`, or `error` and has no JSON equivalent.

`upstream` must be an HTTP(S) URL with a host and no query or fragment. The configuration must be a JSON object; `null` is not valid. Invalid rule JSON and invalid numeric or boolean environment values fail loading. The missing-file startup exception is recorded under [known implementation limits](docs/CODE_REVIEW.md#current-implementation-limits).

### Reloading

The server polls the file's modification time at the configured interval. A valid update replaces the active configuration; an invalid or missing file keeps the last valid one, with repeated identical errors logged once and a recovery message once loading succeeds.

Editing a mounted file needs no container restart: the watcher reloads it in place. `listen`, `watch_seconds`, and the log level apply at startup only; everything else can reload, including the admin token. Environment overrides still win after a reload, so remove a field's override before managing it through the file, and recreate a container to change its environment. `serve -config` with a missing file starts on defaults and environment settings and waits for the file to appear; an existing invalid file prevents startup. `check` and `probe` require the file they name to exist.

### Admin API

Set `admin_token` to enable these endpoints, and authenticate with `Authorization: Bearer <token>` or `X-Admin-Token: <token>`.

| Method | Path | Behavior |
|---|---|---|
| `GET` | `/admin/config` | Read the loaded configuration, omitting `api_key` and `admin_token`. |
| `GET` | `/admin/rules` | Read `{"rules":[...]}`. |
| `PUT` | `/admin/rules` | Replace rules in memory and attempt to save them to the file. |
| `POST` | `/admin/probe` | Probe with `{"model":"...","pipeline":"auto"}`. |
| `POST` | `/admin/reload` | Reload the file immediately. |

Without a token these routes return 404 unless `admin_allow_unauthenticated` is explicitly enabled; with a token set, missing or incorrect credentials return 401 even then. Allow unauthenticated access only in a trusted environment.

```bash
curl -fsS -H "Authorization: Bearer $ADMIN_TOKEN" \
  http://127.0.0.1:8787/admin/rules > rules.json
# Edit rules.json, then submit the complete list.
curl -fsS -X PUT -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H 'Content-Type: application/json' --data-binary @rules.json \
  http://127.0.0.1:8787/admin/rules
```

`PUT` accepts a bare array or `{"rules":[...]}` and rejects `null`, null elements, unknown rule fields, and trailing data; only `[]` clears the list. A successful response reports `applied`, `persisted`, and the current `rules`; if saving fails it also returns `persist_error` and `hint`, and unsaved rules can be replaced by a later reload or lost on restart. `CLINE_PIN_RULES` keeps overriding file rules, so remove that override before managing rules through the API and inspect the returned array.

Saving rewrites only the `rules` value, preserves other JSON values and existing file permission bits, and does not copy environment secrets into the file. Atomic replacement needs a writable directory; the container runs as UID/GID 65532, so on Linux use `sudo chown -R 65532:65532 data`. If atomic replacement is unavailable the proxy may write in place and logs a warning; in-place writes are not atomic, and disk-full or I/O errors do not trigger that fallback. `POST /admin/probe` uses the configured upstream key and probe headers; the admin token is not an upstream credential.

## Provider routing

Rules match the request's `model` value; no namespace is required, and the proxy preserves the model ID. The default `deepseek` rule matches both `cline-pass/deepseek-v4.1-flash` and `deepseek/deepseek-v4-flash`, and the `glm-5.3-flash` rule also matches `z-ai/glm-5.3-flash`. These prefixes are part of the JSON value, not the HTTP path: all of them use `POST /v1/chat/completions`.

Cline uses two routing pipelines, each reading provider preferences from a different place:

| Pipeline | Backend | Routing fields | Response metadata |
|---|---|---|---|
| `planner` | Vercel AI Gateway | `providerOptions.gateway` | `choices[0].message.provider_metadata.gateway.routing` |
| `direct` | OpenRouter | Top-level `provider` | `provider` |

Cline decides the pipeline for each model. With `pipeline: "auto"` the proxy writes both sets of fields; a strict DeepSeek rule adds:

```json
{
  "providerOptions": {"gateway": {"only": ["deepseek"]}},
  "provider": {"only": ["deepseek"]}
}
```

Effective selection also needs account access to the model and gateway support for its routing fields; appearing in `/v1/models` does not establish that. Some models ignore these fields entirely, so successful injection does not guarantee Cline used the requested provider — check the upstream routing metadata, which wrapped responses place under `data`. Tests confirmed switching for `deepseek/deepseek-v4-flash` (`deepseek` → `provider: "DeepSeek"`, `novita` → `"Novita"`) and a `z-ai/glm-5.3-flash` integration request, while some `cline-pass/` models ignored the filters. See the [verification record](docs/VERIFICATION.md#1-模型管道与上游标识).

### Default rules

Rules use case-insensitive substring matching, and the first match wins.

| Model substring, in order | Provider | Mode |
|---|---|---|
| `deepseek` | `deepseek` | `strict` |
| `glm-5.3-flash` | `relace` | `strict` |
| `glm-5.3` | `friendli` | `strict` |

`glm-5.3` also matches the flash model name, so the flash rule must stay first; the two GLM models have different provider lists, so a single broad `glm` rule can select an unsupported provider. The GLM defaults come from measurements on 2026-09-16: `friendli` returned the first stream bytes in 0.31–0.35 seconds for `glm-5.3`, and `relace` in 0.72–0.97 seconds for `glm-5.3-flash`, with four successful requests each. These are historical observations, not latency or quality guarantees; third-party providers may serve quantized models. Provider IDs, known filtering exceptions, and full measurements are in the [verification notes (Chinese)](docs/VERIFICATION.md).

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

On the selected pipelines, `strict` removes an existing `order`, and `preferred` removes existing `only` and `allow_fallbacks`; other provider options are preserved, and `sort` replaces the caller's value only when configured. Adding `sort` to a strict rule neither enables fallback nor removes `only`, so rules cannot express sorting alone. A file's `rules` field replaces the entire default list; use `"rules": []` to disable injection. [config.example.json](config.example.json) is a complete configuration matching the defaults.

## Response compatibility

Cline has been observed returning non-streaming completions wrapped like this, which clients that read top-level `choices` cannot parse:

```json
{"data":{"choices":[{"message":{"role":"assistant","content":"OK"}}]},"success":true}
```

By default the proxy returns the inner `data` object and sets `X-Cline-Pin-Unwrapped: data-envelope`. Unwrapping requires a JSON content type plus all of the following:

- The top level has no `choices` field.
- `data` is an object.
- `data.choices` is a non-empty array.

Other bodies are forwarded unchanged. JSON responses are buffered up to an 8 MiB limit, with one extra byte read to detect overflow; larger responses are forwarded unchanged and marked `X-Cline-Pin-Unwrapped: skipped-too-large`. SSE (`text/event-stream`) is forwarded and flushed as chunks arrive, without JSON parsing or waiting for the complete response. Set `"unwrap_data_envelope": false` to disable the conversion; it applies to JSON responses on all forwarded routes, whether or not a rule matched.

## Probe and validate

With `CLINE_PIN_API_KEY` set, list the providers the gateway reports:

```bash
./cline-pin-proxy probe -model cline-pass/deepseek-v4.1-flash
./cline-pin-proxy probe -model deepseek/deepseek-v4-flash -H 'x-client-type: cline-cli'
```

The probe injects `only: ["__probe__"]` and parses the routing error. Models that honor the filter reject the request before inference; models that ignore it may generate a response and incur charges. The returned list can be incomplete, and a listed provider can still fail a real request. Use `-pipeline planner` or `-pipeline direct` to restrict injection; probes send `x-client-type: cline-cli` by default because some models return 403 without it. The printed suggested rule is an exact match on the first reported provider — review that choice, since replacing `rules` replaces all existing rules.

`check` validates configuration and previews a match without contacting Cline:

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

These headers report the proxy's decisions, not the provider Cline actually used, and are not added to ordinary passthrough routes. Upstream `x-*` headers are forwarded when present; the proxy does not generate an actual-provider header.

Logs use Go's `slog` text format on stderr. A pinned request logs `model`, `rule`, `upstreams`, `mode`, and `pipeline`:

```text
level=INFO msg="pinned request" model=cline-pass/deepseek-v4.1-flash rule=deepseek upstreams=deepseek mode=strict pipeline=auto
```

If a non-streaming completion appears empty, check `X-Cline-Pin-Unwrapped` and where `choices` sits, and allow enough output tokens: a recorded test with `max_tokens: 24` produced `empty response content` errors on five of six reasoning models, and all six succeeded with 512. That is separate from response wrapping.

## Behavior and security

- Provider fields are injected only for `POST /v1/chat/completions`, `/chat/completions`, and `/api/v1/chat/completions`. Other requests under `/v1/` and `/api/v1/` are forwarded without request-body rewriting, and `OPTIONS` is handled locally.
- Unmatched or unparseable chat requests retain their original body. Injection failures also fall back to the original body and are marked in the response headers.
- Upstream status codes are preserved. Redirects are returned with `Location` and are not followed. Response bodies are preserved except for the JSON envelope conversion; HTTP framing and compression may be handled by Go's transport.
- SSE is copied with a fixed-size buffer and flushed after each read. The proxy has no overall HTTP client timeout; dial and TLS timeouts are 10 seconds each, and the response-header timeout is 120 seconds.
- The normal streaming path aborts the downstream connection if the upstream body ends with a read error, and buffered JSON reads do the same. The oversized JSON fallback has a separate [known limitation](docs/CODE_REVIEW.md#current-implementation-limits).
- Each forwarded request uses one configuration snapshot. Injection preserves JSON number precision and does not HTML-escape `<`, `>`, or `&`.
- Request bodies are limited to 64 MiB by default, including on passthrough routes. Over-limit reads return 413; a body of unknown length may already have been partly sent upstream.
- API paths are validated before forwarding: encoded paths and traversal segments are rejected, and requests outside supported routes return 404.
- Request headers are forwarded only for `Content-Type`, `Accept`, `Authorization`, `User-Agent`, and `forward_headers`. Response headers are limited to `Content-Type`, `Content-Encoding`, `Cache-Control`, `Retry-After`, `Location`, and `x-*`; upstream `Content-Length` is not copied, and buffered JSON responses get a computed length.
- The default listener and the Compose host port use loopback. Public API routes have no separate client authentication; protect access with network controls or an authenticated reverse proxy if exposing them beyond trusted clients.
- Keys stored in configuration files are plaintext, and `config.json` and `data/` are git-ignored. `/admin/config` omits the two key fields but returns `probe_headers`; do not treat it as a general secret-redaction endpoint.

## Development and project notes

See [CONTRIBUTING.md](CONTRIBUTING.md) for the source layout, checks, and release workflow. Historical gateway tests are in [docs/VERIFICATION.md](docs/VERIFICATION.md); the review baseline, fixes, and current limitations are in [docs/CODE_REVIEW.md](docs/CODE_REVIEW.md). These detailed records are in Chinese.

Routing knowledge came from public records in [cline-pass-switcher](https://github.com/munmunjaklin458-afk/cline-pass-switcher), [cpagw-gateway](https://github.com/Stabilize7440/cpagw-gateway), and [dsh-cline-pass](https://github.com/yhshzh/dsh-cline-pass). No source code was copied from those projects. See [NOTICE](NOTICE).

The project focuses on provider routing and response compatibility. It does not include an account pool or web UI.

## License

[MIT](LICENSE).
