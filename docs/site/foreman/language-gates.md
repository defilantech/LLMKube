# Running Foreman on non-Go projects

Foreman's verify gate is language-configurable. By default it runs the Go
toolchain (gofmt, golangci-lint, go build, go test), because LLMKube is a Go
project. Point an AgenticTask at a `gateProfile` and the same clean-room gate
runs Python, Rust, Node, or any command set you give it. The coder loop, the
reviewer, and the dispatch model are unchanged: only the gate commands and the
container image swap.

This page covers the `gateProfile` field, the built-in language presets, the
one real gotcha (toolchain availability inside the gate image), and a verified
end-to-end example you can clone and run.

## The gate, briefly

When a coder branch is ready, Foreman runs it through a verify gate before a
reviewer ever sees it. The gate is a short-lived Kubernetes Job: it clones the
branch into a clean workspace, runs format, lint, build, and test commands in
order, checks the tree is clean afterward, and emits `GATE PASS` or
`GATE FAIL`. A failing gate stops the pipeline. Nothing about that shape is
Go-specific except the commands it runs, and those are what `gateProfile`
configures.

## The `gateProfile` field

`gateProfile` lives on `AgenticTask.spec` (and is honored by both the in-loop
fast gate and the clean-room verify Job):

```yaml
spec:
  gateProfile:
    language: python          # selects a built-in preset
    image: python:3.13        # container image the gate Job runs in
    sourceExtensions: [".py"] # extensions the scope guard treats as source
    commands:                 # overrides; empty fields keep the preset value
      format: "ruff format --check ."
      lint: "ruff check ."
      build: "python -m compileall ."
      test: "pytest -q"
```

Every field is optional. The resolver merges your profile over the selected
language preset, so you only specify what differs. An omitted or empty
`gateProfile` resolves to the Go preset, byte-for-byte identical to the gate
that shipped before this field existed.

### Built-in presets

`language` selects one of these presets. Each sets a default image, the source
extensions, and the four (or five) commands.

| Language  | Image         | Format               | Lint                  | Build                   | Test         |
|-----------|---------------|----------------------|-----------------------|-------------------------|--------------|
| `go`      | `golang:1.26` | `gofmt -l .`         | `golangci-lint run ./...` | `go build ./...`    | `go test ./...` |
| `python`  | `python:3.13` | `ruff format --check .` | `ruff check .`     | `python -m compileall .`| `pytest -q`  |
| `rust`    | `rust:1`      | `cargo fmt --check`  | `cargo clippy`        | `cargo build`           | `cargo test` |
| `node`    | `node:22`     | `prettier --check .` | `eslint .`            | (none)                  | `npm test`   |
| `generic` | (none)        | (none)               | (none)                | (none)                  | (none)       |

The `go` preset also runs a codegen check (`make manifests && make
chart-crds`) and the bite check (it confirms new tests fail against pre-change
production code). Both are Go-specific and do not run on the other presets.
Use `generic` when no preset fits: it sets nothing, so every command you want
must be set explicitly under `commands`.

Empty commands are skipped, not run as no-ops. A `node` profile, for example,
runs format, lint, and test but no build step.

## The one gotcha: tools must exist in the image

The gate Job runs your commands inside `gateProfile.image`. The preset images
are the official base images, and those are deliberately minimal: `python:3.13`
ships Python but **not** ruff or pytest; `node:22` ships Node but not prettier
or eslint. If the gate calls a tool the image does not have, the gate fails on
"command not found", not on your code.

Two ways to make the tools present:

### Option 1: install in the command (zero infrastructure)

Prepend the install to each command. Works against any stock image, costs a few
seconds per run while the package manager resolves cached wheels:

```yaml
gateProfile:
  language: python
  image: python:3.13
  commands:
    format: "pip install -q ruff && ruff format --check ."
    lint:   "pip install -q ruff && ruff check ."
    build:  "python -m compileall ."
    test:   "pip install -q pytest && pytest -q"
```

This is the right default for getting started and for the example repo below.

### Option 2: a pre-baked image (faster, you own it)

Publish an image that already has your tools (and your project's dependencies)
and point `gateProfile.image` at it. The gate runs the commands directly:

```yaml
gateProfile:
  language: python
  image: ghcr.io/your-org/your-project-ci:latest   # has ruff + pytest + deps
  commands:
    test: "pytest -q"
```

This is the right choice once the project has real dependencies, because
installing them in-command on every gate run gets slow.

## A verified example

The [`defilantech/foreman-python-example`](https://github.com/defilantech/foreman-python-example)
repo is a minimal Python project wired for exactly this. It ships a
`celsius_to_fahrenheit` function, a seeded task to add its
`fahrenheit_to_celsius` companion, and an `AgenticTask` with the Python
`gateProfile` using the install-in-command approach so it runs against stock
`python:3.13` with no image to build.

Running the gate against a clean branch produces:

```
=== clone defilantech/foreman-python-example @ gate-pass-demo ===
=== pip install -q ruff && ruff format --check . ===
2 files already formatted
=== pip install -q ruff && ruff check . ===
All checks passed!
=== python -m compileall src tests ===
Compiling 'src/temperature.py'...
Compiling 'tests/test_temperature.py'...
=== pip install -q pytest && pytest -q ===
.....                                                                    [100%]
5 passed in 0.01s
GATE PASS
```

And against a branch with a real bug (a `fahrenheit_to_celsius` that forgets to
subtract 32), the same gate catches it:

```
=== pip install -q pytest && pytest -q ===
...FF                                                                    [100%]
    def test_fahrenheit_freezing():
>       assert fahrenheit_to_celsius(32.0) == 0.0
E       assert 17.77777777777778 == 0.0
2 failed, 3 passed in 0.01s
GATE FAIL
```

Same harness, same gate Job, Python commands. The gate passes correct code and
fails buggy code, which is the whole point.

## See also

- [`defilantech/foreman-python-example`](https://github.com/defilantech/foreman-python-example): clone-and-run sample project for this page.
- [Foreman overview](/docs/foreman): the CRDs and the coder / verifier / reviewer pipeline.
- [M4 verifier runbook](./runbook-m4): standing up the verify gate on a node.
