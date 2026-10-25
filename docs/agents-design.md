# ai-model-facade design

**Why the gateway is built the way it is: the rules it keeps, the mechanisms behind them, and the approaches it rejected.**

This document is for coding agents about to change the code; people should start with `ARCHITECTURE.md`, the map of the packages.
The README describes what the gateway does for a user; this document explains why it does it that way and how, so a change can keep the rules the rest of the code relies on.
Facts about other projects are marked with how they were checked: "verified" against a running instance of the named version, "source" when read from that version's code and not run.

## The problem

Servers that load models on demand, such as llama-swap and Ollama, load whatever model a request names and evict others to make room.
A client configured with one fixed model name therefore causes a swap whenever that model isn't loaded, even for a request that any loaded model could answer, such as generating a chat title.
Clients also can't adjust a request per model: a thinking switch or a token limit belongs to the model that answers, which the client doesn't choose.

Fixing this on the backend side doesn't work in general.
llama-swap's per-model params (`setParamsByID`) replace a request's top-level keys, so a preset that sets `chat_template_kwargs` erases every kwarg the client sent under it (llama-swap source).
Open WebUI's task-model settings either fix one model or reuse the chat's model, with its preset.

A second problem is tools.
Open WebUI runs tools server-side, such as web search, memory and connected tool servers, but only for requests from its own UI flow, which takes several requests: create a chat, start the completion, poll for the answer.
A request to its OpenAI-compatible endpoint without that flow gets native tool calls handed back instead of run (Open WebUI 0.12.3, verified), so a client with no tool support of its own can't use them.

## The concept: facades

A client asks for a model name that no backend has.
That name is a facade (`virtualmodel` in the code), and the gateway decides at request time which real model answers it: it reads which models are loaded, picks one from an ordered list, patches the request with configured params, and forwards it.
A candidate can be an Open WebUI model, and then the adapter runs Open WebUI's UI flow on the client's behalf and returns one ordinary OpenAI response or stream, so the same plain request gets Open WebUI's tools.

Combining the two puts the gateway on both sides of Open WebUI: a client asks the gateway for a facade, the gateway asks Open WebUI, and Open WebUI, which has the gateway as a connection, asks the same gateway for another facade backed by whatever model is loaded.
That chain is legitimate, and three rules exist because of it.
A request can come back round to the facade it started from, so loops are detected per facade, not per gateway (Routing loops).
Reading Open WebUI's state makes Open WebUI call this gateway's `/v1/models`, which reads the same backends again, so a backend's state is read at most once per request chain (Fetching) and `/v1/models` never waits on a state fetch already in flight, which may be its own (Not waiting on itself).

Each facade is a self-contained policy in the config.
Shared settings are reused only through YAML anchors that each entry references explicitly, so reading an entry shows everything it does.
Decided against: a `families:` section mapping model-ID patterns to shared overlays.
It made an entry's behaviour depend on patterns defined elsewhere, so a new model could change a facade nobody edited; the duplication it saved is small, and anchors cover it.

Selection is not failover.
A request goes to exactly one model and is never retried on another, because a retry after a failed request could trigger the very load selection exists to avoid.

## Architecture

A request passes through five stages, each a separate package:

```
client -> front end -> facade -> selector -> params -> backend adapter -> backend
          (gateway)  (virtualmodel) (selector) (mergepatch) (backend/...)
```

The front end (`internal/gateway`) speaks the OpenAI API and does HTTP and SSE framing only.
A facade (`internal/virtualmodel`) owns its policy: which selector, which params, which backends.
The selector (`internal/selector`) turns backend state into one chosen model.
Params (`internal/mergepatch`) are applied to the body as byte edits.
An adapter (`internal/backend/...`) forwards the request and returns either a complete response or a stream of SSE payloads; the front end relays both.

### Interfaces

Three interfaces separate the stages, and each one is where a kind of extension goes.

- `backend.Backend` is the adapter: `ListModels` and `Chat`. A new kind of backend is a new package implementing it, usually on the shared `openaihttp` client and state cache, plus one case in `gateway.newBackend`.
- `backend.ModelStateProvider` is an optional capability: `LoadedModels` reports ready, starting and unknown models. It is optional because a backend that can't report state can still serve a facade whose selector doesn't need it.
- `selector.Selector` declares the capabilities it `Requires`, and the gateway checks them against each candidate's backend when the config loads, so a facade that can't work is a startup error, not a request-time one.

Further capabilities follow the same pattern: an interface a backend may implement, found by type assertion (`backend.Has`), and required by the selectors or facade types that need it.
A backend that refers to other backends, as the Open WebUI adapter's `llamaSwap` block does, implements `Link`, which the gateway calls once every backend exists.

### Facade types

A facade entry may set `type`, and each type has its own schema, validator and handler (`virtualmodel.Build`).
The only type is `candidates`, the default.
A new behaviour is a new type, so existing entries never change meaning.

## Request bodies

Request and response bodies are never decoded into Go structs or maps.
Decoding would reorder keys, could change number precision, and would drop or mangle fields the gateway doesn't model, such as tools, `response_format` and vendor extensions.
The gateway reads the few values it needs (`model`, `stream`) with gjson, edits with sjson, and forwards the bytes, so the forwarded body equals the client's except for the keys the config patches.

### Params as merge patches

Every `params` value is a JSON Merge Patch (RFC 7386): objects merge key by key, scalars and arrays replace, `null` deletes.
Merging is the point of the project: a patch that sets `chat_template_kwargs.enable_thinking` keeps the client's other kwargs, which is what llama-swap's top-level replacement can't do.
`params` may also be a list of patches applied in order, because YAML's `<<:` merge is shallow and would replace nested objects when combining anchors.

The order is fixed: client body, then the facade's params, then the winning candidate's params, then `model` rewritten to the chosen ID.
Gateway params come after the client because they are the policy, not defaults.

### The walker

`mergepatch` iterates the patch, not the body, since a patch from config is small and the body may be large.
For each key in config order: `null` deletes the path; an object recurses if the body holds an object there, and otherwise is written as a new object with its own nulls removed (RFC 7386); anything else is written as raw JSON.
Leaf values are encoded once when the config loads.

Keys keep their place for people reading logs and diffs: existing keys stay where the client put them, and sjson appends new keys to their object.
For added keys to follow the config's order, patches are compiled from the `yaml.Node` tree, never from a Go map.

Every key segment is escaped for sjson's path syntax (`.`, `*`, `?`, `|`, `#`, and a leading `:`, which sjson reads as its force-key marker), because model names like `qwen3.6` appear as keys.

Merge patch can't send a literal `null`; nothing has needed one yet.

## Config loading

Loading turns a YAML file into a validated gateway in four steps, and stops at the first error with its file line:

1. Parse to a `yaml.Node` tree.
2. Expand `${NAME}` in scalar string nodes only, so a value can't inject YAML structure; an undefined variable is an error.
3. Decode strictly. yaml.v3's `KnownFields(true)` is a `Decoder` option that `yaml.Node.Decode` ignores, so `config.DecodeStrict` walks the node tree against the target's `yaml` tags, following anchors and `<<:` merges, and rejects unknown keys before decoding. Backend and facade entries stay nodes until their own type decodes them.
4. Build every backend and facade (`gateway.Build`), checking references and capabilities.

### Errors and warnings

A config that is wrong fails: unknown keys, an undefined variable, a duplicated name, a reference to a backend that doesn't exist, a selector whose capability the backend lacks.
A check that depends on a backend being up and current only warns, because a backend that is down, or hasn't loaded yet, must not take down the gateway and every other backend's facades with it.
Open WebUI makes this necessary even with every service up: it lists a gateway's facades only once that gateway serves them, so a gateway with a candidate behind Open WebUI could otherwise never start.

`serve` and reloads log the warnings and start; `check` reports them as errors and exits non-zero, so a config can still be verified before it is deployed.

### Checks against model lists

Checks that need a backend's model list (`backend.ListingCheck`: exact candidate IDs, alias notes, the Open WebUI prefix check) run once every backend they name has been listed.
All backends are listed at the same time, so backends that hang cost one `stateTimeoutSeconds` between them rather than one each, and can't use up the time a reload has.
A check whose backend couldn't be listed is kept, `serve` lists that backend again every `listRetrySeconds`, and the check runs once it answers.
A reload cancels these retries along with the gateway that started them.

Decided against: re-running a skipped check from the backend's own state refresh.
That put model-list calls inside the refreshes that requests wait on, and only one adapter could use it.

### Reload

`SIGHUP` builds a complete new gateway from the file and swaps it in only if that succeeds (`gateway.Live`), so a broken edit leaves the running gateway as it was.
Requests already in flight finish on the gateway that accepted them.
A changed `server.listen` is rejected, because the port is already bound.

Because a failed build is thrown away, building has no side effects that outlive it: `Build` and every adapter's `New` start no goroutines.
Background work, such as listing a backend again for its pending checks, starts in `Gateway.start`, after the new gateway has been swapped in, and stops when a later reload replaces it.

## Selection

The `candidates` type uses the `FirstLoaded` selector, which exists to keep one rule: the gateway never causes a load the candidate list didn't ask for.
It walks the list up to three times:

1. the first candidate its backend reports ready;
2. else the first one still starting, so the request waits for a load already under way rather than start another, which could evict it;
3. else the first `loadIfNone: true` candidate, wherever it sits in the list.

A model that is stopping or stopped never counts, since a request to it would load it again.
A model whose state can't be read counts as not loaded, so only `loadIfNone` can pick it.

Aliases (llama-swap presets, Open WebUI workspace models) are candidates with their base model's state, since loading an alias loads its base.
A pattern matches base models before aliases, each in sorted order, so a pattern matching both resolves to one well-defined model; it says nothing about which is better.
An alias is forwarded under its own ID so the backend applies its settings, and those settings are applied after the gateway's params, which is why an alias candidate with params gets a startup note.

### Candidates on several backends

A candidate may name its own backend, and the list is still one list in order.
Selection starts every backend's state read at once and waits for a backend only when it reaches a candidate there.
A ready candidate wins once its own backend and the backends of all earlier candidates have answered, so a slow backend delays only lists where it comes first.
A starting or `loadIfNone` winner waits for every backend, because a ready candidate anywhere in the list beats it.
Reads no longer needed finish in the background.
If some backends can't be read, their candidates count as not loaded; if none can be, the request fails with 502.

### Observability

Every request logs one line with the facade, each candidate checked and its outcome, the winner and the reason, and the response carries the winner in `X-Gateway-Target`.
Selection depends on state that changes from second to second, so without these a surprising choice couldn't be explained afterwards.
`GET /v1/models` adds a non-standard `current` to each facade: the selection a request would get now, through `Handler.Current`.

## Backend state

Selection is only as good as the state it reads, so state reads have their own rules, implemented once in `openaihttp.StateCache` and shared by every adapter.

### Fetching

A state fetch runs detached from any request's context with its own timeout (`stateTimeoutSeconds`, default 10 s), and concurrent reads join the fetch in flight instead of queuing their own, so a slow backend delays each request once and a client that leaves doesn't fail the others.
A fetched state is reused for `stateCacheSeconds` (default 0).

Within one request, a backend's state is read at most once.
Every request carries its arrival time (`backend.WithRequestStart`), and the cache treats any state fetched since then as fresh enough for that request.
This matters when the gateway sits on both sides of Open WebUI (The concept: facades): reading Open WebUI's state makes Open WebUI call this gateway's `/v1/models`, a nested request that reads the same backends; it arrives later, so what it fetches serves the outer request too.
Decided against: special cases that passed a start time through particular call paths, which covered fewer setups and threaded a time through every adapter.
The reuse works only between reads of the same backend entry in the same process.

`/v1/models` selects every facade against one snapshot (`backend.WithStateSnapshot`), so one listing reads each backend once and never judges two facades against different states.

### Loads the gateway started

A backend's own report can miss a load the gateway began: Ollama lists a model only once it is loaded, and a cached state can predate the load.
So the cache counts each request sent and not yet released (`StateCache.Pending`), under the base model for an alias, and every read reports a counted model the backend doesn't list, or can't report, as starting.
What the backend itself reports as ready or starting is kept.
The count is taken before and after the backend read, so a load that ends or begins during the read still shows.

Counting is wired per adapter: `openaihttp.Client` counts its chat requests only after the adapter's `New` calls `client.TrackPending(state)`, as `llamaswap` and `ollama` do.
An adapter without it still works, but its loads never show as starting, so concurrent requests fall through to a `loadIfNone` fallback and cause the swap the count exists to avoid.
A request is released when its response headers arrive, since llama-swap and Ollama both send headers only once the model is loaded (Ollama verified on 0.40.8: headers arrive with the first token).
The Open WebUI adapter releases at the first output its polling sees, because Open WebUI answers the start of a chat at once.
On release, if the cached state didn't list the model as ready, a load just finished or failed and may have evicted other models, so the cache is dropped (`StateCache.Invalidate`); a refresh already in flight is then neither cached nor joined, since it may predate the change.

Decided against: deciding when a request is sent whether to count it (only if its model isn't loaded).
That needs a current state at send time and misses a model evicted between the send and the backend receiving it.

Limit: a load started by a client that bypasses the gateway is visible only once the backend reports it.
A model also stops counting when its request ends, however it ends; if the client leaves mid-load, Ollama likely finishes the load, but the model reads as not loaded until `/api/ps` lists it, and a request in that window may pick a `loadIfNone` fallback.

### Not waiting on itself

When the gateway is an Open WebUI connection and also has an Open WebUI backend, building `current` for `/v1/models` refreshes Open WebUI's model list, which makes Open WebUI call this gateway's `/v1/models` again.
That nested call would join the fetch already in flight and wait on itself until the state timeout.
So `/v1/models` reads state under `backend.WithoutStateWait`: if a fetch is in flight it answers from the last cached state, or omits `current` if there is none.
Any such loop, through however many gateways and Open WebUI instances, comes back to a gateway whose fetch is in flight, so it ends after one round.
Chat selection still waits as usual.
The cost is that the copy of this gateway's list Open WebUI caches during the loop may show a stale or missing `current` for facades on Open WebUI.

## Backend adapters

Each adapter maps one backend's API onto `Backend` and, where it can, `ModelStateProvider`.
`llamaSwap` and `ollama` forward requests unchanged through `openaihttp.Client`; `openWebUi` runs Open WebUI's chat flow and builds the response itself.
Every adapter takes the same connection keys (`baseUrl`, `apiKey`, `passthroughAuth`, the timing keys), because they all come from `openaihttp.Config`.

### llama-swap

State comes from `GET /running`, where `ready` is ready and `starting` is starting; aliases come from `meta.llamaswap` in `/v1/models`, which llama-swap provides from v241 on (commit `8b61e3d`).

How llama-swap schedules loads (v263 source, `internal/router/scheduler/fifo.go`, not run) decides what a wrong choice costs.
A load already under way for the same model is joined, a ready model with nothing to evict is served at once, and a swap that would collide with one under way, or evict a model with requests in flight, waits in a queue.
So llama-swap never stops a model mid-load or mid-request, and a wrong choice by the gateway costs an extra swap and the wait for it, not a lost load; the pending count exists to avoid that swap.
A queued swap waits for the model to go idle, so a steady stream of requests to a ready model can hold a swap off.
Each model admits at most `concurrencyLimit` requests (default 10) and refuses more with a 429 and `Retry-After`, which the gateway passes on.

### Ollama

State comes from `GET /api/ps`, which lists a model only once it has loaded and has no load state of its own (verified on 0.40.8; a model unloaded to make room disappears as soon as the new load starts).
Every listed model is ready, and loads under way are visible only through the pending count.

Decided: model IDs are the exact `name:tag` that Ollama lists.
A bare name is not mapped to `name:latest`, which would be implicit matching by ID; instead, startup suggests the listed IDs with a tag.

### Open WebUI

The adapter gives a client without tool support Open WebUI's tools, by running a request the way Open WebUI's own UI does and returning one ordinary OpenAI response or stream.
Everything here was read from the Open WebUI 0.12.3 source and checked against a running 0.12.3 unless marked otherwise.

The flow, driven with the backend's key:

1. `POST /api/v1/chats/new` with a history of the user message and an empty assistant placeholder, both with IDs the adapter generates. A `chat_id` Open WebUI doesn't know is a 404, so the chat must exist first.
2. `POST /api/chat/completions` with the client's body plus `chat_id`, the assistant message `id`, a `session_id`, and background tasks (title, tags, follow-ups) off. It returns at once with task IDs; the model call and the tool loop run in the background.
3. Poll `GET /api/v1/chats/{id}` for the assistant message and `GET /api/tasks/chat/{id}` for the task, every `pollIntervalSeconds` (default 0.25). The answer is finished when the message is done and no task is left.
4. Build the response, then delete, archive or keep the chat as `chats` says (default `delete`, since the gateway's clients didn't ask for a chat history). Cleanup runs in the background, so the response doesn't wait on it, and a task the client left is stopped first.

`session_id` is what makes Open WebUI treat the request as coming from its UI: with native function calling it adds the built-in tools enabled by `features` (such as `search_web` with `features.web_search`), runs the tool loop server-side, and publishes partial output while the task runs.
Verified: with `features.web_search` on, the model called `search_web` and the answer used the results.
The fake session has no websocket, so tools that need the browser (direct tool servers, the Pyodide code interpreter, `ask_user`) can't work (source).

Modes considered and not used:
without `chat_id`, a request is passed through synchronously and native tool calls go back to the caller instead of running;
with `chat_id` and `id` but no `session_id`, the loop runs on `tool_ids` only, the POST blocks until done and returns `null`, and polling shows no partial output, so it can't stream (verified);
`params.function_calling: "legacy"` runs `tool_ids` for one prompt-picked round but adds no built-ins;
a `temporary:` chat ID saves nothing, so the result is only on the websocket.

Streaming works by polling: the chat GET overlays the running task's output onto the stored message, and the adapter emits the new part of `content` as chunks, with the model's reasoning as `reasoning_content` ahead of it.
Verified: polling every ~130 ms saw the content grow in 30-60 character steps.
Without Redis, Open WebUI keeps the in-progress snapshot in process memory, so polling still streams with a single Open WebUI process (source; not run).

The response is synthesized from the finished message (`content`, `output` items, `usage`), the one exception to responses passing through unchanged.
The request side is still edited as bytes, so unknown client fields reach Open WebUI.

The client's `messages` are what the model sees.
Open WebUI replaces them with the stored history only when the body names a user message (`user_message`, or `assistant_message_id` for a continuation), and the flow sends neither, so the stored chat is only a record of the last turn (verified with a three-message conversation).
Built-in tool definitions add about 2,000 prompt tokens, which on a model with a small context silently drops the start of the conversation (verified on an Ollama model capped at 2,050 tokens).

Tools are `params`, not backend keys: `features` and `tool_ids` are request-body fields, so a facade or candidate sets them like any other param, and the account's permissions bound them.
Tools attached to an Open WebUI workspace model are added by the browser, not the server, so `tool_ids` must be sent explicitly.
Open WebUI calls Ollama-connection models through Ollama's native API, so thinking is turned off there with `params: {think: false}`; `reasoning_effort: none` and a top-level `think: false` are ignored (both verified).

Open WebUI has no legacy `/v1/completions`, so the adapter refuses that endpoint in `Chat`, keyed on `ChatRequest.Endpoint`.
An API key limited by `API_KEYS_ALLOWED_ENDPOINTS` needs `/api/v1/chats/new`, `/api/chat/completions`, `/api/tasks/chat/{id}` and its `/stop`, `GET` and `DELETE /api/v1/chats/{id}` (`POST .../archive` with `chats: archive`), and `/api/models`.
With `passthroughAuth`, each caller acts as their own Open WebUI user; otherwise every caller acts as the `apiKey` account, whose permissions decide the tools.

#### Open WebUI state

State comes from `GET /api/models?refresh=true`, which any user's key can read.
Without `refresh`, an instance with the base-models cache on serves a stale list (it didn't change through a full llama-swap swap), and `refresh=true` asks every connection again, so `stateCacheSeconds` bounds how often that happens.
Each entry's state depends on where Open WebUI got it:

- **Ollama connection:** `loaded`, set from Ollama's `/api/ps`; `true` is ready (verified through a load and an unload).
- **llama-swap connection** (recognised by `meta.llamaswap`, not by a `status` field, so another provider's `status` is never misread): `status.value` is `loaded` for any process that isn't stopped, so it can't tell starting or stopping from ready (llama-swap source, `handleListModels`; verified on v263). Decided: by default `loaded` reads as ready, accepting those misreads.
- **A facade of an ai-model-facade gateway** (a `current` key): `current.reason` is that gateway's own selection, so it is read as the state and the chain behind it is never followed. No `current` means that gateway couldn't read its backend: unknown.
- **Workspace model:** its base model's state.
- **Anything else:** unknown.

The optional `llamaSwap: {backend, prefixId}` block removes the llama-swap misreads by taking those models' state from a `llamaSwap` backend of the same gateway.
`prefixId` is needed because Open WebUI applies a connection's prefix to the model ID in place, so the original ID can't be read back from the entry.
A wrong prefix or the wrong llama-swap never fixes itself, so a check warns (and `check` fails) when a llama-swap entry, prefix removed, isn't listed by the named backend.
If that backend can't be reached, the `status` in Open WebUI's list is used, as without the block; going down and coming back are each logged once.

The llama-swap state is applied when the state is read, not when Open WebUI's list is fetched: the fetch stores Open WebUI's own fields, and every read applies the `llamaSwap` backend's state of that moment (`StateCache.LoadedModelsAdjusted`).
Decided against: storing the combined state at fetch time, which froze the llama-swap part for the Open WebUI cache's lifetime and hid loads started through the `llamaSwap` backend from facades on Open WebUI.
The llama-swap read may wait even under `WithoutStateWait`, because llama-swap answers `/running` and `/v1/models` from its own state and can't loop back into the gateway (v263 source, `handleRunning` and `handleListModels`).

A request through Open WebUI to a llama-swap model counts as pending in the `llamaSwap` backend's cache, under the llama-swap base model, so facades on both backends see the load, and its release drops only that cache: a `/running` call rather than a refresh of Open WebUI's list.

Decided: llama-swap presets fold into their base model.
With llama-swap's `includeAliasesInList`, Open WebUI lists each preset as its own entry, with `meta.llamaswap.modelID` naming its base.
The adapter folds each preset into the listed entry of that base, as it folds workspace models, so a pattern prefers the base as it does on a `llamaSwap` backend.
Each preset finds its base, rather than the base listing its presets, because the base's `aliases` are gone when the base is hidden in Open WebUI, and the copy under `info.meta` can go stale (verified: it held a preset llama-swap no longer had).
A preset whose base Open WebUI doesn't list stays a model of its own, with the base's state, marked `Preset` so it gets the same params note as an alias.
Decided against: grouping the presets of a hidden base under that base, which a pattern would then prefer and forward to an ID Open WebUI refuses.

Decided against for state: `GET /ollama/api/ps`, which covers Ollama connections only, needs an admin key, and merges connections in a way that only partly matches Open WebUI's model IDs; `/api/models` gives the same Ollama state without those limits.

## Routing loops

A facade can have a candidate behind Open WebUI that routes back to the same gateway, so a request can go round in a circle, and startup can't see it, because it can't tell which gateway is behind an Open WebUI model.
A gateway twice in one chain is legitimate, though: one facade can be a candidate of another through Open WebUI.

Decided: a list of hops in the body.
Each gateway process picks a random ID at startup, and a hop is `<ID>/<facade>`.
The front end refuses (508) a request whose `ai_model_facade_route` field already holds its own hop for the requested facade, removes the field, and passes the route with its hop appended as `backend.Client.Route`.
Only the `openWebUi` adapter writes it back into the body, because a strict OpenAI-compatible server may reject unknown fields.
Open WebUI's OpenAI-connection path forwards unknown body fields (source), so the field reaches the next gateway.
A loop is caught on its first repeat, with no hop limit to choose, and a chain of different facades passes.

## Errors

Before the response starts, an error is an OpenAI-style error body with its status: 503 when no candidate is eligible, 502 when the backend can't be reached or no backend's state can be read.
After a stream's 200 is sent, the status can't change, so a failure ends the stream with a final `data: {"error": ...}` event and no `[DONE]`, and clients show an error instead of a silently cut reply.
A backend `event: error` whose payload is already an OpenAI-style error is relayed as is.

A backend's complete response is passed on with its status, content type and body, plus one header, `Retry-After`, which tells a client refused with 429 or 503 when to try again.
Every other backend header is dropped on purpose: it describes the backend or the gateway's connection to it, which the client doesn't talk to.
A new header is passed on only by adding it to `backend.ChatResponse`.

A few rules guard the byte-level design:

- **Duplicate body keys are rejected (400).** gjson and sjson act on a key's first occurrence while backends keep the last, so a duplicate could carry a value past the gateway's params and the `model` rewrite.
- **Everything buffered is capped:** request bodies at 100 MiB (413 beyond), backend response bodies at 100 MiB, SSE lines at 1 MiB and events at 8 MiB. A client body must arrive within 2 minutes.
- **SSE is parsed per spec.** An event left unterminated at the end of a stream is discarded, and a stream that ends without `[DONE]` is an error that includes the last line, since llama-server may write a bare JSON error before closing.

## Implementation choices

Decided: Go.
Gateway latency is negligible next to inference, so the deciding factors were footprint and deployment: a static binary in a small distroless image, against a Python stack several times the size and memory.
Go's usual weakness, reshaping untyped JSON, doesn't apply, because bodies are never decoded.

Dependencies are the standard library (`net/http` for server and client, SSE through `http.Flusher`, `log/slog`, `path.Match` for patterns), `gopkg.in/yaml.v3`, and `tidwall/gjson` and `tidwall/sjson`.
There is no web framework.

Decided against: doing this inside a reverse proxy such as nginx.
Stock nginx can't rewrite a body by its JSON content; njs or OpenResty could, but would put growing policy logic into a general-purpose front door, share its failure domain, and be harder to test.

## Open decisions

- **Not decided: finishing Open WebUI cleanup on shutdown.** The adapter stops abandoned tasks and deletes chats in the background after the response ends. `serve` waits up to 10 seconds for handlers and then exits, which can cut those calls off, leaving the task running to its end and the chat in Open WebUI. Waiting for them needs a lifecycle step that `Backend` doesn't have.
