# Changelog

Before 1.0, a minor release can change config keys.

## [Unreleased]

### Changed

- **Breaking:** a facade, backend, key or group name starts with a letter, and contains only letters, combining marks and digits, in any script, and `@`, `-`, `_`, `.` and `:`, at most 64 characters; a facade name may also contain `/`, so it can match a model ID such as `openai/gpt-4o`. A config with a name that starts with a digit or punctuation, or contains a space, a control or invisible character, or other punctuation such as `+`, fails to load with an error naming the entry and the character. See Names in the README.
- An error about a reference to a name that isn't defined, such as an access list's, writes a character that isn't visible on its own as an escape, such as `\u200b`, so it shows and can't act on the terminal.
- The selection log line's `duration` is `durationMs`, a number of whole milliseconds, in the text log and the JSON one alike; it was a Go duration, which the JSON log wrote in nanoseconds.
- Every log line about a request names its caller in `client`: a request with no key, which had no `client`, is logged as `client=unknown`, as an unlisted key is, without the fingerprint, and the `unknown model` and `routing loop` lines name the client too.
- **Breaking:** the admin log lines name their caller in `client`, as request lines do, in place of `by`: `draft edited`, `draft applied`, `draft reverted`, `draft loaded`, `config saved` and `backend tested` say `client=alice` where they said `by="key alice"`. A reload logs each request as `reload requested` with its `client`, `<SIGHUP>` for a signal, and the reload once with its outcome, so `config reloaded` and `config reload failed` no longer carry a list in `by`.
- **Breaking:** the selection line names the request's URL path in `path`, for every request, in place of `endpoint`, which named the API's own path without a base path and only for a request other than Chat Completions, and `draft edited` names the config location it changed in `key`, in place of `path`, so `path` is always a URL path and `key` always a config location, as on `restart needed`.

### Fixed

- In the admin UI, a field stacks its name above its value by the width its list of fields has, not the screen's, so beside the tree on a tablet in portrait, such as an iPad, a nested field, such as one of `server.tls`, no longer squeezes its value and description against the card's right edge.
- In the admin UI, a table wider than its card, such as a facade's candidates beside the tree on a tablet in portrait, scrolls sideways inside the card, as it did on a phone, rather than spilling past the card's edge.

## [0.4.2] - 2026-11-22

The gateway serves HTTPS itself, on a port of its own or beside plain HTTP, so the admin UI is a secure origin, and it can be installed as an app.
The admin UI also offers a reload once the gateway runs another build.

### Added

- The gateway serves HTTPS itself: `server.tls` names the certificate and key for HTTPS on the client APIs, and on the admin endpoints and admin UI unless `server.adminApi.listen` moves them, from files relative to the config's folder, which a reload reads again. HTTPS shares `server.listen` with plain HTTP, told apart by each connection's first byte, or has an address of its own in `tls.listen`, and `tls.serve` chooses `both`, the default, `https`, which redirects plain requests with `308`, or `http`. `server.adminApi.tls` does the same for the admin side, on `server.adminApi.listen`, or on `server.listen`, where its HTTPS serves only the admin endpoints and the admin UI. HTTPS is served with HTTP/2.
- The admin UI can be installed as an app, with its own window and icon, from Chrome or Edge, or from Safari on an iPhone or iPad, wherever it is served over HTTPS or on `localhost`. Its icon is the MF logo, with a maskable variant for Android and an Apple touch icon.
- `GET /amf/config-status` reports the gateway's version, and the admin UI, once the gateway runs another build than the page was loaded from, offers a **new version: reload** pill.
- `serve` logs `starting`, with the version and the config's files, before it builds the gateway.

### Changed

- `health` checks over HTTPS, without checking the certificate, where `server.tls` serves HTTPS only.
- The `listening` log line names each HTTPS address, as `https` and `adminHttps`, and no longer carries the version, which `starting` does.
- The admin UI's logo and its favicon both read MF; they read AM and FN.

## [0.4.1] - 2026-11-22

The admin UI is told of each change to the config's status and to what each facade routes to, through two new streams, instead of reading the draft every 5 seconds and the facades every 15.
While it can't reach the gateway, it says so in place of pills that would be out of date, and a reload no longer stays on the sign-in form after the stored key has signed in.
A Docker image built from a clean checkout reports its version without `+dirty`.

### Added

- `GET /amf/config-status?follow` streams the status as server-sent events: the status at once, then again each time it changes, after an edit, apply, revert, load, save or reload, or, by a check every 2 seconds while a stream is open, after an edit to the files on the host. The admin UI follows it instead of reading the draft and the status every 5 seconds, and reads the status every 5 seconds only while it can't follow the stream.
- `GET /amf/models?follow` streams the facades' list as server-sent events: the list at once, then again each time it changes, at once for a load the gateway starts or finishes, a reload or an apply, and by a check of every backend's state every 15 seconds while a stream is open, for a load another client starts. The admin UI follows it instead of reading `/amf/models` every 15 seconds, and reads it every 15 seconds only while it can't follow the stream.
- `GET /amf/config-status` gives each copy's `ETag`, in `draft.etag` and `live.etag`, so a client can tell from the status whether a copy changed.

### Changed

- The admin UI's **Diff** button looks disabled while the draft holds what is running, as **Revert** is, but can still be clicked to confirm there is nothing to show.
- The admin UI calls the config files on disk the current save: **Load the current save** loads them into the draft, and **Reload: load and run the current save** runs them. A backup's button is **Load into draft**.
- The admin UI's **Load a backup...** scrolls the Server sheet to **Files and backups**, where the backups are listed.
- While the admin UI can't reach the gateway, by its streams or by reading, its bar shows only **can't reach the gateway**, in place of pills that would be out of date, and it tries again after 1 second, then twice as long each time, up to 30 seconds, as its log panel does.

### Fixed

- The admin UI could stay on its sign-in form after a reload had already signed in with the stored key, or on its loading message after the gateway had refused a request without a key: a change to the page's state made before a part of the page had started listening for changes was never drawn. The page now opens on **Connecting to the gateway...** until it knows which to show.
- A Docker image built from a clean checkout without `--build-arg VERSION` reports the checkout's version, without `+dirty`. Every such image reported `+dirty`, because the build context leaves out `dev-env`, whose tracked files git then saw as deleted.

## [0.4.0] - 2026-11-22

The config can be edited, applied and saved over HTTP, through the admin API or the Web UI built into the binary, and `ai-model-facade init` writes a starting config to set a gateway up that way. An `openWebUi` backend's `llamaSwap` block is removed; see Removed.

### Added

- `POST /amf/reload` reloads the config from its files, as `SIGHUP` does, and answers with the outcome: the number of facades, the warnings and notes, or the error with `422`, or `500` for a file it can't read. It needs a key in the new `server.adminApi.write`, `allowedKeys` and `allowedGroups` as on a facade; without that list the endpoint answers `404`. A reload asked for while another runs waits, then shares one reload with every request that arrived in the meantime. The reload log line names what asked for it, in `by`.
- The config can be read and edited over HTTP, in steps: `PUT` and `DELETE` on `/amf/config` and its paths edit a draft that every admin shares, `POST /amf/config-apply` puts the draft into service, `POST /amf/config-revert` throws its edits away, `POST /amf/config-load` brings the files on disk into it to review a host edit before it runs, and `POST /amf/config-save` writes the running config to its files. `GET /amf/config` shows the draft, or with `?live` the running config, as JSON or YAML, with the secrets file merged in and every secret hidden, and an `ETag` for `If-Match`. A JSON `PUT` keeps the aliases, merges and comments of whatever it sends back unchanged, so a `GET` followed by a `PUT` changes only what the client changed, and `POST /amf/config-rename/<path>?to=<name>` renames a facade, a backend or any other key in its place. `server.adminApi.read` lists who may read it; `server.adminApi.write` lists who may change it, and may read it too. A reload replaces the draft as well.
- A save writes only the files that changed, each renamed into place, and refuses a file changed on disk since the gateway read it unless asked with `?force`. It first copies both files into a backup, a folder in `backups/` beside the config, which `GET /amf/config-backups` lists and `POST /amf/config-load?backup=<id>` loads into the draft, by ID or by index from the newest, `-1` for the newest. `server.adminApi.configBackups` sets how many are kept, `10` by default. `GET /amf/config-status` says where the draft and the running config came from, whether each was edited, whether the copies differ, and whether each file can be saved.
- `GET /amf/logs` reads the gateway's recent log lines, kept in memory beside stderr, as JSON lines or stderr's text, filtered by `?level=`, `?since=` and `?limit=`, and with `?follow` streams each new line as a server-sent event. It needs a key in `server.adminApi.read`. `server.adminApi.logs.level` keeps lines below `logging.level`, so the endpoint can show debug lines stderr doesn't, and `server.adminApi.logs.maxBytes`, 1 MiB by default, bounds the copy; `0` keeps no history and only streams. A stream that falls behind drops lines and says how many, and ends once its caller may no longer read the log.
- An admin UI at `/amf/ui/`, built into the binary: sign in with a key `server.adminApi` lists to edit the config as a tree of sheets, each change going into the draft, compare the draft with the running config, apply it, save it, load a backup, edit the anchors under `vars`, and follow the log. A key only in `server.adminApi.read` sees it all read-only.
- `server.adminApi.listen` serves the admin endpoints and the admin UI on an address of their own, so the client APIs can face a network the admin side doesn't. `server.adminApi.uiBasePath` moves the admin UI from `/amf/ui`, and with that address to its root, `/`.
- `GET /amf/schema` describes every key the config takes, with its type, default, allowed values and description, for the config and an entry of each backend and facade type. `GET /amf/config-anchors` lists each anchor with the paths that read it. `GET /amf/config-status` names the caller and says whether it may change the config.

- `POST /amf/backend-test/<name>` asks the draft's backend for its models, with the address and key the draft gives it, so they can be checked before they are applied; the admin UI's backend sheet has a **Test** button for it. It needs a key in `server.adminApi.write`.
- `ai-model-facade init <config> [--secrets <file>]` writes a starting config with a new admin key, which it prints once and the files hold as its hash, so a gateway can be set up from nothing in the admin UI. It never replaces a file.

### Changed

- A config with no facades starts if `server.adminApi.write` lists someone, who can add them over HTTP; without that it is still an error.
- A reload that changes `server.listen` is no longer refused: it takes the rest of the config, the gateway goes on listening where it started until a restart, and the log line `restart needed`, and `restart` in `/amf/config-status`, say so. The same holds for an apply, and for `server.adminApi.listen`.
- Building from source needs Go 1.26 or newer, for `slog.MultiHandler`; Go 1.25 no longer gets security fixes.

### Removed

- An `openWebUi` backend's `llamaSwap` block, which `links` replaced in 0.2.1. A config that still has one is refused at startup; move it into `links`: `llamaSwap: {backend: swap, prefixId: p}` becomes `links: [{backend: swap, prefixId: p}]`.

## [0.3.2] - 2026-11-20

A response that stops arriving from a backend is now ended after a timeout, the Docker image's health check passes with a secrets file, and a wrong list in the config, or a backend error with no message, now gets a message that names it.

### Added

- `stallTimeoutSeconds` on every backend: a response that has started and then sends nothing for this long is ended, a stream with the protocol's error event and a complete response with `502`, and the request to the backend is cancelled. On Open WebUI it is the time since the reply last changed, and the chat's task is stopped. Default `600`; `0` sets no limit, as before.

### Changed

- A single name where a list of names goes, such as `allowedKeys: alice` or a group given as `family: alice`, is refused with the key and the list it should be, `allowedKeys must be a list, such as [alice]`, instead of `cannot unmarshal !!str into []string`.
- Release binaries and the Docker image are built with the newest Go release, which still gets security fixes, instead of Go 1.25. Building from source still needs Go 1.25 or newer.

### Fixed

- A backend error that a translation passes on, and that came with no message and no text, such as an empty `502` from a proxy, reaches the client as `the backend answered 502 Bad Gateway with no message`, or for a stream `the backend's stream failed with no message`, instead of an empty message.
- `health`, which the Docker image's health check runs, reads only `server.listen`. It loaded the entire config without the secrets file, so a config whose groups or access lists name a key kept in the secrets file failed every health check, and the container showed as unhealthy while it served.
- An Open WebUI reply now includes an outlet filter's rewrite of the finished message. Before, the reply could carry the message as it was before the rewrite. A stream carries a rewrite only if it adds to the end of what was already sent.

## [0.3.1] - 2026-11-16

The gateway can limit which callers may use each facade and backend, send a backend a key of each caller's own, and read its secrets from a file of their own.

### Added

- Groups of keys, `server.groups`, and access lists on facades and backends, `allowedKeys` and `allowedGroups`. A facade a caller may not use is left out of its model lists and answers it with `404`; a candidate on a backend a caller may not use is skipped, and a caller that may use none of a facade's backends gets `403`. `all` and `unknown` are built-in groups. Without the lists, every caller may use everything, as before. `check` shows a facade's lists, and a backend's under each candidate on that backend.
- `current` and `/amf/models` describe the routing of the caller that asks.
- A secrets file, read by `serve` and `check` with `--secrets <file>`, holds the config's secrets apart from it: `server.apiKeys` entries, and a backend's `apiKey` and `keyMap`. The config doesn't name it, so one config can serve several deployments, and it is read again on every reload, unlike the environment. A backend's `apiKey`, or a `keyMap` entry, set in both files is a startup error.
- A Responses conversation served to a key in `server.apiKeys` can be continued only with that key; to any other caller its response IDs are unknown.
- `keyMap` on a backend: the key it receives on a caller's behalf, by the caller's key name or, in the order the map lists them, the first of its groups, in place of `passthroughAuth`'s or `apiKey`'s, so a backend with accounts of its own, such as Open WebUI, sees each caller as themselves without holding their gateway keys.
- A `server.apiKeys` entry can give the key's hash, `hash: sha256:...`, instead of the key, so the config holds no usable key. `ai-model-facade hash-key` prints a new key with its hash, and `hash-key --stdin` hashes an existing key. Entries with `key` work as before, and every key is hashed when the config loads.

### Changed

- **Breaking:** `all` is reserved as a key name, for the built-in group, as `unknown` already was.
- Two entries with the same key now load with a warning; requests with that key are named after the first entry, as before.

## [0.3.0] - 2026-11-12

Clients built for Claude, or on OpenAI's Responses API, can use every facade, including those on local models.

### Added

- Anthropic's Messages API at `/anthropic/v1/messages`, streamed or not, so a client built for Claude, such as Claude Code, can use the gateway. A request for a candidate on an `anthropic` backend is forwarded unchanged apart from params and the model, and one for any other backend is translated to Chat Completions and its reply translated back.
- OpenAI's Responses API at `/v1/responses`, streamed or not. A request for a candidate on an `openAi` backend is forwarded unchanged apart from params and the model, and one for any other backend is translated to Messages on the Claude API, or to Chat Completions, and its reply translated back. Claude's thinking comes back as a reasoning item that carries its signed thinking block, so a client that sends the item back gives Claude its thinking in a tool loop. Codex's `client_metadata`, which only OpenAI's service reads, is dropped toward other backends.
- `useProtocol` on a candidate names the protocol its backend is sent. With `useProtocol: openAiResponses`, a Chat Completions or Messages request for a candidate on an `openAi` backend is translated to Responses, so a model OpenAI serves only there, such as its codex models, serves every client, and a Messages client in a tool loop gives OpenAI back its own reasoning. A Chat Completions client keeps its log probabilities, web search with citations, and custom tools. A Messages client's `user_id` longer than the 128 characters OpenAI takes, as Claude Code's is, reaches `safety_identifier` as its SHA-256.
- The gateway keeps Responses conversations, so a request that names an earlier response in `previous_response_id` can be served by any candidate, whichever served the turns before. `server.responses.maxBytes` and `maxAgeMinutes` limit what is kept, 256 MiB and 24 hours by default, and a candidate on an `openAi` backend can leave its conversations to OpenAI with `responsesHistory: service`.
- Tools across the APIs. Web search, code execution and remote MCP reach a backend wherever its API has a counterpart, with their records and citations returned in the client's own API. OpenAI's `shell` and Claude's `bash` cross as each other, and `apply_patch` and Claude's text editor as function tools, as all four do on Chat Completions backends. A Responses `namespace` tool, which Codex groups functions in, reaches Claude and Chat Completions backends as its functions, and a web search that Codex limits to OpenAI's cached index is dropped toward them, since their searches are live.
- A Claude turn that stops with `pause_turn` is sent back by the gateway for a Chat Completions or Responses client, up to 5 times, and returned as one reply.
- `X-Gateway-Dropped`, a response header naming the client's fields a translation left out, such as `thinking` or `cache_control`; the selection log line has the same list as `dropped`.
- `/anthropic/v1/messages/count_tokens`, answered by the backend's own count on `anthropic`, `llamaServer` and `llamaSwap` backends, counted by the model's tokenizer with the chat template estimated on `localAi`, and estimated on the others; a count that is partly or wholly an estimate is marked `X-Gateway-Estimated: true`.
- `/anthropic/v1/models` and `/anthropic/v1/models/{id}`, the facades in Anthropic's list format, each with the smallest output limit among the models it can select as its `max_tokens`.
- `/amf/models` and `/amf/models/{id}`, the gateway's own listing: each facade with `current` and the state of every candidate, including those after the winner, so why a facade picked a model can be seen without reading the logs. Clients of either API can call it with the key in `Authorization` or `x-api-key`.
- The `api` section: `default` gives the root to the OpenAI or the Anthropic API, `basePath` sets each API's path, `/openai` and `/anthropic` by default, `addCurrent` adds `current` to each API's model list, and `protocols` turns an API's protocols off gateway-wide.
- A facade's `protocols` list, which narrows the APIs that reach it. A facade it leaves out of every protocol of an API isn't in that API's model list.
- Params blocks: `params` can hold a block per protocol, `openAiChat`, `openAiCompletions`, `openAiResponses` or `anthropicMessages`, plus `all`, for a setting spelled differently in each API. A `params` without them applies to every protocol, as before.
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

[0.4.0]: https://github.com/MadReasonable/ai-model-facade/compare/v0.3.2...v0.4.0
[0.3.2]: https://github.com/MadReasonable/ai-model-facade/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/MadReasonable/ai-model-facade/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/MadReasonable/ai-model-facade/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/MadReasonable/ai-model-facade/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/MadReasonable/ai-model-facade/compare/v0.1.3...v0.2.0
[0.1.3]: https://github.com/MadReasonable/ai-model-facade/compare/v0.1.2...v0.1.3
[0.1.2]: https://github.com/MadReasonable/ai-model-facade/compare/v0.1.1...v0.1.2
[0.1.1]: https://github.com/MadReasonable/ai-model-facade/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/MadReasonable/ai-model-facade/releases/tag/v0.1.0
