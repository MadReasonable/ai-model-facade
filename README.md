# ai-model-facade

An OpenAI-compatible gateway that serves **virtual models** in front of [llama-swap](https://github.com/mostlygeek/llama-swap).

A client asks for a model name that doesn't exist on any backend, such as `task-monkey`.
The gateway decides what that name means: it picks a real model from an ordered candidate list, merges in the request parameters you configured, and forwards the request.

> [!NOTE]
> **Pre-release.** llama-swap and Ollama are the backends so far; an Open WebUI backend is in progress.
> Config keys may still change before the first release.

## Why

Many clients let you set only one model name, and in front of llama-swap a fixed name is a poor fit:

- **If that model isn't loaded**, llama-swap swaps it in and evicts whatever you were using, even for a two-word reply.
- **If you point the client at a preset** to get the right parameters, those parameters are fixed. You can't add to them per client, because llama-swap's `setParamsByID` replaces top-level keys such as `chat_template_kwargs` instead of merging them.

A virtual model keeps the client's model name fixed, while the gateway picks the first model on your list that llama-swap reports as **ready** and applies exactly the parameters you set.
Nothing is evicted unless nothing on the list is loaded, and the config decides what happens then.

Some uses:

- **Open WebUI background tasks.** Titles, tags and follow-ups run on whichever LLM is already loaded, with thinking off, instead of swapping in a fixed task model or inheriting a reasoning preset from the chat. This also covers Open WebUI's `think: false`, an Ollama-style flag that llama-server ignores.
- **A voice assistant that is always available.** Home Assistant or another assistant gets a fast, non-thinking model whatever is loaded, whether you're in a coding set or a general one, without a swap.
- **Any client with a fixed model setting.** Give it a name like `quick` or `reasoning`, and decide in one place what that name means for each model you might have loaded.

## Quick Start

You need a running llama-swap, and Go 1.25 or newer (or Docker; see [Running in Docker](#running-in-docker)).

```bash
git clone https://github.com/MadReasonable/ai-model-facade.git
cd ai-model-facade
cp config.example.yaml config.yaml
```

Edit `config.yaml`: set `backends.llamaSwap.baseUrl` to your llama-swap address, and list your models under `models.task-monkey.candidates`.
Then validate it against llama-swap and start the gateway:

```bash
go run ./cmd/ai-model-facade check config.yaml
go run ./cmd/ai-model-facade serve config.yaml
```

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

```bash
docker build -t ai-model-facade .
```

Run it as a service in the same compose project as llama-swap, so `baseUrl` can use llama-swap's service name:

```yaml
services:
  ai-model-facade:
    image: ai-model-facade
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


To run `check` in a container, attach it to llama-swap's network:

```bash
docker run --rm --network <llama-swap's network> \
  -v "$PWD/config.yaml:/config/config.yaml:ro" \
  ai-model-facade check /config/config.yaml
```

## Connecting Clients

Point any OpenAI-compatible client at `http://<gateway>:8088/v1`, with the key from `server.apiKeys` if you set one, and use a virtual model's name as the model.

| Endpoint | Description |
|----------|-------------|
| `POST /v1/chat/completions` | Chat completions, streamed or not. |
| `POST /v1/completions` | Legacy text completions, streamed or not. Same selection and params as chat. |
| `GET /v1/models` | Lists only the virtual models, so clients that pick from a list see just those. Each includes `current` (below). |
| `GET /v1/models/{id}` | One virtual model, including `current`, or `404`. |
| `GET /health` | `{"status":"ok"}` while the gateway is up. Needs no API key and doesn't contact any backend. |

Clients that should use llama-swap's models directly keep their existing connection to llama-swap.

Model objects have the standard OpenAI fields plus `current`: where a request would be routed right now.

```json
{"id":"task-monkey","object":"model","created":1792628190,"owned_by":"ai-model-facade",
 "description":"Background tasks on whichever LLM is already loaded, thinking off",
 "current":{"model":"qwen3.8-flash-next:iq3_s","reason":"loaded"}}
```

`reason` is `loaded`; `starting` when nothing is ready and a request would wait for that model to finish loading; or `loadIfNone` when nothing is ready or starting and that model would be loaded.
When no request would succeed, `current` has no `model` and the reason is `no candidate loaded`.
`alias_of` is added when `model` is a llama-swap alias.
It's a snapshot, so the next request can be routed differently if what's loaded changes. If llama-swap's state can't be read, `current` is left out.

### Example: Open WebUI task model

1. In **Admin Panel → Settings → Connections**, add a second OpenAI connection with the URL `http://ai-model-facade:8088/v1`.
2. In **Admin Panel → Settings → Models**, hide `task-monkey` so it doesn't appear in the chat model picker.
3. In **Admin Panel → Settings → Interface**, set the **Task Model** to `task-monkey`.

Chat traffic keeps going straight to llama-swap; only background tasks go through the gateway.

## How It Works

### Choosing a model

Each virtual model has an ordered list of candidates. The gateway walks the whole list up to three times, stopping at the first match:

1. **Ready.** The first candidate that llama-swap's `/running` reports in state `ready` wins.
2. **Starting.** If nothing is ready, the first candidate that is still `starting` wins, and the request waits for that load to finish. This avoids asking llama-swap to load a second model, which could evict the one already starting.
3. **Fallback.** If nothing is ready or starting, the first `loadIfNone` candidate wins, and llama-swap loads it.

A model that is stopping or stopped never counts.
Ollama's `/api/ps` lists a model only once it has finished loading, so on an Ollama backend the gateway tracks loads itself: a model counts as starting while a request the gateway forwarded is waiting for it to load. A load started by a client that goes straight to Ollama isn't visible until it finishes.

- A pattern such as `qwen3.8-27b:*` matches ready (or starting) models, and the matched ID is forwarded. If several match, the first in sorted order wins.
- A llama-swap alias (a preset such as `qwen3.6-35b:Slacker`) can be a candidate too. It is ready (or starting) whenever its base model is, and is forwarded under the alias ID so llama-swap applies the preset. Patterns prefer base models over aliases, so `qwen3.8-27b:*` picks the quant rather than its `:Dreamer` preset.
- A pattern can also pick a preset across model families. If each family defines an `:Assistant` preset in llama-swap, `"*:Assistant"` matches whichever one is loaded; `config.example.yaml` uses this for a Home Assistant voice model. Quote a pattern that starts with `*`, because a leading `*` is YAML alias syntax.
- A candidate with `loadIfNone: true` is used like any other candidate when it's ready or starting, and is also the fallback in step 3. It can sit anywhere in the list: its position sets its priority in steps 1 and 2, while the fallback only happens after the whole list has been checked.
- If nothing matches, the request gets `503`. Open WebUI, for example, then skips the task.

There is no failover: a request is never retried against another model.

Prefer base model IDs for anything the gateway must control.
llama-swap applies a preset's own params after the gateway's, replacing whole top-level keys such as `chat_template_kwargs`, so params on an alias candidate may be overridden.
The gateway logs an INFO line at startup for each such candidate, and `check` notes it.

### Rewriting the request

Every `params` value is applied to the request body as a JSON Merge Patch ([RFC 7386](https://www.rfc-editor.org/rfc/rfc7386)):

- Objects merge key by key, so `chat_template_kwargs: { enable_thinking: false }` keeps any other kwargs the client sent.
- Scalars and arrays replace the client's value.
- `null` deletes a key, e.g. `think: null`.

The steps run in this order, with later ones overriding earlier ones:

```
client body → model params → winning candidate params → model set to the concrete ID
```

Gateway params override the client: they are patches, not defaults.
The rest of the request passes through byte-for-byte, including messages, tools, `response_format` and vendor fields, in their original order.

The same params apply to chat and legacy completions.
Some only mean something for chat: `chat_template_kwargs` has no effect on `/v1/completions`, because no chat template is applied, so a model meant for chat won't have thinking turned off there.
Give legacy completions their own virtual model if a client uses them.

`params` can also be a list of maps, applied in order.
Use the list form to combine YAML anchors, because YAML's `<<:` merge is shallow and would overwrite nested objects like `chat_template_kwargs`:

```yaml
vars:
  lowEffort: &lowEffort
    chat_template_kwargs: { enable_thinking: true, reasoning_effort: low }

# ...in a model or candidate:
params:
  - *lowEffort
  - thinking_budget_tokens: 2048
```

### Authentication

Clients authenticate with `Authorization: Bearer <key>`.

- A key listed in `server.apiKeys` is accepted, and its name appears in the log as `client=<name>`.
- An unlisted key is rejected with `401`, unless `server.unknownKeys` is `allow`. Then it's accepted and logged as `client=unknown` with a fingerprint: the first 8 hex characters of the key's SHA-256, so different callers can be told apart. Keys themselves are never logged.
- With no `apiKeys` listed, every request is accepted.
- `GET /health` never needs a key.

By default, a client's key stays at the gateway, and each backend gets its own `apiKey`.
With `passthroughAuth: true` on a backend, completion requests to it carry the client's own `Authorization` header instead, so the backend makes the access decision and sees each caller as themselves.
A client that sent no key is forwarded without one; it never borrows the backend's key.
The backend's `apiKey` is still used for the gateway's own calls, such as validating models at startup and checking which are loaded.

Passthrough is set per backend, so a client's key only goes to backends that opted in.
To pass through every caller's key without listing them all, combine it with `unknownKeys: allow`, and name the keys you want to see in the log.

### Responses and errors

Responses are passed through unchanged, and streams are relayed event by event.
Whenever a model was chosen, the response has an `X-Gateway-Target` header naming it.

| Status | Meaning |
|--------|---------|
| `400` | The body isn't a JSON object, or it repeats a key. Duplicate keys are rejected because the gateway patches the first occurrence while backends read the last, which would let a request slip values past the gateway's params. |
| `401` | `server.apiKeys` is set, `unknownKeys` is `reject`, and the request has no matching `Authorization: Bearer` key. |
| `404` | The requested model isn't a virtual model. |
| `413` | The body is over 100 MiB. |
| `502` | llama-swap couldn't be reached. |
| `503` | No candidate is eligible. |

Once a stream has started, its status can't change.
If the backend fails mid-stream, the gateway sends a final `data: {"error": {...}}` event and closes without `[DONE]`, so clients show an error instead of a silently cut-off reply.

## Configuration

The config is YAML. `config.example.yaml` is a complete setup with two virtual models: `task-monkey` for Open WebUI background tasks, and `assistant` for a Home Assistant voice assistant that uses whichever `:Assistant` preset llama-swap has loaded.
Unknown keys are startup errors reported with their line number, and so are undefined environment variables and exact candidate IDs that the backend doesn't list.

`${NAME}` in any string value is replaced by the environment variable `NAME` when the config loads. It is meant for secrets; there is no other substitution.

`vars` is ignored by the gateway. It is a place to define YAML anchors (`&name`) that entries reference explicitly (`*name`), so every virtual model shows exactly what it uses.

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
| `logSelection` | Log one line per request: virtual model, candidates checked, winner and reason. Failed requests are always logged. |

### `backends`

Each entry is a named backend; `type` selects the adapter: `llamaSwap` or `ollama`. Both take the same keys:

| Key | Description |
|-----|-------------|
| `baseUrl` | The backend's address. Required. |
| `apiKey` | Sent to the backend as `Authorization: Bearer`, on every call unless `passthroughAuth` is on, and then only on the gateway's own calls. Optional. |
| `passthroughAuth` | `true`: completion requests carry the client's own `Authorization` header (or none). Default `false`. |
| `stateCacheSeconds` | How long to cache the list of loaded models (llama-swap's `/running`, Ollama's `/api/ps`). Default `0`, which asks on every request. Concurrent requests share one in-flight check either way. |
| `stateTimeoutSeconds` | Timeout for the loaded-model and `/v1/models` calls. Default `10`. |
| `connectTimeoutSeconds` | Connection timeout. Default `0`, no limit. |
| `requestTimeoutSeconds` | How long to wait for the backend to start responding. Allow for a cold load of a `loadIfNone` candidate; a stream isn't cut off once it has started. Default `0`, no limit. |

Ollama model IDs are the full `name:tag` form that `ollama list` shows, such as `llama3:latest`.
A candidate without a tag, such as `llama3`, isn't treated as `llama3:latest`; it's a startup error that suggests the tagged IDs Ollama lists.
Ollama itself ignores `apiKey`, but a proxy in front of it may need one.

### `models`

Each entry is a virtual model, and its name is what clients request.

| Key | Description |
|-----|-------------|
| `backend` | Name of a `backends` entry. Required. |
| `candidates` | The models to choose from, in order of preference. Required. |
| `params` | Applied to every request for this model. Optional. |
| `description` | Shown in `GET /v1/models`. Optional. |
| `type` | Virtual model type. `candidates`, the default, is the only type today. |

Each candidate takes:

| Key | Description |
|-----|-------------|
| `model` | An exact backend model ID or alias, or a pattern using `*`, `?` or `[...]`. |
| `loadIfNone` | `true`: if no candidate is ready or starting, the first `loadIfNone` candidate is used and llama-swap loads it. Default `false`: only used when ready or starting. Must be an exact ID. |
| `params` | Applied after the model's `params` when this candidate wins. Optional. |

## Commands

```
ai-model-facade serve <config>    run the gateway
ai-model-facade check <config>    validate the config and print each candidate's resolved params
ai-model-facade health <config>   exit 0 if the gateway at server.listen answers /health, 1 if not
```

`serve` and `check` validate against llama-swap, so it must be reachable when they start.

### Reloading the config

Send `serve` a `SIGHUP` to reload the config without a restart:

```bash
kill -HUP <pid>
docker kill -s HUP <container>
docker compose kill -s SIGHUP ai-model-facade
```

The new config is loaded and validated in full first, including against llama-swap, and replaces the running one only if all of it succeeds. Otherwise the running config stays and the error is logged.
Requests already in flight, including streams, finish on the config that accepted them.
Everything can change on reload except `server.listen`, which needs a restart; a reload that changes it is rejected.
Run `check` first to see what a reload would load.

`health` only reads `server.listen` from the config and contacts no backend; a wildcard address such as `0.0.0.0:8088` is checked on `127.0.0.1`.
The endpoints the gateway serves are listed under [Connecting Clients](#connecting-clients).
