# tagger

A small Go service that turns free-form text into a short array of tags, using
an LLM served through [OpenRouter](https://openrouter.ai). It speaks **REST**
and **gRPC** over the same tagging core.

```console
$ curl -sS localhost:8080/tag -H 'Content-Type: application/json' \
    -d '{"text":"Kubernetes operators reconcile desired state by watching custom resources."}'
{"tags":["kubernetes","operators","custom resources","reconciliation"]}
```

## Quick start

```bash
cp .env.example .env       # then put your OpenRouter key in it
task run                   # or: docker compose up --build
```

```bash
task            # list every task
task ci         # everything CI runs: generate:check, lint, test, integration, build
```

Requires Go 1.27.0 (pinned in [`.go-version`](.go-version)), protoc 36.0
(pinned in [`.protoc-version`](.protoc-version)), and
[go-task](https://taskfile.dev). Everything else is fetched by the Go
toolchain.

protoc stamps its own version into the files it generates, so `task generate`
refuses to run against a different protoc rather than let CI fail later on a
diff that is nothing but a version comment. To move to a new protoc, bump
`.protoc-version` and rerun `task generate` — CI reads the same file.

## Environment variables

| Variable | Required | Default | Purpose |
| --- | --- | --- | --- |
| `OPENROUTER_API_KEY` | **yes** | — | OpenRouter key. The service refuses to start without it. |
| `OPENROUTER_MODEL` | no | `openai/gpt-4o-mini` | Any OpenRouter model slug. |
| `OPENROUTER_BASE_URL` | no | `https://openrouter.ai/api/v1` | Point at a local stub or a proxy. Trailing slashes are trimmed. |
| `HTTP_ADDR` | no | `:8080` | REST listen address. |
| `GRPC_ADDR` | no | `:9090` | gRPC listen address. |
| `MAX_TAGS` | no | `8` | Upper bound on returned tags; also told to the model. |
| `MAX_TEXT_BYTES` | no | `32768` | Inputs above this are rejected before reaching the model. |
| `REQUEST_TIMEOUT` | no | `30s` | Bounds one end-to-end request, including the LLM call. |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON on stdout. |
| `APP_URL`, `APP_NAME` | no | —, `tagger` | OpenRouter attribution headers (`HTTP-Referer`, `X-Title`). |

Config is validated at start-up and **all** problems are reported at once, so a
typo fails the deploy rather than the first request.

## REST

Served on `HTTP_ADDR`. The OpenAPI 3.1 document is embedded in the binary and
served at `GET /openapi.yaml`, so the docs can never drift from the build.

### `POST /tag`

```bash
curl -sS localhost:8080/tag \
  -H 'Content-Type: application/json' \
  -d '{"text":"Postgres logical replication lets you stream changes per table."}'
```

```json
{ "tags": ["postgres", "logical replication", "change data capture"] }
```

| Status | When |
| --- | --- |
| `200` | Success. `tags` is always an array, possibly empty. |
| `400` | Body is not JSON, has unknown fields, or `text` is missing/empty/whitespace. |
| `413` | Body or `text` exceeds the configured limit. |
| `502` | The LLM call failed, or its output was not a JSON array of tags. |
| `504` | The LLM did not answer within `REQUEST_TIMEOUT`. |

Errors are `{"error": "..."}`. Upstream provider messages are logged
server-side but never echoed back, so keys and vendor detail cannot leak
through the API.

### `GET /health`

```json
{ "status": "ok", "version": "v1.2.3" }
```

## gRPC

Served on `GRPC_ADDR`, plaintext. [Server
reflection](https://grpc.io/docs/guides/reflection/) and the [standard health
service](https://grpc.io/docs/guides/health-checking/) are registered, so
`grpcurl` needs no local copy of the descriptors.

```protobuf
service Tagger {
  rpc Tag(TagRequest) returns (TagResponse);
}
message TagRequest  { string text = 1; }
message TagResponse { repeated string tags = 1; }
```

```bash
grpcurl -plaintext -d '{"text":"Rust ownership prevents data races at compile time."}' \
  localhost:9090 tagger.v1.Tagger/Tag

grpcurl -plaintext -d '{"service":"tagger.v1.Tagger"}' \
  localhost:9090 grpc.health.v1.Health/Check

grpcurl -plaintext localhost:9090 list   # reflection
```

From Go:

```go
conn, _ := grpc.NewClient("localhost:9090",
    grpc.WithTransportCredentials(insecure.NewCredentials()))
defer conn.Close()

resp, err := taggerv1.NewTaggerClient(conn).
    Tag(ctx, &taggerv1.TagRequest{Text: "..."})
```

Status codes mirror the REST mapping: `InvalidArgument` for bad input,
`Unavailable` for upstream failures, `DeadlineExceeded` for timeouts,
`Internal` otherwise.

## Docker

```bash
docker compose up --build          # needs OPENROUTER_API_KEY in .env
docker compose -f docker-compose.yml -f docker-compose.docs.yml up   # + Swagger UI on :8081
```

The image is published to
[`ghcr.io/ipedrazas/tagger/tagger`](https://ghcr.io/ipedrazas/tagger/tagger).
The runtime layer is `distroless/static` — no shell, no package manager,
non-root. Because there is no `curl` inside, the container health check calls
the binary's own `healthcheck` subcommand:

```bash
docker run --rm -p 8080:8080 -p 9090:9090 \
  -e OPENROUTER_API_KEY="$OPENROUTER_API_KEY" \
  ghcr.io/ipedrazas/tagger/tagger:latest
```

CI and local builds share the one [`Dockerfile`](Dockerfile), whose builder
stage runs the same `task generate build` a developer runs.

## Testing

```bash
task test              # unit tests, race detector, coverage. No API key needed.
task test:integration  # REST + gRPC over real sockets against a stubbed upstream
task smoke             # curl/grpcurl a service that is already running
```

- **Unit tests** mock the LLM through the one-method `tagging.Completer`
  interface, and mock OpenRouter itself with `httptest` to cover retries, rate
  limits, inline error objects and cancellation.
- **Integration tests** (`-tags integration`) start the *real* `app` — both
  servers, both interceptors, the real config loader — on ephemeral ports with
  a stub OpenRouter behind it, then drive it over real sockets. One test asserts
  REST and gRPC return identical tags for identical input.
  Set `TAGGER_HTTP_ADDR` and `TAGGER_GRPC_ADDR` to run the same assertions
  against an already-running instance instead:

  ```bash
  TAGGER_HTTP_ADDR=localhost:8080 TAGGER_GRPC_ADDR=localhost:9090 task test:integration
  ```

Nothing in the test suite needs a real `OPENROUTER_API_KEY`, and CI never sets
one — which is the proof that the LLM is genuinely mocked rather than merely
skipped.

## CI/CD

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs on every push and
PR: `generate:check` (fails if the committed stubs are stale), lint, unit
tests, integration tests, build. On a push to `main` or a `v*` tag it builds
the multi-arch image and pushes it to GHCR with `GITHUB_TOKEN`, then starts the
published image and waits for `/health` before declaring success.

`packages: write` is granted only to the publish job, and that job is gated on
`github.event_name == 'push'`, so a fork PR can never obtain a token that can
write to the registry.

## Layout

```
cmd/tagger/           entrypoint: config, logging, signals, healthcheck subcommand
internal/app/         wires config + LLM + service + both servers; graceful shutdown
internal/config/      environment parsing and validation
internal/tagging/     prompt, strict response parsing, normalisation  <- the core
internal/llm/         OpenRouter client (retries, backoff, error mapping)
internal/api/rest/    HTTP handler, error->status mapping, access log
internal/api/grpc/    gRPC service, error->code mapping
proto/tagger/v1/      tag.proto
proto/gen/            generated stubs (committed; CI verifies they are current)
api/                  openapi.yaml, embedded into the binary
test/integration/     end-to-end tests over real sockets
```

The dependency arrow points one way: `tagging` knows nothing about HTTP, gRPC
or OpenRouter. It talks to a one-method interface, which is what makes the unit
tests trivial and what would make swapping OpenRouter for another provider a
single-file change.

## Dependencies, and why

| Module | Why |
| --- | --- |
| `google.golang.org/grpc` | The only real option for gRPC in Go; brings the health and reflection services used above. |
| `google.golang.org/protobuf` | Runtime for the generated stubs. Non-negotiable given gRPC. |
| `golang.org/x/sync/errgroup` | Runs both servers and the shutdown watcher with correct first-error propagation. ~200 lines, and the hand-rolled version is where the bugs live. |
| `go.yaml.in/yaml/v3` | Test-only. Parses `openapi.yaml` so the spec is checked against the routes actually served. |

Deliberately **not** used:

- **An OpenAI SDK.** OpenRouter is OpenAI wire-compatible, but the service
  calls exactly one endpoint with three fields. `net/http` plus two structs is
  ~150 lines, has no transitive dependencies, and lets the retry and error
  semantics be exactly what the service needs. If richer features are ever
  needed (streaming, tools, structured outputs), swapping in
  `openai-go` means rewriting `internal/llm/openrouter.go` alone —
  `tagging.Completer` is the seam.
- **An HTTP router.** Go 1.22's `net/http.ServeMux` does method-based patterns
  (`POST /tag`), which is all three routes need.
- **A config library.** Six environment variables do not justify a dependency,
  and hand-rolled parsing lets every problem be reported in one error.

## Notes on the prompt

The model is asked for a bare JSON array and told, in the system prompt, to
treat the user's text as data rather than instructions. Responses are parsed
strictly but not naively: a bare array, a ` ```json ` fence, a `{"tags": [...]}`
envelope, and an array surrounded by prose all parse; anything else is a `502`
rather than a guess. Tags are then lower-cased, whitespace-collapsed,
punctuation-trimmed, de-duplicated, length-bounded and capped at `MAX_TAGS`,
so the normalisation is enforced by the service and not merely requested of the
model.

## License

MIT. See [LICENSE](LICENSE).
