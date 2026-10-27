# Development backends

**Real backends in Docker, run through one control service, for checking the gateway's adapters against what each backend actually reports.**

`go test` never uses these: the tests run against fakes in `internal/testkit` and need no network.
The fakes are only as accurate as what was checked against a real server, so this folder runs those servers on demand, with small models that share one set of files.

One service, `control`, runs the environment.
`docker compose up` starts only `control`, and `control` downloads the model files, starts and stops each backend, and loads and unloads its models, through one HTTP API on 127.0.0.1:18090, so no backend's own commands need remembering.

Every backend that loads models on demand is set to hold at most two at once and offers three model IDs, so a request to the third always evicts one, and the order in which the backend evicts can be watched.
The IDs can all load the same file, because nothing here tests the models themselves; Ollama is the exception, described in its section.

A model ID names its backend, its kind of model and its number, `<backend>-llm-<n>`, so the gateway's log and `X-Gateway-Target` show which backend answered, and a pattern such as `*-llm-*` can pick out LLMs once other kinds of model are served.
The IDs say nothing about the model, so each backend's section below says which file each ID loads and what is special about it.

To change the environment, such as to add a backend, see [ARCHITECTURE.md](ARCHITECTURE.md).

It needs Docker with Compose v2.
Any one backend, with as many models loaded as it allows, fits on a Debian machine or VM with 8 GiB of RAM, running on CPU only; running several at once isn't promised.

## Starting the environment

From this folder, start `control`:

    docker compose up -d

The first `up` builds `control`'s image, which takes a minute; later ones reuse it.
Then start a backend through its API, and watch it come up:

    curl -X POST localhost:18090/api/backends/llamaSwap/start
    curl localhost:18090/api/backends/llamaSwap

The first start of any backend downloads the model files, about 0.64 GB, before the backend starts; later starts skip files that are already there.
`POST /api/backends/<backend>/stop` stops a backend again, and `POST /api/stop` stops them all.
A stop removes the backend's container, as `docker compose down <service>` does, and keeps what the backend stores, which is in volumes; a stopped container would keep the project network, and a later start would fail once `down` had removed it.

`docker compose down` stops the backends along with `control`, so it leaves nothing running and no containers behind.
`control` stops them whenever its own container stops, because a `down` and a recreate send it the same signal, so an `up` that rebuilds `control` after a change to its code stops them too, unless `KEEP_BACKENDS` is true (see Settings).

Backend ports listen on 127.0.0.1 only, since the backends have no API key; see Settings to reach them from another machine.

## Configuration

What the environment runs, its model files and each backend's models and settings, is in `config.yaml` in this folder, which describes each entry.
It sits between two other files, each with a job of its own: `.env` holds what differs between machines (see Settings), and `compose.yaml` what each container is, its image, volumes and ports.

`control` reads `config.yaml` on every request that needs it, so an edit needs no restart of `control`, and an unknown key or a model naming an unlisted file is an error that says what is wrong.
It hands the settings a container takes at start, command-line flags or environment variables, to the containers through a Compose override it writes for every Compose run.
So a backend gets its settings only when `control` starts it, and an edit to a running backend's applies once it is stopped and started again; one started by hand with `docker compose` runs without them.
A setup downloads the model files that are missing, and makes Ollama's models match its section, as described under Ollama.

`llamaSwap` keeps its own config file, in llama-swap's format, in `backend-config/`, and `config.yaml` names the file it uses.
It mounts the `backend-config/` folder rather than its one file, so a running backend sees an edit however the editor saved it: a file mounted on its own stays the file the container started with when an editor saves by writing a new file and renaming it over the old one.
After editing it, `POST /api/backends/<backend>/reload` makes the backend read it again without restarting; stopping and starting it works too.
A reload of `ollama`, which has no config file of its own, makes its models match `config.yaml` in the same way.

## The control API

`control` takes and returns JSON, and three rules hold for every endpoint.
A `GET` never changes anything.
A `POST` means "make it so": asked for what is already true, it answers `200` with the current state and does nothing, and work that takes more than a moment, such as a download, a start or a load, answers `202` at once and goes on in the background, where the matching `GET` shows it.
Every backend, setup step and file the API reports has a `state` from a fixed list, always present, which is what a client decides by, and a `detail` beside it, also always present, saying what is happening or, with `failed`, why: it is for people and may be reworded, so a client never parses it. A model's `state` is the exception, in its backend's own words.
Errors are RFC 9457 problem details (`application/problem+json`) whose `detail` says what is wrong: `404` for an unknown backend or model, `409` for what the backend can't do, now or at all, such as anything but `start` on a stopped backend, or reloading a backend that takes all its settings at start.

| Method and path | What it does |
|---|---|
| `GET /api/setup` | Every setup step's state, with `ready: true` once all are done. The only step so far is `files`. |
| `POST /api/setup` | Runs every step that isn't done. |
| `GET /api/files` | Each model file: `done`, `pending` (missing), `running` (downloading, with bytes done and total) or `failed` (with why in `detail`), then each file in the store that `config.yaml` doesn't list, as `unlisted`. Each says whether it is `managed`: downloaded by `control` and unchanged since. |
| `POST /api/files/download` | Downloads and checks every missing file. |
| `POST /api/files/prune` | Removes the files that are `unlisted` and `managed`, and answers with their paths; `409` while a download runs or a backend that uses the files does. |
| `POST /api/stop` | Stops every backend as `stop` does, including a container stopped some other way, and answers with them all once they have. |
| `GET /api/backends` | Every backend, with its `state`: `stopped`; `starting`, from the moment a start begins until it can serve requests, however it was started; `ready`; or `failed`, when its last start failed and it isn't ready. `detail` says what a start is doing, such as waiting for the backend to answer, or why it failed. Each also has its container's state as Docker reports it (`running`, `exited`, `absent` and so on), and its setup. |
| `GET /api/backends/<backend>` | One backend. |
| `POST /api/backends/<backend>/start` | Downloads the files it needs if they are missing, starts it, waits until it can serve requests, then runs its setup; `200` if it is `ready` with its setup done already. |
| `POST /api/backends/<backend>/stop` | Stops it and removes its container, and answers once it has. |
| `POST /api/backends/<backend>/reload` | Makes it read its config file again without restarting, or for Ollama makes its models match `config.yaml`: `200` with its models once it has, `202` for a backend that reloads in the background. |
| `GET`, `POST /api/backends/<backend>/setup` | The backend's setup: the files it needs, and for a backend whose section says so, a step of its own once it is up. |
| `GET /api/backends/<backend>/models` | Its model IDs, each with its state in the backend's own words (`loaded`, `ready`, `loading`, `sleeping` and so on), and `aliasOf` for a llama-swap preset. |
| `POST /api/backends/<backend>/models/<model>/load` | Loads the model. |
| `POST /api/backends/<backend>/models/<model>/unload` | Unloads it, and answers once the backend no longer reports it loaded. |

`<backend>` is one of the backends described under Backends; `GET /api/backends` lists them.
A model ID containing `/` must be sent as `%2F`.

`control` runs Compose itself, through the host's Docker socket, and anything that can use that socket controls Docker, which on most hosts amounts to root.
So `control`'s port stays on 127.0.0.1 whatever `BIND_ADDRESS` says, and nothing else should be given access to it.

## Settings

What differs between machines, the address and ports the backends are published on and where model files are stored, is set in a `.env` file in this folder.
Compose reads it on every command, including the ones `control` runs to start a backend, and the file is ignored by git, so it stays on its machine.
Copy `.env.example`, which lists every setting with its default, and change what you need; without a `.env`, the defaults apply.
A setting given on the command line instead, such as `BIND_ADDRESS=0.0.0.0 docker compose up`, reaches only that command, not the backends `control` starts later.

| Setting | Default | Effect |
|---|---|---|
| `BIND_ADDRESS` | `127.0.0.1` | The host address every backend port is published on. `0.0.0.0` publishes on every interface. |
| `MODELS_DIR` | empty | A host folder for the model files, in place of the `ai-model-facade-dev_models` volume. |
| `KEEP_BACKENDS` | empty | `true` keeps the backends running when `control`'s container stops. The container that stops has the setting that counts, so a change reaches the next stop after the container is recreated with it. |
| `CONTROL_PORT` | `18090` | The host port of `control`, always on 127.0.0.1. |
| `LLAMA_SWAP_PORT` | `18082` | The host port of `llama-swap`. |
| `OLLAMA_PORT` | `18083` | The host port of `ollama`. |

A changed setting takes effect when a backend is next started, since a start recreates the container if its settings differ.
A port published on another address than 127.0.0.1 is reachable from the network even if a host firewall such as ufw denies it, because Docker adds its own iptables rules for published ports, and the backends have no API key.

## Model files

Every backend reads the same model files, so a model needed by several backends is downloaded and stored once.
They live in a Docker volume named `ai-model-facade-dev_models`, unless `MODELS_DIR` names a host folder, which is then used in its place.
`control` downloads the files listed under `files` in `config.yaml` into that folder and checks each against its sha256; a file is moved to its final name only once it matches, so a file that exists is complete, and a download that stops partway resumes where it left off.
`control` runs as root, because the volume is root's, so in a `MODELS_DIR` folder the files it adds are owned by root.
Backends mount the files read-only, except Ollama, which keeps copies in its own store.

A file removed from `files` stays in the store until it is pruned, since downloading a model file again costs gigabytes, so no setup removes one.
`POST /api/files/prune` removes only files `control` downloaded and that are unchanged since, which it records in `.control-downloads.json` in the store, so a `MODELS_DIR` folder that also holds files of your own keeps them; `GET /api/files` shows which files are `managed`, and which `unlisted` ones a prune would remove.
It refuses while a download runs, since the file being written may be one of them, and while a backend that uses the files runs, since it may have started with a config that still listed one.
Files downloaded before the record existed aren't in it, so a prune leaves them; remove those by hand.

Each file is listed with its path, sha256 and URL, with paths laid out as `<publisher>/<repository>/<file>`, as on Hugging Face.

| File | Size | Why |
|---|---|---|
| `Qwen/Qwen3-0.6B-GGUF/Qwen3-0.6B-Q8_0.gguf` | 0.64 GB | Thinking, which `chat_template_kwargs.enable_thinking` turns on and off, and tool calls; it does both on demand, which is all the environment needs. |

`docker compose --profile '*' down -v` removes every container and deletes the volume, and the downloaded files with it.

## Running the gateway against it

`gateway.yaml` in this folder is a gateway config whose backends are this environment's.
It has a facade per backend that picks whichever of the backend's first two models is loaded, loading one only if neither is, and `either`, which spans backends; each turns thinking off in its backend's own way, through the gateway's params.
From the repo's root, check it or serve it:

    go run ./cmd/ai-model-facade check dev-env/gateway.yaml
    go run ./cmd/ai-model-facade serve dev-env/gateway.yaml

Every backend in it is listed whether or not it runs: one that doesn't is a warning, which `check` reports as an error, and its models count as not loaded.
The gateway listens on 127.0.0.1:8088, and the backends' ports in it are `.env.example`'s defaults, so a port changed in `.env` needs changing there too.

## Backends

### llama-swap

The `llamaSwap` backend runs llama-swap v263, `ghcr.io/mostlygeek/llama-swap:v263-cpu-b11552-non-root`, which starts one llama-server process per model on demand; the image bundles llama.cpp b11552.
Its config is `backend-config/llama-swap.yaml`, and every ID loads the Qwen3 0.6B file.

| Service | Port | Model IDs |
|---|---|---|
| `llama-swap` | 18082 | `llamaSwap-llm-1`, `llamaSwap-llm-2`, `llamaSwap-llm-3`, and the preset `llamaSwap-llm-1:NoThink` |

llama-swap reports each running model in `GET /running` as `starting`, `ready` or `stopping`.
Its `matrix` routing lets any two of the three models run at once; to load the third, it evicts the model that leaves the cheapest allowed pair, and with equal costs it takes the first pair in the config's order, not the model used least recently.
`llamaSwap-llm-1:NoThink` is a preset: an alias of `llamaSwap-llm-1` with params of its own, which llama-swap applies after the gateway's, here turning thinking off.
`llamaSwap-llm-3` also unloads itself after 30 idle seconds (`ttl`).
`GET /unload` unloads every model, and a reload sends llama-swap a `SIGHUP`, on which it reloads its config in the background.
The service keeps the image's `-watch-config`, so llama-swap also rereads its config within 2 s of a change without being asked; it reads it from `/config/llama-swap.yaml`, in place of the image's `/app/config.yaml`, so that the folder can be mounted.

### Ollama

The `ollama` backend runs Ollama 0.40.8, `ollama/ollama:0.40.8`, which loads and unloads models on demand from a store of its own; the image is 5 GB, most of it CUDA libraries that only an NVIDIA GPU uses.

| Service | Port | Model IDs |
|---|---|---|
| `ollama` | 18083 | `ollama-llm-1:latest`, `ollama-llm-2:latest`, `ollama-llm-3:latest`, `ollama-llm-4:latest` |

Ollama doesn't load a GGUF from a path: a model is imported into its store first, and its name always gets a tag, `:latest` here.
`control` does the import as the backend's setup step, once Ollama is up, through Ollama's API: it uploads the file's bytes and creates each model from them, with a 4096-token context like the other backends' in place of Ollama's 2048.

The setup step makes Ollama's models match the `ollama` section of `config.yaml`, which is why it is done only when they do.
It compares each model Ollama has with the section, by name, by the blob it was made from and by its context size, then creates each model that is missing or differs and deletes each one the section doesn't list; a model that matches is left alone.
A create is cheap once Ollama has the blob, since it only writes a manifest; only a blob Ollama doesn't have is uploaded, and models are created before any are deleted, because Ollama deletes a blob as soon as no model uses it.
While a model differs, the step's `detail` says how; once they all match, it lists what Ollama found each model can do, such as `tools` and `thinking`.
`POST /api/backends/ollama/reload` does the same on a running Ollama and answers once its models match.

Ollama runs one runner per stored file, not per model name, so models created from the same bytes share it: a request to one is answered by whichever loaded it, at no cost, and `GET /api/ps` lists only that one's name (0.40.8 source, `schedulerModelKey` in `server/sched.go`; verified).
So that two models can be loaded at once and a third evicts one, `ollama-llm-2` and `ollama-llm-3` are created from copies of the file whose `general.name` metadata ends in `(2)` and `(3)`, each a separate blob in Ollama's store; `ollama-llm-4` shares `ollama-llm-1`'s, to show the sharing.
The store holds three copies, 1.9 GB.

Ollama holds two models at once (`OLLAMA_MAX_LOADED_MODELS=2`), and to load a third it evicts an idle model with the shortest keep-alive, and among equals the one whose blob's sha256 sorts first, not the one used least recently (0.40.8 source, `findRunnerToUnload`).
Ollama reports no loading state: `/api/ps` lists a model only once it has loaded.

No model unloads itself on idle (`OLLAMA_KEEP_ALIVE=-1`), because Ollama has no per-model setting for it: a model stays loaded for the `keep_alive` of its latest request, or the server's default.
A request with `"keep_alive": "30s"` unloads that model 30 s after it, unless another request resets it; a gateway facade can send it through its `params`.
Ollama takes its server settings, `maxLoadedModels` and `keepAlive`, from environment variables, so a change to them applies only after a stop and start; a reload changes only its models.

Through its OpenAI endpoint, `/v1/chat/completions`, Ollama puts thinking in a field named `reasoning`, where llama.cpp's server uses `reasoning_content`, and turns it off only with `"reasoning_effort": "none"`; it ignores `chat_template_kwargs.enable_thinking` and its own native API's `think` field there (verified on 0.40.8).
