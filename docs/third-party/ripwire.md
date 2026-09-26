# Third-party: ripwire

LLMKube's Foreman add-on ships the [ripwire](https://github.com/redhat-et/ripwire)
binary to give coder agents a call-graph repo-map and interactive code-graph
queries. This page records what is bundled and under what terms.

## What is bundled

| Artifact | Where | Channel |
|---|---|---|
| `ripwire` binary, linux amd64/arm64 | `/usr/local/bin/ripwire` | Foreman coder image (`Dockerfile.foreman-agent-builder`) |
| `ripwire` binary, macOS arm64 | `<install root>/ripwire` | `make install-foreman-agent` (`Makefile`) |
| ripwire `LICENSE` | `/licenses/ripwire/LICENSE` | Foreman coder image |

Pinned by version and by the release's own per-arch SHA-256, fetched at build
time. A swapped or corrupted asset fails the build.

## License

ripwire is **Apache-2.0** (`LICENSE`, "Copyright 2026 David Brewster"),
compatible with LLMKube's own Apache-2.0. Per Apache-2.0 §4, the binary
redistributed in the coder image ships its `LICENSE`; retain and reproduce it
with any redistribution.

ripwire vendors its parsers and helper libraries rather than downloading them,
so its full license set is recorded in its own
[`THIRD_PARTY.md`](https://github.com/redhat-et/ripwire/blob/main/THIRD_PARTY.md).
All entries are permissive:

- MIT: unordered_dense, svector, the tree-sitter grammars (C/C++, CUDA, Python,
  Go, Rust, Java, JavaScript, TypeScript, Ruby, Bash, C#, JSON, TOML, YAML,
  Objective-C, Swift, PHP, Lua, Markdown, Kotlin, GDScript), doctest, and the
  libuv-derived Windows logic.
- Apache-2.0: the gtl headers.
- zlib: pdqsort.
- Unicode-DFS-2016: the ICU subset under tree-sitter.

No copyleft terms are involved.

## Trademarks

Apache-2.0 §6 grants no trademark rights. ripwire is a project of `redhat-et`;
LLMKube is not affiliated with or endorsed by it. Refer to it factually.

## Updating the pin

1. Pick the release, update `RIPWIRE_VERSION` and the per-arch `RIPWIRE_SHA256`
   in `Dockerfile.foreman-agent-builder` (and `Makefile` for the macOS
   channel), reading the checksums from the release's own `.sha256` assets.
2. Bump the version in this page.
3. The image build's checksum step and `ripwire --version` smoke-test fail loud
   on a bad pin.
