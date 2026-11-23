#!/bin/sh
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

# Starts hailo-ollama 5.1.1 on the simulated Hailo-10H, as any user.
#
#   HAILO_SIM_LISTEN=host:port          hailo-ollama's server address. 5.1.1 reads
#                                       it only from hailo-ollama.json and
#                                       searches XDG_CONFIG_HOME before /etc/xdg,
#                                       so an edited copy goes under a new
#                                       XDG_CONFIG_HOME. Default: the packaged
#                                       0.0.0.0:8000.
#   HAILO_SIM_LIBRARY_LISTEN=host:port  Serves the seeded HEFs at
#                                       /blob/sha256_<digest>, as dev-public.hailo.ai
#                                       does, for HEAD requests. Default
#                                       127.0.0.1:8001; empty turns it off. The
#                                       container exits if it does not come up.
set -eu

die() {
	echo "hailo-sim: $*" >&2
	exit 1
}

# split_hostport NAME VALUE sets host and port from VALUE, or exits.
split_hostport() {
	case $2 in
	*:*) ;;
	*) die "$1=$2: want host:port" ;;
	esac
	host=${2%:*}
	port=${2##*:}
	case $host in
	'' | *[!A-Za-z0-9._:-]*) die "$1=$2: bad host '$host'" ;;
	esac
	case $port in
	'' | *[!0-9]*) die "$1=$2: bad port '$port'" ;;
	esac
	if [ "${#port}" -gt 5 ] || [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
		die "$1=$2: port must be 1-65535"
	fi
}

if [ -n "${HAILO_SIM_LISTEN:-}" ]; then
	split_hostport HAILO_SIM_LISTEN "$HAILO_SIM_LISTEN"
	XDG_CONFIG_HOME=$(mktemp -d)
	export XDG_CONFIG_HOME
	mkdir "$XDG_CONFIG_HOME/hailo-ollama"
	sed -E "/\"server\"/,/\}/ { s/\"host\": *\"[^\"]*\"/\"host\": \"$host\"/; s/\"port\": *[0-9]+/\"port\": $port/ }" \
		/etc/xdg/hailo-ollama/hailo-ollama.json >"$XDG_CONFIG_HOME/hailo-ollama/hailo-ollama.json"
fi

library=${HAILO_SIM_LIBRARY_LISTEN-127.0.0.1:8001}
if [ -n "$library" ]; then
	split_hostport HAILO_SIM_LIBRARY_LISTEN "$library"
	ready=$(mktemp)
	# Writes $ready only once its own bind succeeds; no SO_REUSEPORT, so a port
	# another process holds fails the bind.
	python3 -c '
import functools, http.server, socket, sys
host, port, ready = sys.argv[1], int(sys.argv[2]), sys.argv[3]
class Server(http.server.ThreadingHTTPServer):
    address_family = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)[0][0]
    allow_reuse_port = False
handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory="/srv/hailo-library")
server = Server((host, port), handler)
with open(ready, "w") as f:
    f.write("ok")
server.serve_forever()
' "$host" "$port" "$ready" >/dev/null &
	pid=$!
	tries=0
	until [ -s "$ready" ]; do
		kill -0 "$pid" 2>/dev/null || die "library server on $library exited at startup"
		tries=$((tries + 1))
		[ "$tries" -lt 100 ] || die "library server on $library not listening after 10s"
		sleep 0.1
	done
	rm -f "$ready"
fi

exec /usr/bin/hailo-ollama "$@"
