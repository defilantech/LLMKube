#!/usr/bin/env bash
# Preflight floor gate for the B200 validation matrix.
#
# Parses the chart's platformFloors and asserts the live node meets each floor
# before any GPU row runs, so a row cannot silently "pass" on a node whose
# platform is below the documented floor. Sourced after lib/common.sh.

# Path to the floors file. Defaults to the shipped chart values.
: "${PF_FLOORS_FILE:=$(b200_repo_root)/charts/llmkube/values.yaml}"

# Comparison floors, in the order they are asserted. fabricManager is not a
# comparison floor and is checked separately as an exact match.
PF_FLOOR_KEYS="nvidiaDriver cudaToolkit nccl gpuOperator k8sDevicePlugin dcgmExporter"

# pf_floor_value <floors-file> <key> prints the raw floor string, e.g.
# ">= 580.173.02 (R580 LTS; R570 is EOL)".
pf_floor_value() {
  local file="$1" key="$2" line val
  [ -f "$file" ] || return 1
  while IFS= read -r line; do
    case "$line" in
      "  $key:"*)
        val="${line#*: }"
        val="${val%\"}"
        val="${val#\"}"
        printf '%s\n' "$val"
        return 0
        ;;
    esac
  done < "$file"
  return 1
}

# pf_measured_value <measured-file> <key> prints "key=value" lines' value.
pf_measured_value() {
  local file="$1" key="$2" line
  [ -f "$file" ] || return 1
  while IFS= read -r line; do
    case "$line" in
      "#"*|"") continue ;;
      "$key="*) printf '%s\n' "${line#*=}"; return 0 ;;
    esac
  done < "$file"
  return 1
}

# pf_parse_floor <raw> sets PF_OP and PF_VER from a floor string.
pf_parse_floor() {
  local raw="$1"
  PF_OP="${raw%% *}"
  PF_VER="${raw#* }"
  PF_VER="${PF_VER%% *}"
}

# pf_version_ge <have> <need>: true when have >= need, component-wise. A
# suffix after a dash or space (e.g. "4.6.0-4.8.3") is ignored for the minimum.
pf_version_ge() {
  local have="$1" need="$2"
  have="${have#v}"
  need="${need#v}"
  have="${have%%-*}"
  have="${have%% *}"
  need="${need%%-*}"
  need="${need%% *}"
  [ -n "$have" ] && [ -n "$need" ] || return 1

  local -a h n
  IFS=. read -r -a h <<< "$have"
  IFS=. read -r -a n <<< "$need"

  local i hi ni
  for ((i = 0; i < ${#n[@]}; i++)); do
    hi="${h[i]:-0}"
    ni="${n[i]:-0}"
    case "$hi" in ''|*[!0-9]*) hi=0 ;; esac
    case "$ni" in ''|*[!0-9]*) ni=0 ;; esac
    if [ "$hi" -gt "$ni" ]; then return 0; fi
    if [ "$hi" -lt "$ni" ]; then return 1; fi
  done
  return 0
}

# pf_floor_satisfied <raw-floor> <measured>: true when the measured value meets
# the floor. Only ">=" comparison floors are supported.
pf_floor_satisfied() {
  local raw="$1" measured="$2"
  pf_parse_floor "$raw"
  case "$PF_OP" in
    ">=") pf_version_ge "$measured" "$PF_VER" ;;
    *) return 2 ;;
  esac
}

# pf_assert_floors <floors-file> <measured-file> asserts every floor and the
# Fabric Manager exact match. Prints a PREFLIGHT line per check and returns 1
# when any floor fails or could not be read.
pf_assert_floors() {
  local floors="$1" measured="$2"
  local key raw meas rc=0

  for key in $PF_FLOOR_KEYS; do
    raw="$(pf_floor_value "$floors" "$key")"
    if [ -z "$raw" ]; then
      b200_log "PREFLIGHT FAIL: $key has no floor in $floors"
      rc=1
      continue
    fi
    meas="$(pf_measured_value "$measured" "$key")"
    if [ -z "$meas" ] || [ "$meas" = "unknown" ]; then
      b200_log "PREFLIGHT UNVERIFIED: $key could not be read (floor: $raw)"
      rc=1
      continue
    fi
    pf_parse_floor "$raw"
    if [ "$PF_OP" != ">=" ]; then
      b200_log "PREFLIGHT SKIP: $key floor form unsupported: $raw"
      continue
    fi
    if pf_floor_satisfied "$raw" "$meas"; then
      b200_log "PREFLIGHT OK: $key $meas >= $PF_VER"
    else
      b200_log "PREFLIGHT FAIL: $key $meas below >= $PF_VER"
      rc=1
    fi
  done

  local drv fm
  drv="$(pf_measured_value "$measured" nvidiaDriver)"
  fm="$(pf_measured_value "$measured" fabricManager)"
  if [ -z "$fm" ] || [ "$fm" = "unknown" ]; then
    b200_log "PREFLIGHT UNVERIFIED: fabricManager could not be read"
    rc=1
  elif [ "$fm" = "$drv" ]; then
    b200_log "PREFLIGHT OK: fabricManager $fm matches driver $drv"
  else
    b200_log "PREFLIGHT FAIL: fabricManager $fm does not match driver $drv"
    rc=1
  fi

  return $rc
}

# pf_emit writes one measured key, defaulting to "unknown" so an absent
# reading is an explicit unverified floor rather than a silent pass.
pf_emit() {
  local out="$1" key="$2" val="$3"
  [ -n "$val" ] || val="unknown"
  printf '%s=%s\n' "$key" "$val" >> "$out"
}

# pf_collect_live <out-file> best-effort populates the measured platform
# versions of the host. Anything it cannot read stays "unknown" and is
# reported as unverified by pf_assert_floors. Env overrides exist for the
# readings the container cannot reach (B200_DRIVER_VERSION, B200_CUDA_TOOLKIT,
# B200_NCCL, B200_GPU_OPERATOR, B200_DEVICE_PLUGIN, B200_DCGM_EXPORTER,
# B200_FABRIC_MANAGER).
pf_collect_live() {
  local out="$1"
  : > "$out"

  local drv="${B200_DRIVER_VERSION:-}"
  if [ -z "$drv" ] && command -v nvidia-smi >/dev/null 2>&1; then
    drv="$(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | head -n1)"
  fi

  pf_emit "$out" nvidiaDriver "$drv"
  pf_emit "$out" cudaToolkit "${B200_CUDA_TOOLKIT:-}"
  pf_emit "$out" nccl "${B200_NCCL:-}"
  pf_emit "$out" gpuOperator "${B200_GPU_OPERATOR:-}"
  pf_emit "$out" k8sDevicePlugin "${B200_DEVICE_PLUGIN:-}"
  pf_emit "$out" dcgmExporter "${B200_DCGM_EXPORTER:-}"
  pf_emit "$out" fabricManager "${B200_FABRIC_MANAGER:-}"
}

# pf_preflight_live asserts the floors against this host's readings.
pf_preflight_live() {
  local tmp rc=0
  tmp="$(mktemp)"
  pf_collect_live "$tmp"
  b200_log "preflight: floors from $PF_FLOORS_FILE, readings from $tmp"
  pf_assert_floors "$PF_FLOORS_FILE" "$tmp" || rc=$?
  rm -f "$tmp"
  return $rc
}

# pf_preflight_selfcheck validates the gate itself against fixtures, so the
# harness wiring can be checked without hardware.
pf_preflight_selfcheck() {
  local fx="$B200_FIXTURES" rc=0

  if pf_assert_floors "$fx/floors.yaml" "$fx/measured-ok.kv" >/dev/null 2>&1; then
    b200_log "preflight self-check: a compliant node is accepted"
  else
    b200_log "preflight self-check FAILED: a compliant node was rejected"
    rc=1
  fi

  if pf_assert_floors "$fx/floors.yaml" "$fx/measured-below.kv" >/dev/null 2>&1; then
    b200_log "preflight self-check FAILED: a below-floor node was accepted"
    rc=1
  else
    b200_log "preflight self-check: a below-floor node is rejected"
  fi

  if pf_assert_floors "$fx/floors.yaml" "$fx/measured-fm-mismatch.kv" >/dev/null 2>&1; then
    b200_log "preflight self-check FAILED: a fabric-manager mismatch was accepted"
    rc=1
  else
    b200_log "preflight self-check: a fabric-manager mismatch is rejected"
  fi

  return $rc
}
