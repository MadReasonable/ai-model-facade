# ai-model-facade

**An OpenAI-compatible gateway that gives clients a stable model name and decides, at request time, which real model answers.**

A client asks for a model that exists only in the gateway's config, such as `assistant`.
That name is a facade, a virtual model that exists only in the gateway: behind it, the gateway picks a real model from a list you wrote, based on what is loaded right now, applies the parameters you configured, and forwards the request.
Models, quants and even backends can be swapped without any change on the client side.

It is one static binary and one YAML file, with no runtime dependencies.
The smallest useful config is a backend address and one facade mapped to one model.
Fully configured, one gateway fronts [llama-swap](https://github.com/mostlygeek/llama-swap), [Ollama](https://ollama.com) and [Open WebUI](https://openwebui.com) at once, chooses among them per request, and hides Open WebUI's multi-request tool chats behind the same plain interface, so a client with no tool support of its own gets web search.

> [!NOTE]
> **Early releases (0.x).** Config keys may still change from one 0.x release to the next; the release notes list each change.

## Why

Most clients let you set one model name, and changing it means editing the client.
Everything that should decide which model answers changes more often than client configs do: which models are loaded, which quant you are testing, which machine a backend runs on, whether the request needs tools.
A facade moves that decision out of every client and into one place, where it is made again for each request.

**Swap models without touching clients.** On a server that loads models on demand, such as llama-swap or Ollama, fixed model names can cause the server to waste time juggling models in and out of memory when an already loaded model could easily handle many of the simpler requests. A facade names a list instead, and the first model on it that is already loaded wins. Open WebUI's background tasks run on whatever LLM is up, with thinking off; a voice assistant always has a fast, non-thinking model, whether the coding set or the general set is loaded. Nothing is evicted unless nothing on the list is loaded, and the config decides what happens then.

**Test a model without re-pointing anything.** A pattern such as `qwen3.8-27b:*` matches whichever quant of that model is loaded, so a test build takes over every facade that lists the pattern as soon as it is loaded. A change to the gateway config, followed by a reload, moves every client at once. `check` prints what each facade resolves to at that moment, and each response carries an `X-Gateway-Target` header with the model that answered, so each result can be attributed to the model that produced it.

**Keep keys and addresses out of clients.** Clients get one address and one key, the gateway's. The backends' addresses and keys live in the gateway's config, and a client's own key reaches a backend only where you turn passthrough on. In that respect it is a reverse proxy for LLM endpoints, limited to chat, completions and the model list.

**Open WebUI's tools for any client.** An Open WebUI backend runs each request through Open WebUI's own chat flow (create a chat, run it with its tools, collect the answer, clean up) and returns one ordinary OpenAI response or stream. The model then has what it would have in Open WebUI's chat: web search, memories kept between conversations, search over your knowledge bases and documents, notes and past chats, and any tool server or MCP integration you have connected. The client needs no tool support of its own. Combined with the first use, the gateway sits on both sides: a client asks the gateway for `assistant`, the gateway asks Open WebUI, and Open WebUI asks the same gateway for whichever model is loaded.

## Quick Start

This walkthrough uses llama-swap as the backend, so you need a running instance.
Ollama and Open WebUI differ only in their `backends` entry; see [Configuration](#configuration).
You need a binary from the [releases page](https://github.com/MadReasonable/ai-model-facade/releases), Go 1.25 or newer, or Docker (see [Running in Docker](#running-in-docker)).

```bash
git clone https://github.com/MadReasonable/ai-model-facade.git
cd ai-model-facade
cp config.example.yaml config.yaml
```

Edit `config.yaml`: set `backends.llamaSwap.baseUrl` to your llama-swap address, and list your models under `models.task-monkey.candidates`.
Then validate the config and start the gateway:

```bash
go run ./cmd/ai-model-facade check config.yaml
go run ./cmd/ai-model-facade serve config.yaml
```

With a release binary, run `ai-model-facade check config.yaml` and `ai-model-facade serve config.yaml` instead.

`check` prints what each candidate will send, and what each pattern can currently route to:

```
task-monkey
  backend: llamaSwap
  description: Background tasks on whichever LLM is already loaded, thinking off
  1. qwen3.6-35b-a3b:q8_0
     params: {"max_tokens":1000,"think":null,"chat_template_kwargs":{"enable_thinking":false}}
  2. qwen3.8-flash-next:iq3_s
     params: {"max_tokens":1000,"think":null,"chat_template_kwargs":{"enable_thinking":false}}
  3. qwen3.8-27b:*
     base models: qwen3.8-27b:q6_k
     aliases: none
     params: {"max_tokens":1000,"think":null,"chat_template_kwargs":{"enable_thinking":false}}
  4. qwen2.5-coder-3b:q8_0 (loadIfNone)
     params: {"max_tokens":1000,"think":null}
```

Send a request from another terminal:

```bash
curl -i http://localhost:8088/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "task-monkey", "messages": [{"role": "user", "content": "Reply with exactly one word: hello"}]}'
```

```
HTTP/1.1 200 OK
Content-Type: application/json; charset=utf-8
X-Gateway-Target: qwen3.6-35b-a3b:q8_0

{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"hello"}}], ...}
```

The gateway logs one line per request:

```
level=INFO msg=selection model=task-monkey checked="[qwen3.6-35b-a3b:q8_0: loaded]" winner=qwen3.6-35b-a3b:q8_0 reason=loaded status=200 duration=178ms
```

To build a binary instead of using `go run`:

```bash
go build -o ai-model-facade ./cmd/ai-model-facade
```

## Running in Docker

The image is a static binary on distroless, about 10 MB.
Each release publishes it for amd64 and arm64 as `ghcr.io/madreasonable/ai-model-facade`, tagged with the version, such as `0.1.0`, and `latest`.
To build it from source instead:

```bash
docker build --build-arg VERSION=$(git describe --tags --always) -t ai-model-facade .
```

`VERSION` is what `ai-model-facade version` and the startup log report; without it, they report `dev`.

Run it as a service in the same compose project as your backends, so `baseUrl` can use their service names:

```yaml
services:
  ai-model-facade:
    image: ghcr.io/madreasonable/ai-model-facade:0.1.0
    restart: unless-stopped
    volumes:
      - ./ai-model-facade/config.yaml:/config/config.yaml:ro
    # environment:
    #   GATEWAY_KEY_OPEN_WEBUI: ${GATEWAY_KEY_OPEN_WEBUI}
```

The image runs `serve /config/config.yaml` by default.
Publish a port only if clients outside the Docker network need it.

The image includes a health check: every 30 seconds it runs `ai-model-facade health /config/config.yaml`, which calls `/health` on the address in `server.listen`, so `docker ps` shows the container as `healthy` and compose's `depends_on: condition: service_healthy` works.
If you mount the config somewhere other than `/config/config.yaml`, override the health check to the same path.

To run `check` in a container, attach it to the backends' network:

```bash
docker run --rm --network <backend network> \
  -v "$PWD/config.yaml:/config/config.yaml:ro" \
  ghcr.io/madreasonable/ai-model-facade:0.1.0 check /config/config.yaml
```

## Connecting Clients

Point any OpenAI-compatible client at `http://<gateway>:8088/v1`, with the key from `server.apiKeys` if you set one, and use a facade's name as the model.

| Endpoint | Description |
|----------|-------------|
| `POST /v1/chat/completions` | Chat completions, streamed or not. |
| `POST /v1/completions` | Legacy text completions, streamed or not. Same selection and params as chat. |
| `GET /v1/models` | Lists only the facades, so clients that pick from a list see just those. Each includes `current` (below). |
| `GET /v1/models/{id}` | One facade, including `current`, or `404`. |
| `GET /health` | `{"status":"ok"}` while the gateway is up. Needs no API key and doesn't contact any backend. |

Clients that should use a backend's models directly keep their existing connection to that backend.

Model objects have the standard OpenAI fields plus `current`: where a request would be routed right now.

```json
{"id":"task-monkey","object":"model","created":1792628190,"owned_by":"ai-model-facade",
 "description":"Background tasks on whichever LLM is already loaded, thinking off",
 "current":{"model":"qwen3.8-flash-next:iq3_s","reason":"loaded"}}
```

It is a snapshot, so the next request can be routed differently if what is loaded changes.
The fields are explained under [Choosing a model](#choosing-a-model).

### Example: Open WebUI task model

Open WebUI runs background tasks (titles, tags, follow-ups) on a configurable task model.
Here it is a client of the gateway, and the task model is a facade:

1. In **Admin Panel → Settings → Connections**, add a second OpenAI connection with the URL `http://ai-model-facade:8088/v1`.
2. In **Admin Panel → Settings → Models**, hide `task-monkey` so it doesn't appear in the chat model picker.
3. In **Admin Panel → Settings → Interface**, set the **Task Model** to `task-monkey`.

Chat traffic keeps going straight to the backend; only background tasks go through the gateway.

## How It Works

Handling a request has two parts: choosing the model, then rewriting the request for it.
The sections after those cover who may call, and what comes back.

### Choosing a model

Each facade has an ordered list of candidates, and each candidate is a model on a backend.
The gateway reads the backend's state, walks the list up to three times, and stops at the first match:

1. **Ready.** The first candidate that is loaded wins.
2. **Starting.** If nothing is ready, the first candidate that is still loading wins, and the request waits for that load to finish. This avoids asking the backend to load a second model, which could evict the one already loading.
3. **Fallback.** If nothing is ready or starting, the first `loadIfNone` candidate wins, and the backend loads it.

A model that is stopping or stopped never counts.
If nothing matches, the request gets `503`; Open WebUI, for example, then skips the task.

There is no failover: a request is never retried against another model.

#### What a candidate can be

A candidate's `model` is one of three things.

- **An exact model ID.** If the backend doesn't list it, the gateway warns at startup and `check` reports an error, either way suggesting IDs that differ only by tag.
- **A pattern** such as `qwen3.8-27b:*`, using `*`, `?` or `[...]`. It matches ready (or starting) models, and the matched ID is forwarded. If several match, the first in sorted order wins. Quote a pattern that starts with `*`, because a leading `*` is YAML alias syntax.
- **A llama-swap alias**, a preset such as `qwen3.6-35b:Slacker`. It is ready (or starting) whenever its base model is, and is forwarded under the alias ID so llama-swap applies the preset. Patterns prefer base models over aliases, so `qwen3.8-27b:*` picks the quant rather than its `:Dreamer` preset. A pattern can also pick a preset across model families: if each family defines an `:Assistant` preset, `"*:Assistant"` matches whichever one is loaded.

llama-swap applies a preset's own params after the gateway's, completely replacing top-level keys such as `chat_template_kwargs`, so params on an alias candidate may be overridden.
Prefer base model IDs for anything the gateway must control.
The gateway logs an INFO line at startup for each alias candidate that has params, and `check` notes it.

A candidate with `loadIfNone: true` is used like any other candidate when it is ready or starting, and is also the fallback in step 3.
It can sit anywhere in the list: its position sets its priority in steps 1 and 2, while the fallback only happens after the entire list has been checked.

#### Where state comes from

Each backend reports which models are loaded in its own way, and that decides how exact the three steps can be.

- **llama-swap** reports `ready`, `starting` and `stopping` for each running model, so all three steps apply exactly.
- **Ollama** lists a model only once it has finished loading, so the gateway tracks loads itself: a model counts as starting while a request the gateway forwarded is waiting for it. A load started by a client that goes straight to Ollama isn't visible until it finishes.
- **Open WebUI** passes on what its connections report, which varies by connection; see [Loaded state through Open WebUI](#loaded-state-through-open-webui). A model whose state it can't report is **unknown**: it counts as not loaded, so only `loadIfNone` can pick it, and the log says `state unknown`.

#### Candidates on several backends

Each candidate may name its own `backend`, and the list is still walked in order as one list.
Each backend's state is read once per request, concurrently.
If one backend's state can't be read, its candidates count as not loaded (logged as `state unavailable`) and the others still serve; only when none can be read does the request fail with `502`.
Wherever a candidate's backend isn't the facade's own, the log, `check` and `current` name it, e.g. `qwen3:1.7b on ollama: loaded`.

#### Reading `current`

`current` on a model object is the result of running the three steps now, without sending a request.

- `reason` is `loaded`, `starting` or `loadIfNone`, matching the step that would pick `model`.
- When no request would succeed, `model` is absent and `reason` is `no candidate loaded`.
- `alias_of` is present when `model` is an alias, and `backend` when the model is on another backend than the facade's own.
- If no backend's state can be read, `current` is left out.

### Rewriting the request

Every `params` value is applied to the request body as a JSON Merge Patch ([RFC 7386](https://www.rfc-editor.org/rfc/rfc7386)):

- Objects merge key by key, so `chat_template_kwargs: { enable_thinking: false }` keeps any other kwargs the client sent.
- Scalars and arrays replace the client's value.
- `null` deletes a key, e.g. `think: null`.

The steps run in this order, with later ones overriding earlier ones:

```
client body → facade params → winning candidate params → model set to the concrete ID
```

Gateway params override the client: they are patches, not defaults.
The rest of the request passes through byte-for-byte, including messages, tools, `response_format` and vendor fields, in their original order.

The same params apply to chat and legacy completions.
Some only mean something for chat: `chat_template_kwargs` has no effect on `/v1/completions`, because no chat template is applied, so a model meant for chat won't have thinking turned off there.
Give legacy completions their own facade if a client uses them.

`params` can also be a list of maps, applied in order.
Use the list form to combine YAML anchors, because YAML's `<<:` merge is shallow and would overwrite nested objects like `chat_template_kwargs`:

```yaml
vars:
  lowEffort: &lowEffort
    chat_template_kwargs: { enable_thinking: true, reasoning_effort: low }

# ...in a facade or candidate:
params:
  - *lowEffort
  - thinking_budget_tokens: 2048
```

### Authentication

Clients authenticate with `Authorization: Bearer <key>`.

- A key listed in `server.apiKeys` is accepted, and its name appears in the log as `client=<name>`.
- An unlisted key is rejected with `401`, unless `server.unknownKeys` is `allow`. Then it is accepted and logged as `client=unknown` with a fingerprint: the first 8 hex characters of the key's SHA-256, so different callers can be told apart. Keys themselves are never logged.
- With no `apiKeys` listed, every request is accepted.
- `GET /health` never needs a key.

By default, a client's key stays at the gateway, and each backend gets its own `apiKey`.
With `passthroughAuth: true` on a backend, completion requests to it carry the client's own `Authorization` header instead, so the backend makes the access decision and sees each caller as themselves.
A client that sent no key is forwarded without one; it never borrows the backend's key.
The backend's `apiKey` is still used for the gateway's own calls, such as checking which models exist and which are loaded.

Passthrough is set per backend, so a client's key only goes to backends that opted in.
To pass through every caller's key without listing them all, combine it with `unknownKeys: allow`, and name the keys you want to see in the log.

If a facade's candidates span two or more backends with `passthroughAuth`, the gateway warns at startup and `check` notes it: a client has one key, but it is sent to whichever of those backends wins, so it may be refused by some of them.

### Responses and errors

Responses from llama-swap and Ollama are passed through unchanged, and streams are relayed event by event.
Responses from Open WebUI are assembled by the gateway from the chat it ran; see [Open WebUI](#open-webui).
Whenever a model was chosen, the response has an `X-Gateway-Target` header naming it.

| Status | Meaning |
|--------|---------|
| `400` | The body isn't a JSON object, or it repeats a key. Duplicate keys are rejected because the gateway patches the first occurrence while backends read the last, which would let a request slip values past the gateway's params. |
| `401` | `server.apiKeys` is set, `unknownKeys` is `reject`, and the request has no matching `Authorization: Bearer` key. |
| `404` | The requested model isn't a facade. |
| `413` | The body is over 100 MiB. |
| `502` | The backend couldn't be reached, or failed the request. |
| `503` | No candidate is eligible. |
| `508` | Routing loop: the request has already passed through this facade of this gateway, e.g. via Open WebUI back to itself. |

Once a stream has started, its status can't change.
If the backend fails mid-stream, the gateway sends a final `data: {"error": {...}}` event and closes without `[DONE]`, so clients show an error instead of a silently cut-off reply.

## Configuration

The config is YAML.
`config.example.yaml` is a complete setup with two facades: `task-monkey` for Open WebUI background tasks, and `assistant` for a Home Assistant voice assistant that uses whichever `:Assistant` preset llama-swap has loaded.
It also carries commented-out `ollama` and `openWebUi` backend entries, and a commented-out `researcher` facade that gives any client Open WebUI's web search and memory.

`${NAME}` in any string value is replaced by the environment variable `NAME` when the config loads.
It is meant for secrets; there is no other substitution.

`vars` is ignored by the gateway.
It is a place to define YAML anchors (`&name`) that entries reference explicitly (`*name`), so every facade shows exactly what it uses.

### Errors and warnings

A config that is wrong is a startup error, reported with its line number: unknown keys, undefined environment variables, a reference to a backend that isn't defined.
Checks that depend on a backend being up and current only warn: a backend that can't be reached, or an exact candidate ID it doesn't list.
`serve` logs the warnings and starts, so one backend that is down, or not ready yet, doesn't take the other backends' facades with it; an unlisted candidate never shows as loaded until its backend lists it.
If a backend that a check needs can't be reached, `serve` asks it for its models again every `listRetrySeconds`, and once it answers, runs the checks that were waiting for it and logs what they find.
The retries themselves log nothing, since the startup warning already reported the backend as down.
`check` reports the same problems as errors and exits non-zero, so run it before deploying a config.
Every backend is asked for its models once, all at the same time, so backends that don't answer delay startup by one `stateTimeoutSeconds` between them, not one each.

### `server`

| Key | Description |
|-----|-------------|
| `listen` | Address and port to listen on. Required. |
| `apiKeys` | A list of `name` / `key` pairs. When set, requests need `Authorization: Bearer <key>`, and the key's name appears in the log. The name `unknown` is reserved. Optional. |
| `unknownKeys` | `reject` (default): an unlisted key gets `401`. `allow`: accept it and log it as `unknown` with a fingerprint. See [Authentication](#authentication). |
| `targetHeader` | Name of the response header naming the chosen model. Default `X-Gateway-Target`. |

### `logging`

| Key | Description |
|-----|-------------|
| `level` | `debug`, `info` (default), `warning` or `error`. |
| `logSelection` | Log one line per request: facade, candidates checked, winner and reason. Failed requests are always logged. |

### `backends`

Each entry is a named backend; `type` selects the adapter: `llamaSwap`, `ollama` or `openWebUi`.
All take these keys:

| Key | Description |
|-----|-------------|
| `baseUrl` | The backend's address. Required. |
| `apiKey` | Sent to the backend as `Authorization: Bearer`, on every call unless `passthroughAuth` is on, and then only on the gateway's own calls. Optional. |
| `passthroughAuth` | `true`: completion requests carry the client's own `Authorization` header (or none). Default `false`. |
| `stateCacheSeconds` | How long to cache the list of loaded models. Default `0`, which asks on every request. Concurrent requests share one in-flight check either way. |
| `stateTimeoutSeconds` | Timeout for the state and model-list calls. Default `10`. |
| `listRetrySeconds` | If the backend couldn't be listed at startup, how often to ask it for its models again, so the checks that need them run once it answers; see [Errors and warnings](#errors-and-warnings). Default `30`. |
| `connectTimeoutSeconds` | Connection timeout. Default `0`, no limit. |
| `requestTimeoutSeconds` | How long to wait for the backend to start responding. Allow for a cold load of a `loadIfNone` candidate; a stream isn't cut off once it has started. Default `0`, no limit. |

#### llama-swap

A `llamaSwap` backend is a llama-swap instance, which starts and stops llama-server processes on demand.
Loaded state comes from its `/running` endpoint, which reports each model as `ready`, `starting` or `stopping`.
Its model list includes presets (aliases), which can be candidates; see [What a candidate can be](#what-a-candidate-can-be).
It takes no keys beyond the shared ones.

#### Ollama

An `ollama` backend is an Ollama server, reached through its OpenAI-compatible endpoints.
Loaded state comes from `/api/ps`, with the gateway's own load tracking on top; see [Where state comes from](#where-state-comes-from).
It takes no keys beyond the shared ones.
Ollama itself ignores `apiKey`, but a proxy in front of it may need one.

Model IDs are the full `name:tag` form that `ollama list` shows, such as `llama3:latest`.
A candidate without a tag, such as `llama3`, isn't treated as `llama3:latest`; the startup check warns about it and suggests the tagged IDs Ollama lists.

#### Open WebUI

An `openWebUi` backend is an Open WebUI instance, and its models are whatever Open WebUI serves: its Ollama and OpenAI connections, its workspace models, and the facades of any gateway it is connected to.
A request to one of them runs through Open WebUI's own chat flow, so Open WebUI's tools run server-side, and any OpenAI client gets them; see [What a request can use](#what-a-request-can-use).

The gateway can be on both sides of Open WebUI at once: a facade with an `openWebUi` backend in front of it, and one of Open WebUI's models a facade of the same gateway behind it.
A request that comes back to the same facade of the same gateway is refused with `508`; passing through one gateway twice for two different facades is fine.

For each request, the gateway creates a chat, starts the completion the way Open WebUI's UI does, polls the chat for the answer while it is generated, and returns it as a normal OpenAI response or stream.
Open WebUI has no legacy `/v1/completions`; such a request fails with `502`.

| Key | Description |
|-----|-------------|
| `chats` | What happens to the chat after each request: `delete` (default), `archive` or `keep`. |
| `pollIntervalSeconds` | How often to poll the chat while the answer is generated. Default `0.25`. |
| `llamaSwap` | Optional: `{backend: <name>, prefixId: <prefix>}`. Read the state of Open WebUI's llama-swap models directly from that `llamaSwap` backend; see [Loaded state through Open WebUI](#loaded-state-through-open-webui). |

##### What a request can use

A request through the gateway gets the tools Open WebUI's chat UI would offer the same account, with the same switches.
The model decides which to call, and Open WebUI runs them before the answer comes back.

- **Tool servers and tools**, listed by ID in `tool_ids`: MCP servers and OpenAPI tool servers connected to Open WebUI (an MCP server's ID looks like `server:mcp:<name>`), and Python tools from Open WebUI's workspace. Tools attached to a workspace model are added by Open WebUI's browser UI, not its server, so list them in `tool_ids` too.
- **Built-in tools turned on per request**, through `features`: `web_search` (search the web and fetch pages), `memory` (read and update the memories Open WebUI keeps between conversations), `image_generation`, and `code_interpreter`.
- **Built-in tools that need no switch**, offered whenever they are enabled in Open WebUI and the account has permission: search and read the knowledge bases the account can access (or only those attached to the model), the account's notes and past chats, the current time, and, where enabled, calendar, automations and notifications.

Tools and features are fields of Open WebUI's request, so set them with `params`.
Open WebUI's request also has a `params` field of its own, for model options such as `think` and `num_ctx`, which is why that key appears twice:

```yaml
params:
  features: { web_search: true, memory: true }
  tool_ids: [server:mcp:home-assistant]
  params: { think: false }
```

Some tools can't work through the gateway, because Open WebUI runs them in the user's browser and a gateway request has no browser behind it: direct tool servers, asking the user a question mid-answer, and the code interpreter when it uses the default Pyodide engine. Set Open WebUI's code interpreter to its Jupyter engine to use it through the gateway.

Everything a tool reads or writes belongs to an Open WebUI account: the account behind `apiKey`, or each client's own account with `passthroughAuth`.
That account's permissions decide which built-in tools are offered.
Without passthrough, every client of the gateway shares that account's memories, notes and chat history; with it, each caller has their own.

The built-in tools' definitions take about 2,000 tokens of context.
On a small Ollama model, raise its context in the same `params` block (e.g. `params: { num_ctx: 8192 }`) or the start of the conversation is cut off.

##### Loaded state through Open WebUI

Open WebUI began as a front end for Ollama, and its model list reports load state precisely only for its Ollama connections.
For an OpenAI-compatible connection there is no standard way to ask which models are loaded, so Open WebUI passes on whatever that server's own model list says, and the gateway reads what it can:

- Ollama models: whether Ollama has them loaded, as Open WebUI reports it.
- llama-swap models: llama-swap's `status` field, which says `loaded` for any model that isn't stopped. A model that is still starting, or already stopping, looks ready.
- Facades of a gateway connected to Open WebUI: that gateway's own `current` selection.
- Workspace models: their base model's state.
- Anything else, such as a hosted OpenAI-compatible provider: unknown, which selects like not loaded.

If Open WebUI uses llama-swap as one of its connections, and the gateway can reach that llama-swap too, the `llamaSwap` key lets the gateway skip Open WebUI for state and ask llama-swap directly.
llama-swap's `/running` reports each model as `ready`, `starting` or `stopping`, so the selection steps apply exactly, as they do for a `llamaSwap` backend.
Set `backend` to the gateway's `llamaSwap` backend entry for that same llama-swap, and `prefixId` to the prefix Open WebUI's connection adds to its model IDs, if it has one:

```yaml
backends:
  llamaSwap:
    type: llamaSwap
    baseUrl: http://llama-swap:8080
  openWebUi:
    type: openWebUi
    baseUrl: http://open-webui:8080
    apiKey: ${OPEN_WEBUI_API_KEY}
    llamaSwap: { backend: llamaSwap }
```

If that llama-swap can't be reached, its models fall back to the `status` Open WebUI reports, as without the key.
If a llama-swap model in Open WebUI's list isn't listed by the named backend, `prefixId` or `backend` is wrong, and those models would never show as loaded; a warning says so, at startup, or once both backends answer if one of them was down.

The API key needs these Open WebUI endpoints: `/api/models`, `/api/v1/chats/new`, `/api/chat/completions`, `/api/v1/chats/{id}` (`GET` and `DELETE`), `/api/tasks/chat/{id}` and `/api/tasks/chat/{id}/stop`, plus `/api/v1/chats/{id}/archive` with `chats: archive`.

### `models`

Each entry is a facade, and its name is what clients request.

| Key | Description |
|-----|-------------|
| `backend` | Name of a `backends` entry: the default for its candidates. Required unless every candidate names its own. |
| `candidates` | The models to choose from, in order of preference. Required. |
| `params` | Applied to every request for this facade. Optional. |
| `description` | Shown in `GET /v1/models`. Optional. |
| `type` | Facade type. `candidates`, the default, is the only type today. |

Each candidate takes:

| Key | Description |
|-----|-------------|
| `model` | An exact backend model ID or alias, or a pattern using `*`, `?` or `[...]`. |
| `backend` | The backend this candidate is on. Default: the facade's `backend`. |
| `loadIfNone` | `true`: if no candidate is ready or starting, the first `loadIfNone` candidate is used and its backend loads it. Default `false`: only used when ready or starting. Must be an exact ID. |
| `params` | Applied after the facade's `params` when this candidate wins. Optional. |

## Commands

```
ai-model-facade serve <config>    run the gateway
ai-model-facade check <config>    validate the config and print each candidate's resolved params
ai-model-facade health <config>   exit 0 if the gateway at server.listen answers /health, 1 if not
ai-model-facade version           print the version, which the startup log line also reports
```

`serve` and `check` make the same checks; `check` fails on what `serve` only warns about.
See [Errors and warnings](#errors-and-warnings).

### Reloading the config

Send `serve` a `SIGHUP` to reload the config without a restart:

```bash
kill -HUP <pid>
docker kill -s HUP <container>
docker compose kill -s SIGHUP ai-model-facade
```

The new config is loaded and validated in full first, and replaces the running one only if it has no errors.
Otherwise the running config stays and the error is logged.
Requests already in flight, including streams, finish on the config that accepted them.
Everything can change on reload except `server.listen`, which needs a restart; a reload that changes it is rejected.
Run `check` first to see what a reload would load.

`health` only reads `server.listen` from the config and contacts no backend; a wildcard address such as `0.0.0.0:8088` is checked on `127.0.0.1`.
The endpoints the gateway serves are listed under [Connecting Clients](#connecting-clients).
