# Architecture

**A map of the code: what each package owns, how a request moves through them, and where a change goes.**

This file is for people about to read or change the code.
The [README](README.md) describes what the gateway does.
[docs/agents-design.md](docs/agents-design.md) is the detailed account, written for coding agents: the reasoning behind each rule, the mechanisms that keep it, what was verified against which backend version, and the approaches already rejected.
Read it too before changing a part whose reasons this file doesn't give.

## Bird's-eye view

The gateway is an HTTP server for OpenAI and Claude clients whose model names are facades: entries in its config, each with an ordered list of candidate models on one or more backends.
A facade decides which candidate answers; everything after that choice is the same for every facade, and runs in one pipeline:

```
client -> front end -> facade -> selector -> pipeline -> backend adapter -> backend
          gateway    virtualmodel selector  pipeline    backend/...
                                            translate, mergepatch, history
```

The front end parses nothing but what it must, the facade decides the policy, and the selector picks one model from the backends' current state.
The pipeline then finds the route to that model's backend, translates the request when the backend serves another protocol, patches in the params as byte edits, calls the adapter, and translates the reply back.
The adapter forwards the request and hands back a response or a stream.

Two rules shape every package.
Request bodies stay raw bytes from end to end on a request the backend serves as it came: values are read with gjson and edited with sjson, never decoded into Go types, so whatever the gateway doesn't patch reaches the backend unchanged; only a translated request is rebuilt.
And a facade never causes a model load its candidate list didn't ask for, which is why selection reads backend state before it chooses.

## Code map

### `cmd/ai-model-facade`

The commands: `serve`, `check`, `health` and `version`.
`run` takes the arguments and output writers and returns the exit code, so the commands are tested in-process.
`serve` binds the port, serves a `gateway.Live`, and reloads it on `SIGHUP`.

### `internal/config`

Turns the YAML file into a `Config`: parse to a `yaml.Node` tree, expand `${NAME}` in string values, decode strictly.
`DecodeStrict` rejects unknown keys, which yaml.v3 doesn't do for node trees.
Backend and facade entries stay as nodes, because each type decodes its own schema.

### `internal/protocols`

What the gateway knows about each protocol it serves and forwards, as a `Descriptor`: its endpoint path, its API family, how its stream is framed and ended, and its stream error event.
Code that needs one of these reads the descriptor (`protocols.Of`) instead of switching on the protocol, so a new protocol is a new descriptor.
`protocols.Stream` reads a backend's stream by its protocol's framing, so no adapter special-cases a stream's end.

### `internal/gateway`

Builds a running gateway from a config (`Build`) and serves it.
`Build` creates every backend, links the ones that refer to others, lists every backend's models once, builds every facade, and runs the checks against those lists; problems that depend on a backend being up become `Gateway.Warnings` rather than errors.
`Live` holds the current gateway and swaps in a new one on reload, only if it builds.
`family.go` holds the API families, OpenAI's, Anthropic's and the gateway's own `/amf`: each family's routes come from its protocols' descriptors, less those `api.<family>.protocols` turns off, with its credential header, error shape and model list.
`server.go` is the front end: auth, the routing-loop check, the selection log line, and SSE relay.
It does framing only; anything about choosing or rewriting belongs to the facade and the pipeline.

### `internal/virtualmodel`

Facade types, each with its own schema, validator and `Handler`.
`candidates`, the only type and the default, compiles the facade's and each candidate's params, checks candidate IDs against their backends, and on each request runs the selector and hands the winner to the pipeline as a `pipeline.Selected`.
`Handler.Current` predicts the selection without making a request, for `current` in the model lists; `Handler.Inspect` adds the state of every candidate, for `/amf/models`; `Handler.OutputCap` is the smallest output cap among the models the facade can select, for the Anthropic list's `max_tokens`.
A new behaviour is a new type here, so existing config entries never change meaning.

### `internal/pipeline`

Runs a request after selection, the same way for every facade type: the route to the backend (`translate.Route`), the request protocol's hook, translation, params in the protocol the backend receives, the model rewritten to the candidate's ID, the backend call, and the reply translated back and shown to the hook.
A hook is a protocol's own work around a request; Responses has one, the conversation store.
`pipeline.Error` is the one error type a request fails with, given the client's family's shape by the front end.

### `internal/translate`

Converts a request and its reply between protocols, for a request whose protocol the selected backend doesn't accept.
`Route` picks the identity route when the backend accepts the protocol, otherwise a direct translator for the pair.
The code is in two layers.
Each protocol has a module, `chat.go`, `messages.go` and `responses.go`, which reads that protocol's requests into small values and writes its replies through one writer used streamed and not (`chatWriter`, `messagesWriter`, `responseBuilder`), so every translator answering a client of that protocol produces the same shapes.
Each pair of protocols has a file, `chat_messages.go`, `chat_responses.go` and `messages_responses.go`, holding the two translators between them, built on the two modules.
`effort.go` and `estimate.go` hold the effort levels and the token estimate the pairs share.
It runs before params, which are written in the protocol the adapter receives.

### `internal/history`

Keeps Responses conversations, so a request that names an earlier response in `previous_response_id` can be served by any candidate.
It is the pipeline's hook for Responses: after selection, `Store.Prepare` expands the conversation into the request's input, or passes the ID on to a service that keeps the conversation itself, and `Turn.Record` keeps the new turn from the reply, in the client's protocol.
The store belongs to the process, not to a gateway: `Live.Reload` builds the new gateway around the old one's store.

### `internal/selector`

`FirstLoaded` picks the first ready candidate, else the first starting one, else the first `loadIfNone` one, sending a backend that is loading a model nothing it would refuse or that would abort the load.
It reads each backend's state concurrently and waits for a backend only when the walk reaches one of its candidates.
A selector declares the backend capabilities it `Requires`, and config loading checks them.

### `internal/mergepatch`

Compiles `params` from YAML nodes into JSON Merge Patches (`Compile`) and applies them to raw bodies (`Patch.Apply`).
It walks the patch, not the body, so keys the config doesn't mention are never touched, and added keys follow config order.

### `internal/backend`

The contracts between the gateway and its adapters.
`Backend` (`ListModels`, `Chat`, `Protocols`) is what every adapter implements; `ModelStateProvider` (`LoadedModels`) is an optional capability, found through `backend.Has`.
`ChatResult` carries either a complete `ChatResponse` or a `ChatStream`, and `SSEStream` parses a backend's event stream; `EndingStream` ends one at its last event, for Messages and Responses, which send no `[DONE]`.
Further optional capabilities are found by type assertion: `TokenCounter`, a backend's own Messages token count; `Tokenizer`, counting raw text with a model's tokenizer; and `ResponsesKeeper`, a backend that keeps Responses conversations and can read one back.
The markers the translators put on what the gateway made, such as unsigned thinking, are defined here, so the `anthropic` adapter can remove them before the Claude API sees them.
Context helpers control how state reads are shared within one request or one model listing, so a backend's state is read once even when a request loops back through Open WebUI.

### `internal/backend/openaihttp`

What adapters share: `Client` (HTTP with timeouts, auth and passthrough, chat forwarding) and `StateCache`.
`StateCache` is where state reads are cached, shared between concurrent requests, and adjusted for loads the gateway started itself (`Pending`).
Rules about backend state are implemented here once, not in each adapter.

### `internal/backend/llamaswap`, `llamaserver`, `lmstudio`, `localai`, `ollama`, `openai`, `anthropic`, `openwebui`

One adapter per kind of backend.
`llamaswap`, `llamaserver`, `lmstudio`, `localai`, `ollama` and `openai` read their backend's state and forward requests unchanged; `llamaswap` and `llamaserver` also forward a token count, and `localai` tokenizes text for one.
`anthropic` forwards Messages to the Claude API, fills a missing `max_tokens` from the model's output cap, and removes what the gateway made that only Claude may write.
`openwebui` runs Open WebUI's own chat flow and builds the OpenAI response itself; it can also take the state of the models another backend of the gateway serves through Open WebUI from that backend (`Link`).
A new kind of backend is a new package here, built on `openaihttp`, plus one case in `gateway.newBackend`.

### `internal/testkit`

Fake llama-swap, llama-server, LM Studio, LocalAI, Ollama, Open WebUI, OpenAI and Claude API servers that record what they receive and let a test hold a load or a poll, and `LiveBackend`, which drives a real backend in dev-env through its `control` service for the live checks.
[CONTRIBUTING.md](CONTRIBUTING.md) says how tests use them.

## Where a change goes

- A new config option for a backend: its adapter's `Config`, or `openaihttp.Config` if every adapter takes it.
- A new rule about which model wins: the selector, or a new facade type if it isn't load-aware selection.
- A new rule about how backend state is read or reused: `openaihttp.StateCache`.
- A new endpoint or response header: `internal/gateway/server.go`, or `family.go` for a family's own endpoints.
- A new protocol: a descriptor in `internal/protocols`, a module in `internal/translate`, and a translator for each pair it joins.
- A field that crosses a translation differently: the pair's file, or the protocol's module if every pair from or to it shares the change.
- A new step every request takes after selection: `internal/pipeline`.
- A new kind of backend: a package under `internal/backend`, and a fake in `internal/testkit` for its tests.
