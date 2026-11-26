# flake-ledger fixtures

Recorded from `defilantech/LLMKube` on 2026-11-03 with `gh api`, then trimmed
to the fields and lines the ledger reads. Nothing in them is invented except
where noted.

| File | Source |
|------|--------|
| `runs-test.yml.json`, `runs-test-e2e.yml.json` | `actions/runs/{id}` for the runs below, as one `workflows/{file}/runs` page |
| `jobs-{run}.json` | `actions/runs/{run}/jobs?filter=all` |
| `artifacts-{run}.json` | `actions/runs/{run}/artifacts`, `go-test-report` only |
| `artifact-{id}/*.jsonl` | that artifact's `test-report-seed*.jsonl`: make's first plain lines, every event of the failing test and one passing test, and the package-level events. The fake server zips them on request. |
| `log-{job}.txt` | `actions/jobs/{job}/logs`, cut to the suite header, the failure and Ginkgo's summary (timestamps and colors kept) |
| `timing-report.txt` | log of job 125527126456: the `-json` stream around the failure and the timing report |
| `beforesuite.txt` | log of job 125777466511 (E2E Tests run 41594153627): a BeforeSuite failure |
| `subtest-failure.jsonl` | seed22 file of artifact 12515713332: a failing subtest, from before the toolchain marked `t.Error` output |

The runs:

- Tests 41518210793: push to `main`, `TestMetalNodeLeaseEventReachesItsMetalNode` failed.
- Tests 41570133041: dependabot PR, attempt 1 failed the same test, attempt 2 passed.
- Tests 39985914548: one PR's own commit failed `TestModelMultiFileSHA256_Behavioral`, never rerun (PR-only).
- Tests 38539659280: push to `main` on 2026-10-14, before the artifact existed: `TestServer_StreamsChunks` from the job log.
- E2E Tests 40215925566: fork PR, attempt 1 waited for approval, attempt 2 failed a ModelRouter spec, attempt 3 passed.
- E2E Tests 40604254204: a PR whose rerun failed the same spec (PR-only).

Recorded locally with `go test -json` (go1.27.1), with the scratch directory,
GOROOT and package names rewritten to the CI runner's:

- `ginkgo-suite.jsonl`: a Ginkgo suite with one failing spec.
- `timeout-and-build-failure.jsonl`: a package killed by `-timeout` and a
  package that does not build.

test2json drops the ESC byte of Ginkgo's color codes; the jsonl files keep
what it wrote.
