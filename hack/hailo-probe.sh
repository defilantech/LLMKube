#!/usr/bin/env bash
# Copyright 2026 The LLMKube Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Probe a hailo-ollama server for every route the metal-agent's Ollama
# executor and ingress allowlist use, against hailo-ollama on a host with a
# Hailo-10H or against test/hailosim. The host section describes the machine
# the script runs on.
#
# Usage: hack/hailo-probe.sh [base-url] [model]
#   base-url  default http://127.0.0.1:8000
#   model     default qwen2.5-instruct:1.5b
set -u

BASE="${1:-http://127.0.0.1:8000}"
MODEL="${2:-qwen2.5-instruct:1.5b}"

section() { printf '\n== %s\n' "$1"; }

section "host"
printf 'model:     %s\n' "$(tr -d '\0' 2>/dev/null </proc/device-tree/model || echo unknown)"
printf 'kernel:    %s\n' "$(uname -r)"
printf 'pagesize:  %s\n' "$(getconf PAGESIZE)"
printf 'memtotal:  %s\n' "$(awk '/^MemTotal/{print $2" kB"}' /proc/meminfo 2>/dev/null)"
printf 'hailo dev: %s\n' "$(find /dev -maxdepth 1 -name 'hailo*' 2>/dev/null | tr '\n' ' ')"
command -v lspci >/dev/null && lspci | grep -i hailo
command -v hailortcli >/dev/null && hailortcli fw-control identify 2>&1 | head -8

# probe METHOD PATH [JSON-BODY] [TIMEOUT]
# Prints the HTTP status (000 when there is no response), then curl's error
# if it reported one, else the start of the body.
probe() {
	local method="$1" path="$2" body="${3:-}" timeout="${4:-15}" out err code detail
	out="$(mktemp)"
	err="$(mktemp)"
	local args=(-sS -o "$out" -w '%{http_code}' -m "$timeout" -X "$method")
	if [ -n "$body" ]; then
		args+=(-H 'Content-Type: application/json' -d "$body")
	fi
	code=$(curl "${args[@]}" "$BASE$path" 2>"$err")
	if [ -s "$err" ]; then
		detail="$(tr '\n' ' ' <"$err")"
	else
		detail="$(head -c 160 "$out" | tr '\n' ' ')"
	fi
	printf '%-5s %-24s %s  %s\n' "$method" "$path" "${code:-000}" "$detail"
	rm -f "$out" "$err"
}

section "used by OllamaExecutor (pkg/agent/executor_ollama.go)"
probe GET / ""
probe POST /api/pull "{\"model\":\"$MODEL\",\"stream\":false}" 900
probe POST /api/generate "{\"model\":\"$MODEL\",\"prompt\":\"\"}" 120
probe GET /api/ps ""
probe POST /api/generate "{\"model\":\"$MODEL\",\"prompt\":\"\",\"keep_alive\":0}" 30

section "ollama routes the ingress allows (pkg/agent/ingress/paths.go)"
probe GET /api/version ""
probe GET /api/tags ""
probe POST /api/show "{\"model\":\"$MODEL\"}"
probe POST /api/chat "{\"model\":\"$MODEL\",\"stream\":false,\"messages\":[{\"role\":\"user\",\"content\":\"Say hi\"}]}" 120
probe POST /api/embed "{\"model\":\"$MODEL\",\"input\":\"hello\"}" 120
probe POST /api/embeddings "{\"model\":\"$MODEL\",\"prompt\":\"hello\"}" 120

section "routes the ingress allows for every runtime (pkg/agent/ingress/paths.go)"
probe GET /health ""
probe GET /metrics ""
probe GET /v1/models ""
probe GET "/v1/models/$MODEL" ""
probe POST /v1/chat/completions "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hi\"}]}" 120
probe POST /v1/completions "{\"model\":\"$MODEL\",\"prompt\":\"Say hi\",\"max_tokens\":16}" 120
probe POST /v1/embeddings "{\"model\":\"$MODEL\",\"input\":\"hello\"}" 120
probe POST /v1/responses "{\"model\":\"$MODEL\",\"input\":\"Say hi\"}" 120

section "hailo-specific"
probe GET /hailo/v1/list ""
