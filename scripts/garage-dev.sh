#!/usr/bin/env bash
# A single-node Garage in Docker for development and CI.
#
#   eval "$(scripts/garage-dev.sh up)"     start (idempotent), print export lines
#   scripts/garage-dev.sh env               print export lines for a running instance
#   scripts/garage-dev.sh env --plain       KEY=value lines (for $GITHUB_ENV)
#   scripts/garage-dev.sh env --ps          PowerShell $env: lines
#   scripts/garage-dev.sh down              stop and delete it, data included
#
# Same image and bucket layout as deploy/garage.yaml, but replication_factor 1:
# the three-node StatefulSet is a cluster concern, not a development one.
# Everything is configured through `docker cp` and `docker exec`, with no bind
# mounts, so it behaves the same from Git Bash on Windows as on Linux CI.
set -euo pipefail

NAME="${GARAGE_DEV_NAME:-obsync-garage}"
IMAGE="${GARAGE_IMAGE:-dxflrs/garage:v2.3.0}"
S3_PORT="${GARAGE_DEV_S3_PORT:-3900}"
WEB_PORT="${GARAGE_DEV_WEB_PORT:-3902}"
BUCKET="${GARAGE_DEV_BUCKET:-obsync}"
KEY_NAME="${GARAGE_DEV_KEY:-obsync-dev}"

log() { echo "garage-dev: $*" >&2; }

# The garage CLI logs every RPC connection to stderr. Keep that only when a
# command fails, where it is the useful part.
g() {
  local err rc=0
  err="$(mktemp)"
  MSYS_NO_PATHCONV=1 docker exec "$NAME" /garage "$@" 2>"$err" || rc=$?
  [ "$rc" = 0 ] || cat "$err" >&2
  rm -f "$err"
  return "$rc"
}

running() { [ "$(docker inspect -f '{{.State.Running}}' "$NAME" 2>/dev/null)" = "true" ]; }

up() {
  if ! docker inspect "$NAME" >/dev/null 2>&1; then
    log "creating $NAME from $IMAGE"
    local conf
    conf="$(mktemp)"
    cat >"$conf" <<EOF
metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "lmdb"
replication_factor = 1

rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "$(openssl rand -hex 32)"

[s3_api]
s3_region = "garage"
api_bind_addr = "[::]:3900"
root_domain = ".s3.garage.localhost"

[s3_web]
bind_addr = "[::]:3902"
root_domain = ".web.garage.localhost"
index = "index.html"

[admin]
api_bind_addr = "[::]:3903"
admin_token = "$(openssl rand -hex 32)"
EOF
    docker create --name "$NAME" \
      -p "127.0.0.1:${S3_PORT}:3900" -p "127.0.0.1:${WEB_PORT}:3902" \
      -v "${NAME}-meta:/var/lib/garage/meta" -v "${NAME}-data:/var/lib/garage/data" \
      "$IMAGE" >/dev/null
    # Git Bash: docker.exe needs the Windows form of the temp path, and path
    # conversion must stay off so "$NAME:/etc/..." is not rewritten.
    local src="$conf"
    command -v cygpath >/dev/null 2>&1 && src="$(cygpath -w "$conf")"
    if ! MSYS_NO_PATHCONV=1 docker cp "$src" "$NAME:/etc/garage.toml"; then
      rm -f "$conf"
      docker rm -f "$NAME" >/dev/null 2>&1 || true
      log "could not copy the config into the container"
      exit 1
    fi
    rm -f "$conf"
  fi
  running || docker start "$NAME" >/dev/null

  local i
  for i in $(seq 1 60); do
    g status >/dev/null 2>&1 && break
    [ "$i" = 60 ] && { log "garage did not become ready"; docker logs --tail 30 "$NAME" >&2; exit 1; }
    sleep 1
  done

  if g layout show 2>/dev/null | grep -qiE "no nodes|current cluster layout version: 0"; then
    local node
    node="$(g node id -q 2>/dev/null | cut -d@ -f1)"
    log "assigning layout to node ${node:0:16}"
    g layout assign -z dev -c 1G "$node" >/dev/null
    g layout apply --version 1 >/dev/null
  fi

  g bucket info "$BUCKET" >/dev/null 2>&1 || { log "creating bucket $BUCKET"; g bucket create "$BUCKET" >/dev/null; }
  g key info "$KEY_NAME" >/dev/null 2>&1 || { log "creating key $KEY_NAME"; g key create "$KEY_NAME" >/dev/null; }
  g bucket allow --read --write --owner "$BUCKET" --key "$KEY_NAME" >/dev/null
  # Website access lets a plain, unsigned GET read the bucket on the web port,
  # the shape the plugin uses behind Tailscale or an auth proxy.
  g bucket website --allow "$BUCKET" >/dev/null 2>&1 || true

  env_lines "${1:-}"
}

env_lines() {
  running || { log "$NAME is not running; use: $0 up"; exit 1; }
  local info key_id secret
  info="$(g key info --show-secret "$KEY_NAME")"
  key_id="$(printf '%s\n' "$info" | awk -F': *' 'tolower($1) ~ /key id/ {print $2; exit}' | tr -d '[:space:]')"
  secret="$(printf '%s\n' "$info" | awk -F': *' 'tolower($1) ~ /secret key/ {print $2; exit}' | tr -d '[:space:]')"
  [ -n "$key_id" ] && [ -n "$secret" ] || { log "could not read key credentials:"; printf '%s\n' "$info" >&2; exit 1; }

  local vars=(
    "GARAGE_ENDPOINT=http://127.0.0.1:${S3_PORT}"
    "GARAGE_BUCKET=${BUCKET}"
    "GARAGE_REGION=garage"
    "GARAGE_ACCESS_KEY=${key_id}"
    "GARAGE_SECRET_KEY=${secret}"
  )
  local v
  for v in "${vars[@]}"; do
    case "${1:-}" in
      --plain) echo "$v" ;;
      --ps) echo "\$env:${v%%=*} = '${v#*=}'" ;;
      *) echo "export $v" ;;
    esac
  done
}

down() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  docker volume rm "${NAME}-meta" "${NAME}-data" >/dev/null 2>&1 || true
  log "removed $NAME and its volumes"
}

case "${1:-}" in
  up) shift; up "$@" ;;
  env) shift; env_lines "$@" ;;
  down) down ;;
  *) echo "usage: $0 up|env [--plain|--ps]|down" >&2; exit 2 ;;
esac
