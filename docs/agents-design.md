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

A request passes through these stages, each a separate package:

```
client -> front end -> facade -> selector -> pipeline -> backend adapter -> backend
          (gateway)  (virtualmodel) (selector) (pipeline)  (backend/...)
                                              translate, mergepatch
```

The front end (`internal/gateway`) serves the OpenAI API at the paths in the protocols' descriptors (`internal/protocols`), and does HTTP and SSE framing only.
A facade (`internal/virtualmodel`) owns its policy: which selector, which params, which backends.
The selector (`internal/selector`) turns backend state into one chosen model.
The pipeline (`internal/pipeline`) runs everything after the choice, the same for every facade type: the route to the backend, translation (`internal/translate`), params as byte edits (`internal/mergepatch`), the model rewritten, the backend call, and the reply translated back.
An adapter (`internal/backend/...`) forwards the request and returns either a complete response or a stream of SSE payloads; the front end relays both.

Decided: the pipeline is a package of its own, not part of the facade type, so a new facade type decides only which candidate answers and inherits translation and params unchanged.

### Interfaces

Three interfaces separate the stages, and each one is where a kind of extension goes.

- `backend.Backend` is the adapter: `ListModels`, `Chat` and `Protocols`, the formats its `Chat` accepts. Selection runs over the candidates that can serve the request's protocol, directly or through a translator, so a ready model that can't never wins over one that can. A new kind of backend is a new package implementing it, usually on the shared `openaihttp` client and state cache, plus one case in `gateway.newBackend`.
- `backend.ModelStateProvider` is an optional capability: `LoadedModels` reports ready, starting and unknown models. It is optional because a backend that can't report state can still serve a facade whose selector doesn't need it.
- `selector.Selector` declares the capabilities it `Requires`, and the gateway checks them against each candidate's backend when the config loads, so a facade that can't work is a startup error, not a request-time one.

Further capabilities follow the same pattern: an interface a backend may implement, found by type assertion (`backend.Has`), and required by the selectors or facade types that need it.
A backend that refers to other backends, as the Open WebUI adapter's `links` do, implements `Link`, which the gateway calls once every backend exists.

### Facade types

A facade entry may set `type`, and each type has its own schema, validator and handler (`virtualmodel.Build`).
The only type is `candidates`, the default.
A new behaviour is a new type, so existing entries never change meaning.

## Request bodies

Request and response bodies are never decoded into Go structs or maps.
Decoding would reorder keys, could change number precision, and would drop or mangle fields the gateway doesn't model, such as tools, `response_format` and vendor extensions.
The gateway reads the few values it needs (`model`, `stream`) with gjson, edits with sjson, and forwards the bytes, so the forwarded body equals the client's except for the keys the config patches.
There are two exceptions: the Open WebUI adapter synthesizes its response from a chat it ran, and a translator (`internal/translate`) rebuilds a request whose protocol the backend doesn't accept, and its reply.
Both still carry over, unchanged, the request keys they have no translation for.

### Params as merge patches

Every `params` value is a JSON Merge Patch (RFC 7386): objects merge key by key, scalars and arrays replace, `null` deletes.
Merging is the point of the project: a patch that sets `chat_template_kwargs.enable_thinking` keeps the client's other kwargs, which is what llama-swap's top-level replacement can't do.
`params` may also be a list of patches applied in order, because YAML's `<<:` merge is shallow and would replace nested objects when combining anchors.

The order is fixed: client body, then any translation to the protocol the backend receives, then the facade's params, then the winning candidate's params, each `all` before its protocol's block, then `model` rewritten to the chosen ID.
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

A backend that serves nothing until a load ends is skipped whole while it loads: a request sent then would fail, so none of its candidates is waited on or picked as the fallback.
An adapter says so by wrapping `backend.ErrLoading` in its failed state read, as single-model llama-server's does for a 503, LM Studio's for a 500, and LocalAI's, under a limit on loaded models, for a load of the gateway's under way.
The log calls such a backend's candidates busy, and since its state was read, a request that finds nothing else gets `503` as with no candidate loaded, not the `502` of a backend whose state can't be read.
LM Studio needs more (`backend.LoadsOneAtATime`): loading one model aborts a load of another under way and fails the request that started it, and it lists the model it is loading as loaded from the moment the load begins.
So while one of its models has a request of the gateway's pending (`ModelInfo.Pending`), only candidates for that model are used there, ready, starting or as the fallback, since a request for it waits for the load; a candidate for another model is skipped, even one a state read from before the load still lists as loaded.
On every other backend, and on LM Studio for the model it is loading, a second request is served once the load ends, so a candidate starting there is still waited on.
Limit: the gateway knows of a load on LM Studio only through its own pending requests, so a load another client starts there reads as its model loaded, and a request for another model the gateway sends then aborts it; nothing stops another client aborting a load the gateway started either.

Aliases (llama-swap presets, Open WebUI workspace models, llama-server aliases) are candidates with their base model's state, since loading an alias loads its base.
A pattern matches base models before aliases, each in sorted order, so a pattern matching both resolves to one well-defined model; it says nothing about which is better.
An alias is forwarded under its own ID so the backend applies its settings, and those settings are applied after the gateway's params, which is why an alias candidate with params gets a startup note.
A llama-server alias is only another name for its model, with no settings of its own, so the adapter marks the model `PlainAliases` and its aliases get no note (llama-server).

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
`GET /amf/models` reports each facade's `current`, the selection a request would get now, and every candidate's state, through `Handler.Inspect`, which shares its classification with selection, so what it reports is what a request would act on.
`GET /amf/models/{id}` is one entry of that list, so a client needs one shape for both.
Decided against: a param to choose whether candidates are returned, since they come from the state read `current` already needs, and the list is for seeing why a facade picks what it does.
`/amf` belongs to the gateway rather than to a client API, so it needs no client's format and can always read state.

`GET /v1/models` adds `current` as a non-standard field only with `api.openAi.addCurrent` on, because a gateway behind Open WebUI reports its facades' state to another gateway that way (Open WebUI).
Decided: off by default, so a listing for an ordinary client answers from the config and contacts no backend.

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

`/amf/models` and `/v1/models` select every facade against one snapshot (`backend.WithStateSnapshot`), so one listing reads each backend once and never judges two facades against different states.

### Loads the gateway started

A backend's own report can miss a load the gateway began: Ollama lists a model only once it is loaded, and a cached state can predate the load.
So the cache counts each request sent and not yet released (`StateCache.Pending`), under the base model for an alias, and every read reports a counted model the backend doesn't list, or can't report, as starting.
What the backend itself reports as ready or starting is kept.
The count is taken before and after the backend read, so a load that ends or begins during the read still shows.

Counting is wired by the adapter's base: `openaihttp.NewBackend` builds the client and the state cache together, with the client's chat requests counted, and `llamaswap`, `llamaserver`, `lmstudio`, `localai`, `ollama`, `openai` and `anthropic` are built on it.
An adapter built on `openaihttp.NewClient` and `NewStateCache` instead still works, but its loads never show as starting, so concurrent requests fall through to a `loadIfNone` fallback and cause the swap the count exists to avoid.
A request is released when its response headers arrive, since llama-swap, llama-server's router, LM Studio, LocalAI and Ollama all send headers only once the model is loaded (verified on Ollama 0.40.8, where headers arrive with the first token, on llama-server b11580, on LM Studio's llmster 0.0.25-1 and on LocalAI v4.11.0).
The Open WebUI adapter releases at the first output its polling sees, because Open WebUI answers the start of a chat at once.
On release, if the cached state didn't list the model as ready, a load just finished or failed and may have evicted other models, so the cache is dropped (`StateCache.Invalidate`); a refresh already in flight is then neither cached nor joined, since it may predate the change.

Decided against: deciding when a request is sent whether to count it (only if its model isn't loaded).
That needs a current state at send time and misses a model evicted between the send and the backend receiving it.

Limit: a load started by a client that bypasses the gateway is visible only once the backend reports it.
A model also stops counting when its request ends, however it ends; if the client leaves mid-load, Ollama likely finishes the load, but the model reads as not loaded until `/api/ps` lists it, and a request in that window may pick a `loadIfNone` fallback.

### Not waiting on itself

With `api.openAi.addCurrent` on, when the gateway is an Open WebUI connection and also has an Open WebUI backend, building `current` for `/v1/models` refreshes Open WebUI's model list, which makes Open WebUI call this gateway's `/v1/models` again.
That nested call would join the fetch already in flight and wait on itself until the state timeout.
So `/v1/models` reads state under `backend.WithoutStateWait`: if a fetch is in flight, it answers from the last cached state, or omits `current` if there is none.

The gateway can't tell whether a server's answer depends on this gateway, so `loopsBack: true` on a backend says it does.
With it, `/v1/models` doesn't wait for that backend's fetch; without it, the default, a read waits for the fetch in flight and reports its state.
A wrong `true` costs a stale or missing `current`.
A wrong `false` makes the fetch wait on itself until `stateTimeoutSeconds` and fail, so the listing stalls, Open WebUI's own model list stalls with it, and a chat request needing that state fails with 502 (verified through a real loop on Open WebUI v0.12.3, 10 s at the default timeout).
Decided: off by default anyway, because a loop needs a deliberate setup, `api.openAi.addCurrent` on and this gateway as a connection of a server it reads, and most deployments would otherwise pay with a stale `current` for a case they don't have.
llama-swap is an example of why this can't be decided per adapter: it answers `/running` and `/v1/models` from memory (read from source at `1249e7c`, not run), but a future version could call out, and it can reach a gateway through a model's `proxy`, a peer, or the capability detection it runs when a model becomes ready.
Decided against: fixing the choice per adapter, which turns a fact about one deployment's servers into a fact about the adapter's code.
Any such loop, through however many gateways and Open WebUI instances, comes back to a gateway whose fetch is in flight, so it ends after one round.
Chat selection still waits as usual, and so does `/amf/models`: Open WebUI's refresh calls `/v1/models`, never `/amf/models`, so that refresh can't be waiting on it.
The cost is that the copy of this gateway's list Open WebUI caches during the loop may show a stale or missing `current` for facades on Open WebUI.

## Backend adapters

Each adapter maps one backend's API onto `Backend` and, where it can, `ModelStateProvider`.
`llamaSwap`, `llamaServer`, `lmStudio`, `localAi`, `ollama` and `openAi` forward requests unchanged through `openaihttp.Client`; `anthropic` forwards Messages to the Claude API; `openWebUi` runs Open WebUI's chat flow and builds the response itself.
Every adapter takes the same connection keys (`baseUrl`, `apiKey`, `passthroughAuth`, the timing keys), because they all come from `openaihttp.Config`.

### llama-swap

State comes from `GET /running`, where `ready` is ready and `starting` is starting; aliases come from `meta.llamaswap` in `/v1/models`, which llama-swap provides from v241 on (commit `8b61e3d`).

How llama-swap schedules loads (v263 source, `internal/router/scheduler/fifo.go`, not run) decides what a wrong choice costs.
A load already under way for the same model is joined, a ready model with nothing to evict is served at once, and a swap that would collide with one under way, or evict a model with requests in flight, waits in a queue.
So llama-swap never stops a model mid-load or mid-request, and a wrong choice by the gateway costs an extra swap and the wait for it, not a lost load; the pending count exists to avoid that swap.
A queued swap waits for the model to go idle, so a steady stream of requests to a ready model can hold a swap off.
Each model admits at most `concurrencyLimit` requests (default 10) and refuses more with a 429 and `Retry-After`, which the gateway passes on.

### llama-server

llama.cpp's own server runs either as a router, which loads and unloads models on demand, or with one model for as long as it runs.
Everything here was verified on b11580, using the environment in `dev-env/`, unless marked as source.

The adapter takes the mode from `GET /props` on every state read: a router's has `role: router`, a single-model server's has no `role` (source, `server-models.cpp`).
In single-model mode `/props` is the state anyway, and in router mode it costs one small request, so a server restarted in the other mode behind the same address is read correctly without a reload.
Decided against: a `mode` config key, which could disagree with the server.

In router mode, state is each model's `status.value` in `/v1/models`, which then lists the same entries as `/models`.
`loaded` is ready and `loading` is starting.
`unloaded`, `sleeping`, `downloading` and `incomplete` don't count, because a request to any of them loads the model; a failed load reads as `unloaded`, with `failed: true`.
A sleeping model is one that `--sleep-idle-seconds` unloaded from memory, and the next request takes a full load (the router's memory fell to 68 MiB, and the request waited for the load).

How the router schedules loads (source, `server_lru_sched`; the order also seen with three IDs and `--models-max 2`) decides what a wrong choice costs.
With `--models-max` models loaded, a new load first unloads the least recently used idle model: never one with a request in flight, one still loading, or one that a queued request waits for.
If every loaded model is busy, the request waits in a queue until one finishes.
So, as with llama-swap, a wrong choice by the gateway costs an extra load and the wait for it, not a lost request.

Limit: the router reports no stopping state.
It tracks the models it is stopping (`stopping_models`), but `/models` lists them as `loaded` until they are gone: 0.1 to 0.2 s for an idle model after `POST /models/unload`, and at most the preset's `stop-timeout` (default 10 s) before the process is killed.
In that window a facade can pick the model as ready, and its request then loads it again.
This applies to every unload, an eviction the gateway's own request caused included, since the pending count covers only the model being loaded.

In single-model mode the one model is ready, unless `/props` reports `is_sleeping`.
While the model loads, the server answers every endpoint with 503 "Loading model", a chat included (source, `middleware_server_state` in `server-http.cpp`; seen on `/health` and `/props`), so a request sent then fails rather than waits.
So the adapter reports the 503 as `backend.ErrLoading`, and selection skips the server while it loads (Selection), rather than send a request into the 503 as a starting model or the fallback.
The adapter reads any 503 from `/props` this way, so a proxy in front of a llama-server that is down, answering 503, reads as busy rather than as state that can't be read, and a request that finds nothing else gets `503`.
The server ignores the request's `model` (any name was answered), so only the startup check catches a candidate with the wrong ID; selection treats that candidate as unlisted, never loaded.

Decided: llama-server aliases are plain names.
A preset's `alias` key gives its model more IDs, which the router resolves to the same instance with the same arguments (source, alias lookup in `server_models`), so an alias has no params of its own.
The adapter marks each model `PlainAliases`, and `Catalog.OwnParams`, and through it the startup note and `check`, treat its aliases as the model itself.
In single-model mode the list repeats the model's own ID among its aliases, and the adapter drops it.

### LM Studio

LM Studio's server loads models on demand, and by default holds one model loaded by a request at a time: loading another unloads it (`developer.unloadPreviousJITModelOnLoad`).
Everything here was verified on its headless daemon, llmster 0.0.25-1, using the environment in `dev-env/`, unless marked as docs.

State comes from LM Studio's own model list, `GET /api/v1/models`, which lists each model by key with its `loaded_instances`; a model with one is ready.
The OpenAI-style `/v1/models` lists the models without their state.
The list has LM Studio's embedding models too, which the adapter leaves out, since the gateway relays only completions.
LM Studio reports no loading state, so a load shows as starting only through the pending count, as with Ollama, and headers arrive only once the model has loaded.

Limit: LM Studio lists the model it is loading as loaded from the moment the load begins, for the whole load (0.85 s for the dev model from the page cache, 6.4 s cold from a network share), so the model reads as ready rather than starting.
A request for it picked then waits in LM Studio for the load, so it causes no second load; the cost is only that the log says `loaded` where `starting` would be exact.
A request for another model during a load aborts it, failing the request that started it with "Engine protocol startup was aborted", so selection uses the gateway's pending count to rule that out (Selection); a load another client started isn't counted, and stays exposed.
For about 25 s after its container starts, LM Studio unpacks its inference engines, and meanwhile its list answers 500 ("Model does not exist"), a load asked for waits until it is done, and every other request answers 500 (each verified on llmster 0.0.25-1, with one load and one concurrent request).
So the adapter reports a list read that fails with 500 as `backend.ErrLoading`, and selection skips LM Studio meanwhile (Selection); dev-env's `control` waits for the unpacking before it reports LM Studio started.
Only a 500 means that, because that is how the list fails then; any other failure, such as a 401 for a wrong `apiKey` or a 404 for a wrong `baseUrl`, is reported as the error it is.
Decided against: retrying the read, which would hide a backend that keeps failing behind a delay on every request.
Decided against: waiting out the unpacking, which LM Studio answers with a 500, and a queue in the gateway holding requests until it ends, which this project doesn't add.

LM Studio answers a request for a model it doesn't have with whichever model is loaded, and refuses it (400, "No models loaded") only when none is.
So only the startup check catches a candidate with the wrong ID, as with single-model llama-server; selection treats that candidate as unlisted, never loaded.

A model loaded through LM Studio's own load endpoint (`POST /api/v1/models/load`) counts as loaded by hand and is exempt from the auto-unload (docs, Idle TTL and Auto-Evict; community reports), which the gateway never does: it loads only by sending the request.

### LocalAI

LocalAI loads models on demand, each in a backend process of its own, and keeps them until they are unloaded, unless `LOCALAI_MAX_ACTIVE_BACKENDS` limits how many stay loaded.
Everything here was verified on v4.11.0 with its llama.cpp engine, using the environment in `dev-env/` with limits of one and two, unless marked as source.

Models come from `GET /v1/models`, and state from `GET /system`, whose `loaded_models` lists a model only once its load has finished (source, `ModelLoader.loadModel` adds it to the store after the load).
LocalAI reports no loading state, so a load shows as starting only through the pending count, as with Ollama; response headers wait for the load, since a chat writes nothing before its model has loaded (source, `ChatEndpoint` and `processStream`).
The model list holds every config, embedding and image models included, since `/v1/models` doesn't say what a model does.

Decided: LocalAI aliases are plain names.
An alias config redirects every request to its target and ignores every other field (source, `ModelConfig.Alias`), and `/system` lists a model loaded through an alias under the target's ID.
The adapter folds each alias from `GET /api/aliases` into its target and marks the model `PlainAliases`, as for llama-server.
`/system`, `/api/aliases` and `/api/settings` sit behind LocalAI's admin check, so with its API keys set the gateway's key must be an admin's.

Under a limit, a request for a model that isn't loaded first makes room, and only then joins a load under way (source, `ModelLoader.Load` calls `enforceLRULimit` before `loadModel`).
Making room counts a loading model twice, as tracked and as pending, and evicts a model with no last use first, which a loading model is (source, `WatchDog.EnforceLRULimit`); the eviction waits for the load to end, then stops the model.
So any such request during a load, for another model or the loading one, ends that load, even under a limit of two: the request that started it fails with 500 once its model is stopped, and the later request then loads its own (verified for both).
A request for a model that is loaded is served without making room.
So while `/api/settings` reports a limit and a model of the gateway's has a request pending and isn't listed, the adapter reports `backend.ErrLoading`, and selection sends LocalAI nothing until the load ends (Selection).
Under a limit of one that rules out nothing that could have worked; under a higher limit it also holds back requests for models still loaded, which would have been served.
Without the web UI there is no `/api/settings` (source, `RegisterUIAPIRoutes`), and the adapter assumes a limit, since that only holds requests back, where assuming none fails them.
Decided against: `backend.LoadsOneAtATime`, LM Studio's rule, which still lets a request through for the model that is loading; on LocalAI that request ends the load too.
Decided against: reporting the backend busy whether or not a limit is set, which would hold back every request during any load on LocalAI's default, unlimited setup.
Limit: a load another client starts is invisible until it ends, so a request the gateway sends meanwhile can end it, and the other way round.

### Ollama

State comes from `GET /api/ps`, which lists a model only once it has loaded and has no load state of its own (verified on 0.40.8; a model unloaded to make room disappears as soon as the new load starts).
Every listed model is ready, and loads under way are visible only through the pending count.

Ollama runs one runner per stored blob, not per model name (0.40.8 source, `schedulerModelKey` in `server/sched.go`; verified).
Two names created from the same bytes share a runner, and `/api/ps` lists only the name that loaded it, so the other reads as not loaded although a request to it is answered at once; a facade that lists both names picks them as it would two separate models.
To make room, Ollama evicts an idle model with the shortest keep-alive, and among equals the one whose blob's sha256 sorts first, not the one used least recently (0.40.8 source, `findRunnerToUnload`), so which model a load evicts can't be told from the order of requests.

On `/v1/chat/completions`, Ollama puts thinking in `reasoning`, not llama.cpp's `reasoning_content`, and turns it off only with `"reasoning_effort": "none"`; it ignores `chat_template_kwargs.enable_thinking` and the native API's `think` there (verified on 0.40.8).
So a facade turning thinking off on Ollama needs params of its own, not the ones it gives llama-swap.

Decided: model IDs are the exact `name:tag` that Ollama lists.
A bare name is not mapped to `name:latest`, which would be implicit matching by ID; instead, startup suggests the listed IDs with a tag.

### OpenAI

OpenAI's API serves Chat Completions and legacy Completions at their own paths, and so do other hosted APIs, such as OpenRouter's, which the same adapter serves by `baseUrl`.
Everything here is from OpenAI's API documentation; the testkit's live claim, run with `OPENAI_API_KEY` set, checked the model list, a chat and the refusal of an unknown model against the API.

A hosted model needs no load, so every model `/v1/models` lists is ready while the API answers.
The state is the latest list (`openaihttp.ListedState`), read at startup and on a reload, and read again in two cases: when a request to the API gets no answer or a 5xx, which drops the list and the cached state, and when the list is older than `stateCacheSeconds`, if that is set.
A listing that fails leaves no list, so the backend reads as unavailable and selection passes over its candidates, and every state read lists again until one succeeds.
So a healthy API costs no call per request, an outage costs one listing per request until it ends, and a backend that goes away stops ending the walk through a facade's list.
A client that leaves mid-request isn't the API failing, so it drops nothing; a 429 doesn't either, since it means not now rather than down, and it is passed on with its `Retry-After`.
Decided against: reading `/v1/models` on every state read, which with the default `stateCacheSeconds: 0` is a call to the API on every request, adding its latency to each and counting against its rate limits, to report a list that rarely changes.
Decided against: the latest list standing until a reload, which kept an API that had gone down reading as ready, so every request picked for it failed.
Limit: the request that finds the outage still fails with 502; there is no failover.

A candidate that is ready ends the walk through a facade's list wherever it sits, so a hosted candidate placed after the local ones acts as the fallback without `loadIfNone`.
The list holds embedding, image and audio models too, since `/v1/models` doesn't say what each does.

### Anthropic

The adapter forwards Messages to Anthropic's Claude API; a client of another protocol reaches it through a translator (Translation).
Everything here is from Anthropic's API documentation (read 2026-11-01) unless marked verified, which means checked against the API, by the testkit's live claims, run with `ANTHROPIC_API_KEY` set, or by hand.

Decided: the Messages API, which other protocols reach through a translator, not Anthropic's OpenAI-compatible endpoint.
The compatible endpoint takes the gateway's bodies unchanged, but with Anthropic's limits: responses carry no thinking, prompt caching isn't supported, and many fields are ignored; Anthropic calls it mainly for testing and comparison.
Through Messages a facade gets what that endpoint can't give: thinking in the response, and prompt caching, thinking settings and effort set through its params.
The cost is a second exception to bodies passing through as bytes, beside the Open WebUI adapter's response, on translated routes only.
Decided against: Anthropic's Go SDK. Its typed params would drop or hide the Messages fields that params pass on, and its own HTTP client would bypass `openaihttp`'s failure hook, timeouts and `Retry-After`, and retry by itself the failures the gateway must notice.

Both endpoints need an `anthropic-version` header, which the documentation lists as optional (verified: the API refuses either call without one, with 400 "anthropic-version: header is required"), and both take the key as a bearer token (verified).
The adapter sends `2023-06-01`, through `Client.GetJSONWith` for the model list and `Client.Send` for chats: `2026-10-21` refuses `temperature`, `top_p` and `top_k` on every model, which Chat Completions clients send routinely.
Models come from `GET /v1/models`, a page at a time, up to 1000 a page, following `last_id` while `has_more` is set; by default it lists active and deprecated models, not retired ones.
As for OpenAI, every listed model is ready while the API answers, and the list is read again after a failed request or once older than `stateCacheSeconds` (`openaihttp.ListedState`); `Client.Send` reports a failed chat to it as `Client.Chat` does.
The adapter declares only `anthropicMessages`, so a legacy completion can't reach it, and params for it are always in Messages spelling.
Limit: a key that isn't scoped to one workspace needs `anthropic-workspace-id` on every request, which the gateway doesn't send.

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

The response is synthesized from the finished message (`content`, `output` items, `usage`), an exception to responses passing through unchanged, as a translated one is.
The request side is still edited as bytes, so unknown client fields reach Open WebUI.

The client's `messages` are what the model sees.
Open WebUI replaces them with the stored history only when the body names a user message (`user_message`, or `assistant_message_id` for a continuation), and the flow sends neither, so the stored chat is only a record of the last turn (verified with a three-message conversation).
Built-in tool definitions add about 2,000 prompt tokens, which on a model with a small context silently drops the start of the conversation (verified on an Ollama model capped at 2,050 tokens).

Tools are `params`, not backend keys: `features` and `tool_ids` are request-body fields, so a facade or candidate sets them like any other param, and the account's permissions bound them.
Tools attached to an Open WebUI workspace model are added by the browser, not the server, so `tool_ids` must be sent explicitly.
Open WebUI calls Ollama-connection models through Ollama's native API, so thinking is turned off there with `params: {think: false}`; `reasoning_effort: none` and a top-level `think: false` are ignored (both verified).

Open WebUI has no legacy `/v1/completions`, so the adapter declares only `openAiChat`.
An API key limited by `API_KEYS_ALLOWED_ENDPOINTS` needs `/api/v1/chats/new`, `/api/chat/completions`, `/api/tasks/chat/{id}` and its `/stop`, `GET` and `DELETE /api/v1/chats/{id}` (`POST .../archive` with `chats: archive`), and `/api/models`.
With `passthroughAuth`, each caller acts as their own Open WebUI user; otherwise every caller acts as the `apiKey` account, whose permissions decide the tools.

#### Open WebUI state

State comes from `GET /api/models?refresh=true`, which any user's key can read.
Without `refresh`, an instance with the base-models cache on serves a stale list (it didn't change through a full llama-swap swap), and `refresh=true` asks every connection again, so `stateCacheSeconds` bounds how often that happens.
Each entry's state depends on where Open WebUI got it:

- **Ollama connection:** `loaded`, set from Ollama's `/api/ps`; `true` is ready (verified through a load and an unload).
- **llama-swap connection** (recognised by `meta.llamaswap`, not by a `status` field, so another provider's `status` is never misread): `status.value` is `loaded` for any process that isn't stopped, so it can't tell starting or stopping from ready (llama-swap source, `handleListModels`; verified on v263). Decided: by default `loaded` reads as ready, accepting those misreads.
- **A facade of an ai-model-facade gateway** (a `current` key): `current.reason` is that gateway's own selection, so it is read as the state and the chain behind it is never followed. No `current` means that gateway has `api.openAi.addCurrent` off or couldn't read its backend: unknown.
- **Workspace model:** its base model's state.
- **Anything else:** unknown, LM Studio and llama-server included. LM Studio's `/v1/models` carries no load state (believed; not checked against a running LM Studio). llama-server's router puts a `status` in its `/v1/models`, which Open WebUI likely passes on (not checked), but nothing in the entry says it came from llama-server, so it isn't read; a link gives both exact state.

Links remove the misreads and the unknowns for a server the gateway also has a backend for: `links: [{backend, prefixId}]` takes the state of that server's models from the gateway's backend for it.
The older `llamaSwap: {backend, prefixId}` block is one more link, kept so existing configs work, and still requires a `llamaSwap` backend.
A link may name any backend built on `openaihttp.NewBackend`, since each reads its state from its server and has a state cache to count pending requests in (`Pending`); an `openWebUi` backend can't be linked.

An entry belongs to a link when its ID, prefix removed, is a model or alias the linked backend lists, and Open WebUI's list doesn't say it came from another kind of server: an entry with `meta.llamaswap` matches only a link to a `llamaSwap` backend, an Ollama connection's (`owned_by: ollama`) only a link to an `ollama` backend, and a facade's (`current`) never, since its state is exact already.
`prefixId` is needed because Open WebUI applies a connection's prefix to the model ID in place, so the original ID can't be read back from the entry.
Each Open WebUI fetch lists every linked backend for this, so a model added to a server is linked from the next fetch; a backend that can't be listed is left out of that fetch, and its models keep the state Open WebUI reports.
Decided: matching by ID.
Decided against: identifying the connection by Open WebUI's `urlIdx`, which depends on the order of Open WebUI's connections and is unverified; and requiring a prefix on every linked connection, which forces a setting on Open WebUI.
Limit: two connections that list the same ID with no prefix can't be told apart; the entry takes the first link's state, with a warning.

A wrong prefix or the wrong backend never fixes itself, so a check warns (and `check` fails) when a linked backend lists none of Open WebUI's models, when a llama-swap entry isn't listed by any linked `llamaSwap` backend, and when two linked backends list one entry.
If a linked backend's state can't be read, its models take the state in Open WebUI's list, as without the link; going down and coming back are each logged once.

The linked state is applied when the state is read, not when Open WebUI's list is fetched: the fetch stores Open WebUI's own fields and which link each model has, and every read applies each linked backend's state of that moment (`StateCache.LoadedModelsAdjusted`).
Decided against: storing the combined state at fetch time, which froze the linked part for the Open WebUI cache's lifetime and hid loads started through the linked backend from facades on Open WebUI.
A linked read follows the same rule as any state read (Not waiting on itself); if it may not wait, its models take the state in Open WebUI's list for that read, and it isn't logged as down.

A request through Open WebUI to a linked model counts as pending in the linked backend's cache, under the ID that backend gives the model's base, so facades on both backends see the load, and its release drops only that cache: a call to that backend rather than a refresh of Open WebUI's list.
For LM Studio this is the only way the gateway knows of such a load, and selection on the `lmStudio` backend then sends LM Studio nothing for another model.
To do: the same protection for facades on the `openWebUi` backend; `LoadsOneAtATime` is checked against a candidate's own backend, so a candidate on Open WebUI for another LM Studio model can still abort the load.

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

## Translation

A request whose protocol the selected backend doesn't accept is translated to one it does, and the reply translated back, streamed or not (`internal/translate`).
Translation is the one place request bodies are rebuilt rather than patched, so it keeps two rules: what crosses keeps its meaning, and what can't cross is either dropped or refused with 400 before anything is sent.
A field is dropped when the request still means what the client asked without it, such as a sampling setting the target lacks, and refused when the client would otherwise go without what it asked for and not know, such as `n` above 1.

### Routes and pairs

Decided: a direct translator from Chat Completions to Messages, for the Claude API, held in the pair's file (`chat_messages.go`).
`translate.Route` picks the identity route when the backend accepts the request's protocol, else the direct translator to the first protocol it accepts.
Decided against, for now: an intermediate form that every protocol translates to and from, a hub.
A direct pair carries what both of its protocols have, where a hub carries only what its form has, so the pair stays a valid route beside a hub when one is built.

Each protocol has a module: `chat.go` reads Chat Completions requests into small values (messages and parts, tools, tool choice, format, effort) and writes their replies through one writer used streamed and not, `chatWriter`, and `messages.go` builds a Messages request's blocks.
Decided: a writer per protocol, so every translator answering a client of that protocol writes the same shapes and events, and a fix to a reply's shape is made once.
A translator runs before params, since params are written in the protocol the adapter receives.

### Chat Completions and Messages

A Chat Completions request is translated by `toMessages` (`chat_messages.go`), which keeps the client's top-level keys in their order and translates each in place:

- `messages`: system and developer messages are gathered into `system`, wherever they stand, since not every model takes a system message mid-conversation. A tool message becomes a `tool_result` block of a user turn, and consecutive turns of one role merge, so the results of parallel tool calls share a turn, as the API asks. Images go by URL or base64 data URL, a PDF file part as a document block, and empty text is left out, since the API refuses it.
- `max_completion_tokens`, else `max_tokens`, becomes `max_tokens`. Messages requires one and Chat Completions doesn't, so the adapter, after params, gives a request without it the model's `max_tokens` from the latest model list, its output cap (verified: a non-streamed request at the full cap is accepted).
- `stop` becomes `stop_sequences`; function tools become tools with an `input_schema`; `tool_choice` maps `required` to `any` and a named function to `tool`; `parallel_tool_calls: false` becomes `disable_parallel_tool_use`; `reasoning_effort` becomes `output_config.effort`, and a `json_schema` `response_format` becomes `output_config.format`; `user` or `safety_identifier` becomes `metadata.user_id`.
- Fields that only tune sampling or configure OpenAI's own services, such as `seed`, the penalties, `logit_bias`, `store`, `service_tier` and OpenAI's `metadata`, are dropped, since the API refuses any field it doesn't know (verified: "seed: Extra inputs are not permitted").
- Any other key passes on unchanged, so a facade's params can set Messages fields such as `thinking`, `cache_control` or `top_k`; where a native `output_config` meets a translated one, its own keys win.
- A request that asks for what Messages can't give is refused with 400 before anything is sent: `n` above 1, `logprobs`, audio, a `json_object` `response_format`, OpenAI's web search, a custom tool, or a role or content part Messages has no counterpart for. Refused rather than dropped, because the client would otherwise go without what it asked for and not know.

A response becomes a chat completion through `chatWriter`: text blocks joined as `content`, thinking as `reasoning_content`, as llama-server reports reasoning, and `tool_use` blocks as tool calls, each input as its arguments string; other blocks, such as server tool results, are left out.
`stop_reason` maps to `finish_reason`: `max_tokens` to `length`, `tool_use` to `tool_calls`, `refusal` to `content_filter`, anything else to `stop`.
OpenAI's `prompt_tokens` includes the cache reads and writes that Messages counts apart from `input_tokens`, with the reads again as `cached_tokens`.
A stream becomes chunks the same way, through the same writer, one event at a time: tool calls numbered in the order they arrive, a usage chunk last when the client set `stream_options.include_usage`, from `message_delta`'s usage, which is cumulative and repeats the input counts (verified).
It ends at `message_stop`, since Messages sends no `[DONE]`; an error event ends it with a stream error in OpenAI's shape, and a body that ends before `message_stop` is an unexpected end.
An API error keeps its status and `Retry-After`, with its message and type in OpenAI's error shape.

Limit: Chat Completions has no place for a thinking block's signature, without which the API won't take thinking back, so an assistant turn's `reasoning_content` isn't sent back. The API accepts a tool loop without it (verified, with thinking on), and the model continues without its earlier reasoning.

### Effort

Reasoning effort levels differ between vendors (verified 2026-11-10): Claude takes `low`, `medium`, `high`, `xhigh` and `max`, and OpenAI `none`, `low`, `medium`, `high` and `xhigh`, and `minimal` on some models.
A level crosses as itself where Claude has it, else as the nearest Claude has (`effort.go`): `none` and `minimal` reach Claude as `low`.
A level neither knows passes on as sent, for the backend to accept or refuse.

## Errors

Before the response starts, an error is an OpenAI-style error body with its status: 503 when no candidate is eligible, 502 when the backend can't be reached or no backend's state can be read.
A request fails with one error type, `pipeline.Error`, holding the status, an OpenAI error type and the message.
After a stream's 200 is sent, the status can't change, so a failure ends the stream with the protocol's own error event (its descriptor's `ErrorEvent`): a final `data: {"error": ...}` and no `[DONE]`.
Clients then show an error instead of a silently cut reply.
A backend's error event already in the protocol's shape is relayed as is.

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

- **To do: a hub form.** An intermediate form every protocol translates to and from, so a new protocol needs one translator pair instead of one per existing protocol. The direct pairs stay as routes beside it (Translation).
- **Not decided: finishing Open WebUI cleanup on shutdown.** The adapter stops abandoned tasks and deletes chats in the background after the response ends. `serve` waits up to 10 seconds for handlers and then exits, which can cut those calls off, leaving the task running to its end and the chat in Open WebUI. Waiting for them needs a lifecycle step that `Backend` doesn't have.
