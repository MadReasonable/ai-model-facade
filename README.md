# ai-model-facade

**An OpenAI-compatible gateway that gives clients a stable model name and decides, at request time, which real model answers.**

A client asks for a virtual model defined in the gateway's config, such as `assistant`.
That model is a facade, and the gateway picks a real model from a list you wrote, based on what is currently loaded, applies the parameters you configured, and forwards the request.
Models and even backends can be swapped without any change on the client side.

It is one compiled binary with one configuration file, and runs as a service or as a container from the published image.
It serves OpenAI's Chat Completions API, and translates it for the Claude API, so a client built for it works with any backend.
OpenAI, Claude, llama-swap, llama.cpp's llama-server, LM Studio, LocalAI, Ollama and Open WebUI are all supported.

The simplest config would map a single facade to a single backend model.
Fully configured, one gateway could present multiple facade models, each choosing from a mix of models from multiple backends.
An Open WebUI backend gives a facade Open WebUI's tools, such as web search, memory and tool servers, so a client with no tool support of its own can use them.

*Requires a model server to point it at. Runs on Linux, macOS and Windows, or in Docker.*

> [!NOTE]
> **Early releases (0.x).** Config keys may still change from one 0.x release to the next; [CHANGELOG.md](CHANGELOG.md) lists each change and what a config has to change.

## What it's for

Most clients let you set one model name, and changing it means editing the client.
Everything that should decide which model answers changes more often than client configs do: which models are loaded, which quant you are testing, which machine a backend runs on, whether the request needs tools.
A facade moves that decision out of every client and into one place, where it is made again for each request.

**Swap models without touching clients.** On a server that loads models on demand, such as llama-swap or Ollama, fixed model names can cause the server to waste time juggling models in and out of memory when an already loaded model could easily handle many of the simpler requests. A facade names a list instead, and the first model on it that is already loaded wins. [Open WebUI's background tasks](#open-webui-task-model) run on whatever LLM is up, with thinking off; [a voice assistant](#voice-assistant-on-whichever-preset-is-loaded) always has a fast, non-thinking model, whether the coding set or the general set is loaded. Nothing is evicted unless nothing on the list is loaded, and the config decides what happens then.

**Evaluate models without re-pointing clients.** A facade wired into clients such as Cline, llama.vscode or Home Assistant can list every model under evaluation as its candidates: quants of one model, or entirely different generations and families. Requests are routed to whichever model is loaded, so trying the next model means loading it on the backend, with no change to any client or to the gateway's config. Each response carries an `X-Gateway-Target` header naming the model that answered, so each result can be attributed to the model that produced it.

**Keep keys and addresses out of clients.** Clients get one address and one key, the gateway's. The backends' addresses and keys live in the gateway's config, and a client's own key reaches a backend only where you turn passthrough on. In that respect it is a reverse proxy for LLM endpoints, limited to each API's chat, completion and model-list endpoints.

**Open WebUI's tools for any client.** An Open WebUI backend runs each request through Open WebUI's own chat flow (create a chat, run it with its tools, collect the answer, clean up) and returns one ordinary response or stream. The model then has what it would have in Open WebUI's chat: web search, memories kept between conversations, search over your knowledge bases and documents, notes and past chats, and any tool server or MCP integration you have connected. The client needs no tool support of its own. Combined with the first use, the gateway sits on both sides: a client asks the gateway for `assistant`, the gateway asks Open WebUI, and Open WebUI asks the same gateway for whichever model is loaded.

The gateway doesn't retry, fail over or balance load: each request goes to exactly one model, and an error from it reaches the client.
It relays chat and completions only, so embeddings, images and audio go straight to the backend.

## Installation

The gateway is one executable that reads one YAML config file.
Each release publishes it as a Docker image for amd64 and arm64, and on the [releases page](https://github.com/MadReasonable/ai-model-facade/releases) as an archive for Linux (amd64, arm64), macOS (amd64, arm64) and Windows (amd64).
Each archive holds the binary, `LICENSE`, this README, `CHANGELOG.md` and `config.example.yaml`, a complete config to start from.
Every install below ends with the gateway running `serve` on a config; [Getting started](#getting-started) says what to put in it.

### Docker

The image is a static binary on distroless, about 10 MB, published as `ghcr.io/madreasonable/ai-model-facade`.
It is tagged with each release's version, such as `0.3.0`, with its minor version, such as `0.3`, and with `latest`; `latest` and the minor tag move to each new release, and a version tag stays on its release.

Run it as a service in the same compose project as your backends, so `baseUrl` can use their service names:

```yaml
services:
  ai-model-facade:
    image: ghcr.io/madreasonable/ai-model-facade:0.3
    restart: unless-stopped
    volumes:
      - ./ai-model-facade/config.yaml:/config/config.yaml:ro
    environment:
      GATEWAY_KEY_OPEN_WEBUI: ${GATEWAY_KEY_OPEN_WEBUI}
    # ports:
    #   - "8088:8088"
```

The image runs `serve /config/config.yaml`, as a non-root user, so the mounted config must be readable by every user.
Keep keys out of it: the config can read them from environment variables (`${NAME}`), which compose passes in.
Inside a container, set `server.listen` to `0.0.0.0:8088`, since `127.0.0.1` there accepts connections only from the container itself.
Other containers on the compose network reach the gateway by its service name, so publish the port only for clients outside that network.

The image has a health check: every 30 seconds it runs `ai-model-facade health /config/config.yaml`, which calls `/health` on the address in `server.listen`, so `docker ps` shows the container as `healthy` and compose's `depends_on: condition: service_healthy` works.
If you mount the config somewhere other than `/config/config.yaml`, override the command and the health check to the same path.

To reload the config without a restart, see [Reloading the config](#reloading-the-config):

```bash
docker compose kill -s SIGHUP ai-model-facade
```

To run `check` against the backends, run it on their network:

```bash
docker run --rm --network <backend network> \
  -v "$PWD/config.yaml:/config/config.yaml:ro" \
  ghcr.io/madreasonable/ai-model-facade:0.3 check /config/config.yaml
```

### Linux

Download the archive for your architecture and `checksums.txt`, check the archive against it, and install the binary:

```bash
VERSION=v0.3.0
ARCH=amd64   # or arm64
curl -LO https://github.com/MadReasonable/ai-model-facade/releases/download/$VERSION/ai-model-facade_${VERSION}_linux_${ARCH}.tar.gz
curl -LO https://github.com/MadReasonable/ai-model-facade/releases/download/$VERSION/checksums.txt
sha256sum --ignore-missing -c checksums.txt
tar -xzf ai-model-facade_${VERSION}_linux_${ARCH}.tar.gz
sudo install ai-model-facade_${VERSION}_linux_${ARCH}/ai-model-facade /usr/local/bin/
sudo install -d /etc/ai-model-facade
sudo cp ai-model-facade_${VERSION}_linux_${ARCH}/config.example.yaml /etc/ai-model-facade/config.yaml
```

To run it as a systemd service, save this unit as `/etc/systemd/system/ai-model-facade.service`:

```ini
[Unit]
Description=ai-model-facade LLM gateway
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=/usr/local/bin/ai-model-facade serve /etc/ai-model-facade/config.yaml
ExecReload=/bin/kill -HUP $MAINPID
EnvironmentFile=-/etc/ai-model-facade/env
DynamicUser=yes
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

`DynamicUser=yes` runs the gateway as a user that systemd creates for it, with no write access to the system, since the gateway writes nothing; it logs to the journal.
That user can read the config only if every user can, so keep keys in `/etc/ai-model-facade/env` instead, as `NAME=value` lines, readable by root alone (`chmod 600`): systemd reads that file as root and passes the variables in, and the config refers to them as `${NAME}`.
`ExecReload` makes `systemctl reload ai-model-facade` reload the config without a restart; see [Reloading the config](#reloading-the-config).

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now ai-model-facade
journalctl -u ai-model-facade -f
```

### macOS

Download the archive for your Mac, `arm64` for Apple silicon or `amd64` for Intel, check it, and install the binary:

```bash
VERSION=v0.3.0
ARCH=arm64   # or amd64
curl -LO https://github.com/MadReasonable/ai-model-facade/releases/download/$VERSION/ai-model-facade_${VERSION}_darwin_${ARCH}.tar.gz
curl -LO https://github.com/MadReasonable/ai-model-facade/releases/download/$VERSION/checksums.txt
grep _darwin_${ARCH}.tar.gz checksums.txt | shasum -a 256 -c
tar -xzf ai-model-facade_${VERSION}_darwin_${ARCH}.tar.gz
sudo mkdir -p /usr/local/bin /usr/local/etc/ai-model-facade
sudo cp ai-model-facade_${VERSION}_darwin_${ARCH}/ai-model-facade /usr/local/bin/
sudo cp ai-model-facade_${VERSION}_darwin_${ARCH}/config.example.yaml /usr/local/etc/ai-model-facade/config.yaml
```

The binary isn't signed by an Apple developer ID.
macOS refuses to run an unsigned file that a browser downloaded, because the browser marks it as quarantined; `curl` doesn't, so the commands above need nothing more.
For an archive downloaded with a browser, remove the mark: `xattr -d com.apple.quarantine ai-model-facade`.

To run it as a launchd service, save this as `/Library/LaunchDaemons/com.github.madreasonable.ai-model-facade.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.github.madreasonable.ai-model-facade</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/ai-model-facade</string>
    <string>serve</string>
    <string>/usr/local/etc/ai-model-facade/config.yaml</string>
  </array>
  <key>UserName</key>
  <string>nobody</string>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardErrorPath</key>
  <string>/Library/Logs/ai-model-facade.log</string>
</dict>
</plist>
```

The gateway runs as `nobody`, and logs to `/Library/Logs/ai-model-facade.log`.
A plist in `/Library/LaunchDaemons` is readable by every user, so keep keys out of it: write them into the config, and make the config and the log belong to `nobody` alone.
`KeepAlive` restarts the gateway if it exits.

```bash
sudo touch /Library/Logs/ai-model-facade.log
sudo chown nobody /Library/Logs/ai-model-facade.log /usr/local/etc/ai-model-facade/config.yaml
sudo chmod 600 /usr/local/etc/ai-model-facade/config.yaml
sudo launchctl bootstrap system /Library/LaunchDaemons/com.github.madreasonable.ai-model-facade.plist
```

To reload the config without a restart, see [Reloading the config](#reloading-the-config):

```bash
sudo launchctl kill HUP system/com.github.madreasonable.ai-model-facade
```

### Windows

Unzip `ai-model-facade_v0.3.0_windows_amd64.zip`, copy `config.example.yaml` to `config.yaml`, and run the gateway from a console:

```
ai-model-facade.exe check config.yaml
ai-model-facade.exe serve config.yaml
```

Windows runs a program as a service only if it is written for Windows' service manager, which the gateway isn't, so a service needs a wrapper that runs it and stops it.
Two open-source wrappers, each with its own documentation:

- [Shawl](https://github.com/mtkennerly/shawl) is a single small executable that does only that. Its release binaries aren't code-signed.
- [Servy](https://github.com/aelassas/servy) adds a GUI, health checks and alerts, and its releases are code-signed.

Whichever you use, it needs these settings for the gateway:

- The command, with the full path to the config: `ai-model-facade.exe serve C:\path\to\config.yaml`. A service starts in `C:\Windows\System32`, so a relative path would be looked up there.
- A stop that sends Ctrl+C, which both send. The gateway then stops accepting requests and gives those in flight up to 10 seconds to finish, so the wrapper's stop timeout should be longer than that; Shawl's default is 3 seconds (`--stop-timeout`, in milliseconds).
- A restricted account, such as Network Service, instead of the default Local System, with read access to the config.
- Keys as environment variables in the wrapper's settings, which the config refers to as `${NAME}`.

Windows has no `SIGHUP`, so the config can't be reloaded in place there: restart the service to load a changed config, after running `check` on it.

### From source

Building needs Go 1.25 or newer:

```bash
go install github.com/MadReasonable/ai-model-facade/cmd/ai-model-facade@latest
```

Or, from a clone:

```bash
git clone https://github.com/MadReasonable/ai-model-facade.git
cd ai-model-facade
go build -o ai-model-facade ./cmd/ai-model-facade
docker build -t ai-model-facade .
```

`ai-model-facade version` and the startup log report the version Go reads from the module or the git checkout: the tag on a tagged commit, otherwise a pseudo-version such as `v0.1.3-0.20261025182945-7cbab4d5a404`, with `+dirty` if there are uncommitted changes.
`docker build --build-arg VERSION=...` sets it instead, and a build outside a git checkout without it reports `dev`.

## Getting started

This walkthrough connects the gateway to one llama-swap instance, so you need one running.
Every other backend differs only in its `backends` entry; see [Backends](#backends).

Save this as `config.yaml`, with your llama-swap address and your own model IDs:

```yaml
server:
  listen: 127.0.0.1:8088

logging:
  logSelection: true

backends:
  llamaSwap:
    type: llamaSwap
    baseUrl: http://localhost:8080

models:
  quick:
    backend: llamaSwap
    params: { max_tokens: 1000 }
    candidates:
      - model: qwen3.8-flash-next:iq3_s
      - model: qwen3.8-27b:*
        params: { chat_template_kwargs: { enable_thinking: false } }
      - model: qwen2.5-coder-3b:q8_0
        loadIfNone: true
```

- `server.listen` is where clients connect. `127.0.0.1` accepts connections from this machine only.
- `logging.logSelection` logs one line per request, saying which model answered and why.
- `backends` names each server the gateway forwards to. This one is called `llamaSwap`, and its `type` says how to talk to it.
- `models` defines the facades, here one called `quick`, which is the name a client asks for.
- `candidates` are the real models behind `quick`, in order of preference. The first one that llama-swap has loaded wins; `qwen3.8-27b:*` is a pattern that matches whichever quant of that model is loaded; and `loadIfNone` makes the small coder model the one to load if none of them is. [Choosing a model](#choosing-a-model) has the full rule.
- `params` are patched into each request: the facade's into every request, and a candidate's only when it wins. Here every request gets a 1000-token limit, and the 27B model also gets thinking turned off. [Params](#params) explains how a patch combines with what the client sent.

`check` loads the config, asks llama-swap which models it has, and prints what each candidate would send:

```bash
ai-model-facade check config.yaml
```

```
quick
  backend: llamaSwap
  1. qwen3.8-flash-next:iq3_s
     params: {"max_tokens":1000}
  2. qwen3.8-27b:*
     base models: qwen3.8-27b:q6_k
     aliases: none
     params: {"max_tokens":1000,"chat_template_kwargs":{"enable_thinking":false}}
  3. qwen2.5-coder-3b:q8_0 (loadIfNone)
     params: {"max_tokens":1000}
```

A pattern lists the models it matches now.
A model ID that llama-swap doesn't list is an error here, with the IDs it does list that differ only by tag.
Then start the gateway:

```bash
ai-model-facade serve config.yaml
```

An OpenAI client uses `http://localhost:8088/v1` as its base URL and `quick` as its model:

```bash
curl -i http://localhost:8088/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "quick", "messages": [{"role": "user", "content": "Reply with exactly one word: hello"}]}'
```

```
HTTP/1.1 200 OK
Content-Type: application/json
X-Gateway-Target: qwen3.8-27b:q6_k

{"choices":[{"finish_reason":"stop","index":0,"message":{"role":"assistant","content":"hello"}}], ...}
```

`X-Gateway-Target` names the model that answered.
The log has one line for each request, with every candidate checked, the winner, and why it won:

```
level=INFO msg=selection model=quick checked="[qwen3.8-flash-next:iq3_s: not loaded qwen3.8-27b:*: loaded as qwen3.8-27b:q6_k]" winner=qwen3.8-27b:q6_k reason=loaded status=200 duration=3ms
```

The first candidate wasn't loaded, so the pattern won with the quant llama-swap had loaded, and nothing was loaded for the request.

`config.example.yaml` is a fuller starting point, with two facades for Open WebUI's background tasks and a voice assistant, and a commented-out entry for each other backend.

## How it works

Handling a request has two parts: choosing the model, then rewriting the request for it.
The sections after those cover the client APIs, who may call, and Open WebUI, which works differently from every other backend.

### Facades and candidates

Each facade has an ordered list of candidates, and each candidate is a model on a backend.
A candidate's `model` is one of three things.

- **An exact model ID.** If the backend doesn't list it, the gateway warns at startup and `check` reports an error, either way suggesting IDs that differ only by tag.
- **A pattern** such as `qwen3.8-27b:*`, using `*`, `?` or `[...]`. It matches ready (or starting) models, and the matched ID is forwarded. If several match, the first in sorted order wins. Quote a pattern that starts with `*`, because a leading `*` is YAML alias syntax.
- **An alias**, a variant of a base model that the backend serves under its own ID: a llama-swap preset such as `qwen3.6-35b:Slacker`, a llama-server or LocalAI alias, or, through Open WebUI, a workspace model or a llama-swap preset whose base model Open WebUI also lists. It is ready (or starting) whenever its base model is, and is forwarded under its own ID so the backend applies the variant's settings. A pattern matches base models before aliases, so a pattern that matches both resolves to one well-defined model: `qwen3.8-27b:*` picks the quant, not its `:Dreamer` preset. A pattern can also pick a preset across model families: if each family defines an `:Assistant` preset, `"*:Assistant"` matches whichever one is loaded.

A llama-swap preset can set params of its own, and llama-swap applies them after the gateway's.
Each top-level key the preset sets replaces the gateway's value entirely, even an object such as `chat_template_kwargs`; every other key keeps the gateway's value.
The gateway logs an INFO line at startup for each alias candidate that has params, and `check` notes it.
That includes a llama-swap preset that Open WebUI lists without its base model: with no base to group it under, it is an ordinary model there, but llama-swap still applies its params.
A llama-server or LocalAI alias is only another name for its model, with no params of its own, so it gets no such note.

A candidate with `loadIfNone: true` is used like any other candidate when it is ready or starting, and is also the fallback when nothing else is.
It can sit anywhere in the list: its position sets its priority among ready and starting candidates, while the fallback happens only after the entire list has been checked.

Each candidate may name its own `backend`, so one facade can span several backends; the list is still walked in order as one list.

### Choosing a model

The gateway reads each backend's state, walks the facade's list up to three times, and stops at the first match:

1. **Ready.** The first candidate that is loaded wins.
2. **Starting.** If nothing is ready, the first candidate that is still loading wins, and the request waits for that load to finish. This avoids an extra swap: asked for a second model during a load, llama-swap finishes the first load, serves its requests, and only then unloads it to load the second.
3. **Fallback.** If nothing is ready or starting, the first `loadIfNone` candidate wins, and the backend loads it.

A model that is stopping or stopped never counts.
If nothing matches, the request gets `503`; Open WebUI, for example, then skips the task.
There is no failover: a request is never retried against another model.

Some backends can't take every request while they load a model, so the gateway holds back what would fail or interrupt the load:

- Single-model llama-server answers every request with an error while it starts, and so does LM Studio while it unpacks its engines after starting, so such a backend is skipped until it has finished.
- LM Studio aborts a load to start another, so while it loads a model for one request, the gateway asks it for no other model.
- LocalAI with a limit on loaded models evicts a load under way for a request for any model that isn't loaded, so while it loads a model for one request, the gateway asks it for nothing; see [LocalAI](#localai).

#### Where state comes from

Each backend reports which models are loaded in its own way, and that decides how exact the three steps can be:

| Backend | What it reports | What that means for selection |
|---|---|---|
| llama-swap | `ready`, `starting` and `stopping` for each running model | All three steps apply exactly. |
| llama-server, router mode | `loaded` and `loading` for each model, and nothing for one being unloaded | A model being unloaded reads as loaded until it is gone. |
| llama-server, single-model mode | Its one model, once the server has finished starting | The model is loaded unless it is sleeping. |
| LM Studio | The models it has loaded, including one still loading, and none as loading | A model counts as loaded from the moment its load begins. |
| LocalAI, Ollama | A model only once it has finished loading | A load shows as starting only through the gateway's own requests, below. |
| OpenAI's API, Anthropic's Claude API | Every model they serve | Every listed model counts as loaded while the API answers, since a hosted model needs no load. |
| Open WebUI | What its connections report, which varies by connection | See [Loaded state through Open WebUI](#loaded-state-through-open-webui). A model whose state it can't report is unknown, which counts as not loaded, so only `loadIfNone` can pick it; the log says `state unknown`. |

On top of what a backend reports, a model counts as starting while a request the gateway forwarded to it is waiting for the model: until the response headers arrive, or for Open WebUI, until the reply's first output appears.
This covers a backend that lists a model only once loaded, and a cached state (`stateCacheSeconds`) that predates the load, so a second request waits for that load instead of starting another.
A request for an alias counts for its base model, since loading the alias loads the base.
What the backend reports as ready or starting is kept; only a model it doesn't list, or can't report, is marked starting this way.
A load started by a client that goes straight to the backend isn't visible this way.

#### Candidates on several backends

Each backend's state is read once per request, concurrently.
A loaded candidate is chosen as soon as its own backend and the backends of every candidate before it have answered, so a slow backend whose candidates all come later doesn't delay the request.
Choosing a starting or `loadIfNone` candidate waits for every backend, because a loaded candidate anywhere in the list comes first.
If one backend's state can't be read, its candidates count as not loaded (logged as `state unavailable`) and the others still serve; only when none can be read does the request fail with `502`.
Wherever a candidate's backend isn't the facade's own, the log, `check` and `current` name it, e.g. `qwen3:1.7b on ollama: loaded`.

#### Seeing the choice before a request

`GET /amf/models` runs the three steps for each facade without sending a request, so it never triggers a load.
Its entry for `quick` from [Getting started](#getting-started):

```json
{"id":"quick",
 "current":{"model":"qwen3.8-27b:q6_k","reason":"loaded"},
 "candidates":[
  {"model":"qwen3.8-flash-next:iq3_s","backend":"llamaSwap","state":"not loaded"},
  {"model":"qwen3.8-27b:*","backend":"llamaSwap","state":"loaded","match":"qwen3.8-27b:q6_k","winner":true},
  {"model":"qwen2.5-coder-3b:q8_0","backend":"llamaSwap","loadIfNone":true,"state":"not loaded"}]}
```

`current` is where a request made now would go:

- `reason` is `loaded`, `starting` or `loadIfNone`, matching the step that would pick `model`.
- When no request would succeed, `model` is absent and `reason` is `no candidate loaded`.
- `aliasOf` is present when `model` is an alias, and `backend` when the model is on another backend than the facade's own.
- If no backend's state can be read, `current` is left out.

A candidate's `state` is `loaded`, `starting` or `not loaded`, or one of these:

- `unknown`: its backend doesn't report this model's state, which selects like not loaded.
- `unavailable`: its backend's state couldn't be read.
- `busy`: its backend is loading a model and serves nothing until it has.
- `blocked`: its backend has a request pending for another model, which a request for this one would abort.

`match` is the model a pattern resolved to, `aliasOf` is present when that model is an alias, and `winner` marks the candidate a request made now would go to.
It is a snapshot, so the next request can be routed differently if what is loaded changes.
The client APIs' own model lists can carry `current` too, with [`addCurrent`](#api).

### Params

Every `params` value is applied to the request body as a JSON Merge Patch ([RFC 7386](https://www.rfc-editor.org/rfc/rfc7386)):

- Objects merge key by key, so `chat_template_kwargs: { enable_thinking: false }` keeps any other kwargs the client sent.
- Scalars and arrays replace the client's value.
- `null` deletes a key, e.g. `think: null`.

The steps run in this order, with later ones overriding earlier ones:

```
client body → facade params → winning candidate params → model set to the concrete ID
```

Gateway params override the client: they are patches, not defaults.
The rest of the request passes through byte for byte, including messages, tools, `response_format` and vendor fields, in their original order.
A request for the Claude API is the exception: its adapter translates the patched request to Messages; see [Chat Completions on the Claude API](#chat-completions-on-the-claude-api).

Some params only mean something for chat: `chat_template_kwargs` has no effect on `/v1/completions`, which applies no chat template.

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

### Client APIs

The gateway serves OpenAI's API, which most clients and servers speak: Chat Completions at `/v1/chat/completions` and legacy completions at `/v1/completions`, so `http://<gateway>:8088/v1` is an OpenAI client's base URL.
Every backend but the Claude API serves it, and the Claude API serves Anthropic's Messages API instead.

A request in the API its backend serves is forwarded as it came, apart from params and the model.
A Chat Completions request for the Claude API is translated, and the reply, streamed or not, is translated back.
Legacy completions are the exception: neither the Claude API nor Open WebUI serves them, and there is no translation for them, so a legacy completion that selects a candidate on either fails with `502`.

#### Chat Completions on the Claude API

A Chat Completions request that selects a candidate on an `anthropic` backend is translated to Messages, keeping what a Chat Completions client relies on:

- System and developer messages become the Messages system prompt, wherever they stand in the conversation.
- Tools, tool calls and tool results, images by URL or base64 data URL, and inline PDF files translate both ways.
- `max_completion_tokens` or `max_tokens` becomes `max_tokens`; a request without either gets the model's output limit, since the Claude API requires one.
- `stop`, `tool_choice`, `parallel_tool_calls`, `reasoning_effort` (as `output_config.effort`), a `json_schema` `response_format` and `user` translate to their Messages counterparts; `temperature`, `top_p` and `top_k` pass on as they are.
- Thinking comes back as `reasoning_content`, as llama-server reports it; cache reads count into `prompt_tokens` and again as `cached_tokens`.
- An API error keeps its status and `Retry-After`, with its message in OpenAI's error shape.

Fields that only tune sampling or configure OpenAI's own services, such as `seed`, `presence_penalty`, `frequency_penalty`, `logit_bias`, `store` and `service_tier`, are dropped, since the Claude API refuses fields it doesn't know.
A request that asks for what the Claude API can't give is refused with `400` before it is sent: `n` above 1, `logprobs`, audio, a `json_object` `response_format`, OpenAI's web search, or a tool that isn't a function.

A Chat Completions client can't send thinking back, since Chat Completions has no place for the signature it needs, so in a tool loop Claude continues without its earlier reasoning.

### Keys

Clients authenticate with `Authorization: Bearer <key>`.

- A key listed in `server.apiKeys` is accepted, and its name appears in the log as `client=<name>`.
- An unlisted key is rejected with `401`, unless `server.unknownKeys` is `allow`. Then it is accepted and logged as `client=unknown` with a fingerprint: the first 8 hex characters of the key's SHA-256, so different callers can be told apart. Keys themselves are never logged.
- With no `apiKeys` listed, every request is accepted.
- `GET /health` never needs a key.

By default, a client's key stays at the gateway, and each backend gets its own `apiKey`.
With `passthroughAuth: true` on a backend, completion requests to it carry the client's own key in `Authorization` instead, so the backend makes the access decision and sees each caller as themselves.
A client that sent no key is forwarded without one; it never borrows the backend's key.
The backend's `apiKey` is still used for the gateway's own calls, such as checking which models exist and which are loaded.

Passthrough is set per backend, so a client's key only goes to backends that opted in.
To pass through every caller's key without listing them all, combine it with `unknownKeys: allow`, and name the keys you want to see in the log.

If a facade's candidates span two or more backends with `passthroughAuth`, the gateway warns at startup and `check` notes it: a client has one key, but it is sent to whichever of those backends wins, so it may be refused by some of them.

### Open WebUI

An `openWebUi` backend is an Open WebUI instance, and its models are whatever Open WebUI serves: its Ollama and OpenAI connections, its workspace models, and the facades of any gateway it is connected to.
Instead of forwarding a request, the gateway runs it through Open WebUI's own chat flow, the one Open WebUI's UI uses, so Open WebUI's tools run on its server and any client gets them.

For each request, the gateway creates a chat, starts the completion, polls the chat for the answer while it is generated, and returns it as a normal response or stream.
The model's reasoning comes back as `reasoning_content`, streamed ahead of the answer as llama-server sends it, so a client that shows reasoning has something to show while the model thinks.
After the request, the chat is deleted, archived or kept, as `chats` says.
A refusal from a server behind Open WebUI, such as llama-swap's `429`, reaches the gateway only as an error in the chat, so the client gets `502` with Open WebUI's error text.

#### What a request can use

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

Some tools can't work through the gateway, because Open WebUI runs them in the user's browser and a gateway request has no browser behind it: direct tool servers, asking the user a question mid-answer, and the code interpreter when it uses the default Pyodide engine.
Set Open WebUI's code interpreter to its Jupyter engine to use it through the gateway.
A client's own tools don't work either, since Open WebUI runs the tool loop with its own tools only.

Everything a tool reads or writes belongs to an Open WebUI account: the account behind `apiKey`, or each client's own account with `passthroughAuth`.
That account's permissions decide which built-in tools are offered.
Without passthrough, every client of the gateway shares that account's memories, notes and chat history; with it, each caller has their own.

The built-in tools' definitions take about 2,000 tokens of context.
On a small Ollama model, raise its context in the same `params` block (e.g. `params: { num_ctx: 8192 }`) or the start of the conversation is cut off.

#### The gateway on both sides

One of Open WebUI's connections can be this same gateway, so a request can pass through the gateway twice: a client asks for a facade on Open WebUI, and Open WebUI's model is a facade of the same gateway, which picks whichever model is loaded.
Passing through one gateway twice for two different facades is fine.
A request that comes back to the same facade of the same gateway is a loop, and is refused with `508`.

#### Loaded state through Open WebUI

Open WebUI's model list reports load state precisely only for its Ollama connections.
For an OpenAI-compatible connection there is no standard way to ask which models are loaded, so Open WebUI passes on whatever that server's own model list says, and the gateway reads what it can:

- Ollama models: whether Ollama has them loaded, as Open WebUI reports it.
- llama-swap models: llama-swap's `status` field, which says `loaded` for any model that isn't stopped. A model that is still starting, or already stopping, looks ready.
- Facades of a gateway connected to Open WebUI: that gateway's own `current` selection, which it reports only with [`api.openAi.addCurrent`](#api) on. With it off, they are unknown.
- Workspace models: their base model's state, as aliases of it; see [Facades and candidates](#facades-and-candidates).
- Anything else, such as LM Studio, llama-server or a hosted OpenAI-compatible provider: unknown, which selects like not loaded.

A gateway can read its own facades this way, when it is itself one of the connections of the Open WebUI it reads.
Reading Open WebUI's state then makes Open WebUI call this gateway's `/v1/models`, which with `api.openAi.addCurrent` on reads the same state, so the `openWebUi` backend needs `loopsBack: true`.
Without it, the read waits on itself until `stateTimeoutSeconds` and fails: listings and Open WebUI's own model list stall, and chats on that backend fail with `502`.

If the gateway also has a backend for a server that Open WebUI connects to, a link lets the gateway skip Open WebUI for that server's state and ask its backend directly.
Each link names the gateway's backend entry for the same server, and `prefixId`, the prefix Open WebUI's connection adds to its model IDs, if it has one:

```yaml
backends:
  llamaSwap:
    type: llamaSwap
    baseUrl: http://llama-swap:8080
  lmStudio:
    type: lmStudio
    baseUrl: http://lm-studio:1234
  openWebUi:
    type: openWebUi
    baseUrl: http://open-webui:8080
    apiKey: ${OPEN_WEBUI_API_KEY}
    links:
      - backend: llamaSwap
      - backend: lmStudio
        prefixId: studio
```

An Open WebUI model belongs to a link when its ID, with the prefix removed, is a model or alias that the linked backend lists.
Open WebUI's list marks llama-swap's models and those of its Ollama connections, so those match only a link to a backend of the same type; any other model can match any link.
A linked model takes the state its backend reports, so the selection steps apply exactly, as they do for a facade on that backend: llama-swap's `starting` and `stopping`, llama-server's `loading`, LM Studio's loaded instances.
The gateway reads that state through the linked backend each time it needs Open WebUI's state, not only when it fetches Open WebUI's model list.
It also counts its own requests to linked models through Open WebUI in that backend.
So a load the gateway started through either backend shows as starting for facades on both at once.
A llama-swap preset takes its base model's state this way even if Open WebUI doesn't list the base.
LM Studio's rule of one load at a time does not extend through a link: a facade on Open WebUI can still send a request for another LM Studio model during a load, which aborts it.

If a linked backend can't be reached, its models fall back to the state Open WebUI reports, as without the link.
A link that can't be right gets a warning, at startup, or once the backends answer if one of them was down: a linked backend that lists none of Open WebUI's models, a llama-swap model that no linked `llamaSwap` backend lists, or a model that two linked backends list, which takes the state of the first.
Each means a wrong `backend` or `prefixId`, and two links to servers with the same model IDs need a prefix on at least one connection.

## Recipes

Each recipe is a complete setup for one use, built from the parts explained under [How it works](#how-it-works).

### Open WebUI task model

Open WebUI runs background tasks (titles, tags, follow-ups and search queries) on a configurable task model, and loading a model only for a title evicts the one you are chatting with.
A facade whose candidates are your chat models, with thinking off, runs each task on whichever is loaded, and `config.example.yaml` has one, `task-monkey`.
Here Open WebUI is a client of the gateway:

1. In **Admin Panel → Settings → Connections**, add a second OpenAI connection with the URL `http://ai-model-facade:8088/v1`.
2. In **Admin Panel → Settings → Models**, hide `task-monkey` so it doesn't appear in the chat model picker.
3. In **Admin Panel → Settings → Interface**, set the **Task Model** to `task-monkey`.

Chat traffic keeps going straight to the backend; only background tasks go through the gateway.
If no candidate is loaded and the facade has no `loadIfNone` candidate, the task gets `503`, and Open WebUI skips it.

### Voice assistant on whichever preset is loaded

A voice assistant needs a fast answer with no thinking, whatever model set is loaded at the time.
With llama-swap, give each model family an `:Assistant` preset that turns thinking off, and point the assistant at a facade whose one pattern matches them all:

```yaml
models:
  assistant:
    backend: llamaSwap
    description: Voice assistant on whichever model is loaded
    candidates:
      - model: "*:Assistant"
      - model: qwen2.5-coder-3b:q8_0
        params: { chat_template_kwargs: { enable_thinking: false } }
        loadIfNone: true
```

A preset is an alias, so `"*:Assistant"` matches the preset of whichever family is loaded, and llama-swap applies that preset's own params, so this candidate sets none.
The second candidate is loaded only if no family with a preset is.

### Evaluating models

To compare models from the clients you already use, list every model under evaluation as a candidate of the facade those clients use:

```yaml
models:
  assistant:
    backend: llamaSwap
    candidates:
      - model: qwen3.8-27b:*
      - model: qwen3.6-35b-a3b:*
      - model: gemma4-31b:*
```

Whichever one is loaded answers, so the next model is tried by loading it on the backend, with no change to any client or to this config.
Each response's `X-Gateway-Target` header, and the `winner` on the log line, names the model that produced it.
A model that isn't loaded is never loaded for a request here, since no candidate has `loadIfNone`; a request when none is loaded gets `503`.

### Open WebUI's tools for any client

To give a client with no tool support of its own web search and memories, put a facade on an `openWebUi` backend, with the features turned on in its params:

```yaml
backends:
  openWebUi:
    type: openWebUi
    baseUrl: http://open-webui:8080
    apiKey: ${OPEN_WEBUI_API_KEY}

models:
  researcher:
    backend: openWebUi
    description: Assistant with web search and memory
    params:
      features: { web_search: true, memory: true }
      params: { think: false }
    candidates:
      - model: task-monkey
        loadIfNone: true
```

The candidate is a model as Open WebUI names it, and here it is a facade of this same gateway, reached through the gateway's connection from the [task model recipe](#open-webui-task-model), so the answer comes from whichever model is loaded.
Open WebUI loads nothing for a facade, so `loadIfNone` only makes the gateway willing to send to it when its state is unknown.

For the gateway to see whether `task-monkey` has a loaded model through Open WebUI, which it then reports as loaded rather than unknown, it has to report its own selection to Open WebUI, and reading that leads back to itself:

```yaml
api:
  openAi:
    addCurrent: true

backends:
  openWebUi:
    type: openWebUi
    baseUrl: http://open-webui:8080
    apiKey: ${OPEN_WEBUI_API_KEY}
    loopsBack: true
```

See [Loaded state through Open WebUI](#loaded-state-through-open-webui) for why both are needed.

## Configuration reference

The config is YAML, read by `serve`, `check` and `health`.
`config.example.yaml` is a complete setup with two facades: `task-monkey` for Open WebUI background tasks, and `assistant` for a voice assistant that uses whichever `:Assistant` preset llama-swap has loaded.
It also carries a commented-out entry for each other backend, and a commented-out `researcher` facade that gives any client Open WebUI's web search and memory.

`${NAME}` in any string value is replaced by the environment variable `NAME` when the config loads.
It is meant for secrets, so they can live outside the config file; there is no other substitution.

`vars` is ignored by the gateway.
It is a place to define YAML anchors (`&name`) that entries reference explicitly (`*name`), so every facade shows exactly what it uses.

### Errors and warnings

A config that is wrong is a startup error, reported with its line number: unknown keys, undefined environment variables, a backend or model name defined twice, a reference to a backend that isn't defined.
Checks that depend on a backend being up and current only warn: a backend that can't be reached, or an exact candidate ID it doesn't list.
`serve` logs the warnings and starts, so one backend that is down, or not ready yet, doesn't take the other backends' facades with it; an unlisted candidate never shows as loaded until its backend lists it.
If a backend that a check needs can't be reached, `serve` asks it for its models again every `listRetrySeconds`, and once it answers, runs the checks that were waiting for it and logs what they find.
The retries themselves log nothing, since the startup warning already reported the backend as down.
`check` reports the same problems as errors and exits non-zero, so run it before deploying a config.
Every backend is asked for its models once, all at the same time, so backends that don't answer delay startup by one `stateTimeoutSeconds` between them, not one each.

### `server`

| Key | Description |
|-----|-------------|
| `listen` | Address and port to listen on. `127.0.0.1` accepts local connections only; to accept them from other machines or containers, use `0.0.0.0` or a specific IP, and set `apiKeys`. Required. |
| `apiKeys` | A list of `name` / `key` pairs. When set, requests need one of these keys, and the key's name appears in the log. The name `unknown` is reserved. Optional. |
| `unknownKeys` | `reject` (default): an unlisted key gets `401`. `allow`: accept it and log it as `unknown` with a fingerprint. See [Keys](#keys). |
| `targetHeader` | Name of the response header naming the chosen model. Default `X-Gateway-Target`. |

### `logging`

| Key | Description |
|-----|-------------|
| `level` | `debug`, `info` (default), `warning` or `error`. |
| `logSelection` | `true`: log one line per request: facade, candidates checked, winner and reason. Default `false`. Failed requests are always logged. |

### `api`

The `api` section configures what the gateway presents to the clients of each [client API](#client-apis).

| Key | Description |
|-----|-------------|
| `openAi.addCurrent` | `true`: `/v1/models` and `/v1/models/{id}` include [`current`](#seeing-the-choice-before-a-request). Default `false`, so those endpoints answer from the config and read no backend's state. A gateway that is a connection of an Open WebUI which a gateway reads needs it on: see [Loaded state through Open WebUI](#loaded-state-through-open-webui). |

### `backends`

Each entry is a named backend; `type` selects the adapter: `llamaSwap`, `llamaServer`, `lmStudio`, `localAI`, `ollama`, `openWebUi`, `openAi` or `anthropic`.
All take these keys:

| Key | Description |
|-----|-------------|
| `baseUrl` | The backend's address. Required. |
| `apiKey` | Sent to the backend as `Authorization: Bearer`, on every call unless `passthroughAuth` is on, and then only on the gateway's own calls. Optional. |
| `passthroughAuth` | `true`: completion requests carry the client's own key (or none). Default `false`. See [Keys](#keys). |
| `stateCacheSeconds` | How long to cache the list of loaded models. Default `0`, which asks once per request: a request reads each backend's state once, even for several facades in a `/amf/models` listing, or when it loops back to the gateway through Open WebUI. Concurrent requests share one in-flight check either way. |
| `stateTimeoutSeconds` | Timeout for the state and model-list calls. Default `10`. |
| `loopsBack` | `true`: reading this backend's state can lead back to this gateway's `/v1/models`, as reading Open WebUI can when this gateway has `api.openAi.addCurrent` on and is one of its connections. `/v1/models` then doesn't wait for a read of this backend already in flight, and answers from the last state, or leaves `current` out if there is none. Default `false`: `/v1/models` waits, so `current` reflects that read. If the read does lead back to this gateway with this off, the read waits on itself until `stateTimeoutSeconds` and fails, so listings stall and chat requests that need that state fail with `502`. |
| `listRetrySeconds` | If the backend couldn't be listed at startup, how often to ask it for its models again, so the checks that need them run once it answers; see [Errors and warnings](#errors-and-warnings). Default `30`. |
| `connectTimeoutSeconds` | Connection timeout. Default `0`, no limit. |
| `requestTimeoutSeconds` | How long to wait for the backend to start responding. Allow for a cold load of a `loadIfNone` candidate; a stream isn't cut off once it has started. Default `0`, no limit. |

#### llama-swap

A `llamaSwap` backend is a llama-swap instance, which starts and stops llama-server processes on demand.
Model IDs are the IDs in llama-swap's config, and its presets are aliases, which can be candidates; see [Facades and candidates](#facades-and-candidates).
State comes from its `/running` endpoint, which reports each model as `ready`, `starting` or `stopping`.
It takes no keys beyond the shared ones.

llama-swap marks presets and their base models in that list from v241 on; with an earlier version, an alias candidate never counts as loaded.
llama-swap decides itself when a model is swapped, and two of its rules, as of v263, affect how long a request through the gateway can take:

- A request whose model needs a busy model unloaded waits until that model has no requests in flight; meanwhile, new requests for the busy model are still served, so a steady stream to it keeps the swap waiting.
- Each model accepts a limited number of requests in flight or waiting, set by `concurrencyLimit` in llama-swap's config (default 10). llama-swap refuses the rest with `429` and a `Retry-After` header, which the gateway passes on.

#### llama-server

A `llamaServer` backend is llama.cpp's own server, `llama-server`, in either of its modes.
The gateway asks the server which mode it is in each time it reads its state, so no key says which, and a server restarted in the other mode needs no change here.
It takes no keys beyond the shared ones.
This describes llama-server as of b11580.

- **Router mode**, started without a model, loads and unloads models on demand, up to `--models-max` at once. A model counts as loaded when the server reports it `loaded`, and as starting when it reports `loading`; a sleeping model, unloaded by `--sleep-idle-seconds`, counts as not loaded, since the next request loads it again. To load a model when `--models-max` are loaded, the server unloads the least recently used one that has no request in flight, and if every loaded model is busy, the request waits until one finishes.
- **Single-model mode** holds one model for as long as the server runs, and the model counts as loaded unless it is sleeping. While the server starts, it refuses every request, a chat included, so the model counts as not loaded until the server has finished starting.

Model IDs are the IDs the server lists: in router mode each preset's name, in single-model mode the `--alias`.
A preset's `alias` key gives its model more IDs; unlike a llama-swap preset, such an alias is only another name for the model, with no params of its own.
Single-model mode answers whatever `model` a request names, so a candidate with the wrong ID would still be served; the startup check warns about it, and the candidate never counts as loaded.

#### LM Studio

An `lmStudio` backend is LM Studio's server, the desktop app's or its headless daemon's, reached through its OpenAI-compatible endpoints.
Model IDs are the keys LM Studio lists, such as `qwen3-0.6b`; its embedding models are left out, since the gateway relays only chat and completions.
State comes from LM Studio's own model list, `/api/v1/models`: a model is loaded when LM Studio lists a loaded instance of it.
It takes no keys beyond the shared ones.
This describes the headless daemon as of llmster 0.0.25-1.

LM Studio answers a request for a model it doesn't have with whichever model is loaded, so a candidate with the wrong ID would still be served; the startup check warns about it, and the candidate never counts as loaded.
By default LM Studio holds one model loaded by a request at a time, and loading another unloads it.

#### LocalAI

A `localAI` backend is a LocalAI server, reached through its OpenAI-compatible endpoints.
Model IDs are the names of LocalAI's model configs.
A LocalAI alias, a config with only a `name` and an `alias`, is another name for its model, with no params of its own: it can be a candidate, and counts as loaded whenever its model is.
The model list holds every model LocalAI has, embedding and image models included, because LocalAI's list doesn't say what each model does.
It takes no keys beyond the shared ones, but the gateway reads LocalAI's state from its admin endpoints (`/system`, `/api/aliases` and `/api/settings`), so with LocalAI's API keys set, `apiKey` must be an admin key.
This describes LocalAI as of v4.11.0.

With a limit on loaded models (`LOCALAI_MAX_ACTIVE_BACKENDS`), a request for any model that isn't loaded, even the one loading, evicts a load under way, and the request that started that load fails with `500`.
So while `/api/settings` reports such a limit and the gateway has a load under way there, the backend is busy: none of its candidates is used, and a request that finds nothing else gets `503`.
Under a limit of one nothing else is loaded meanwhile anyway; under a higher limit, the models still loaded are skipped too.
With LocalAI's web UI off there is no `/api/settings`, and the gateway assumes a limit.
A load that another client starts isn't visible, so a request the gateway sends then can evict it, and the other way round.

#### Ollama

An `ollama` backend is an Ollama server, reached through its OpenAI-compatible endpoints.
Model IDs are the full `name:tag` form that `ollama list` shows, such as `llama3:latest`.
A candidate without a tag, such as `llama3`, isn't treated as `llama3:latest`; the startup check warns about it and suggests the tagged IDs Ollama lists.
State comes from `/api/ps`.
It takes no keys beyond the shared ones; Ollama itself ignores `apiKey`, but a proxy in front of it may need one.

#### OpenAI

An `openAi` backend is OpenAI's API, `https://api.openai.com`, or another hosted API that serves Chat Completions the same way, such as OpenRouter's, at its own `baseUrl`.
Model IDs are the ones its `/v1/models` lists, which includes the API's embedding, image and audio models, since that list doesn't say what each one does.
It takes no keys beyond the shared ones; `apiKey` is the API key.

Every model listed counts as loaded while the API answers, so the first such candidate a list reaches wins, and the candidates after it are never reached.
One placed after the local candidates is used when none of them is loaded, with no `loadIfNone` needed.

The gateway reads the list when it starts or reloads, and a request doesn't read it again, so a healthy API costs no state call per request.
It reads the list again in two cases, so an API that goes down stops counting as loaded:

- A request to the API gets no answer, or a `5xx`. That request still fails with `502`, since there is no failover, but the next request finds out whether the API answers.
- The list is older than `stateCacheSeconds`, if that is set, so an outage shows within that time even before a request fails. With the default of `0`, only a failed request makes it check.

While the list can't be read, the backend's candidates count as `state unavailable`, and selection passes over them; each request then tries the list again, so the backend is back as soon as the API answers.
A `429` rate limit doesn't count as down: it is passed on with its `Retry-After`.
A model the API adds becomes a candidate the next time the list is read.

#### Anthropic

An `anthropic` backend is Anthropic's Claude API, `https://api.anthropic.com`, reached through its Messages API.
Model IDs are the ones its `/v1/models` lists, and it counts them as loaded exactly as an `openAi` backend does: every listed model while the API answers, with the list read again after a failed request or once older than `stateCacheSeconds`.
It takes no keys beyond the shared ones; `apiKey` is a Claude API key.
A key that isn't scoped to one workspace needs Anthropic's `anthropic-workspace-id` header, which the gateway doesn't send, so use a workspace key.

A Chat Completions client's request is translated; see [Chat Completions on the Claude API](#chat-completions-on-the-claude-api).
Any key the translation doesn't cover passes on unchanged, so a facade's params can set Messages fields, such as `thinking: {display: summarized}` to see a summary of Claude's thinking, `cache_control` to turn on prompt caching, or `output_config`, whose keys win over a translated `reasoning_effort`.

Claude 5 models think by default, which counts towards `max_tokens`.
The Claude API has no legacy `/v1/completions`; such a request fails with `502`.

#### Open WebUI

An `openWebUi` backend is an Open WebUI instance, which runs each request through its own chat flow; [Open WebUI](#open-webui) explains how, and what a request can use there.
Model IDs are the ones Open WebUI lists, prefixes included.
The API key needs these Open WebUI endpoints: `/api/models`, `/api/v1/chats/new`, `/api/chat/completions`, `/api/v1/chats/{id}` (`GET` and `DELETE`), `/api/tasks/chat/{id}` and `/api/tasks/chat/{id}/stop`, plus `/api/v1/chats/{id}/archive` with `chats: archive`.

Open WebUI has no legacy `/v1/completions`; such a request fails with `502`.

Beyond the shared keys, it takes:

| Key | Description |
|-----|-------------|
| `chats` | What happens to the chat after each request: `delete` (default), `archive` or `keep`. |
| `pollIntervalSeconds` | How often to poll the chat while the answer is generated. Default `0.25`. |
| `links` | Optional: a list of `{backend: <name>, prefixId: <prefix>}`. Read the state of the Open WebUI models that one of the gateway's other backends serves directly from that backend; see [Loaded state through Open WebUI](#loaded-state-through-open-webui). |
| `llamaSwap` | Optional: `{backend: <name>, prefixId: <prefix>}`. One more link, to a `llamaSwap` backend only. |

### `models`

Each entry is a facade, and its name is what clients request.

| Key | Description |
|-----|-------------|
| `backend` | Name of a `backends` entry: the default for its candidates. Required unless every candidate names its own. |
| `candidates` | The models to choose from, in order of preference. Required. |
| `params` | Applied to every request for this facade. Optional. See [Params](#params). |
| `description` | Shown in the model lists and `/amf/models`. Optional. |
| `type` | Facade type. `candidates`, the default, is the only type today. |

Each candidate takes:

| Key | Description |
|-----|-------------|
| `model` | An exact backend model ID or alias, or a pattern using `*`, `?` or `[...]`. |
| `backend` | The backend this candidate is on. Default: the facade's `backend`. |
| `loadIfNone` | `true`: if no candidate is ready or starting, the first `loadIfNone` candidate is used and its backend loads it. Default `false`: only used when ready or starting. Must be an exact ID. |
| `params` | Applied after the facade's `params` when this candidate wins. Optional. |

## API reference

The gateway serves these endpoints, shown at their default paths.
Every endpoint but `/health` needs a key if `server.apiKeys` is set; see [Keys](#keys).

| Endpoint | Description |
|----------|-------------|
| `POST /v1/chat/completions` | Chat Completions, streamed or not. |
| `POST /v1/completions` | Legacy text completions, streamed or not. Same selection and params as chat. |
| `GET /v1/models` | Lists only the facades, so clients that pick from a list see only those. |
| `GET /v1/models/{id}` | One facade, or `404`. |
| `GET /amf/models` | The gateway's own listing: each facade with `current` and every candidate's state; see [Seeing the choice before a request](#seeing-the-choice-before-a-request). |
| `GET /amf/models/{id}` | One entry of that list, or `404`. |
| `GET /health` | `{"status":"ok"}` while the gateway is up. Needs no API key and doesn't contact any backend. |

Model objects on `/v1/models` have the standard OpenAI fields, and also `current` if you turn on [`api.openAi.addCurrent`](#api).
The `/amf` endpoints belong to the gateway rather than to a client API, so they report what a model object has no place for.

### Replies and headers

A response from a backend is passed on with its status, content type and body, and a stream is relayed event by event.
A translated response keeps its status, with its body or stream translated back to the client's API.
Of the backend's other headers, only `Retry-After` is passed on, so a client refused with `429` or `503` knows when to try again; the rest describe the backend or the connection to it.
Responses from Open WebUI are assembled by the gateway from the chat it ran; see [Open WebUI](#open-webui).

The gateway adds its own headers:

| Header | Meaning |
|--------|---------|
| `X-Gateway-Target` | The model that was chosen, on every response where one was. Renamed by `server.targetHeader`. |

### Status codes

The gateway's own errors use these codes:

| Status | Meaning |
|--------|---------|
| `400` | The body isn't a JSON object, or it repeats a key. Duplicate keys are rejected because the gateway patches the first occurrence while backends read the last, which would let a request slip values past the gateway's params. A translated request also gets `400` when it asks for something the backend's API has no counterpart for. |
| `401` | `server.apiKeys` is set, `unknownKeys` is `reject`, and the request has no matching key. |
| `404` | The requested model isn't a facade, or the path doesn't exist. |
| `405` | The path doesn't take this method. |
| `413` | The body is over 100 MiB. |
| `502` | The backend couldn't be reached or failed the request, no backend's state could be read, or the chosen candidate's backend can't serve the request's protocol. |
| `503` | No candidate is eligible. |
| `508` | Routing loop: the request has already passed through this facade of this gateway, e.g. via Open WebUI back to itself. |

Each error is in OpenAI's shape; a wrong path or method gets a plain-text answer.

Once a stream has started, its status can't change.
If the backend fails mid-stream, the gateway sends a final error event and closes, so clients show an error instead of a silently cut-off reply.
That is a `data: {"error": {...}}` event with no `[DONE]`.

## Commands

```
ai-model-facade serve <config>    run the gateway
ai-model-facade check <config>    validate the config and print each candidate's resolved params
ai-model-facade health <config>   exit 0 if the gateway at server.listen answers /health, 1 if not
ai-model-facade version           print the version, which the startup log line also reports
```

`serve` and `check` make the same checks; `check` fails on what `serve` only warns about.
See [Errors and warnings](#errors-and-warnings).
`serve` logs to standard error.

`health` only reads `server.listen` from the config and contacts no backend; a wildcard address such as `0.0.0.0:8088` is checked on `127.0.0.1`.

On Ctrl+C or `SIGTERM`, `serve` stops accepting requests and gives those in flight up to 10 seconds to finish, then exits; a stream still running then is cut off.

### Reloading the config

Send `serve` a `SIGHUP` to reload the config without a restart:

```bash
kill -HUP <pid>
```

The [Installation](#installation) section gives the same for each service manager and for Docker.
The new config is loaded and validated in full first, and replaces the running one only if it has no errors.
Otherwise the running config stays and the error is logged.
Requests already in flight, including streams, finish on the config that accepted them.
Everything can change on reload except `server.listen`, which needs a restart; a reload that changes it is rejected.
Run `check` first to see what a reload would load.
Windows has no `SIGHUP`, so there a changed config needs a restart.

## Upgrading

[CHANGELOG.md](CHANGELOG.md) lists every release's changes, and, under **Changed** and **Removed**, what a config written for the release before it has to change.
