# hailo-sim

The real [hailo-ollama](https://github.com/hailo-ai/hailo_model_zoo_genai)
5.1.1 server, compiled against HailoRT 5.1.1's public headers and linked to
`fake_hailort.cpp`, a stand-in `libhailort` that emulates a Hailo-10H. The
HTTP layer, chat templates, model store, pull cache and model swapping are
hailo-ollama's own code; only the device is fake. Files are laid out as
Hailo's `hailo_gen_ai_model_zoo_5.1.1_arm64.deb` lays them out.

5.1.1 is the version Raspberry Pi's apt repository ships for the AI HAT+ 2,
the Hailo-10H board this was first tested against.

```sh
make docker-build-hailo-sim
docker run --rm -d -p 8000:8000 --name hailo-sim \
  localhost/llmkube-hailo-sim:dev
hack/hailo-probe.sh http://127.0.0.1:8000
docker rm -f hailo-sim
```

`HAILO_SIM_PLATFORM` (default the host's architecture) picks the image
platform. Requests with a body need `Content-Type: application/json`;
hailo-ollama answers 500 without it. The image runs as any user, for
example `--user 65534:65534`. Both listen variables take `host:port`; the
container exits with an error on anything else.

| Variable | Default | Effect |
|---|---|---|
| `HAILO_SIM_LISTEN` | `0.0.0.0:8000` (the packaged config) | hailo-ollama's listen address, written into a copy of `hailo-ollama.json` under a new `XDG_CONFIG_HOME` |
| `HAILO_SIM_LIBRARY_LISTEN` | `127.0.0.1:8001` | Serves the seeded HEFs at `/blob/sha256_<digest>` for HEAD requests, as `dev-public.hailo.ai` does; empty turns it off. The container exits if the server can't start |
| `HAILO_SIM_DEVICE` | `1` | `0`: VDevice creation fails, as with no Hailo device |
| `HAILO_SIM_DEVICE_MEMORY` | 8 GiB | A HEF larger than this fails to load |
| `HAILO_SIM_TOKEN_DELAY_MS` | `0` | Delay per generated token |

Build argument `HAILO_SIM_HEF_SIZE` (default 64 MiB) sets the size of each
fake HEF. The build writes one per installed manifest, points the manifest's
`hef_h10h` at its digest and seeds it in the blob cache, so `/api/pull` is a
cache hit and never downloads. Generation is deterministic: `hailo-sim:`
followed by the last user message.

The library server on `HAILO_SIM_LIBRARY_LISTEN` is for the agent's
`--hailo-library-url` size check, which HEADs a `hailo://` model's HEF
before anything is pulled. hailo-ollama itself still points at
`dev-public.hailo.ai`; its pulls never leave the seeded cache.

Not emulated: real token output, device memory used by the KV cache,
HailoRT's exact error codes for a HEF that does not fit, and kernels with
16K pages.
