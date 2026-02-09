#!/usr/bin/env bash
set -euo pipefail

INTERVAL="5"
OUTFILE="docker-stats.csv"
COMPOSE_FILE=""

usage() {
  cat <<'EOF'
Usage:
  daemon.sh [INTERVAL] [OUTFILE] [COMPOSE_FILE]
  daemon.sh [--interval N] [--outfile FILE] [--compose-file PATH]

Options:
  -i, --interval      Polling interval in seconds (default: 5)
  -o, --outfile       CSV output file (default: docker-stats.csv)
  -c, --compose-file  Path to docker-compose file. If set, logs only containers
                      from that compose project.
  -h, --help          Show this help
EOF
}

positional=()
while (($# > 0)); do
  case "$1" in
    -i|--interval)
      [ $# -ge 2 ] || { echo "Missing value for $1" >&2; exit 1; }
      INTERVAL="$2"
      shift 2
      ;;
    -o|--outfile)
      [ $# -ge 2 ] || { echo "Missing value for $1" >&2; exit 1; }
      OUTFILE="$2"
      shift 2
      ;;
    -c|--compose-file)
      [ $# -ge 2 ] || { echo "Missing value for $1" >&2; exit 1; }
      COMPOSE_FILE="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      while (($# > 0)); do
        positional+=("$1")
        shift
      done
      ;;
    -*)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 1
      ;;
    *)
      positional+=("$1")
      shift
      ;;
  esac
done

if [ "${#positional[@]}" -gt 3 ]; then
  echo "Too many positional arguments" >&2
  usage >&2
  exit 1
fi

# Backward-compatible positional args: INTERVAL OUTFILE COMPOSE_FILE
if [ "${#positional[@]}" -ge 1 ]; then
  INTERVAL="${positional[0]}"
fi
if [ "${#positional[@]}" -ge 2 ]; then
  OUTFILE="${positional[1]}"
fi
if [ "${#positional[@]}" -ge 3 ]; then
  COMPOSE_FILE="${positional[2]}"
fi

if ! [[ "$INTERVAL" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
  echo "INTERVAL must be a positive number, got: $INTERVAL" >&2
  exit 1
fi

if ! awk "BEGIN { exit !($INTERVAL > 0) }"; then
  echo "INTERVAL must be greater than 0, got: $INTERVAL" >&2
  exit 1
fi

if [ -n "$COMPOSE_FILE" ] && [ ! -f "$COMPOSE_FILE" ]; then
  echo "Compose file not found: $COMPOSE_FILE" >&2
  exit 1
fi

header="timestamp,container,cpu_pct,mem_usage_mb,mem_limit_mb,mem_pct"

if [ ! -f "$OUTFILE" ]; then
  echo "$header" > "$OUTFILE"
fi

echo "Logging docker stats every ${INTERVAL}s → ${OUTFILE}"
if [ -n "$COMPOSE_FILE" ]; then
  echo "Using compose filter: ${COMPOSE_FILE}"
fi
echo "Press Ctrl+C to stop"

collect_stats() {
  if [ -z "$COMPOSE_FILE" ]; then
    docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}},{{.MemPerc}}'
    return
  fi

  local containers=()
  local cid
  while IFS= read -r cid; do
    [ -n "$cid" ] && containers+=("$cid")
  done < <(docker compose -f "$COMPOSE_FILE" ps -q 2>/dev/null || true)

  # Avoid falling back to all containers when compose has no running services.
  if [ "${#containers[@]}" -eq 0 ]; then
    return
  fi

  docker stats --no-stream --format '{{.Name}},{{.CPUPerc}},{{.MemUsage}},{{.MemPerc}}' "${containers[@]}"
}

while true; do
  ts=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
  collect_stats | while IFS=',' read -r name cpu mem_usage mem_pct; do
    [ -n "$name" ] || continue
    cpu="${cpu//%/}"
    mem_pct="${mem_pct//%/}"
    # parse "123.4MiB / 1.5GiB" → usage_mb, limit_mb
    usage_raw=$(echo "$mem_usage" | awk -F'/' '{gsub(/^ +| +$/,"",$1); print $1}')
    limit_raw=$(echo "$mem_usage" | awk -F'/' '{gsub(/^ +| +$/,"",$2); print $2}')
    usage_mb=$(echo "$usage_raw" | awk '{
      val=$1+0;
      if (index($0,"GiB")) val=val*1024;
      else if (index($0,"KiB")) val=val/1024;
      printf "%.2f", val
    }')
    limit_mb=$(echo "$limit_raw" | awk '{
      val=$1+0;
      if (index($0,"GiB")) val=val*1024;
      else if (index($0,"KiB")) val=val/1024;
      printf "%.2f", val
    }')
    echo "${ts},${name},${cpu},${usage_mb},${limit_mb},${mem_pct}"
  done >> "$OUTFILE"
  sleep "$INTERVAL"
done
