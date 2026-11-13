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

# Real hailo-ollama 5.1.1 server built against HailoRT 5.1.1's public headers
# and a fake libhailort that emulates a Hailo-10H (AI HAT+ 2). See
# fake_hailort.cpp. Files are laid out as Hailo's
# hailo_gen_ai_model_zoo_5.1.1_arm64.deb lays them out.
FROM debian:trixie-slim AS build

ARG HAILORT_REF=1adb3306b57f0bc44166a5ecb1b87c4c116ae0e7
ARG HAILO_GENAI_REF=eb3270c386752379465ccd35c360a115eb456cba
# hailo-ollama 5.1.1 fetches oatpp's default branch; these are its heads.
ARG OATPP_REF=f83d648fd82dc222ef88aabbafb68efbd7d7bf50
ARG OATPP_OPENSSL_REF=32c6ff8b59406470dbdff6dd65a21b671052abad
# Size of each fake HEF, in bytes.
ARG HAILO_SIM_HEF_SIZE=67108864

RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates cmake g++ git libssl-dev make pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
RUN fetch() { git init -q "$1" && git -C "$1" remote add origin "$2" \
        && git -C "$1" fetch -q --depth 1 origin "$3" && git -C "$1" checkout -q FETCH_HEAD; } \
    && fetch genai https://github.com/hailo-ai/hailo_model_zoo_genai.git "$HAILO_GENAI_REF" \
    && fetch oatpp https://github.com/oatpp/oatpp.git "$OATPP_REF" \
    && fetch oatpp-openssl https://github.com/oatpp/oatpp-openssl.git "$OATPP_OPENSSL_REF" \
    && git init -q hailort && cd hailort \
    && git remote add origin https://github.com/hailo-ai/hailort.git \
    && git fetch -q --depth 1 --filter=blob:none origin "$HAILORT_REF" \
    && git sparse-checkout set hailort/libhailort/include \
    && git checkout -q FETCH_HEAD

COPY fake_hailort.cpp cmake/ /src/sim/
RUN mkdir -p /opt/hailort/lib/cmake/HailoRT \
    && cp -r hailort/hailort/libhailort/include /opt/hailort/include \
    && cp -r /opt/hailort/include/hailo /usr/local/include/ \
    && g++ -std=c++17 -O2 -fPIC -shared -fvisibility=hidden -Wl,-soname,libhailort.so.5.1.1 \
        -I/opt/hailort/include -o /opt/hailort/lib/libhailort.so.5.1.1 sim/fake_hailort.cpp \
    && ln -s libhailort.so.5.1.1 /opt/hailort/lib/libhailort.so \
    && cp sim/HailoRTConfig.cmake sim/HailoRTConfigVersion.cmake /opt/hailort/lib/cmake/HailoRT/

RUN cmake -S genai -B build -DCMAKE_BUILD_TYPE=Release -DCMAKE_PREFIX_PATH=/opt/hailort \
        -DFETCHCONTENT_SOURCE_DIR_OATPP=/src/oatpp -DFETCHCONTENT_SOURCE_DIR_OATPP-OPENSSL=/src/oatpp-openssl \
    && cmake --build build -j"$(nproc)" --target hailo-ollama

# hailo-ollama 5.1.1 checks a cached HEF against its manifest's hef_h10h, so
# each manifest gets the digest of a fake HEF seeded in the blob cache.
RUN models=/out/usr/share/hailo-ollama/models \
    && mkdir -p /out/usr/bin /out/etc/xdg/hailo-ollama "$models/blob" \
    && cp build/src/apps/server/hailo-ollama /out/usr/bin/ \
    && cp genai/config/hailo-ollama.json /out/etc/xdg/hailo-ollama/ \
    && cp -r genai/models/manifests "$models/" \
    && for m in "$models"/manifests/*/*/manifest.json; do \
        hef=$(mktemp) \
        && { printf 'hailo-sim %s\n' "$m"; head -c "$HAILO_SIM_HEF_SIZE" /dev/zero; } \
            | head -c "$HAILO_SIM_HEF_SIZE" >"$hef" \
        && digest=$(sha256sum "$hef" | cut -d' ' -f1) \
        && mv "$hef" "$models/blob/sha256_$digest" \
        && sed -i -E "s/\"hef_h10h\": *\"[0-9a-f]{64}\"/\"hef_h10h\": \"$digest\"/" "$m" \
        && grep -q "$digest" "$m"; \
    done \
    && chmod -R a+rwX "$models"

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libssl3t64 python3 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ /
COPY --from=build /opt/hailort/lib/libhailort.so.5.1.1 /usr/lib/
RUN ldconfig \
    && mkdir -p /srv/hailo-library \
    && ln -s /usr/share/hailo-ollama/models/blob /srv/hailo-library/blob
COPY entrypoint.sh /usr/local/bin/hailo-sim-entrypoint
EXPOSE 8000
ENTRYPOINT ["/usr/local/bin/hailo-sim-entrypoint"]
