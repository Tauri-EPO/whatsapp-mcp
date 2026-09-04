#!/usr/bin/env bash
# Hot backup / restore of the compose store volume (session, messages.db,
# media, .bridge-token) without stopping the bridge.
#
#   scripts/backup.sh backup  [DEST_DIR]        # default ./backups/<UTC timestamp>
#   scripts/backup.sh restore SRC_DIR           # stack must be stopped first
#   scripts/backup.sh prune   [DIR] [KEEP=7]    # keep the newest KEEP snapshots
#
# backup runs a throwaway alpine container with the sqlite3 CLI: each database
# is copied with `.backup` (a consistent snapshot even while the bridge writes
# in WAL mode), the media tree is tarred, and .bridge-token is copied. The
# result is a plain directory:
#
#   messages.db  whatsapp.db  [notes.db]  media.tar  bridge-token  MANIFEST
#
# The backup contains live credentials (whatsapp.db holds the WhatsApp session
# keys, bridge-token the REST bearer). Store it like a password: encrypted at
# rest, not in a shared drive. Restoring whatsapp.db on a second machine while
# the first still runs makes WhatsApp replace the stream on both — restore
# onto one host only.
#
# Env: COMPOSE_PROJECT_NAME (default: directory name, as compose does),
#      WHATSAPP_STORE_VOLUME (override the resolved volume name),
#      BACKUP_IMAGE (default alpine:3.22).
set -euo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null || dirname "$0")/."
IMAGE="${BACKUP_IMAGE:-alpine:3.22}"

usage() { sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit 1; }

resolve_volume() {
  if [ -n "${WHATSAPP_STORE_VOLUME:-}" ]; then echo "$WHATSAPP_STORE_VOLUME"; return; fi
  local project="${COMPOSE_PROJECT_NAME:-$(basename "$PWD" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9_-\n' '_')}"
  local vol
  vol="$(docker volume ls -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.volume=whatsapp-store" | head -n1)"
  if [ -z "$vol" ]; then
    echo "store volume for project '$project' not found; set WHATSAPP_STORE_VOLUME (docker volume ls)" >&2
    exit 2
  fi
  echo "$vol"
}

# Host path -> absolute, Docker-friendly (Git Bash on Windows keeps /c/... which
# docker handles; MSYS_NO_PATHCONV stops the shell from rewriting /store).
abs() { mkdir -p "$1"; (cd "$1" && pwd -W 2>/dev/null || pwd); }

run_alpine() { # $1 volume  $2 host dir  $3 script  (store mounted rw so sqlite can read WAL/-shm)
  MSYS_NO_PATHCONV=1 docker run --rm -v "$1:/store" -v "$2:/backup" "$IMAGE" sh -euc \
    "apk add --no-cache sqlite >/dev/null; $3"
}

cmd_backup() {
  local vol dest
  vol="$(resolve_volume)"
  dest="$(abs "${1:-backups/$(date -u +%Y%m%dT%H%M%SZ)}")"
  echo "backing up volume $vol -> $dest"
  run_alpine "$vol" "$dest" '
    for db in messages whatsapp notes; do
      [ -f /store/$db.db ] || continue
      sqlite3 /store/$db.db ".backup /backup/$db.db"
      sqlite3 /backup/$db.db "PRAGMA integrity_check" | grep -qx ok || { echo "$db.db backup failed integrity_check" >&2; exit 3; }
    done
    # Media lives in per-chat directories under the store root.
    cd /store && rm -f /backup/media.tar && find . -mindepth 1 -maxdepth 1 -type d -exec tar -cf /backup/media.tar {} +
    [ -f /store/.bridge-token ] && cp /store/.bridge-token /backup/bridge-token && chmod 600 /backup/bridge-token
    { echo "created_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ)"; echo "volume='"$vol"'"; ls -l /backup | tail -n +2; } > /backup/MANIFEST
    du -sh /backup | cut -f1 | sed "s/^/size: /"'
  echo "done: $dest"
}

cmd_restore() {
  [ $# -ge 1 ] && [ -d "$1" ] || usage
  local vol src
  vol="$(resolve_volume)"
  src="$(abs "$1")"
  if docker ps -q --filter "label=com.docker.compose.service=bridge" --filter "volume=$vol" | grep -q .; then
    echo "bridge is running on $vol; run 'docker compose stop' first" >&2
    exit 4
  fi
  echo "restoring $src -> volume $vol"
  run_alpine "$vol" "$src" '
    cd /store
    rm -f ./*.db-wal ./*.db-shm .bridge.lock
    for db in messages whatsapp notes; do
      [ -f /backup/$db.db ] && cp /backup/$db.db ./$db.db
    done
    [ -f /backup/media.tar ] && tar -xf /backup/media.tar -C /store
    [ -f /backup/bridge-token ] && cp /backup/bridge-token .bridge-token && chmod 600 .bridge-token
    chown -R 1000:1000 /store'
  echo "done; start with: docker compose up -d"
}

cmd_prune() {
  local dir keep
  dir="${1:-backups}"; keep="${2:-7}"
  [ -d "$dir" ] || exit 0
  ls -1d "$dir"/*/ 2>/dev/null | sort | head -n "-$keep" | while read -r old; do
    echo "removing $old"; rm -rf "$old"
  done
}

case "${1:-}" in
  backup)  shift; cmd_backup "$@" ;;
  restore) shift; cmd_restore "$@" ;;
  prune)   shift; cmd_prune "$@" ;;
  *) usage ;;
esac
