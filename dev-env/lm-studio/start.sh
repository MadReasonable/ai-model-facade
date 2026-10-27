#!/bin/sh
# Starts LM Studio's daemon, llmster, with its OpenAI-compatible server on
# port 1234, for the lmStudio backend; see dev-env/README.md.
set -eu

# LM Studio names a model after the folder that holds its file, lowercased,
# and follows symlinks, so a folder per model linking to a shared file gives
# each model its ID without copying the file. control passes the models from
# config.yaml as LM_STUDIO_MODELS, space-separated <id>=<file> pairs.
rm -rf /root/.lmstudio/models/dev
for model in ${LM_STUDIO_MODELS:-}; do
  id=${model%%=*}
  file=${model#*=}
  mkdir -p "/root/.lmstudio/models/dev/$id"
  ln -sf "$file" "/root/.lmstudio/models/dev/$id/$(basename "$file")"
done

# A model loaded by a request unloads the previous one (LM Studio's default,
# developer.unloadPreviousJITModelOnLoad) and, with this, stays until then,
# rather than unloading after an hour idle. The daemon fills in the rest of
# the file on its first start and keeps this setting in it, so a restarted
# container already has it.
settings=/root/.lmstudio/settings.json
if [ ! -f "$settings" ]; then
  echo '{"developer": {"jitModelTTL": {"enabled": false, "ttlSeconds": 3600}}}' > "$settings"
fi

/root/.lmstudio/llmster/*/llmster --docker-compatible-platforms cpu-only &
daemon=$!
trap 'kill "$daemon"' TERM INT

# The server is started once the daemon is up, and once only: an lms
# command that finds no daemon starts one of its own, and two daemons
# unpacking LM Studio's runtimes at once leave them broken. lms daemon status
# only reports.
until lms daemon status --json | grep -q '"status":"running"'; do
  sleep 1
done
lms server start --bind 0.0.0.0 --port 1234 >/dev/null
echo "LM Studio server listening on port 1234"
wait "$daemon"
