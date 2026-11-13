#!/bin/sh
# Starts LocalAI on port 8080 for the localAi backend, with the models
# config.yaml lists; see dev-env/README.md.
set -eu

# control passes the models as LOCALAI_DEV_MODELS, space-separated
# <id>=<file> pairs with each file under the models store, and the aliases
# as LOCALAI_DEV_ALIASES, <alias>=<id> pairs. LocalAI reads one file of
# model configs (LOCALAI_MODELS_CONFIG_FILE); a model's file is relative to
# its models path, the read-only store.
config=/tmp/models.yaml
: > "$config"
for model in ${LOCALAI_DEV_MODELS:-}; do
  cat >> "$config" <<YAML
- name: ${model%%=*}
  backend: llama-cpp
  parameters:
    model: ${model#*=}
  template:
    use_tokenizer_template: true
YAML
done
for alias in ${LOCALAI_DEV_ALIASES:-}; do
  cat >> "$config" <<YAML
- name: ${alias%%=*}
  alias: ${alias#*=}
YAML
done

export LOCALAI_MODELS_PATH=/models
export LOCALAI_MODELS_CONFIG_FILE="$config"
exec /entrypoint.sh
