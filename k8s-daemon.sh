#!/usr/bin/env bash
set -euo pipefail

INTERVAL="5"
OUTFILE="k8s-stats.csv"
NAMESPACE=""
SELECTOR=""
KUBE_CONTEXT=""

usage() {
  cat <<'EOF'
Usage:
  k8s-daemon.sh [INTERVAL] [OUTFILE] [NAMESPACE]
  k8s-daemon.sh [--interval N] [--outfile FILE] [--namespace NS] [--selector LABELS] [--context NAME]

Options:
  -i, --interval    Polling interval in seconds (default: 5)
  -o, --outfile     CSV output file (default: k8s-stats.csv)
  -n, --namespace   Namespace filter (default: all namespaces)
  -l, --selector    Label selector, e.g. app=my-service
      --context     kubectl context name
  -h, --help        Show this help
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
    -n|--namespace)
      [ $# -ge 2 ] || { echo "Missing value for $1" >&2; exit 1; }
      NAMESPACE="$2"
      shift 2
      ;;
    -l|--selector)
      [ $# -ge 2 ] || { echo "Missing value for $1" >&2; exit 1; }
      SELECTOR="$2"
      shift 2
      ;;
    --context)
      [ $# -ge 2 ] || { echo "Missing value for $1" >&2; exit 1; }
      KUBE_CONTEXT="$2"
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

# Backward-compatible positional args: INTERVAL OUTFILE NAMESPACE
if [ "${#positional[@]}" -ge 1 ]; then
  INTERVAL="${positional[0]}"
fi
if [ "${#positional[@]}" -ge 2 ]; then
  OUTFILE="${positional[1]}"
fi
if [ "${#positional[@]}" -ge 3 ]; then
  NAMESPACE="${positional[2]}"
fi

if ! [[ "$INTERVAL" =~ ^[0-9]+([.][0-9]+)?$ ]]; then
  echo "INTERVAL must be a positive number, got: $INTERVAL" >&2
  exit 1
fi

if ! awk "BEGIN { exit !($INTERVAL > 0) }"; then
  echo "INTERVAL must be greater than 0, got: $INTERVAL" >&2
  exit 1
fi

if ! command -v kubectl >/dev/null 2>&1; then
  echo "kubectl not found in PATH" >&2
  exit 1
fi

header="timestamp,container,cpu_pct,mem_usage_mb,mem_limit_mb,mem_pct"
if [ ! -f "$OUTFILE" ]; then
  echo "$header" > "$OUTFILE"
fi

KUBECTL=(kubectl)
if [ -n "$KUBE_CONTEXT" ]; then
  KUBECTL+=(--context "$KUBE_CONTEXT")
fi

echo "Logging Kubernetes pod stats every ${INTERVAL}s -> ${OUTFILE}"
if [ -n "$NAMESPACE" ]; then
  echo "Namespace filter: ${NAMESPACE}"
fi
if [ -n "$SELECTOR" ]; then
  echo "Label selector: ${SELECTOR}"
fi
if [ -n "$KUBE_CONTEXT" ]; then
  echo "Context: ${KUBE_CONTEXT}"
fi
echo "Press Ctrl+C to stop"

collect_limits() {
  local jsonpath
  local cmd
  jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{range .spec.containers[*]}{.resources.limits.cpu}{";"}{end}{"\t"}{range .spec.containers[*]}{.resources.limits.memory}{";"}{end}{"\n"}{end}'
  cmd=("${KUBECTL[@]}" get pods -A)
  if [ -n "$SELECTOR" ]; then
    cmd+=(-l "$SELECTOR")
  fi
  cmd+=(-o "jsonpath=${jsonpath}")
  "${cmd[@]}" 2>/dev/null || true
}

collect_top() {
  local cmd
  cmd=("${KUBECTL[@]}" top pods -A --no-headers)
  if [ -n "$SELECTOR" ]; then
    cmd+=(-l "$SELECTOR")
  fi
  "${cmd[@]}" 2>/dev/null || true
}

limits_tmp=""
top_tmp=""
cleanup() {
  if [ -n "${limits_tmp:-}" ] && [ -f "$limits_tmp" ]; then
    rm -f "$limits_tmp"
  fi
  if [ -n "${top_tmp:-}" ] && [ -f "$top_tmp" ]; then
    rm -f "$top_tmp"
  fi
}
trap cleanup EXIT INT TERM

warned_no_metrics=0

while true; do
  ts=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

  limits_tmp=$(mktemp)
  top_tmp=$(mktemp)
  collect_limits > "$limits_tmp"
  collect_top > "$top_tmp"

  if [ ! -s "$top_tmp" ]; then
    if [ "$warned_no_metrics" -eq 0 ]; then
      echo "No data from 'kubectl top pods'. Make sure metrics-server is installed and pods are running." >&2
      warned_no_metrics=1
    fi
    cleanup
    sleep "$INTERVAL"
    continue
  fi
  warned_no_metrics=0

  awk -v ts="$ts" -v ns_filter="$NAMESPACE" '
    function trim(s) {
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", s)
      return s
    }
    function to_cpu_m(q) {
      q = trim(q)
      if (q == "") return 0
      if (q ~ /m$/) { sub(/m$/, "", q); return q + 0 }
      if (q ~ /u$/) { sub(/u$/, "", q); return (q + 0) / 1000 }
      if (q ~ /n$/) { sub(/n$/, "", q); return (q + 0) / 1000000 }
      return (q + 0) * 1000
    }
    function to_mem_mb(q, val, unit) {
      q = trim(q)
      if (q == "") return 0
      if (q !~ /^[0-9]+([.][0-9]+)?[A-Za-z]*$/) return 0

      unit = q
      sub(/^[0-9]+([.][0-9]+)?/, "", unit)
      val = q
      sub(/[A-Za-z]*$/, "", val)
      val = val + 0

      if (unit == "" || unit == "B") return val / 1048576
      if (unit == "Ki") return val / 1024
      if (unit == "Mi") return val
      if (unit == "Gi") return val * 1024
      if (unit == "Ti") return val * 1024 * 1024
      if (unit == "Pi") return val * 1024 * 1024 * 1024
      if (unit == "Ei") return val * 1024 * 1024 * 1024 * 1024

      if (unit == "K") return val * 1000 / 1048576
      if (unit == "M") return val * 1000000 / 1048576
      if (unit == "G") return val * 1000000000 / 1048576
      if (unit == "T") return val * 1000000000000 / 1048576
      if (unit == "P") return val * 1000000000000000 / 1048576
      if (unit == "E") return val * 1000000000000000000 / 1048576

      return val / 1048576
    }

    FNR == NR {
      ns = $1
      pod = $2
      if (ns == "" || pod == "") next
      if (ns_filter != "" && ns != ns_filter) next
      key = ns "/" pod

      cpu_sum = 0
      mem_sum = 0

      n = split($3, cpu_parts, ";")
      for (i = 1; i <= n; i++) cpu_sum += to_cpu_m(cpu_parts[i])

      n = split($4, mem_parts, ";")
      for (i = 1; i <= n; i++) mem_sum += to_mem_mb(mem_parts[i])

      cpu_limit[key] = cpu_sum
      mem_limit[key] = mem_sum
      next
    }

    {
      ns = $1
      pod = $2
      cpu_q = $3
      mem_q = $4
      if (ns == "" || pod == "") next
      if (ns_filter != "" && ns != ns_filter) next

      key = ns "/" pod
      cpu_used = to_cpu_m(cpu_q)
      mem_used = to_mem_mb(mem_q)
      cpu_lim = (key in cpu_limit) ? cpu_limit[key] : 0
      mem_lim = (key in mem_limit) ? mem_limit[key] : 0

      cpu_pct = (cpu_lim > 0) ? (cpu_used / cpu_lim * 100) : 0
      mem_pct = (mem_lim > 0) ? (mem_used / mem_lim * 100) : 0

      printf "%s,%s,%.2f,%.2f,%.2f,%.2f\n", ts, key, cpu_pct, mem_used, mem_lim, mem_pct
    }
  ' "$limits_tmp" "$top_tmp" >> "$OUTFILE"

  cleanup
  sleep "$INTERVAL"
done
