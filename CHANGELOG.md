# Changelog

Before 1.0, a minor release can change config keys.

## [Unreleased]

Clients built for Claude can use every facade, including those on local models, through Anthropic's Messages API.

### Added

- Anthropic's Messages API at `/anthropic/v1/messages`, streamed or not, so a client built for Claude, such as Claude Code, can use the gateway. A request for a candidate on an `anthropic` backend is forwarded unchanged apart from params and the model, and one for any other backend is translated to Chat Completions and its reply translated back.
- `X-Gateway-Dropped`, a response header naming the client's fields a translation left out, such as `thinking` or `cache_control`; the selection log line has the same list as `dropped`.
- `/anthropic/v1/models` and `/anthropic/v1/models/{id}`, the facades in Anthropic's list format, each with the smallest output limit among the models it can select as its `max_tokens`.
- `/amf/models` and `/amf/models/{id}`, the gateway's own listing: each facade with `current` and the state of every candidate, including those after the winner, so why a facade picked a model can be seen without reading the logs. Clients of either API can call it with the key in `Authorization` or `x-api-key`.
- The `api` section: `default` gives the root to the OpenAI or the Anthropic API, `basePath` sets each API's path, `/openai` and `/anthropic` by default, `addCurrent` adds `current` to each API's model list, and `protocols` turns an API's protocols off gateway-wide.
- A facade's `protocols` list, which narrows the APIs that reach it. A facade it leaves out of every protocol of an API isn't in that API's model list.
- Params blocks: `params` can hold a block per protocol, `openAiChat`, `openAiCompletions` or `anthropicMessages`, plus `all`, for a setting spelled differently in each API. A `params` without them applies to every protocol, as before.
- `loopsBack` on a backend, for one whose state read can lead back to this gateway, such as an Open WebUI that has this gateway as a connection.
- A wrong path or method under `/anthropic` gets a `404` or `405` in Anthropic's JSON error shape, as Anthropic's own API answers one.

### Changed

- **Breaking:** the backend type `openAI` is now `openAi`, as config names treat an initialism as a word. A config with the old spelling fails to load with `unknown type 'openAI'`.
- **Breaking:** the backend type `localAI` is now `localAi`, for the same reason. A config with the old spelling fails to load with `unknown type 'localAI'`.
- **Breaking:** `current` on `/v1/models` is off by default, so that list reads no backend's state. A gateway that is a connection of an Open WebUI which a gateway reads needs `api.openAi.addCurrent: true`, or its facades read as unknown there; if that is the same gateway, its `openWebUi` backend also needs `loopsBack: true`.
- **Breaking:** params for a candidate on an `anthropic` backend now patch the Messages body, after translation, so they are written in Messages spelling. `reasoning_effort`, `response_format` and `stop` in them were translated before, and now reach the Claude API, which refuses them; use `output_config` and `stop_sequences`. The gateway warns at load about those keys.
- **Breaking:** `alias_of` in `current` is now `aliasOf`, as the gateway's other field names are camelCase.
- A candidate whose backend can't serve a request's protocol is now skipped, so a legacy completion reaches a later candidate instead of failing at the first.
- A chat's token limit now reaches an `openAi` backend as `max_completion_tokens`, which OpenAI's newer models require, and an `ollama` or `openWebUi` backend as `max_tokens`, the only name Ollama honours. Params that set the other name are renamed the same way.
- A request without `max_tokens` to an `anthropic` backend gets the model's output limit whichever API the client used.

### Fixed

- A response through Open WebUI from a model on the Claude or OpenAI API now reports usage, since the gateway always asks Open WebUI for it.

## [0.2.1] - 2026-11-02

Support for hosted AI providers and LocalAI, and Open WebUI models linked to any backend.

### Added

- An `openAI` backend for OpenAI's API, or any hosted API that serves Chat Completions the same way, such as OpenRouter's. A hosted model needs no load, so a hosted candidate placed after the local ones acts as their fallback, and is skipped while its API is down.
- An `anthropic` backend for Anthropic's Claude API. It translates to the Claude API's own Messages API, so `reasoning_content`, custom tools, images and PDFs, prompt caching and JSON schema output work through the gateway's OpenAI-style API.
- A facade's `params` can set Claude's `thinking`, `cache_control` or `output_config` directly, and a request without `max_tokens` gets the model's output limit.
- A `localAI` backend for LocalAI, with its aliases, and its load limit respected so the gateway never interrupts a load in progress.
- `links` on the `openWebUi` backend: the load state of Open WebUI's models now comes from the gateway's own backend for the same server, for every supported backend, not only llama-swap. `links` takes a list of `{backend, prefixId}` entries.
- `dev-env/` runs LocalAI too, with a model limit.
- Fake LocalAI, OpenAI and Claude API servers for the offline tests. With `OPENAI_API_KEY` or `ANTHROPIC_API_KEY` set, the hosted APIs' claims run against the real APIs, at the cost of a few tokens.

### Changed

- A llama-swap model in Open WebUI that no link covers now keeps the status Open WebUI reports, instead of reading as unloaded.
- Live tests in different packages wait for each other, so one `go test ./...` with `AMF_LIVE` set runs them all.

### Deprecated

- `llamaSwap` on the `openWebUi` backend; use `links`. It still works.

## [0.2.0] - 2026-10-28

LM Studio and llama-server backends, and a Docker environment of real backends to develop against.

### Added

- An `lmStudio` backend for LM Studio's server, the desktop app's or its headless daemon's.
- A `llamaServer` backend for llama.cpp's llama-server, in router mode or serving a single model.
- `dev-env/` runs a real copy of each supported backend in Docker, with small models, for testing the gateway against. Its `control` service starts and stops the backends and loads and unloads their models, and `dev-env/gateway.yaml` has a facade per backend and one across backends.
- Fake llama-server and LM Studio servers for the tests. With `AMF_LIVE` set, each fake's claims about its backend also run against the real one in `dev-env`, and live checks serve `dev-env/gateway.yaml` and check what a client gets from each facade.

### Changed

- The example config listens on `127.0.0.1:8088`, which accepts local connections only. To accept external connections, listen on `0.0.0.0` or a specific IP, and set `apiKeys`.

### Fixed

- A backend's startup is detected more reliably.

## [0.1.3] - 2026-10-25

### Added

- `CONTRIBUTING.md`, `ARCHITECTURE.md`, and `AGENTS.md` with `docs/agents-design.md`, for contributors and coding agents.
- Shared fake llama-swap, Ollama and Open WebUI servers for the tests, golden files for `check`'s output, and `gofmt`, `go vet` and the race-enabled tests in CI.

### Changed

- Through Open WebUI, a pattern now picks a llama-swap preset's base model, not a preset that sorts before it, since llama-swap presets are folded into their base model there, as workspace models already were.
- A loaded candidate is chosen as soon as its backend answers, instead of waiting for every backend in the list.
- Loads the gateway started count as starting on every backend, not only Ollama.
- Each backend's state is read once per request, even when the request loops back through Open WebUI.
- With the `llamaSwap` block, llama-swap's current state applies to Open WebUI's models on every read, and requests through Open WebUI count in the llama-swap backend.
- A backend's `Retry-After` header is passed on to the client.

### Fixed

- Images built without `VERSION` report the version from git instead of `dev`.
- `listening` is logged only once the port is open.

## [0.1.2] - 2026-10-24

### Added

- Reasoning from Open WebUI models reaches the client as `reasoning_content`: streamed ahead of the answer, and in the message of a reply that isn't streamed. It is the field llama-server uses, so reasoning looks the same from a facade on Open WebUI as from one on llama-swap.

## [0.1.1] - 2026-10-24

### Changed

- **Breaking:** a backend or model name defined twice is a config error, at startup and on reload, with both line numbers. Before, `/v1/models` listed both definitions while requests used the last one.
- `GET /v1/models` reads each backend's state once per listing instead of once per facade, so every entry is judged against the same state.
- Requests refused before selection (a missing or wrong API key, invalid JSON, a duplicate key, an oversized body) are logged as `request rejected`, with the client's name or the fingerprint of an unlisted key.
- Open WebUI: an error is logged if a chat start is answered without task IDs, which would mean replies can no longer stream.

### Fixed

- Open WebUI: usage covers every model call in a tool loop, not only the last.
- Open WebUI: when a client disconnects, the task is stopped and the chat deleted in the background, so the request ends at once and its logged duration is accurate.

## [0.1.0] - 2026-10-23

First release: an OpenAI-compatible gateway that serves stable virtual models whose real model is chosen per request.

### Added

- Each request goes to the first candidate model that is already loaded, with the configured params applied. Candidates are exact model IDs, patterns such as `qwen3:*`, or llama-swap presets. If nothing is loaded, the request waits for a model that is still starting, or a `loadIfNone` candidate is loaded.
- Backends for llama-swap, Ollama and Open WebUI, which can be mixed behind one facade.
- An Open WebUI backend runs Open WebUI's own chat flow, so its tools (web search, memory, knowledge bases, tool servers) work from any OpenAI client.
- `GET /v1/models` lists the facades, each with `current`: where a request would be routed right now.
- API keys per client, with optional passthrough of the client's own key to a backend.
- The config reloads on `SIGHUP`, and `check` validates a config and prints what each facade resolves to.

[Unreleased]: https://github.com/MadReasonable/ai-model-facade/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/MadReasonable/ai-model-facade/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/MadReasonable/ai-model-facade/compare/v0.1.3...v0.2.0
[0.1.3]: https://github.com/MadReasonable/ai-model-facade/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/MadReasonable/ai-model-facade/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/MadReasonable/ai-model-facade/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/MadReasonable/ai-model-facade/releases/tag/v0.1.0
