# Changelog

All notable changes to LLMKube will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.9.20](https://github.com/defilantech/LLMKube/compare/v0.9.19...v0.9.20) (2026-08-22)


### Bug Fixes

* **controller:** carry Model tolerations onto the prefetch Job ([#1622](https://github.com/defilantech/LLMKube/issues/1622)) ([d0db838](https://github.com/defilantech/LLMKube/commit/d0db838a6ca442a1adf70b64cc023eb18eb86e48))
* Empty payload.repo silently defeats the upstream-base fetch: task branches cut from a stale fork HEAD ([#1625](https://github.com/defilantech/LLMKube/issues/1625)) ([#1626](https://github.com/defilantech/LLMKube/issues/1626)) ([27a015a](https://github.com/defilantech/LLMKube/commit/27a015a279863392bbac33dc06e46fa4a1087d39))


### Documentation

* fix guide instructions that fail against the shipped API ([#1629](https://github.com/defilantech/LLMKube/issues/1629)) ([fba0bf4](https://github.com/defilantech/LLMKube/commit/fba0bf4d8cdeab13d521f0fa32da8272a2c4a0d1))
* reframe the multi-GPU guide from an Issue [#2](https://github.com/defilantech/LLMKube/issues/2) validation plan into a deployment guide ([#1631](https://github.com/defilantech/LLMKube/issues/1631)) ([907ac8d](https://github.com/defilantech/LLMKube/commit/907ac8d8ee5bef229562ec5340ecdcb9e0576f0d))
* **runbook:** correct the metal-agent memory-pressure runbook against pkg/agent ([#1630](https://github.com/defilantech/LLMKube/issues/1630)) ([f665674](https://github.com/defilantech/LLMKube/commit/f6656741234aa605d3f891598663df142a815171))

## [0.9.19](https://github.com/defilantech/LLMKube/compare/v0.9.18...v0.9.19) (2026-08-20)


### Features

* **foreman:** content-based test-coverage vouch for feature-named test files ([#1614](https://github.com/defilantech/LLMKube/issues/1614)) ([477a599](https://github.com/defilantech/LLMKube/commit/477a599b06f3196438a055ab419a7661846587ba))
* **foreman:** deterministic cross-stage contradiction detection ([#1589](https://github.com/defilantech/LLMKube/issues/1589)) ([0bd102a](https://github.com/defilantech/LLMKube/commit/0bd102a95514d33066b016f5edae278d3986257b))
* **foreman:** execution-verification section in the reviewer rubric ([#1617](https://github.com/defilantech/LLMKube/issues/1617)) ([03702a0](https://github.com/defilantech/LLMKube/commit/03702a0a54858ad21fcd398adceee4786f9b8564))
* **foreman:** extract behaviour clauses from an issue body ([#1590](https://github.com/defilantech/LLMKube/issues/1590)) ([041fab7](https://github.com/defilantech/LLMKube/commit/041fab76c7901589c86f96c586b275c7291d8b7a))
* **foreman:** implement the Agent Validated condition and stamp under-configured runs ([#1611](https://github.com/defilantech/LLMKube/issues/1611)) ([61e65a4](https://github.com/defilantech/LLMKube/commit/61e65a4d3435256a3605614fc2ab6cfbe9d9bd3b))
* **foreman:** let GateProfile declare a test-layout convention ([#1607](https://github.com/defilantech/LLMKube/issues/1607)) ([16b42ce](https://github.com/defilantech/LLMKube/commit/16b42ce0ffd8f88cb907e0fde79118a8c754ad64))
* **foreman:** MCP call aggregation for the foreman.audit.v1 record ([#1591](https://github.com/defilantech/LLMKube/issues/1591)) ([7b8e489](https://github.com/defilantech/LLMKube/commit/7b8e489763490824967a7b9327de020204606b7c))
* **foreman:** rail detecting a GO from a reviewer that never diffed ([#1588](https://github.com/defilantech/LLMKube/issues/1588)) ([376fa58](https://github.com/defilantech/LLMKube/commit/376fa5869015d1dbc90998064a6574c63ad6f94a))
* **foreman:** test-layout mapping for parallel test trees ([#1592](https://github.com/defilantech/LLMKube/issues/1592)) ([df7f719](https://github.com/defilantech/LLMKube/commit/df7f719b468919da69a68f1f3e7bf3d005d6b708))
* **foreman:** vouch modified test files on their added lines ([#1619](https://github.com/defilantech/LLMKube/issues/1619)) ([dd6087b](https://github.com/defilantech/LLMKube/commit/dd6087bb117495b3d3dd6f0d485c10e99ad9d3e0))


### Bug Fixes

* **chart:** grant the inference operator batch/jobs and pods/log, and stop the RBAC guard covering one operator with the other's grants ([#1595](https://github.com/defilantech/LLMKube/issues/1595)) ([bb8d505](https://github.com/defilantech/LLMKube/commit/bb8d5050c663b0538a137bceb06b1238814866e4))
* **controller:** bound the prefetch Job read and stop one Model stalling the rest ([#1598](https://github.com/defilantech/LLMKube/issues/1598)) ([82f4e47](https://github.com/defilantech/LLMKube/commit/82f4e47081da1fa85c14879dd6f5037f27dc62f5))
* **foreman:** decide bash writes on the command word, not a substring ([#1587](https://github.com/defilantech/LLMKube/issues/1587)) ([9a0aaba](https://github.com/defilantech/LLMKube/commit/9a0aaba99e56148e13905db24c3aa9699a226de7))
* **foreman:** record why a reviewer rail could not run ([#1606](https://github.com/defilantech/LLMKube/issues/1606)) ([135ba88](https://github.com/defilantech/LLMKube/commit/135ba88f5bbf36745c68561e67979110a2b052f2))
* **foreman:** skip the NoSystemPrompt floor for deterministic agents ([#1615](https://github.com/defilantech/LLMKube/issues/1615)) ([3c99ac0](https://github.com/defilantech/LLMKube/commit/3c99ac04d0bf3770ac7028a996f2db9e7e24adff))


### Documentation

* **foreman:** document gateProfile.testLayout ([#1612](https://github.com/defilantech/LLMKube/issues/1612)) ([4466e6f](https://github.com/defilantech/LLMKube/commit/4466e6fd87d60c991703e603ecac5d4d3125196e))

## [0.9.18](https://github.com/defilantech/LLMKube/compare/v0.9.17...v0.9.18) (2026-08-18)


### Features

* **controller:** architecture-aware runtime placement ([#1544](https://github.com/defilantech/LLMKube/issues/1544)) ([6694d0b](https://github.com/defilantech/LLMKube/commit/6694d0b010e64834e884a81f85412f7082ec9adf))
* **controller:** speculativeDecoding.draftModel for companion-file drafters ([#1564](https://github.com/defilantech/LLMKube/issues/1564)) ([a8526a8](https://github.com/defilantech/LLMKube/commit/a8526a848ba018e38bbdbcbca86286a1b79c5b85))
* **controller:** surface actual GPU offload in InferenceService status ([#1566](https://github.com/defilantech/LLMKube/issues/1566)) ([a2335c6](https://github.com/defilantech/LLMKube/commit/a2335c60929f62b97d5d63943a04c13b68a319ea))
* draft-model speculative decoding on InferenceService ([#1533](https://github.com/defilantech/LLMKube/issues/1533)) ([81881d6](https://github.com/defilantech/LLMKube/commit/81881d6813563bd3210089d6dabb3e642869468b))
* **foreman:** add optional Agent.spec.maxConcurrentTasks to bound in-flight tasks ([#1546](https://github.com/defilantech/LLMKube/issues/1546)) ([0afa9c9](https://github.com/defilantech/LLMKube/commit/0afa9c9d6e7e402330ce3fc39aacfe363142ea3d))
* **foreman:** flag issue references removed by a diff ([#1574](https://github.com/defilantech/LLMKube/issues/1574)) ([0f2d30d](https://github.com/defilantech/LLMKube/commit/0f2d30d6d40b2c04cb695d2513a1ed03ee2c5718))
* **foreman:** honour the target repository's PR template when opening a PR ([#1562](https://github.com/defilantech/LLMKube/issues/1562)) ([5d09e07](https://github.com/defilantech/LLMKube/commit/5d09e078a1bf44866c73c7672cd1e28df5a74ec3))
* **foreman:** mutation-check decision logic (revert the fix, require a test to fail) ([#1578](https://github.com/defilantech/LLMKube/issues/1578)) ([07754f1](https://github.com/defilantech/LLMKube/commit/07754f1c9bf9805eeb2f05e6a376a46551e527db))
* **foreman:** operator-emitted task outcome counters ([#1563](https://github.com/defilantech/LLMKube/issues/1563)) ([2386ad8](https://github.com/defilantech/LLMKube/commit/2386ad8a80641bbab202e57839b3cab127a30f88))
* **foreman:** pre-flight staleness detection for already-addressed issues ([#1577](https://github.com/defilantech/LLMKube/issues/1577)) ([cc9d594](https://github.com/defilantech/LLMKube/commit/cc9d59405454b7845b476413b6375a70c15c4ef6))


### Bug Fixes

* **controller:** AND the arch requirement into nodeAffinity instead of ORing a new term ([#1584](https://github.com/defilantech/LLMKube/issues/1584)) ([4e5e5d4](https://github.com/defilantech/LLMKube/commit/4e5e5d42ea573cb9602f67a1793e0ec74f556ea4))
* **controller:** do not roll over in-flight work when idleness is undetermined ([#1543](https://github.com/defilantech/LLMKube/issues/1543)) ([daba4e1](https://github.com/defilantech/LLMKube/commit/daba4e11c1c6b21b2aac5bc473c84de91b908f1a))
* **controller:** expose --dri-render-gid through the chart and stop defaulting it to the video group ([#1586](https://github.com/defilantech/LLMKube/issues/1586)) ([66029e7](https://github.com/defilantech/LLMKube/commit/66029e7b959537a8a2d28cef19515b2dfaabb7e1))
* **controller:** inject the DRI render GID into Vulkan pod supplementalGroups ([#1569](https://github.com/defilantech/LLMKube/issues/1569)) ([79f8f08](https://github.com/defilantech/LLMKube/commit/79f8f0846c06f72e8873acabb562f12be2ed2bfd))
* **controller:** let draft-mtp accept an optional draftModelRef ([#1547](https://github.com/defilantech/LLMKube/issues/1547)) ([98fd3a0](https://github.com/defilantech/LLMKube/commit/98fd3a0f886f9f9a85dd6d156c1071fc68e116f1))
* **controller:** let ModelTransferHealthy recover after a retried download succeeds ([#1539](https://github.com/defilantech/LLMKube/issues/1539)) ([978f5ee](https://github.com/defilantech/LLMKube/commit/978f5ee7b7875c3b9ed7c9dbdd3f6ef2fced32b6))
* **deps:** bump the Go toolchain to 1.26.6 for two stdlib advisories ([#1534](https://github.com/defilantech/LLMKube/issues/1534)) ([627ca93](https://github.com/defilantech/LLMKube/commit/627ca936dad7db35db8c5419a0d318f8530fe5ee))
* **foreman:** do not let an unavailable diff disable the grounded-finding rail ([#1576](https://github.com/defilantech/LLMKube/issues/1576)) ([812186b](https://github.com/defilantech/LLMKube/commit/812186b5fca0a1eec906adaa749b1a34514d0a7d))
* **foreman:** do not reserve a FleetNode for Job-mode AgenticTasks ([#1558](https://github.com/defilantech/LLMKube/issues/1558)) ([55a1434](https://github.com/defilantech/LLMKube/commit/55a1434408be54e29b71886a6953498ad24b1bb1))
* **foreman:** honour Workload spec.repo for gate, push and PR, not just clone ([#1545](https://github.com/defilantech/LLMKube/issues/1545)) ([d949e3f](https://github.com/defilantech/LLMKube/commit/d949e3f0f82e4d4eb3487eb2c9aab20cd0784ab1))
* **foreman:** let the reviewer author an uncapped PR body via extra.prBody ([#1575](https://github.com/defilantech/LLMKube/issues/1575)) ([827cad8](https://github.com/defilantech/LLMKube/commit/827cad8c32ac05552307eb6b866c60686df1e4fd))
* **foreman:** map test files to their modules in scope-overlap, and dedupe issue file refs ([#1573](https://github.com/defilantech/LLMKube/issues/1573)) ([fa02f25](https://github.com/defilantech/LLMKube/commit/fa02f25cb04b3091eeea0c066d8ee5457a194c26))
* **foreman:** mount the agent's own gate-cache PVC, and honour an empty name ([#1557](https://github.com/defilantech/LLMKube/issues/1557)) ([b09c3ad](https://github.com/defilantech/LLMKube/commit/b09c3ad12677d0dfbc56a2ba83a2bf21592d8ed2))
* **foreman:** remap unsupported emptiness claims to ERROR instead of NO-GO ([#1565](https://github.com/defilantech/LLMKube/issues/1565)) ([ba7e3f9](https://github.com/defilantech/LLMKube/commit/ba7e3f9de9aef81fff9dbefc10ce5e5a93b966a9))
* **foreman:** restore the coder grounding rail's MCP prefix after the [#1527](https://github.com/defilantech/LLMKube/issues/1527) rename ([#1582](https://github.com/defilantech/LLMKube/issues/1582)) ([1b289e6](https://github.com/defilantech/LLMKube/commit/1b289e63cff796749326491bda8c6ac2e45b1078))
* **foreman:** stop Job-mode AgenticTasks orphaning their previous Job on re-dispatch ([#1556](https://github.com/defilantech/LLMKube/issues/1556)) ([32d1d65](https://github.com/defilantech/LLMKube/commit/32d1d6524592e58ddf445ba704db54f7f874b1fa))
* namespace MCP tools with __ so names match the OAI pattern ([#1527](https://github.com/defilantech/LLMKube/issues/1527)) ([1cd05d7](https://github.com/defilantech/LLMKube/commit/1cd05d7c074ddbe71171558468e78d3652ac2f1f))
* omit the issue reference when a task has no issue number ([#1532](https://github.com/defilantech/LLMKube/issues/1532)) ([f881ac3](https://github.com/defilantech/LLMKube/commit/f881ac310cc30c5044bab0470f34931c3d1f2ffc))
* stop discarding a coder's diff when submit_result omits a message ([#1529](https://github.com/defilantech/LLMKube/issues/1529)) ([3cd01ac](https://github.com/defilantech/LLMKube/commit/3cd01ac70f2b0fb3a6634b026248c0032eecb2c9))


### Documentation

* make spec.files discoverable from spec.source for multi-artifact models ([#1540](https://github.com/defilantech/LLMKube/issues/1540)) ([25777da](https://github.com/defilantech/LLMKube/commit/25777da1d00bce365d596fdde39b9eaa4e3da260))

## [0.9.17](https://github.com/defilantech/LLMKube/compare/v0.9.16...v0.9.17) (2026-08-13)


### Features

* **chart:** foreman.crs.namespace — render the CRS ConfigMap where kube-state-metrics can mount it ([#1492](https://github.com/defilantech/LLMKube/issues/1492)) ([5bb8c19](https://github.com/defilantech/LLMKube/commit/5bb8c19c577dce55c502fb2f990b3648bd24a5e9))
* fetch_pull_request: include check-run output (title/summary/text) for failed runs ([#1481](https://github.com/defilantech/LLMKube/issues/1481)) ([#1505](https://github.com/defilantech/LLMKube/issues/1505)) ([e323f32](https://github.com/defilantech/LLMKube/commit/e323f32d18556db91e255ce61123d48b79c077d6))


### Bug Fixes

* **chart:** heartbeat alert uses time(), not the nonexistent time_seconds() ([#1494](https://github.com/defilantech/LLMKube/issues/1494)) ([bf8c1a0](https://github.com/defilantech/LLMKube/commit/bf8c1a034b0209aad030905e2591941a209d63b5))
* **chart:** scope agent Deployment selectors to their pool ([#1524](https://github.com/defilantech/LLMKube/issues/1524)) ([0da2a9e](https://github.com/defilantech/LLMKube/commit/0da2a9ee6f4b4205d6316850c988f4f92cdba1d4))
* detect the scheduler's other wording for an unusable cache volume ([#1513](https://github.com/defilantech/LLMKube/issues/1513)) ([64ee9d7](https://github.com/defilantech/LLMKube/commit/64ee9d7788dc8fe4c17b229c461ac3f7ddae1562))
* EnsurePR treats a closed unmerged PR as satisfied — retried work ends with no PR ([#1476](https://github.com/defilantech/LLMKube/issues/1476)) ([#1507](https://github.com/defilantech/LLMKube/issues/1507)) ([82baff8](https://github.com/defilantech/LLMKube/commit/82baff88362cac188c3b4c68654734eaffa5e9b3))
* extract path refs the extension gate was discarding ([#1516](https://github.com/defilantech/LLMKube/issues/1516)) ([7166920](https://github.com/defilantech/LLMKube/commit/71669204cddcff8f0dd05671c5593403c8e02410))
* **foreman:** bound edit-free resets earned from bash-shaped writes ([#1521](https://github.com/defilantech/LLMKube/issues/1521)) ([41a2829](https://github.com/defilantech/LLMKube/commit/41a282920345b0a5792d68ce6362d5d739b1f15a))
* **foreman:** bound grep match lines so one minified file cannot end a run ([#1488](https://github.com/defilantech/LLMKube/issues/1488)) ([952f8af](https://github.com/defilantech/LLMKube/commit/952f8af1fb2eee2f8918383a40f148c32db82ae0))
* **foreman:** one tool list, so a tool cannot ship unusable again ([#1486](https://github.com/defilantech/LLMKube/issues/1486)) ([e77d7cf](https://github.com/defilantech/LLMKube/commit/e77d7cf3435ac485bd4424b2f6c6c0df80585043))
* **foreman:** reviewers review the change, not the base branch (+ cross-project remote guard) ([#1490](https://github.com/defilantech/LLMKube/issues/1490)) ([0f3fa83](https://github.com/defilantech/LLMKube/commit/0f3fa83f5ce8bdb89d0deb176844de7dc9346195))
* **foreman:** route all three consumers through the provider seams ([#1510](https://github.com/defilantech/LLMKube/issues/1510)) ([79448fc](https://github.com/defilantech/LLMKube/commit/79448fca464c3377fc89685db8850b28b5d82a4d))
* **modelpool:** guard the nil HTTPClient that panics every swap ([#1485](https://github.com/defilantech/LLMKube/issues/1485)) ([dc3fa50](https://github.com/defilantech/LLMKube/commit/dc3fa50faa2d5bc8eb38b11e5c6548acffbb86f5))
* name the cause when a model cache PV can never bind ([#1511](https://github.com/defilantech/LLMKube/issues/1511)) ([772c99d](https://github.com/defilantech/LLMKube/commit/772c99db80580d2b5a3db56536c600ce21dd70c2))
* normalizeContainers strips operator-owned SecurityContext fields before drift comparison ([#1462](https://github.com/defilantech/LLMKube/issues/1462)) ([#1506](https://github.com/defilantech/LLMKube/issues/1506)) ([8d291ca](https://github.com/defilantech/LLMKube/commit/8d291ca2ca36fbcb0d06ef587c30dce88b86b96f))
* **operator:** apply nodeSelector and tolerations to non-GPU services ([#1504](https://github.com/defilantech/LLMKube/issues/1504)) ([dc3bac3](https://github.com/defilantech/LLMKube/commit/dc3bac3a7476dbe05fae8d90daab30655a880eee))
* populate status.size and status.gguf for s3:// model sources ([#1522](https://github.com/defilantech/LLMKube/issues/1522)) ([69a76fd](https://github.com/defilantech/LLMKube/commit/69a76fdd9ff4d10c3e7c563382b12c2bd633bf05))
* **router-proxy:** honour 0 as disable, pin the metrics port in tests, refresh the dashboard catalog ([#1519](https://github.com/defilantech/LLMKube/issues/1519)) ([774e0b9](https://github.com/defilantech/LLMKube/commit/774e0b95865668fc422192575c4fc7b19279e90c))
* **router-proxy:** reject colliding listener addresses at startup ([#1518](https://github.com/defilantech/LLMKube/issues/1518)) ([fc49a5d](https://github.com/defilantech/LLMKube/commit/fc49a5d2c3c0deed376e5fe62d3ef625f8eb07c3))
* surface the unbindable-cache diagnosis instead of dropping it ([#1512](https://github.com/defilantech/LLMKube/issues/1512)) ([03c0f17](https://github.com/defilantech/LLMKube/commit/03c0f179dd507cecb9a707f440ddbf007815904a))


### Documentation

* **foreman:** refresh model compatibility from what the fleet has proven ([#1483](https://github.com/defilantech/LLMKube/issues/1483)) ([8ccdbee](https://github.com/defilantech/LLMKube/commit/8ccdbee27c8cf2cfc1352e62094e5aa3d48ddec6))
* how to recognise a cache volume the provisioner never stamped ([#1514](https://github.com/defilantech/LLMKube/issues/1514)) ([12d59c2](https://github.com/defilantech/LLMKube/commit/12d59c2f62629ee636ae83bd6853e378aca5abae))
* **operations:** document ModelPool in the GPU-sharing guide ([#1508](https://github.com/defilantech/LLMKube/issues/1508)) ([c2c3a84](https://github.com/defilantech/LLMKube/commit/c2c3a8406434b133e9855b00e17d40793af7cc6a))

## [0.9.16](https://github.com/defilantech/LLMKube/compare/v0.9.15...v0.9.16) (2026-08-10)


### Features

* **chart:** PodMonitor for router-proxy metrics ([#1473](https://github.com/defilantech/LLMKube/issues/1473)) ([fd7c640](https://github.com/defilantech/LLMKube/commit/fd7c6404fe30b2da37170c43ab04e047488a0640))
* **foreman:** advisory two-site parity signal in the coder gate ([#1456](https://github.com/defilantech/LLMKube/issues/1456)) ([944f90d](https://github.com/defilantech/LLMKube/commit/944f90d85aafa89a3e5302c38eda025901968a49))
* **foreman:** image input, so a vision-capable coder can see what it rendered ([#1471](https://github.com/defilantech/LLMKube/issues/1471)) ([6c122be](https://github.com/defilantech/LLMKube/commit/6c122bea5d77f1ffca975890c9d1ae6faa461321))
* **foreman:** read-only fetch_pull_request tool ([#1458](https://github.com/defilantech/LLMKube/issues/1458)) ([7f4910b](https://github.com/defilantech/LLMKube/commit/7f4910b83f6dafcfcf5a1b49b04de49f3afe7db7))
* **modelpool:** cross-pod sticky model swapping on a shared GPU slot ([#1393](https://github.com/defilantech/LLMKube/issues/1393)) ([ba7777d](https://github.com/defilantech/LLMKube/commit/ba7777d42d1b8e03f3ddea8e8cdf0f11ffd3ae8d))
* **router-proxy:** expose llmkube_router_* metrics on a dedicated listener ([#1457](https://github.com/defilantech/LLMKube/issues/1457)) ([ca3730d](https://github.com/defilantech/LLMKube/commit/ca3730d1453edd451a235fec369bb32aa359af2d))


### Bug Fixes

* **controller:** make the custom CA additive to the system trust store ([#1469](https://github.com/defilantech/LLMKube/issues/1469)) ([d490059](https://github.com/defilantech/LLMKube/commit/d490059f8b64113fd70284f43b07373ba52e56ab))
* **controller:** stage files and mmproj from an s3:// object store ([#1475](https://github.com/defilantech/LLMKube/issues/1475)) ([769b60d](https://github.com/defilantech/LLMKube/commit/769b60d22dd3772f6d86a0c6ff5a40a007a079a8))
* **dashboard:** AMD dashboard vendor hygiene — remove runtime-agnostic llama.cpp panels, standardize on DS_PROMETHEUS ([#1446](https://github.com/defilantech/LLMKube/issues/1446)) ([c6c0670](https://github.com/defilantech/LLMKube/commit/c6c06706f8df0f12c4c48ea2d2eebe9137a4f63e))
* **dashboard:** retarget amd-gpu-observability off amdgpu_*-only queries ([#1474](https://github.com/defilantech/LLMKube/issues/1474)) ([a02e8e8](https://github.com/defilantech/LLMKube/commit/a02e8e84ae0f22c914ae5f5793db4879dca46312))

## [0.9.15](https://github.com/defilantech/LLMKube/compare/v0.9.14...v0.9.15) (2026-08-08)


### Features

* **chart:** expose --ca-cert-configmap as controllerManager.caCertConfigMap ([#1440](https://github.com/defilantech/LLMKube/issues/1440)) ([606c7f9](https://github.com/defilantech/LLMKube/commit/606c7f9393a412e9553e93ff3fde862bded1773d))
* **controller:** diagnose CUDA driver/runtime mismatch from crashed pods ([#1425](https://github.com/defilantech/LLMKube/issues/1425)) ([06670f9](https://github.com/defilantech/LLMKube/commit/06670f9905664ad9ae0f7f40a04139da9cf9c1cc))
* **controller:** diagnose model-transfer failures from init containers ([#1463](https://github.com/defilantech/LLMKube/issues/1463)) ([a092de3](https://github.com/defilantech/LLMKube/commit/a092de318b6cf959af6cec44c194e5272ccb6b88))
* **foreman:** advisory gate for command-string changes tested only by shape ([#1421](https://github.com/defilantech/LLMKube/issues/1421)) ([b580e18](https://github.com/defilantech/LLMKube/commit/b580e18803f3ed6d1660c5d81e324290f6b795df))
* **inferenceservice:** add spec.modelCache.persistence for an ephemeral model cache ([#1452](https://github.com/defilantech/LLMKube/issues/1452)) ([c4ae7ee](https://github.com/defilantech/LLMKube/commit/c4ae7ee6e5ffd3f7bdd9463891e3f959b57a0f5c))


### Bug Fixes

* **api:** reject unservable speculativeDecoding type draft at admission ([#1455](https://github.com/defilantech/LLMKube/issues/1455)) ([5d3ab73](https://github.com/defilantech/LLMKube/commit/5d3ab73b3396046aedfd0f26a262de807164c534))
* **chart:** default the router-proxy image tag to the chart appVersion ([#1431](https://github.com/defilantech/LLMKube/issues/1431)) ([3e07b81](https://github.com/defilantech/LLMKube/commit/3e07b814f07bb954acfb446419d5e9ecbcb2496c))
* **controller:** clean up orphaned .tmp files from interrupted model transfers ([#1459](https://github.com/defilantech/LLMKube/issues/1459)) ([2f453d6](https://github.com/defilantech/LLMKube/commit/2f453d621279c787fa3592020901120e93ed36d6))
* **controller:** emit draft-simple for speculativeDecoding type draft ([#1417](https://github.com/defilantech/LLMKube/issues/1417)) ([ccccd02](https://github.com/defilantech/LLMKube/commit/ccccd0203f0b0e3a76732f52aeb2b7bbf458a772))
* **controller:** publish model downloads atomically so an interrupted transfer is never cached ([#1432](https://github.com/defilantech/LLMKube/issues/1432)) ([206b6c3](https://github.com/defilantech/LLMKube/commit/206b6c3c56d98b03377242ec86f000d9a9315803))
* **controller:** route s3:// model sources to the runtime-resolved path ([#1450](https://github.com/defilantech/LLMKube/issues/1450)) ([f054960](https://github.com/defilantech/LLMKube/commit/f0549605b029783fb2095d205a990d69c0bd6a7c))
* **controller:** set TerminationMessagePolicy on generated init containers ([#1460](https://github.com/defilantech/LLMKube/issues/1460)) ([bc732b3](https://github.com/defilantech/LLMKube/commit/bc732b3afd5ce931360efd1ce4d625ae61975643))
* **dashboard:** select the latency success series by le!="" so llmkube-slo error-budget panels render for any threshold ([#1444](https://github.com/defilantech/LLMKube/issues/1444)) ([d381112](https://github.com/defilantech/LLMKube/commit/d381112c63930d92f305d7bd942b97205266d2be))
* **foreman:** add chart validation to the coder gate, and install helm so it can run ([#1442](https://github.com/defilantech/LLMKube/issues/1442)) ([b26c6ae](https://github.com/defilantech/LLMKube/commit/b26c6ae11a26b9e5567524680621ef8e63714a24))
* **foreman:** detect assertion-value churn in the test-dilution gate ([#1416](https://github.com/defilantech/LLMKube/issues/1416)) ([b53df14](https://github.com/defilantech/LLMKube/commit/b53df14d95652647c10b0f32e4b7425efb69435c))
* **foreman:** preserve the coder Job name's uniqueness suffix on truncation ([#1412](https://github.com/defilantech/LLMKube/issues/1412)) ([929f0a8](https://github.com/defilantech/LLMKube/commit/929f0a8795e8ad3070b3717e130aea9d8a88e4a0))
* ground the PR body summary against the branch diff ([#1448](https://github.com/defilantech/LLMKube/issues/1448)) ([6f24527](https://github.com/defilantech/LLMKube/commit/6f2452743f0f30168a3f00de8db25f63d36170e5))
* **metrics:** correct the AMD doc's metric contract and guard prose docs ([#1420](https://github.com/defilantech/LLMKube/issues/1420)) ([36631a9](https://github.com/defilantech/LLMKube/commit/36631a914a412caa13bf9a7741028cfdc10c9f74))
* **runtime:** disable the llama.cpp prompt cache for embedding and rerank ([#1413](https://github.com/defilantech/LLMKube/issues/1413)) ([407de59](https://github.com/defilantech/LLMKube/commit/407de59a8efa5cef8eb060d83ac5faab37bb4bcc))


### Documentation

* **b200:** record GB10 partial-proxy reachability and why NVLink/MIG/sm_100 stay gated ([#1422](https://github.com/defilantech/LLMKube/issues/1422)) ([e6ab2e9](https://github.com/defilantech/LLMKube/commit/e6ab2e9d0d8220add604230ffb527ff7b1660a88))
* **chart:** dashboard datasources examples must use the datasource UID, not its display name ([#1433](https://github.com/defilantech/LLMKube/issues/1433)) ([a57d499](https://github.com/defilantech/LLMKube/commit/a57d499fcbc3261feccb31148f6fb3796a751db0))

## [0.9.14](https://github.com/defilantech/LLMKube/compare/v0.9.13...v0.9.14) (2026-08-05)


### Features

* **api:** add mxfp4 quantization and correct Blackwell runtime reality ([#1381](https://github.com/defilantech/LLMKube/issues/1381)) ([382af35](https://github.com/defilantech/LLMKube/commit/382af35ffee1d600b6bc05db0fb580607241a4d1))
* **controller:** opt-in Prometheus scrape annotations on inference pods ([#1366](https://github.com/defilantech/LLMKube/issues/1366)) ([2e8f8a8](https://github.com/defilantech/LLMKube/commit/2e8f8a8c8c122db46b67ede960152f0d80ff8590))
* **dashboards:** add a llama.cpp runtime dashboard ([#1371](https://github.com/defilantech/LLMKube/issues/1371)) ([5fedb88](https://github.com/defilantech/LLMKube/commit/5fedb88e6bbba1e7a4d827c13538a362debeb382))
* **dashboards:** add an operator-health row to llmkube-inference ([#1369](https://github.com/defilantech/LLMKube/issues/1369)) ([ee5145b](https://github.com/defilantech/LLMKube/commit/ee5145bc6bb57b7fb2dc686d3fccd09fa15d8ed6))
* **dashboards:** SGLang queue-wait, stage latency, token mix and HiCache L3 panels ([#1370](https://github.com/defilantech/LLMKube/issues/1370)) ([d1a57ca](https://github.com/defilantech/LLMKube/commit/d1a57cab65abbeea23dd841da768360e01128e05))
* **observability:** AMD GPU tier — amdgpu-exporter DaemonSet, metric contract, dashboard reconcile ([#1354](https://github.com/defilantech/LLMKube/issues/1354)) ([a9e560f](https://github.com/defilantech/LLMKube/commit/a9e560f4fbd989cddf7dff1aa45184652150538e))


### Bug Fixes

* **chart:** correct stale Blackwell platform floors (R570 is EOL) ([#1380](https://github.com/defilantech/LLMKube/issues/1380)) ([445664e](https://github.com/defilantech/LLMKube/commit/445664e907bc8d5e7e0a0b8bb6646706c39f82c6))
* **chart:** make GPU alerts exporter-agnostic instead of DCGM-only ([#1372](https://github.com/defilantech/LLMKube/issues/1372)) ([402a686](https://github.com/defilantech/LLMKube/commit/402a6865e3863294d0aa779d58a8a98ddb83e80c))
* **controller:** correct the speculative-decoding draft flag and dedupe --metrics ([#1392](https://github.com/defilantech/LLMKube/issues/1392)) ([48d7d49](https://github.com/defilantech/LLMKube/commit/48d7d49a61c74a90b897eb273ab9aaebe21c2365))
* **controller:** skip model-cache PVC for pvc:// sources ([#1363](https://github.com/defilantech/LLMKube/issues/1363)) ([9ec0bf9](https://github.com/defilantech/LLMKube/commit/9ec0bf972fcf6f8bf228e40a844ee95f8b996ddb))
* **foreman:** fail loudly when a revision's prior attempt cannot be restored ([#1365](https://github.com/defilantech/LLMKube/issues/1365)) ([b9b7685](https://github.com/defilantech/LLMKube/commit/b9b76857e2ef281ad8a6387a74c5f42e2b7eaad1))
* **foreman:** run the coder gate's Go checks with GOTOOLCHAIN=auto ([#1389](https://github.com/defilantech/LLMKube/issues/1389)) ([ce828dd](https://github.com/defilantech/LLMKube/commit/ce828dd6baeb2039dc15b2be8afebada48d3e285))
* **router:** apply external.model as modelNameOverride on backendRefs ([#1398](https://github.com/defilantech/LLMKube/issues/1398)) ([847b14d](https://github.com/defilantech/LLMKube/commit/847b14d7da7c8fb4cd19aca68dea1af0db047918))
* **router:** compile external backends in the Gateway data plane ([#1396](https://github.com/defilantech/LLMKube/issues/1396)) ([5045dd0](https://github.com/defilantech/LLMKube/commit/5045dd033b732544e1fdb4e83f443766fa6453f7))
* **router:** rewrite body.model for external backends via bodyMutation ([#1401](https://github.com/defilantech/LLMKube/issues/1401)) ([2fc6992](https://github.com/defilantech/LLMKube/commit/2fc69923e7f4309958302fc76973e0064eb24d18))
* **router:** surface a gateway reconcile failure as a Warning Event ([#1400](https://github.com/defilantech/LLMKube/issues/1400)) ([4145a67](https://github.com/defilantech/LLMKube/commit/4145a678e76653555de5b4bc749fcecbb94c4af4))
* **router:** use the non-deprecated events API for the gateway recorder ([#1409](https://github.com/defilantech/LLMKube/issues/1409)) ([435dd72](https://github.com/defilantech/LLMKube/commit/435dd7218f6b48b11ae083449e3bf29e4e92c193))
* skip GitHub auth and repo clone for freeform tasks without a repo ([#1350](https://github.com/defilantech/LLMKube/issues/1350)) ([c1d0cc1](https://github.com/defilantech/LLMKube/commit/c1d0cc1611c87ee040453e32cf13a6cce800577c))
* stop wasted-turn context ballooning on max_tokens truncation ([#1352](https://github.com/defilantech/LLMKube/issues/1352)) ([eaac3ef](https://github.com/defilantech/LLMKube/commit/eaac3efbe1a8b1b290f7c828cdf8776ebcdf128d))
* **test:** make t.Fatal terminality explicit for staticcheck SA5011 ([#1410](https://github.com/defilantech/LLMKube/issues/1410)) ([7cf58c7](https://github.com/defilantech/LLMKube/commit/7cf58c781188d58465caa6d12bc0b487a396127b))


### Reverts

* remove the inert model-rewrite paths for external backends ([#1402](https://github.com/defilantech/LLMKube/issues/1402)) ([422b9ef](https://github.com/defilantech/LLMKube/commit/422b9ef6f679e4bbdb3ab5abefbaeefec14f18ee))


### Documentation

* add observability maintainer ([#1407](https://github.com/defilantech/LLMKube/issues/1407)) ([3fb54e2](https://github.com/defilantech/LLMKube/commit/3fb54e296040de8f323e0a91a83b61be9933d3c1))
* fix autoscaling guide to use a metric llama.cpp actually exports ([#1386](https://github.com/defilantech/LLMKube/issues/1386)) ([bc3fca2](https://github.com/defilantech/LLMKube/commit/bc3fca237ff938d91d0b6bf046f43838d2ebe29d))

## [0.9.13](https://github.com/defilantech/LLMKube/compare/v0.9.12...v0.9.13) (2026-07-29)


### Features

* **chart:** template Grafana dashboards, publishing runtime ones only while their runtime serves ([#1267](https://github.com/defilantech/LLMKube/issues/1267)) ([c2d56b7](https://github.com/defilantech/LLMKube/commit/c2d56b7d31746726e5dc79e2f4ce5f94b6452560))
* **foreman:** test-dilution advisory for the coder gate ([#1332](https://github.com/defilantech/LLMKube/issues/1332)) ([#1337](https://github.com/defilantech/LLMKube/issues/1337)) ([43a30fc](https://github.com/defilantech/LLMKube/commit/43a30fc68f7d1729c131ae4e7e4ba8cbb662dd1b))


### Bug Fixes

* **controller:** append TGI extraArgs once, not four times ([#1306](https://github.com/defilantech/LLMKube/issues/1306)) ([6d675f2](https://github.com/defilantech/LLMKube/commit/6d675f23f758da6eb6168e1786b5ff07598cbdac))
* **controller:** Available goes False when no replicas are ready ([#1304](https://github.com/defilantech/LLMKube/issues/1304)) ([f3bd243](https://github.com/defilantech/LLMKube/commit/f3bd2435f3d43e84ad7cb455df04ee6f89000038))
* **controller:** charge GPUQuota by the resolved GPU count, closing the Model-declared-count bypass ([#1324](https://github.com/defilantech/LLMKube/issues/1324)) ([e052229](https://github.com/defilantech/LLMKube/commit/e0522294a8ec499ca10d49f4bb89bebb74ab69fa))
* **controller:** cpu-offload verify advisory, parallelism validation, and a sample that actually runs ([#1321](https://github.com/defilantech/LLMKube/issues/1321)) ([9740dad](https://github.com/defilantech/LLMKube/commit/9740dade9e6f5fcaf790ec5c8349f6452ffba260))
* **controller:** gate HPA scale-up by GPUQuota via maxReplicas accounting ([#1311](https://github.com/defilantech/LLMKube/issues/1311)) ([#1334](https://github.com/defilantech/LLMKube/issues/1334)) ([dcefc45](https://github.com/defilantech/LLMKube/commit/dcefc450049812531e8203334a3dd8939902f25c))
* **controller:** recognise huggingface.co URL sources as HF repos, not single-file downloads ([#1322](https://github.com/defilantech/LLMKube/issues/1322)) ([#1325](https://github.com/defilantech/LLMKube/issues/1325)) ([36fc79f](https://github.com/defilantech/LLMKube/commit/36fc79f14e40cb9671d2ae8e71cd3012f6c3ffd8))
* **controller:** revalidate model cache via HEAD + Content-Length, atomic download ([#1309](https://github.com/defilantech/LLMKube/issues/1309)) ([#1326](https://github.com/defilantech/LLMKube/issues/1326)) ([3e86988](https://github.com/defilantech/LLMKube/commit/3e86988d951e825e25d73343c0e4c2f9e16b499a))
* **foreman:** accept multi-segment repo slugs (GitLab subgroups, nested Forgejo orgs) ([#1299](https://github.com/defilantech/LLMKube/issues/1299)) ([#1341](https://github.com/defilantech/LLMKube/issues/1341)) ([1b7c47a](https://github.com/defilantech/LLMKube/commit/1b7c47ae63828bea4ab93038dcf65785c2a1a346))
* **foreman:** auto-apply gofmt in the coder gate instead of failing the coder ([#1327](https://github.com/defilantech/LLMKube/issues/1327)) ([#1333](https://github.com/defilantech/LLMKube/issues/1333)) ([954c67b](https://github.com/defilantech/LLMKube/commit/954c67bd56764436727036e00446fe4969b0e8ec))
* **foreman:** do not downgrade a mixed footprint whose classes are all self-GO ([#1342](https://github.com/defilantech/LLMKube/issues/1342)) ([#1344](https://github.com/defilantech/LLMKube/issues/1344)) ([d7c6569](https://github.com/defilantech/LLMKube/commit/d7c656968064c5c9c3c6390c720f9e60a746a073))
* **foreman:** make coder self-gate diff-aware, skip Go tier on non-Go diffs ([#1292](https://github.com/defilantech/LLMKube/issues/1292)) ([#1336](https://github.com/defilantech/LLMKube/issues/1336)) ([9f4acef](https://github.com/defilantech/LLMKube/commit/9f4acef6a39742894f0ba434d48c3e116b02bc50))
* **foreman:** ParseRepo delegates to the shared codehost slug validator ([#1343](https://github.com/defilantech/LLMKube/issues/1343)) ([#1345](https://github.com/defilantech/LLMKube/issues/1345)) ([ae2dfc2](https://github.com/defilantech/LLMKube/commit/ae2dfc2ed7f2d130a12d9e6364f5e7ed03f70f11))
* **foreman:** record IsError on MCP transport failures ([#1330](https://github.com/defilantech/LLMKube/issues/1330)) ([#1335](https://github.com/defilantech/LLMKube/issues/1335)) ([c42f873](https://github.com/defilantech/LLMKube/commit/c42f87316e667c80753401089ad20a00f1150214))
* **foreman:** split git committer identity from author for human DCO sign-off ([#1323](https://github.com/defilantech/LLMKube/issues/1323)) ([44ef09f](https://github.com/defilantech/LLMKube/commit/44ef09fddeaa5d89200ad145f4d2c592dcdb2f10))
* **foreman:** test-presence accepts behavioral coverage of unexported helpers ([#1329](https://github.com/defilantech/LLMKube/issues/1329)) ([#1340](https://github.com/defilantech/LLMKube/issues/1340)) ([f1aea17](https://github.com/defilantech/LLMKube/commit/f1aea178e3d05487f4dee5e42202b8ce555cb448))


### Documentation

* **api:** correct MemoryLimitGiB doc, it is quota-accounting only with no runtime enforcement ([#1312](https://github.com/defilantech/LLMKube/issues/1312)) ([#1339](https://github.com/defilantech/LLMKube/issues/1339)) ([90e6ca3](https://github.com/defilantech/LLMKube/commit/90e6ca38228f89e6818db8bb0b5312409ed8637a))
* **api:** stop the GPU-memory fields implying enforcement that does not exist ([#1314](https://github.com/defilantech/LLMKube/issues/1314)) ([8660c85](https://github.com/defilantech/LLMKube/commit/8660c85d047965445d668bea8e77e691048fea2a))
* **foreman:** replace em dash with colon in README heading ([#1296](https://github.com/defilantech/LLMKube/issues/1296)) ([8586a37](https://github.com/defilantech/LLMKube/commit/8586a377eaa15bd3142ba5a00226d8bcc5fbf250))
* **multi-tenancy:** document GPUQuota is off by default + opt-in/Kueue/HPA/placement notes ([#1313](https://github.com/defilantech/LLMKube/issues/1313)) ([#1338](https://github.com/defilantech/LLMKube/issues/1338)) ([3081897](https://github.com/defilantech/LLMKube/commit/3081897bca396918f67cad7b7ea0823ad7444be2))

## [0.9.12](https://github.com/defilantech/LLMKube/compare/v0.9.11...v0.9.12) (2026-07-26)


### Features

* add --prompt-depth-sweep to benchmark command ([#1280](https://github.com/defilantech/LLMKube/issues/1280)) ([eb511f9](https://github.com/defilantech/LLMKube/commit/eb511f93fec92bbe1520981f9cb186e65abae8c9))
* **foreman:** support chat_template_kwargs on agent chat requests ([#1285](https://github.com/defilantech/LLMKube/issues/1285)) ([1028335](https://github.com/defilantech/LLMKube/commit/102833581be53fdcc56b0ee5f28b62f17e8fe599))
* **grafana:** add SGLang and vLLM runtime dashboards ([#1266](https://github.com/defilantech/LLMKube/issues/1266)) ([07f340a](https://github.com/defilantech/LLMKube/commit/07f340acda8b0035be4190eea5c50dffbb3e5f0c))
* **grafana:** union llama.cpp, sglang and vLLM metrics on the inference dashboard ([#1248](https://github.com/defilantech/LLMKube/issues/1248)) ([7cb226b](https://github.com/defilantech/LLMKube/commit/7cb226b3fce501d68e1e89756e0c61c9091cb397))
* **hack:** spine probes for models given repo access ([#1274](https://github.com/defilantech/LLMKube/issues/1274)) ([b7b3d9b](https://github.com/defilantech/LLMKube/commit/b7b3d9b9612c7587f4a2aed1bd32c95849c97343))


### Bug Fixes

* Available condition reflects Stopped and Suspended phases ([#1273](https://github.com/defilantech/LLMKube/issues/1273)) ([208bd79](https://github.com/defilantech/LLMKube/commit/208bd795cd093068efd163ee3d2cd0cd616de79d))
* disable llama.cpp prompt cache in benchmark requests ([#1272](https://github.com/defilantech/LLMKube/issues/1272)) ([eaa61ad](https://github.com/defilantech/LLMKube/commit/eaa61ad79607e5d239a63fac728a4962b324cb98))
* **foreman:** apply the empty-assistant guard to the preserved truncated turn ([#1282](https://github.com/defilantech/LLMKube/issues/1282)) ([b1f51cc](https://github.com/defilantech/LLMKube/commit/b1f51cc181d4028c1a2effc4f95bb4fb28ec6900))
* **foreman:** hold the session compaction drop point steady across turns ([#1287](https://github.com/defilantech/LLMKube/issues/1287)) ([febcb2a](https://github.com/defilantech/LLMKube/commit/febcb2a75945471167231b7e9e3b1daf6bd10163))
* **foreman:** three first-run failures on a clean cluster ([#1295](https://github.com/defilantech/LLMKube/issues/1295)) ([5ca3e64](https://github.com/defilantech/LLMKube/commit/5ca3e64d26647501db0c104b26918c5b6a8a2f97))
* **gate:** check non-Go changes against the shared merge-base, not HEAD~1 ([#1284](https://github.com/defilantech/LLMKube/issues/1284)) ([6940033](https://github.com/defilantech/LLMKube/commit/6940033fc9eb6b8448a7cc09919eff695428bb9e))
* **spine-probes:** restore --max-tokens and the 'none' ablation arm ([#1278](https://github.com/defilantech/LLMKube/issues/1278)) ([9493201](https://github.com/defilantech/LLMKube/commit/94932012939c5ea9bba404faae5e7da2609a0150))


### Documentation

* **foreman:** fix AgenticTask names in minimal example ([#1290](https://github.com/defilantech/LLMKube/issues/1290)) ([7659f29](https://github.com/defilantech/LLMKube/commit/7659f295cd95d60b6716c0f566ecfa667156212d))

## [0.9.11](https://github.com/defilantech/LLMKube/compare/v0.9.10...v0.9.11) (2026-07-24)


### Features

* cache prep subcommand with direct chown/chmod syscalls ([#890](https://github.com/defilantech/LLMKube/issues/890) item 3) ([#1258](https://github.com/defilantech/LLMKube/issues/1258)) ([bde1a87](https://github.com/defilantech/LLMKube/commit/bde1a87ecaeee6ea4cadecc7612942ed706fe1cd))
* **charts/foreman:** expose the agent --accelerator flag ([#1242](https://github.com/defilantech/LLMKube/issues/1242)) ([#1261](https://github.com/defilantech/LLMKube/issues/1261)) ([a39fd71](https://github.com/defilantech/LLMKube/commit/a39fd719791a528c79f96d2275d5967d871f328e))
* declarative bindAddress on InferenceService ([#1240](https://github.com/defilantech/LLMKube/issues/1240)) ([#1247](https://github.com/defilantech/LLMKube/issues/1247)) ([bf01598](https://github.com/defilantech/LLMKube/commit/bf01598d4dbd9583cf28d072307d05369eeb2281))
* **federation:** FederatedCluster registry + fleet status rollup ([#1234](https://github.com/defilantech/LLMKube/issues/1234), [#1235](https://github.com/defilantech/LLMKube/issues/1235)) ([#1241](https://github.com/defilantech/LLMKube/issues/1241)) ([1b5bf08](https://github.com/defilantech/LLMKube/commit/1b5bf08f37f5c4b531642f6a205da1fc534f761d))
* **foreman:** tiered str_replace matching (exact, trailing-ws, uniform-indent) ([#1231](https://github.com/defilantech/LLMKube/issues/1231)) ([0372e24](https://github.com/defilantech/LLMKube/commit/0372e2425fbf2431d0fa20a2f7f02c07a69e7f9c))
* **grafana:** chart the InferenceService metrics with no panel ([#1245](https://github.com/defilantech/LLMKube/issues/1245)) ([a5ce923](https://github.com/defilantech/LLMKube/commit/a5ce92340fa94fe9ec08edbd41147836898221a0))
* Kueue prerequisites: spec.suspend, GPUQuota deferral, pkg/apiutil GPU mapping ([#1254](https://github.com/defilantech/LLMKube/issues/1254)) ([c95e06f](https://github.com/defilantech/LLMKube/commit/c95e06f0ddf1a86f1e867c65ce30a63331e7a709))
* **metrics:** publish operator state metrics from observed state ([#1229](https://github.com/defilantech/LLMKube/issues/1229)) ([5db0915](https://github.com/defilantech/LLMKube/commit/5db09158d58fd700af6748b1c3198580af264410))
* prefetch Model artifacts into the shared cache without an InferenceService ([#1218](https://github.com/defilantech/LLMKube/issues/1218)) ([7c1161b](https://github.com/defilantech/LLMKube/commit/7c1161b819eeafebf91140e9b28cee1f594497b8))


### Bug Fixes

* **chart:** make PrometheusRule select the chart's own scrape targets ([#1239](https://github.com/defilantech/LLMKube/issues/1239)) ([ffc697a](https://github.com/defilantech/LLMKube/commit/ffc697a744898c1513a10d4164f93c393489b28f))
* delete GPUQuota metric series when the CR is deleted ([#1230](https://github.com/defilantech/LLMKube/issues/1230)) ([#1246](https://github.com/defilantech/LLMKube/issues/1246)) ([1f58c3c](https://github.com/defilantech/LLMKube/commit/1f58c3c86a03d6486a857cfaa49a8c6572f727c2))
* **foreman:** edits disarm a nudged RepeatedToolCall hash ([#1215](https://github.com/defilantech/LLMKube/issues/1215)) ([#1216](https://github.com/defilantech/LLMKube/issues/1216)) ([60df2ad](https://github.com/defilantech/LLMKube/commit/60df2ade6879cc5d154ade41185de7b1a51e992d))
* gate bite check reverts to the upstream merge-base, never the fork tip ([#1260](https://github.com/defilantech/LLMKube/issues/1260)) ([8a6928f](https://github.com/defilantech/LLMKube/commit/8a6928ffc08ee64d126129bf12cd722b01b55080))
* **grafana:** delete dashboard panels for metrics nothing emits ([#1244](https://github.com/defilantech/LLMKube/issues/1244)) ([9c586d9](https://github.com/defilantech/LLMKube/commit/9c586d9458fa85a338d0fc316e1270b4bbc9e00b))
* **metrics:** report GPU queue depth per namespace ([#1243](https://github.com/defilantech/LLMKube/issues/1243)) ([5779c05](https://github.com/defilantech/LLMKube/commit/5779c05e7dfd98d5f24acc2cbfab44301c08b0b4))
* recycle inference pods without invalid deadlines ([#1232](https://github.com/defilantech/LLMKube/issues/1232)) ([0b716d7](https://github.com/defilantech/LLMKube/commit/0b716d7510cc9bfb172e37f5b6c1c3075a32c524))
* waitForIdle proceeds past crashlooping pods; truthful deferral reason on mixed state ([#1250](https://github.com/defilantech/LLMKube/issues/1250)) ([#1262](https://github.com/defilantech/LLMKube/issues/1262)) ([88c87f9](https://github.com/defilantech/LLMKube/commit/88c87f9770f7e1c2d5026e52c768841a35e07b8c))


### Documentation

* envtest test-craft guide (status subresource, probe stubs, asset hygiene) ([#1263](https://github.com/defilantech/LLMKube/issues/1263)) ([#1264](https://github.com/defilantech/LLMKube/issues/1264)) ([ea4eee6](https://github.com/defilantech/LLMKube/commit/ea4eee66633053f75789988b94ab736cc5508a59))

## [0.9.10](https://github.com/defilantech/LLMKube/compare/v0.9.9...v0.9.10) (2026-07-22)


### Features

* **grafana:** window-suffix variable so the SLO dashboard works for non-default windows ([#1148](https://github.com/defilantech/LLMKube/issues/1148)) ([#1211](https://github.com/defilantech/LLMKube/issues/1211)) ([8f7b70e](https://github.com/defilantech/LLMKube/commit/8f7b70e98fa1dc24715ace680bd0cc8d11ddc3ef))


### Bug Fixes

* **deps:** bump google.golang.org/grpc to v1.82.1 (GHSA-hrxh-6v49-42gf) ([#1212](https://github.com/defilantech/LLMKube/issues/1212)) ([188d8eb](https://github.com/defilantech/LLMKube/commit/188d8eb04e08fd7f608c54b683e8bcbdd3cb74b3))
* **foreman:** preserve the in-pod outcome envelope across the coder Job hop ([#1077](https://github.com/defilantech/LLMKube/issues/1077)) ([#1210](https://github.com/defilantech/LLMKube/issues/1210)) ([6ed7edf](https://github.com/defilantech/LLMKube/commit/6ed7edf1a4bd54ea2aaf88b6e7fec3051ab88fa3))

## [0.9.9](https://github.com/defilantech/LLMKube/compare/v0.9.8...v0.9.9) (2026-07-22)


### Features

* **api:** vendor-neutral gpuSharing tiers on InferenceService ([#1196](https://github.com/defilantech/LLMKube/issues/1196) stories 1+2) ([#1199](https://github.com/defilantech/LLMKube/issues/1199)) ([6cbd1fd](https://github.com/defilantech/LLMKube/commit/6cbd1fd7a8fd446783d0ed37f9b1955e5fbd8290))
* **chart:** gpuSharing pool + VRAM values wired to operator flags ([#1196](https://github.com/defilantech/LLMKube/issues/1196) story 3) ([#1205](https://github.com/defilantech/LLMKube/issues/1205)) ([95eed30](https://github.com/defilantech/LLMKube/commit/95eed30e9ab67150779ff6b1523249a94b804f17))
* **controller:** bound InferenceService pod lifetime via maxPodLifetimeSeconds ([#1182](https://github.com/defilantech/LLMKube/issues/1182)) ([3b39ee9](https://github.com/defilantech/LLMKube/commit/3b39ee9d59c5745315aedbaecc7c0804127e5118))
* **foreman:** make the verify gate optional in the issue-batch decomposition ([#1166](https://github.com/defilantech/LLMKube/issues/1166)) ([e4e49aa](https://github.com/defilantech/LLMKube/commit/e4e49aa34e8809947679cd6bfe9b08964637aa20))
* **metrics:** emit GPUQuota usage + admission-denial metrics ([#416](https://github.com/defilantech/LLMKube/issues/416)) ([#1193](https://github.com/defilantech/LLMKube/issues/1193)) ([dad2687](https://github.com/defilantech/LLMKube/commit/dad2687ec4b4f0c86407fe58993d8ff5e4374c04))
* **quota:** VRAM-based GPUQuota accounting from the gpuSharing tier ([#1196](https://github.com/defilantech/LLMKube/issues/1196) story 4) ([#1200](https://github.com/defilantech/LLMKube/issues/1200)) ([2c9000c](https://github.com/defilantech/LLMKube/commit/2c9000cf6e1a55ad4b1e98dde4fb11ba5840f84f))
* **runtime:** Blackwell-ready image pins, NVIDIA llama.cpp divert, runtimeImages overrides, platform floors ([#1197](https://github.com/defilantech/LLMKube/issues/1197)) ([#1204](https://github.com/defilantech/LLMKube/issues/1204)) ([9d40b36](https://github.com/defilantech/LLMKube/commit/9d40b369dd093f8135165e0292ca0c97ab6f16a2))
* **webhook:** admission-time gpuSharing validation ([#1196](https://github.com/defilantech/LLMKube/issues/1196) story 5) ([#1206](https://github.com/defilantech/LLMKube/issues/1206)) ([a9de8f7](https://github.com/defilantech/LLMKube/commit/a9de8f7a9d0d57d7588ae96a830275cbe431e482))


### Bug Fixes

* **agent:** inject current date into issue-fix prompt and anchor research queries ([#1202](https://github.com/defilantech/LLMKube/issues/1202)) ([#1207](https://github.com/defilantech/LLMKube/issues/1207)) ([5067095](https://github.com/defilantech/LLMKube/commit/506709596bd2d50c72c3948a33b054cd4de9398e))
* **chart:** nil-safe gpuSharing navigation in deployment args ([#1209](https://github.com/defilantech/LLMKube/issues/1209)) ([bbbcdcf](https://github.com/defilantech/LLMKube/commit/bbbcdcfc43dc859d59f25e4d389a052becf44349))
* **codegen:** regenerate stale committed codegen and widen the sync check ([#1190](https://github.com/defilantech/LLMKube/issues/1190)) ([aedcb80](https://github.com/defilantech/LLMKube/commit/aedcb800a65125ea9e93e86c39919316a69991bd))
* **deps:** bump golang.org/x/text to v0.39.0 (GO-2026-5970) ([#1192](https://github.com/defilantech/LLMKube/issues/1192)) ([af5c78e](https://github.com/defilantech/LLMKube/commit/af5c78ebd8d8ba0fe565182aaaaba6ae08225e39))
* **foreman:** bound reasoning-model coder turns with a per-turn token budget ([#1175](https://github.com/defilantech/LLMKube/issues/1175)) ([5100c11](https://github.com/defilantech/LLMKube/commit/5100c112873211b880bc208897864ba8d42fdcec))
* **foreman:** count identical read_file re-reads as no-progress under the edit-free nudge ([#1179](https://github.com/defilantech/LLMKube/issues/1179)) ([2fad7ec](https://github.com/defilantech/LLMKube/commit/2fad7ecb7edb12d2ed23a588377908cd53af7790))
* **foreman:** inject Workload.spec.intent into coder prompts ([#1201](https://github.com/defilantech/LLMKube/issues/1201)) ([#1203](https://github.com/defilantech/LLMKube/issues/1203)) ([9a8371c](https://github.com/defilantech/LLMKube/commit/9a8371c41d8fd72de9a77768c1b84c5d7d213de9))
* **foreman:** warn when an MCP allowlist entry matches no server tool ([#1184](https://github.com/defilantech/LLMKube/issues/1184)) ([0664814](https://github.com/defilantech/LLMKube/commit/0664814236b24448b74802cb7935ae0298100f86))
* **foreman:** widen coder scope-overlap gate to a bottom-quartile floor ([#1181](https://github.com/defilantech/LLMKube/issues/1181)) ([f07cba4](https://github.com/defilantech/LLMKube/commit/f07cba4c9aed5761a5c3a810437d1688744db241))
* **webhook:** serve GPUQuota webhook at its configured path, with e2e coverage ([#416](https://github.com/defilantech/LLMKube/issues/416)) ([#1195](https://github.com/defilantech/LLMKube/issues/1195)) ([b377f52](https://github.com/defilantech/LLMKube/commit/b377f52e88aa919ca379e3b29d7e34bc1ef22725))


### Documentation

* **grafana:** AMD (Strix/gfx1151) GPU observability dashboard ([#1188](https://github.com/defilantech/LLMKube/issues/1188)) ([3da1f2b](https://github.com/defilantech/LLMKube/commit/3da1f2b45ebea4ff04c69efc1b721a1c4bf5ed70))
* **observability:** multi-tenancy guide + GPUQuota Grafana dashboard ([#1097](https://github.com/defilantech/LLMKube/issues/1097)) ([#1194](https://github.com/defilantech/LLMKube/issues/1194)) ([aedbd52](https://github.com/defilantech/LLMKube/commit/aedbd52e1c3f082c7dc8ec3dc8b0786cacccdcbf))
* **operations:** GPU sharing guide ([#1196](https://github.com/defilantech/LLMKube/issues/1196) story 6) ([#1208](https://github.com/defilantech/LLMKube/issues/1208)) ([c9007a3](https://github.com/defilantech/LLMKube/commit/c9007a3b15540478c55aef040b69917ea7560bb6))

## [0.9.8](https://github.com/defilantech/LLMKube/compare/v0.9.7...v0.9.8) (2026-07-19)


### Features

* **controller:** add InferenceService hardware-labels info metric ([#1121](https://github.com/defilantech/LLMKube/issues/1121)) ([#1160](https://github.com/defilantech/LLMKube/issues/1160)) ([039adaa](https://github.com/defilantech/LLMKube/commit/039adaa926b76eb31bc36a512ea82e1ee70dd511))
* **controller:** add llamacpp-router runtime for multi-model serving ([#1152](https://github.com/defilantech/LLMKube/issues/1152)) ([fae5983](https://github.com/defilantech/LLMKube/commit/fae5983242226dd70eb1345c221b1188072b6d27))
* **controller:** explicit vllm serve entrypoint for image-agnostic vLLM ([#1164](https://github.com/defilantech/LLMKube/issues/1164)) ([#1165](https://github.com/defilantech/LLMKube/issues/1165)) ([3eeb580](https://github.com/defilantech/LLMKube/commit/3eeb580f31d6ac47c265b447638fdfbd2f92b0c3))
* **controller:** pass staged model directory to vLLM/SGLang for multi-file models ([#1157](https://github.com/defilantech/LLMKube/issues/1157)) ([#1159](https://github.com/defilantech/LLMKube/issues/1159)) ([a24fa94](https://github.com/defilantech/LLMKube/commit/a24fa948038561d3e5b80ec0da84038844688c37))
* **controller:** ROCm/HIP runtime tier for AMD nodes ([#701](https://github.com/defilantech/LLMKube/issues/701)) ([#1154](https://github.com/defilantech/LLMKube/issues/1154)) ([3704d47](https://github.com/defilantech/LLMKube/commit/3704d476fa650449598330ca4b91950bdc3c7ed0))
* **finalize:** gate foreman-finalize on the Foreman verify verdict ([#1150](https://github.com/defilantech/LLMKube/issues/1150)) ([#1153](https://github.com/defilantech/LLMKube/issues/1153)) ([5c7b2cd](https://github.com/defilantech/LLMKube/commit/5c7b2cde7476c24dee0507d082e3751236385588))
* **foreman:** in-executor envtest gate feedback loop ([#768](https://github.com/defilantech/LLMKube/issues/768)) ([#1135](https://github.com/defilantech/LLMKube/issues/1135)) ([074544a](https://github.com/defilantech/LLMKube/commit/074544a64ec35c9fe08ed8e5b003b22799947f19))
* **foreman:** provider-neutral CodeHost/WorkItems/ChangePolicy seams ([#1158](https://github.com/defilantech/LLMKube/issues/1158)) ([#1161](https://github.com/defilantech/LLMKube/issues/1161)) ([d3a8ad6](https://github.com/defilantech/LLMKube/commit/d3a8ad65b36375ef7808d8c3f0f52b72282efdfd))
* **observability:** per-InferenceService SLO declaration via Pyrra ([#415](https://github.com/defilantech/LLMKube/issues/415)) ([#1149](https://github.com/defilantech/LLMKube/issues/1149)) ([4ae022c](https://github.com/defilantech/LLMKube/commit/4ae022cc862ed0200ba4ea51727324a06fa51166))


### Bug Fixes

* bump boilerplate copyright year 2025 -&gt; 2026 ([#1162](https://github.com/defilantech/LLMKube/issues/1162)) ([#1163](https://github.com/defilantech/LLMKube/issues/1163)) ([30dfaf2](https://github.com/defilantech/LLMKube/commit/30dfaf22958b8cd2d9aa98a58af4a1bd0d24da09))


### Documentation

* document strict-taint model caches ([#1132](https://github.com/defilantech/LLMKube/issues/1132)) ([9250996](https://github.com/defilantech/LLMKube/commit/9250996c42f377dfd2915cfded6fbb219d3fe7ce))

## [0.9.7](https://github.com/defilantech/LLMKube/compare/v0.9.6...v0.9.7) (2026-07-16)


### Features

* **cli:** envtest-verify the slicer's integrated branch (Fixes [#1137](https://github.com/defilantech/LLMKube/issues/1137)) ([#1141](https://github.com/defilantech/LLMKube/issues/1141)) ([a7622c2](https://github.com/defilantech/LLMKube/commit/a7622c2a988fe8f1479d2411a21d27ffd7a18405))
* **foreman:** guide coders to verify volatile external facts via MCP research tools ([#1134](https://github.com/defilantech/LLMKube/issues/1134)) ([390c945](https://github.com/defilantech/LLMKube/commit/390c945cf5d64beb46863fe974ba4c8e039a99cd))
* **foreman:** opt-in coder escalation on capability-wall failures ([#1108](https://github.com/defilantech/LLMKube/issues/1108)) ([#1128](https://github.com/defilantech/LLMKube/issues/1128)) ([68e2211](https://github.com/defilantech/LLMKube/commit/68e221129cbd3e512614faeca5ca8f6ad9e496d1))
* **metal-agent:** expose oMLX paged SSD cache configuration ([#278](https://github.com/defilantech/LLMKube/issues/278)) ([#1129](https://github.com/defilantech/LLMKube/issues/1129)) ([095e4a2](https://github.com/defilantech/LLMKube/commit/095e4a21733a2873d2282645c6d1337fc7a9e00f))
* **router:** in-memory rolling-window budget accounting engine ([#434](https://github.com/defilantech/LLMKube/issues/434)) ([#1130](https://github.com/defilantech/LLMKube/issues/1130)) ([c6ba850](https://github.com/defilantech/LLMKube/commit/c6ba8504a6135ea457290803c85416994356e0de))
* **scripts:** add foreman-finalize.sh post-GO branch finalizer ([#1126](https://github.com/defilantech/LLMKube/issues/1126)) ([8d17e25](https://github.com/defilantech/LLMKube/commit/8d17e251f9babb9534e273e34303a5edf6d1c8a7))
* Support pinned multi-file Hugging Face models ([#1110](https://github.com/defilantech/LLMKube/issues/1110)) ([#1138](https://github.com/defilantech/LLMKube/issues/1138)) ([e2d3441](https://github.com/defilantech/LLMKube/commit/e2d34410950d1ec91819bb7049d811f56852a1de))


### Bug Fixes

* **foreman:** abort orphaned agent loop when its AgenticTask is deleted (Fixes [#1136](https://github.com/defilantech/LLMKube/issues/1136)) ([#1140](https://github.com/defilantech/LLMKube/issues/1140)) ([471a00c](https://github.com/defilantech/LLMKube/commit/471a00c4811405fc251b9bd69a927d3e1c817637))
* **scripts:** foreman-finalize handles Foreman CRDs, clean-tree, and title override ([#1131](https://github.com/defilantech/LLMKube/issues/1131)) ([db3eeaf](https://github.com/defilantech/LLMKube/commit/db3eeafb3b5933591f7a796a99a41bb020cce77a))

## [0.9.6](https://github.com/defilantech/LLMKube/compare/v0.9.5...v0.9.6) (2026-07-14)


### Features

* **api:** add GPUQuota CRD types for multi-tenant GPU governance ([#1101](https://github.com/defilantech/LLMKube/issues/1101)) ([5dd867a](https://github.com/defilantech/LLMKube/commit/5dd867a5118dccdbb38a14e44a159d7cbcaf8727))
* **controller:** add GPUQuota status reconciler ([#1117](https://github.com/defilantech/LLMKube/issues/1117)) ([1d583e4](https://github.com/defilantech/LLMKube/commit/1d583e4cd611b5a5b19d2a73e254267c3f6464e1))
* **controller:** add GPUQuota validating webhook for InferenceService ([#1118](https://github.com/defilantech/LLMKube/issues/1118)) ([26804bd](https://github.com/defilantech/LLMKube/commit/26804bd8debe7c5884b50dcb5a2fafef69975926))
* **controller:** add s3:// model source via curl --aws-sigv4 ([#1098](https://github.com/defilantech/LLMKube/issues/1098)) ([#1125](https://github.com/defilantech/LLMKube/issues/1125)) ([ed35142](https://github.com/defilantech/LLMKube/commit/ed3514269a668328c571e8e7d6583251ed58f81d))
* **foreman:** preserve a coder's gate-failed branch instead of discarding it ([#1115](https://github.com/defilantech/LLMKube/issues/1115)) ([bc39c77](https://github.com/defilantech/LLMKube/commit/bc39c77fec4707b553d4925ebe7e7ea0304f680e))
* **helm:** gate InferenceService quota webhook and tenant RBAC behind multitenancy toggle ([#1122](https://github.com/defilantech/LLMKube/issues/1122)) ([26a994e](https://github.com/defilantech/LLMKube/commit/26a994ef54f71d84614abd9db8b789f0b57e4d2f))
* **quota:** add GPUQuota admission decision function ([#1107](https://github.com/defilantech/LLMKube/issues/1107)) ([4897093](https://github.com/defilantech/LLMKube/commit/4897093b557b589e0382c40832f0070eb00e2ef5))
* **runtime:** close out the SGLang kitchen-sink ([#1060](https://github.com/defilantech/LLMKube/issues/1060)): minor flags, accept thresholds, typed LoRA adapters, LoRAAdapter CRD ([#1103](https://github.com/defilantech/LLMKube/issues/1103)) ([8edf8bd](https://github.com/defilantech/LLMKube/commit/8edf8bdbcb49d7541c187bc750f673dc05581b12))


### Bug Fixes

* **foreman:** honor GateProfile source extensions in scope-overlap issue-ref extraction ([#1120](https://github.com/defilantech/LLMKube/issues/1120)) ([0c20431](https://github.com/defilantech/LLMKube/commit/0c20431fe280c56d542c17cf101fa5a168d1145d))


### Documentation

* **samples:** add air-gapped local file-path model source example ([#1099](https://github.com/defilantech/LLMKube/issues/1099)) ([#1124](https://github.com/defilantech/LLMKube/issues/1124)) ([913ff3d](https://github.com/defilantech/LLMKube/commit/913ff3d05b8e52266952d727361adaddd8ff501b))

## [0.9.5](https://github.com/defilantech/LLMKube/compare/v0.9.4...v0.9.5) (2026-07-13)


### Features

* **ci:** validate config/samples against CRD schemas (Fixes [#1021](https://github.com/defilantech/LLMKube/issues/1021)) ([#1083](https://github.com/defilantech/LLMKube/issues/1083)) ([b3ae847](https://github.com/defilantech/LLMKube/commit/b3ae84711c8447e6e33c02ef3c55d9c1dc7d3741))
* **cli:** add --planner-token for gateway-routed planner (Fixes [#1053](https://github.com/defilantech/LLMKube/issues/1053)) ([#1090](https://github.com/defilantech/LLMKube/issues/1090)) ([f71d2cc](https://github.com/defilantech/LLMKube/commit/f71d2cc425aefa093c08acd696896ba9ad6a7170))
* **controller:** add extraVolumes/extraVolumeMounts passthrough on InferenceService ([#1079](https://github.com/defilantech/LLMKube/issues/1079)) ([80e004c](https://github.com/defilantech/LLMKube/commit/80e004c3716c58f900ac9c43d2b5cd94ba25d896))
* **controller:** extend drain-before-roll idle checks to vLLM, TGI, SGLang, and multi-replica services ([#1088](https://github.com/defilantech/LLMKube/issues/1088)) ([1d884df](https://github.com/defilantech/LLMKube/commit/1d884dfa13627d2064e970727b5ae78cc52f2f10))
* **foreman:** expose CRD status as Prometheus metrics via CRS (Fixes [#1001](https://github.com/defilantech/LLMKube/issues/1001)) ([#1086](https://github.com/defilantech/LLMKube/issues/1086)) ([a459748](https://github.com/defilantech/LLMKube/commit/a45974879ed135c49bb38e525c8281e40de53c55))
* **foreman:** honest-verdict slice 1 (claim evidence + work-class policy) ([#1078](https://github.com/defilantech/LLMKube/issues/1078)) ([6f2c216](https://github.com/defilantech/LLMKube/commit/6f2c2167747c546d72476691bf371b410690008c))


### Bug Fixes

* **ci:** honor x-kubernetes-preserve-unknown-fields in validate-samples (Fixes [#1085](https://github.com/defilantech/LLMKube/issues/1085)) ([#1091](https://github.com/defilantech/LLMKube/issues/1091)) ([5447e88](https://github.com/defilantech/LLMKube/commit/5447e883983640e6718c6a5bce355e8ea77dd1d4))
* **cli:** scope slicer branch names by run id (Fixes [#1054](https://github.com/defilantech/LLMKube/issues/1054)) ([#1081](https://github.com/defilantech/LLMKube/issues/1081)) ([9288401](https://github.com/defilantech/LLMKube/commit/92884013ebf1140ece46971c960fe27f378025b2))
* **controller:** default Model accelerator from gpu.runtime (Fixes [#1074](https://github.com/defilantech/LLMKube/issues/1074)) ([#1087](https://github.com/defilantech/LLMKube/issues/1087)) ([0390c0f](https://github.com/defilantech/LLMKube/commit/0390c0f2248b04ab316a7183ad9108936ba819d3))
* **slicer:** reject trailing-underscore pins at plan validation (Fixes [#1058](https://github.com/defilantech/LLMKube/issues/1058)) ([#1082](https://github.com/defilantech/LLMKube/issues/1082)) ([d4118d3](https://github.com/defilantech/LLMKube/commit/d4118d3e090d243fe49e7be9f373468d02a08de8))
* **slicer:** surface pinned-prefix drift (Fixes [#1084](https://github.com/defilantech/LLMKube/issues/1084)) ([#1089](https://github.com/defilantech/LLMKube/issues/1089)) ([0f6edd2](https://github.com/defilantech/LLMKube/commit/0f6edd23b9852f0abdb08105ac80f8fdb4ac711c))


### Documentation

* **proposals:** honest-verdict harness design (declare-then-verify coder gates) ([#1076](https://github.com/defilantech/LLMKube/issues/1076)) ([630de33](https://github.com/defilantech/LLMKube/commit/630de33b069c2c52a2824666a77aa9963d1f0696))

## [0.9.4](https://github.com/defilantech/LLMKube/compare/v0.9.3...v0.9.4) (2026-07-11)


### Features

* **chart:** support multiple agent fleets in one release ([#1028](https://github.com/defilantech/LLMKube/issues/1028)) ([a9a6c6b](https://github.com/defilantech/LLMKube/commit/a9a6c6bf2e40efddb16df80ac0ba4bdc7f7a71bb))
* **cli:** --revise-from-branch on foreman dispatch for in-place PR refresh ([#1043](https://github.com/defilantech/LLMKube/issues/1043)) ([26f807b](https://github.com/defilantech/LLMKube/commit/26f807bfe28652ae628ab9e5eca1a9ba33781ede))
* **cli:** llmkube foreman slice renders a sliced Workload from a plan ([#1049](https://github.com/defilantech/LLMKube/issues/1049)) ([ce586ed](https://github.com/defilantech/LLMKube/commit/ce586ed73b5b51a6509273c0b08adf183db195a3))
* **cli:** plan an issue into slices in llmkube foreman slice ([#1050](https://github.com/defilantech/LLMKube/issues/1050)) ([c4a967c](https://github.com/defilantech/LLMKube/commit/c4a967c89735836980ea04e4666d69c9f2ca89c7))
* **foreman:** flag unverifiable facts instead of inventing them (slicer anti-confabulation) ([#1062](https://github.com/defilantech/LLMKube/issues/1062)) ([81bbfe9](https://github.com/defilantech/LLMKube/commit/81bbfe9f03fd10018dfce4e2fa2944c779b17fbe))
* **foreman:** integrate + reconcile AgenticTask kinds + slice payload ([#1037](https://github.com/defilantech/LLMKube/issues/1037)) ([0899e9f](https://github.com/defilantech/LLMKube/commit/0899e9f9651747df5837d46b229bca6de80b9cc3))
* **foreman:** integrate library for disjoint slice union ([#1036](https://github.com/defilantech/LLMKube/issues/1036)) ([39131f6](https://github.com/defilantech/LLMKube/commit/39131f6d707b9190385175c89fa8d4f718a9726e))
* **foreman:** make the EditFreeStreak guard grounding-aware ([#1067](https://github.com/defilantech/LLMKube/issues/1067)) ([63a71ae](https://github.com/defilantech/LLMKube/commit/63a71ae5e3278cf927f3f0e9c1c7e3781b77d36e))
* **foreman:** reconcile library for sliced-workload interface drift ([#1034](https://github.com/defilantech/LLMKube/issues/1034)) ([b14f687](https://github.com/defilantech/LLMKube/commit/b14f687b07c6afb88c9902f9a7812627cf8a4346))
* **foreman:** run_integrate tool for sliced-workload union ([#1045](https://github.com/defilantech/LLMKube/issues/1045)) ([5098b4e](https://github.com/defilantech/LLMKube/commit/5098b4ec86f11414f3e4de0df9040d0f0dd2853e))
* **foreman:** run_reconcile tool for sliced-workload drift check ([#1046](https://github.com/defilantech/LLMKube/issues/1046)) ([6bde143](https://github.com/defilantech/LLMKube/commit/6bde14346aee1acd53753688e9f8b016a5048874))
* **foreman:** ship default integrate/reconcile Agents for sliced workloads ([#1052](https://github.com/defilantech/LLMKube/issues/1052)) ([00dd7c0](https://github.com/defilantech/LLMKube/commit/00dd7c003a393698a467b2a19d0ea3f646b02684))
* **runtime:** add SGLang runtime backend ([#974](https://github.com/defilantech/LLMKube/issues/974)) ([#1059](https://github.com/defilantech/LLMKube/issues/1059)) ([9daa186](https://github.com/defilantech/LLMKube/commit/9daa186ae9ed6ff2b53c8f7d4c193daa19343047))


### Bug Fixes

* **cli:** keep the slicer planner output to YAML only (fixes [#1062](https://github.com/defilantech/LLMKube/issues/1062) regression) ([#1064](https://github.com/defilantech/LLMKube/issues/1064)) ([5d0eb9c](https://github.com/defilantech/LLMKube/commit/5d0eb9c67e85450e40c73095d52408f50091f9cd))
* **foreman:** add payload.branchStrategy to stop stale revisions reverting merged work ([#1042](https://github.com/defilantech/LLMKube/issues/1042)) ([9f806d4](https://github.com/defilantech/LLMKube/commit/9f806d476befe91d10afda0d682115720d6abc55))
* **foreman:** anchor recovery soft-reset at merge-base branch point ([#1044](https://github.com/defilantech/LLMKube/issues/1044)) ([2492d15](https://github.com/defilantech/LLMKube/commit/2492d15b1b7f8245f043bf22dbd62c52f3b5ad53))
* **foreman:** default to rebase when reviseFromBranch is set without branchStrategy ([#1065](https://github.com/defilantech/LLMKube/issues/1065)) ([2792634](https://github.com/defilantech/LLMKube/commit/27926347e67f4042f0b73f6f7452ac4318ce0c1d))
* **foreman:** set fsGroup on the gate pod so non-root gates can write XDG_DATA_HOME ([#1068](https://github.com/defilantech/LLMKube/issues/1068)) ([7465e2b](https://github.com/defilantech/LLMKube/commit/7465e2bbb0764ad3ff2d5f12a7cce3db3202ea32))
* **foreman:** stop the coder from running envtest it cannot run ([#1070](https://github.com/defilantech/LLMKube/issues/1070)) ([bf203fe](https://github.com/defilantech/LLMKube/commit/bf203fe04ba3452b5ffc167cbab3b4578a09c5d6))
* **release:** publish a cross-platform Homebrew formula, not a macOS-only cask ([#1041](https://github.com/defilantech/LLMKube/issues/1041)) ([32dcfea](https://github.com/defilantech/LLMKube/commit/32dcfea7d90c73d1cf333bafe61a1bac0abd6fcf))
* **slicer:** give the union commit an explicit git identity ([#1057](https://github.com/defilantech/LLMKube/issues/1057)) ([15d906f](https://github.com/defilantech/LLMKube/commit/15d906fe58b6edeb69837833ab7927add3ae69cd))

## [0.9.3](https://github.com/defilantech/LLMKube/compare/v0.9.2...v0.9.3) (2026-07-09)


### Bug Fixes

* **controller:** drop invalid --enable-metrics flag from vLLM runtime ([#1031](https://github.com/defilantech/LLMKube/issues/1031)) ([9b9450d](https://github.com/defilantech/LLMKube/commit/9b9450dd2cdaf1df5874209778f11eaf2ba89bbc))

## [0.9.2](https://github.com/defilantech/LLMKube/compare/v0.9.1...v0.9.2) (2026-07-09)


### Features

* **foreman:** coder grounding rail (flag doc-contradicting metric writes) ([#1017](https://github.com/defilantech/LLMKube/issues/1017)) ([aa969ed](https://github.com/defilantech/LLMKube/commit/aa969ed6c490ef075aeb4ec0f5cabdd71c384c7e))
* **foreman:** distinct ALREADY-RESOLVED coder outcome ([#970](https://github.com/defilantech/LLMKube/issues/970)) ([#1011](https://github.com/defilantech/LLMKube/issues/1011)) ([8e041e0](https://github.com/defilantech/LLMKube/commit/8e041e0d56b2b0f0649d87954f97c6ebeb7a8198))
* **foreman:** flag a GO that changes no functional production code ([#1024](https://github.com/defilantech/LLMKube/issues/1024)) ([3e1a6fb](https://github.com/defilantech/LLMKube/commit/3e1a6fb32bcdb7156eec17ea5fdf07823b40108e))
* **foreman:** MCP client for agents (Phase 1: HTTP transport, context7) ([#1014](https://github.com/defilantech/LLMKube/issues/1014)) ([d34343f](https://github.com/defilantech/LLMKube/commit/d34343fb8a53cec4bf7caa41380fcf2dfd253835))
* **foreman:** semantic issueAsk verification instead of verbatim substring ([#1020](https://github.com/defilantech/LLMKube/issues/1020)) ([85d92ee](https://github.com/defilantech/LLMKube/commit/85d92eebaab65aa902675ef2d70f1d97283901c6))
* **foreman:** str_replace escalates to write_file after repeated failures ([#1026](https://github.com/defilantech/LLMKube/issues/1026)) ([de1b8cb](https://github.com/defilantech/LLMKube/commit/de1b8cbc89d1a9da6de123156b7563b2a5ae513b))


### Bug Fixes

* **foreman:** exempt REJECT from grounded-finding demotion ([#1008](https://github.com/defilantech/LLMKube/issues/1008)) ([18098cb](https://github.com/defilantech/LLMKube/commit/18098cb3b98d4a947da8ffc76b03ac706fa8d832))
* **foreman:** gate Job downloads the go.mod toolchain (GOTOOLCHAIN=auto) ([#1019](https://github.com/defilantech/LLMKube/issues/1019)) ([5f4c51c](https://github.com/defilantech/LLMKube/commit/5f4c51cf04d1755dcf666c35ae35d03fe5b57f53))
* **foreman:** guard file-less findings + resolve bare/absolute paths in grounding rail ([#1009](https://github.com/defilantech/LLMKube/issues/1009)) ([304a2b8](https://github.com/defilantech/LLMKube/commit/304a2b851e7ae1135ecc9ca5dc2ff1627696917f))
* **foreman:** reap audit ConfigMaps older than retention ([#990](https://github.com/defilantech/LLMKube/issues/990)) ([#1027](https://github.com/defilantech/LLMKube/issues/1027)) ([08d06d6](https://github.com/defilantech/LLMKube/commit/08d06d6bd3595af114cc21ae9c492202c1184893))
* **foreman:** reviewer diffs against the upstream base, not stale local fork main ([#1006](https://github.com/defilantech/LLMKube/issues/1006)) ([6ef7278](https://github.com/defilantech/LLMKube/commit/6ef72781ad1f74e4f703a53b57df46dde5ee9d09))
* **metrics:** remove dead metrics + phantom PrometheusRule error-rate rule ([#1010](https://github.com/defilantech/LLMKube/issues/1010)) ([7c92f3f](https://github.com/defilantech/LLMKube/commit/7c92f3fa6d1b6f9a7b9f1ea8a9e13efe9d77262a))

## [0.9.1](https://github.com/defilantech/LLMKube/compare/v0.9.0...v0.9.1) (2026-07-07)


### Features

* **chart:** gate CRD installation behind crds.enabled toggle ([#998](https://github.com/defilantech/LLMKube/issues/998)) ([acf2e6a](https://github.com/defilantech/LLMKube/commit/acf2e6a23de8072ddd62ff129be7300ec8c12bf1))
* **foreman:** append actionable steers to structural-lint gate feedback ([#984](https://github.com/defilantech/LLMKube/issues/984)) ([4d496c6](https://github.com/defilantech/LLMKube/commit/4d496c6b0a01365ecfbbe6a80ad6ff392fe91f61))
* **foreman:** closest-line fallback when str_replace has no unique anchor ([#1000](https://github.com/defilantech/LLMKube/issues/1000)) ([02ec677](https://github.com/defilantech/LLMKube/commit/02ec6776634ee6732b2c238f0921d691c67e1264))
* **foreman:** grounded-finding rail for reviewer NO-GO verdicts ([#988](https://github.com/defilantech/LLMKube/issues/988)) ([cadbd30](https://github.com/defilantech/LLMKube/commit/cadbd3035253886accb37a5ded49291be5bda512))
* **foreman:** verdict-from-findings rail (promote GO with a grounded blocker) ([#992](https://github.com/defilantech/LLMKube/issues/992)) ([b82fcca](https://github.com/defilantech/LLMKube/commit/b82fccab3f8408583df6c70ec4a625b1d1f6bfa7))


### Bug Fixes

* **cachekey:** unify model cache-key derivation across controller and CLI ([#985](https://github.com/defilantech/LLMKube/issues/985)) ([1fb122c](https://github.com/defilantech/LLMKube/commit/1fb122c77a88b3acce1e335645354f618ef2ae24))
* **foreman:** grant coder Job pods get on secrets for cloud-proxy auth ([#987](https://github.com/defilantech/LLMKube/issues/987)) ([ffe45d7](https://github.com/defilantech/LLMKube/commit/ffe45d7e661d4a02a3525a8b20564eff0b472fb3))
* **foreman:** key test-presence gate on net-new functions only ([#983](https://github.com/defilantech/LLMKube/issues/983)) ([0236630](https://github.com/defilantech/LLMKube/commit/0236630ec16cbb9ef6108adfc443e405834ea348))
* **foreman:** tolerate self-committed model work and detect git apply edits ([#995](https://github.com/defilantech/LLMKube/issues/995)) ([730d1cd](https://github.com/defilantech/LLMKube/commit/730d1cd17c4d2fa4c9ec39e3eeaf86fecafb9b3c))
* **foreman:** truncate over-length submit_result summaries instead of rejecting ([#999](https://github.com/defilantech/LLMKube/issues/999)) ([8c36edd](https://github.com/defilantech/LLMKube/commit/8c36edd734ed200bbee6bb89f7fe7c03ff56e527))


### Documentation

* **cachekey:** note the metal literal mirrors acceleratorMetal ([#993](https://github.com/defilantech/LLMKube/issues/993)) ([ae9e20d](https://github.com/defilantech/LLMKube/commit/ae9e20d800b5a6ad486d7e2cce5fc4dc40178c91))

## [0.9.0](https://github.com/defilantech/LLMKube/compare/v0.8.28...v0.9.0) (2026-07-05)


### ⚠ BREAKING CHANGES

* **security:** gate local/hostPath model sources + block controller SSRF (GHSA-jw3m-8q7m-f35r) ([#981](https://github.com/defilantech/LLMKube/issues/981))

### Features

* **foreman:** use the reviewer's summary as the opened-PR body ([#975](https://github.com/defilantech/LLMKube/issues/975)) ([e031ff5](https://github.com/defilantech/LLMKube/commit/e031ff56c892f1cde0908f7939ba9818278abcfd))


### Bug Fixes

* **foreman:** populate FleetNode.Status.CurrentTask so the scheduler spreads work ([#978](https://github.com/defilantech/LLMKube/issues/978)) ([d731a90](https://github.com/defilantech/LLMKube/commit/d731a9020c5f01cc7a03d2ff562549731c8a08d9))
* **foreman:** reap orphaned Draining FleetNodes so they stop leaking ([#980](https://github.com/defilantech/LLMKube/issues/980)) ([241ac4a](https://github.com/defilantech/LLMKube/commit/241ac4a036a9338203691f6789eeddb0cab1171b))
* **runtime:** bind inference servers to :: for IPv6-only clusters ([#973](https://github.com/defilantech/LLMKube/issues/973)) ([398d5c1](https://github.com/defilantech/LLMKube/commit/398d5c1582266cae66f50e5bbd4e5df84023e361))
* **security:** gate local/hostPath model sources + block controller SSRF (GHSA-jw3m-8q7m-f35r) ([#981](https://github.com/defilantech/LLMKube/issues/981)) ([ec2647f](https://github.com/defilantech/LLMKube/commit/ec2647fd4557962a971a1bc9435790b8e77ce961))

## [0.8.28](https://github.com/defilantech/LLMKube/compare/v0.8.27...v0.8.28) (2026-07-04)


### Features

* **foreman:** bounded fix iteration on reviewer NO-GO instead of terminal failure ([#959](https://github.com/defilantech/LLMKube/issues/959)) ([d820fff](https://github.com/defilantech/LLMKube/commit/d820fffa8839a591d448f5368bee9f1c9e3d58a2))
* **foreman:** coder-tier escalation, re-dispatch a failed coder to a larger model ([#964](https://github.com/defilantech/LLMKube/issues/964)) ([ce8f655](https://github.com/defilantech/LLMKube/commit/ce8f6555115602c3bcc7b116a6bf63471ec3638d))
* **foreman:** executor-owned revise-from-branch restore for revision tasks ([#967](https://github.com/defilantech/LLMKube/issues/967)) ([b76051c](https://github.com/defilantech/LLMKube/commit/b76051c77ab352a7d2585af87b0fa53cc5a11522))
* **foreman:** open the pull request on review GO ([#956](https://github.com/defilantech/LLMKube/issues/956)) ([fd852e1](https://github.com/defilantech/LLMKube/commit/fd852e1957874fda837e5d96dc4b2c28f4116d38))
* **inference:** add spec.modelCache.claimName for user-owned cache PVCs ([#960](https://github.com/defilantech/LLMKube/issues/960)) ([aab5a58](https://github.com/defilantech/LLMKube/commit/aab5a58bf70b4752ab94a53d0f5175e877327172))


### Bug Fixes

* **foreman:** accept workspace-internal absolute paths in resolveInside ([#957](https://github.com/defilantech/LLMKube/issues/957)) ([34b126c](https://github.com/defilantech/LLMKube/commit/34b126c27465471bac0b89c9f9f32824e56aa0c4))
* **foreman:** defer generic self-gate when runtime is missing from coder image ([#958](https://github.com/defilantech/LLMKube/issues/958)) ([df185ec](https://github.com/defilantech/LLMKube/commit/df185ec9d3538c9e524fbc54d5fbef2ecbaaf2e9))
* **foreman:** reject no-op str_replace where old_string equals new_string ([#969](https://github.com/defilantech/LLMKube/issues/969)) ([c71f38b](https://github.com/defilantech/LLMKube/commit/c71f38b2d0818e26311dbfed39204195adbcd60d))
* **foreman:** scope-overlap check catches Go files in new directories ([#962](https://github.com/defilantech/LLMKube/issues/962)) ([486a944](https://github.com/defilantech/LLMKube/commit/486a944cb4630a8da1fe6126ed267469ab271bbf))
* **inference:** warn when modelCache.claimName is silently ignored ([#966](https://github.com/defilantech/LLMKube/issues/966)) ([d49cd22](https://github.com/defilantech/LLMKube/commit/d49cd2202a74d92de3d188c7439b1d69d69362ad))


### Documentation

* register the Karpenter GPU autoscaling guide in nav.yaml ([#954](https://github.com/defilantech/LLMKube/issues/954)) ([88b9c7d](https://github.com/defilantech/LLMKube/commit/88b9c7d17d1565193cd8c4be9d1f85482b070aa5))

## [0.8.27](https://github.com/defilantech/LLMKube/compare/v0.8.26...v0.8.27) (2026-07-02)


### Features

* **foreman:** str_replace fuzzy recovery for old_string drift + write_file steering ([#943](https://github.com/defilantech/LLMKube/issues/943)) ([bab4631](https://github.com/defilantech/LLMKube/commit/bab463120c56a4067ebf0fa17eecfa9e96a5902a))


### Bug Fixes

* **foreman:** agent claims tasks depth-first so verify/review don't starve ([#947](https://github.com/defilantech/LLMKube/issues/947)) ([323423d](https://github.com/defilantech/LLMKube/commit/323423d51f5d53d5988c7e50e746c4582b06b630))
* **foreman:** chart exposes --foreman-namespace so gate Jobs land with the cache PVC ([#939](https://github.com/defilantech/LLMKube/issues/939)) ([0a9ff11](https://github.com/defilantech/LLMKube/commit/0a9ff11bd870099d5ed12e38e3adcb2176ea9635))
* **foreman:** do not resynthesize children on a terminating Workload ([#950](https://github.com/defilantech/LLMKube/issues/950)) ([f90cc49](https://github.com/defilantech/LLMKube/commit/f90cc49c55961c90d1aaaa7e1e1b375258bec07d))
* **foreman:** replace stale agent-owned branches on push rejection (force-with-lease) ([#948](https://github.com/defilantech/LLMKube/issues/948)) ([c55c6ce](https://github.com/defilantech/LLMKube/commit/c55c6cea3b962a610490565ac9ab010e61761548))
* **foreman:** substitute placeholder for empty assistant replies to avoid poisoning history ([#940](https://github.com/defilantech/LLMKube/issues/940)) ([6eb67f3](https://github.com/defilantech/LLMKube/commit/6eb67f38cc1486fbe3034006f34bde4a8c9d6993))


### Documentation

* metrics-driven autoscaling tutorial for InferenceService (HPA, custom metrics, node autoscaling) ([#952](https://github.com/defilantech/LLMKube/issues/952)) ([ebd5996](https://github.com/defilantech/LLMKube/commit/ebd5996eada69d7f4569be74f7660989693de925))

## [0.8.26](https://github.com/defilantech/LLMKube/compare/v0.8.25...v0.8.26) (2026-07-02)


### Features

* **foreman:** two-tier gate-check suite for the coder gate ([#931](https://github.com/defilantech/LLMKube/issues/931)) ([f7f469e](https://github.com/defilantech/LLMKube/commit/f7f469e60b9e46466e8125936cdea39257344147))

## [0.8.25](https://github.com/defilantech/LLMKube/compare/v0.8.24...v0.8.25) (2026-07-01)


### Features

* **foreman:** coder clones/pushes each task's own repo when no static remote ([#924](https://github.com/defilantech/LLMKube/issues/924)) ([88c334d](https://github.com/defilantech/LLMKube/commit/88c334df23d694b3db0779f94ef04ba2b203992a))

## [0.8.24](https://github.com/defilantech/LLMKube/compare/v0.8.23...v0.8.24) (2026-07-01)


### Bug Fixes

* preserve init-container SecurityContext in podTemplatesDiffer ([#922](https://github.com/defilantech/LLMKube/issues/922)) ([#923](https://github.com/defilantech/LLMKube/issues/923)) ([859841e](https://github.com/defilantech/LLMKube/commit/859841e7026c7c65ef850af89a3639851c2ca98b))


### Documentation

* add Jory Irving as Foreman area maintainer ([#926](https://github.com/defilantech/LLMKube/issues/926)) ([7513176](https://github.com/defilantech/LLMKube/commit/751317688dae26eb01b6931d648c23bb05169fe5))

## [0.8.23](https://github.com/defilantech/LLMKube/compare/v0.8.22...v0.8.23) (2026-07-01)


### Features

* **controller:** defer rollouts until idle ([#913](https://github.com/defilantech/LLMKube/issues/913)) ([aa404ff](https://github.com/defilantech/LLMKube/commit/aa404ff0dd49e376bd4c50bffbc553a39db33791))
* expose oMLX TurboQuant KV cache quantization settings ([#905](https://github.com/defilantech/LLMKube/issues/905)) ([68e0291](https://github.com/defilantech/LLMKube/commit/68e02916ab594625c33daa008b1d1b025362594c))
* **foreman:** controller/metal-agent runtime-arg parity guard ([#918](https://github.com/defilantech/LLMKube/issues/918)) ([92d3c1b](https://github.com/defilantech/LLMKube/commit/92d3c1ba649ff3d11d3ab009cef6fe1788e3a363))
* **foreman:** propagate gateProfile from Workload to decomposed tasks ([#920](https://github.com/defilantech/LLMKube/issues/920)) ([270b2fa](https://github.com/defilantech/LLMKube/commit/270b2facc0bfdb706fba7be5daebc55a9f29ae2f))
* **foreman:** reference-grounding check in the coder gate ([#906](https://github.com/defilantech/LLMKube/issues/906)) ([6296496](https://github.com/defilantech/LLMKube/commit/62964966b514c1bed0705f74086dc56d4089edba))


### Bug Fixes

* cache hf:// multi-file models instead of emptyDir ([#912](https://github.com/defilantech/LLMKube/issues/912)) ([8e69d74](https://github.com/defilantech/LLMKube/commit/8e69d74a63d0f3ebb3dec3c4c95214130f9b2580))
* **foreman:** recover str_replace from near-miss old_string ([#917](https://github.com/defilantech/LLMKube/issues/917)) ([4ce29e8](https://github.com/defilantech/LLMKube/commit/4ce29e85f8c3945d17acb50e3b140225b0fe3863))
* grant events.k8s.io to the operator event recorder ([#914](https://github.com/defilantech/LLMKube/issues/914)) ([5b23c4b](https://github.com/defilantech/LLMKube/commit/5b23c4bc2b1ce77271f42456e2c90afb78d2b655))

## [0.8.22](https://github.com/defilantech/LLMKube/compare/v0.8.21...v0.8.22) (2026-06-29)


### Features

* **foreman:** coder gate mutation checks (test-presence + neuter-survival) ([#899](https://github.com/defilantech/LLMKube/issues/899)) ([b235932](https://github.com/defilantech/LLMKube/commit/b235932461106ac887d77223e48f79163db15f15))


### Bug Fixes

* **foreman:** don't force-terminate a coder mid-verification after it has edited ([#896](https://github.com/defilantech/LLMKube/issues/896)) ([#898](https://github.com/defilantech/LLMKube/issues/898)) ([b097c29](https://github.com/defilantech/LLMKube/commit/b097c29963ec1c1588ac53b0ca7e4ce477630566))
* **foreman:** stamp real AgenticTask name on the post-push gate Job (not 'unknown') ([#894](https://github.com/defilantech/LLMKube/issues/894)) ([f8096d4](https://github.com/defilantech/LLMKube/commit/f8096d465ee030b3685cefc9563bdf5e04bb10ac))
* **metal-agent:** reap orphaned legacy Endpoints so mirror slices stop blackholing traffic ([#895](https://github.com/defilantech/LLMKube/issues/895)) ([6274fc5](https://github.com/defilantech/LLMKube/commit/6274fc5135c44eb635f0cd6289a3fd10dcdb1d2d))

## [0.8.21](https://github.com/defilantech/LLMKube/compare/v0.8.20...v0.8.21) (2026-06-28)


### Bug Fixes

* **controller:** run model-cache-prep init as root again (0.8.20 regression) ([#888](https://github.com/defilantech/LLMKube/issues/888)) ([8e98e50](https://github.com/defilantech/LLMKube/commit/8e98e500960bdbdf005b61e21ca2396132f1e141))


### Documentation

* **strix:** document amdgpu.lockup_timeout for device-lost resets ([#886](https://github.com/defilantech/LLMKube/issues/886)) ([4468d62](https://github.com/defilantech/LLMKube/commit/4468d6286f6d31d85337041d58e074f2ff66e7e9))

## [0.8.20](https://github.com/defilantech/LLMKube/compare/v0.8.19...v0.8.20) (2026-06-28)


### Features

* add spec.mode to auto-serve embedding and rerank models ([#866](https://github.com/defilantech/LLMKube/issues/866)) ([a7fa09f](https://github.com/defilantech/LLMKube/commit/a7fa09f295039ae7d3328f9155e745935931e9af))
* **cli:** add `llmkube foreman dispatch` to fan issues across coder agents ([#879](https://github.com/defilantech/LLMKube/issues/879)) ([1530754](https://github.com/defilantech/LLMKube/commit/1530754dbff1a1e6e47a187bbbcd71515d2ce4ef))
* **controller:** run model-cache-prep init non-root + document PSA/multi-tenancy caveats ([#883](https://github.com/defilantech/LLMKube/issues/883)) ([a8049f1](https://github.com/defilantech/LLMKube/commit/a8049f1c5de258efb54c32167114cae1143ef34a))
* **foreman:** add a goreleaser-config check to the coder gate ([#882](https://github.com/defilantech/LLMKube/issues/882)) ([c8535a7](https://github.com/defilantech/LLMKube/commit/c8535a7c14a3d50eb9e1bbd3d7da464bd8424767))
* **foreman:** add vulkan to the accelerator routing enum ([#878](https://github.com/defilantech/LLMKube/issues/878)) ([315dadc](https://github.com/defilantech/LLMKube/commit/315dadc53286c88f50bd50a4a2093403899488d5))
* **foreman:** branch the coder off upstream/main, not the stale fork base ([#875](https://github.com/defilantech/LLMKube/issues/875)) ([f4c382c](https://github.com/defilantech/LLMKube/commit/f4c382c168dc9acb989f5d9518492cd9f73763a5))
* **foreman:** deterministically resolve codegen drift in the coder gate ([#874](https://github.com/defilantech/LLMKube/issues/874)) ([332f7eb](https://github.com/defilantech/LLMKube/commit/332f7eb6b14fa01156ff4226622e429e852309bb))
* **foreman:** durable FleetNode labels via --node-labels (survive pod restart) ([#885](https://github.com/defilantech/LLMKube/issues/885)) ([fdda5a8](https://github.com/defilantech/LLMKube/commit/fdda5a8f0b99fe1da528659ea54cfece091983d7))
* **foreman:** scope-overlap guard catches wrong-subsystem coder drift ([#876](https://github.com/defilantech/LLMKube/issues/876)) ([795bf23](https://github.com/defilantech/LLMKube/commit/795bf2380168d0351b3259205f6644b59a581bfe))


### Bug Fixes

* **foreman:** raise coder agent memory limit default to 8Gi ([#884](https://github.com/defilantech/LLMKube/issues/884)) ([8b3ee32](https://github.com/defilantech/LLMKube/commit/8b3ee321fb12597394782e1691989d84f05bd625))


### Documentation

* add AI-assisted contribution policy ([#872](https://github.com/defilantech/LLMKube/issues/872)) ([e65f2fa](https://github.com/defilantech/LLMKube/commit/e65f2fa90f88982390e83e905b123a9d7e114579))
* **foreman:** enable a coder-capable in-cluster agent (image in llmkube-runtimes) ([#877](https://github.com/defilantech/LLMKube/issues/877)) ([9bc3d2c](https://github.com/defilantech/LLMKube/commit/9bc3d2cd280f87449c62d672c70c745bb85d36a3))

## [0.8.19](https://github.com/defilantech/LLMKube/compare/v0.8.18...v0.8.19) (2026-06-27)


### Features

* **foreman:** detect changed envtest packages + gate-job delegation seam (part 1 of [#859](https://github.com/defilantech/LLMKube/issues/859)) ([#860](https://github.com/defilantech/LLMKube/issues/860)) ([c745127](https://github.com/defilantech/LLMKube/commit/c7451274e4c47ffb2c1c481ea7ba33f67ddc4a84))
* **foreman:** ModelProfile CRD for per-model harness tuning ([#862](https://github.com/defilantech/LLMKube/issues/862)) ([#863](https://github.com/defilantech/LLMKube/issues/863)) ([90ad28b](https://github.com/defilantech/LLMKube/commit/90ad28bca52fcb04128fdf6f44bef8bbc4698203))
* **foreman:** post-push envtest gate (closes the in-loop timing flaw) ([#859](https://github.com/defilantech/LLMKube/issues/859)) ([#864](https://github.com/defilantech/LLMKube/issues/864)) ([71da4e2](https://github.com/defilantech/LLMKube/commit/71da4e2ed383a064377be6252f3d8577418478b3))


### Bug Fixes

* **controller:** make model cache writable on fsGroupPolicy=None CSIs ([#855](https://github.com/defilantech/LLMKube/issues/855)) ([#858](https://github.com/defilantech/LLMKube/issues/858)) ([8e1a30d](https://github.com/defilantech/LLMKube/commit/8e1a30d1ab3a734c54ca2379ecaaaf431372bc76))

## [0.8.18](https://github.com/defilantech/LLMKube/compare/v0.8.17...v0.8.18) (2026-06-26)


### Features

* **model:** stage multi-file artifacts ([#853](https://github.com/defilantech/LLMKube/issues/853)) ([ca22b84](https://github.com/defilantech/LLMKube/commit/ca22b84f508219269d4c6ca3df32b254bf70163c))
* **router:** TTFT + budget-utilization metrics, backend.provider span attr, Grafana dashboard ([#852](https://github.com/defilantech/LLMKube/issues/852)) ([b82cde1](https://github.com/defilantech/LLMKube/commit/b82cde1408adb33843c15f4d6bff7996d91ffcb3))


### Bug Fixes

* **controller:** propagate backend displayName into router-proxy config ([#849](https://github.com/defilantech/LLMKube/issues/849)) ([9f3b360](https://github.com/defilantech/LLMKube/commit/9f3b360b167034c5a898d1a506674df8ad5bb430))


### Documentation

* **foreman:** fix overview link that 404s the site build ([#848](https://github.com/defilantech/LLMKube/issues/848)) ([2f9b82b](https://github.com/defilantech/LLMKube/commit/2f9b82b3f84ccc3564f201a1574bd9133b0c41b3))
* **foreman:** non-Go projects (language gates) page ([#845](https://github.com/defilantech/LLMKube/issues/845)) ([d48cf6d](https://github.com/defilantech/LLMKube/commit/d48cf6d8fe2e942e204eec6686b62a64c3da5bdc))

## [0.8.17](https://github.com/defilantech/LLMKube/compare/v0.8.16...v0.8.17) (2026-06-25)


### Features

* **cli:** explicit model-cache control flags on deploy/delete (Fixes [#722](https://github.com/defilantech/LLMKube/issues/722)) ([#830](https://github.com/defilantech/LLMKube/issues/830)) ([9fc4187](https://github.com/defilantech/LLMKube/commit/9fc41876649a15f03d3121cd6a7bda07458b53af))
* **foreman:** durable audit-log record + llmkube audit export ([#837](https://github.com/defilantech/LLMKube/issues/837)) ([#838](https://github.com/defilantech/LLMKube/issues/838)) ([57d2ad0](https://github.com/defilantech/LLMKube/commit/57d2ad08ce24cf690ce9fd9e99adce0bce8f145e))
* **foreman:** GateProfile API type + language presets + resolver (Addresses [#839](https://github.com/defilantech/LLMKube/issues/839)) ([#840](https://github.com/defilantech/LLMKube/issues/840)) ([165a375](https://github.com/defilantech/LLMKube/commit/165a375ec52a2e9a786e2d0eca67853f3be0e9a8))
* **foreman:** run the fast gate from the resolved GateProfile (Addresses [#839](https://github.com/defilantech/LLMKube/issues/839)) ([#841](https://github.com/defilantech/LLMKube/issues/841)) ([4225da4](https://github.com/defilantech/LLMKube/commit/4225da46950ef467693300369480f69d89f57ec5))
* **foreman:** scope guard reads source extensions from GateProfile (Addresses [#839](https://github.com/defilantech/LLMKube/issues/839)) ([#842](https://github.com/defilantech/LLMKube/issues/842)) ([c36ac87](https://github.com/defilantech/LLMKube/commit/c36ac8795588f43a2560f5a0e0f375052c52817d))
* **foreman:** verify-gate Job image from GateProfile (Addresses [#839](https://github.com/defilantech/LLMKube/issues/839)) ([#843](https://github.com/defilantech/LLMKube/issues/843)) ([327dd88](https://github.com/defilantech/LLMKube/commit/327dd88e8f7f547a46f3f1ce656aa87477c73132))
* **foreman:** verify-gate Job runs resolved commands for non-Go profiles (Addresses [#839](https://github.com/defilantech/LLMKube/issues/839)) ([#844](https://github.com/defilantech/LLMKube/issues/844)) ([84994fd](https://github.com/defilantech/LLMKube/commit/84994fd0283a92faf182b0761f66e83ad3735eea))
* **inferenceservice:** speculativeDecoding for llama.cpp MTP/draft (Fixes [#502](https://github.com/defilantech/LLMKube/issues/502)) ([#827](https://github.com/defilantech/LLMKube/issues/827)) ([564726a](https://github.com/defilantech/LLMKube/commit/564726a773e06a2eab8fb2951836ca2891dd7a50))
* **router:** optional backend displayName for /v1/models id (Fixes [#792](https://github.com/defilantech/LLMKube/issues/792)) ([#826](https://github.com/defilantech/LLMKube/issues/826)) ([7090f83](https://github.com/defilantech/LLMKube/commit/7090f832557e9c520cb5e3a25a92276f026add2f))
* **router:** Prometheus metrics + OTel spans for routing decisions (Addresses [#433](https://github.com/defilantech/LLMKube/issues/433)) ([#834](https://github.com/defilantech/LLMKube/issues/834)) ([b8fa96d](https://github.com/defilantech/LLMKube/commit/b8fa96d52e5cd73e3d3b9a8dfa210be8b3f5f5dd))


### Bug Fixes

* **foreman:** scope guard skips check on zero-Go-file diffs; NUL-safe git status (Fixes [#800](https://github.com/defilantech/LLMKube/issues/800)) ([#823](https://github.com/defilantech/LLMKube/issues/823)) ([92e5501](https://github.com/defilantech/LLMKube/commit/92e5501b0faaf85d348f7dcfcb2bab17f5269e4e))


### Documentation

* **examples:** spot-capacity GPU NodePool for Foreman gate Jobs (Fixes [#659](https://github.com/defilantech/LLMKube/issues/659)) ([#824](https://github.com/defilantech/LLMKube/issues/824)) ([9fa4846](https://github.com/defilantech/LLMKube/commit/9fa4846338ac6bd84c0f7ae57bb1da2d6cb49231))
* Karpenter GPU autoscaling guide (Fixes [#658](https://github.com/defilantech/LLMKube/issues/658)) ([#825](https://github.com/defilantech/LLMKube/issues/825)) ([2d586ef](https://github.com/defilantech/LLMKube/commit/2d586efecb539e236b387609974e73f270cea18e))

## [0.8.16](https://github.com/defilantech/LLMKube/compare/v0.8.15...v0.8.16) (2026-06-24)


### Bug Fixes

* derive BTP perRetry timeout from configured router timeout (Fixes [#817](https://github.com/defilantech/LLMKube/issues/817)) ([#818](https://github.com/defilantech/LLMKube/issues/818)) ([5c41d1e](https://github.com/defilantech/LLMKube/commit/5c41d1e42406df3c3942cb8697e67a7be7ae6b32))
* **foreman:** retry transient SSE/transport errors in the OAI client ([#815](https://github.com/defilantech/LLMKube/issues/815)) ([#816](https://github.com/defilantech/LLMKube/issues/816)) ([1bf8d3d](https://github.com/defilantech/LLMKube/commit/1bf8d3dabcfee3d78d2d19cae19bf3cac84f233f))


### Documentation

* **readme:** lead hero with heterogeneous hardware + Foreman; add AMD to comparison ([#820](https://github.com/defilantech/LLMKube/issues/820)) ([1439ae5](https://github.com/defilantech/LLMKube/commit/1439ae546b82a6635e35b32c959ac3952a801e3d))

## [0.8.15](https://github.com/defilantech/LLMKube/compare/v0.8.14...v0.8.15) (2026-06-23)


### Features

* **foreman:** reviewer.md single source of truth for reviewer prompts ([#804](https://github.com/defilantech/LLMKube/issues/804)) ([#812](https://github.com/defilantech/LLMKube/issues/812)) ([7b62b17](https://github.com/defilantech/LLMKube/commit/7b62b1766eb46693d122b6f3d753a94c9fb35ac5))


### Bug Fixes

* **foreman:** make the verify-gate bite check actually run, enable by default ([#805](https://github.com/defilantech/LLMKube/issues/805)) ([acbe631](https://github.com/defilantech/LLMKube/commit/acbe631b5ba28982ba6e08a526cfe64c53095797))
* **metrics:** remove inert InferenceTTFT and InferenceRequestErrors metrics ([#786](https://github.com/defilantech/LLMKube/issues/786)) ([#810](https://github.com/defilantech/LLMKube/issues/810)) ([9e42217](https://github.com/defilantech/LLMKube/commit/9e42217fe612946c6479d90988b7909c69f1dfaf))

## [0.8.14](https://github.com/defilantech/LLMKube/compare/v0.8.13...v0.8.14) (2026-06-22)


### Features

* add defaultRouteStrategy with BackendNameMatch to ModelRouter ([#791](https://github.com/defilantech/LLMKube/issues/791)) ([b7da779](https://github.com/defilantech/LLMKube/commit/b7da779b3eac8ba76a376ef3e49596759831ffbe))
* add topologySpreadConstraints and affinity to InferenceService ([#793](https://github.com/defilantech/LLMKube/issues/793)) ([#794](https://github.com/defilantech/LLMKube/issues/794)) ([0a24fed](https://github.com/defilantech/LLMKube/commit/0a24fed918bda02198ff0fa2a7181a9935043bbb))
* **foreman:** bite check in the verify-gate Job (reject non-biting tests) ([#803](https://github.com/defilantech/LLMKube/issues/803)) ([6f14b43](https://github.com/defilantech/LLMKube/commit/6f14b432f34c73f341dfc08ddcb0f5d6f7de8537))
* **foreman:** reviewer rubric checks for test/prod fidelity (real values + wired-up) ([#802](https://github.com/defilantech/LLMKube/issues/802)) ([4353f3a](https://github.com/defilantech/LLMKube/commit/4353f3adf9492e43b2e4684830cc466ffd2046af))


### Bug Fixes

* **chart:** disable model cache when modelCache.enabled is false ([#801](https://github.com/defilantech/LLMKube/issues/801)) ([638663b](https://github.com/defilantech/LLMKube/commit/638663bb396001d5bcef1529afe3d980fb85b7af))

## [0.8.13](https://github.com/defilantech/LLMKube/compare/v0.8.12...v0.8.13) (2026-06-21)


### Bug Fixes

* **metal-agent:** register llamacpp runtime alias for llama.cpp executor ([#785](https://github.com/defilantech/LLMKube/issues/785)) ([73b7c90](https://github.com/defilantech/LLMKube/commit/73b7c90575affde62f7564e28e97699e1ff556a1))

## [0.8.12](https://github.com/defilantech/LLMKube/compare/v0.8.11...v0.8.12) (2026-06-21)


### Features

* **cli:** download heartbeat for llmkube deploy --wait ([#719](https://github.com/defilantech/LLMKube/issues/719)) ([#780](https://github.com/defilantech/LLMKube/issues/780)) ([e6f9774](https://github.com/defilantech/LLMKube/commit/e6f9774816177a36136877dc0a31ffc63021179a))
* **cli:** make cache list per-InferenceService cache aware ([#731](https://github.com/defilantech/LLMKube/issues/731)) ([#767](https://github.com/defilantech/LLMKube/issues/767)) ([d03c9ee](https://github.com/defilantech/LLMKube/commit/d03c9eed3727639a322d7143fe6ec993505fc66d))
* **controller:** shield inference pods from disruption during startup ([#660](https://github.com/defilantech/LLMKube/issues/660)) ([#781](https://github.com/defilantech/LLMKube/issues/781)) ([370084a](https://github.com/defilantech/LLMKube/commit/370084a64282e848cc7238077272c354a8fa01ca))
* **foreman:** codegen-drift check in the coder fast gate ([#775](https://github.com/defilantech/LLMKube/issues/775)) ([#778](https://github.com/defilantech/LLMKube/issues/778)) ([b04a28d](https://github.com/defilantech/LLMKube/commit/b04a28dcbd0660c4c21e36bedc17a83a0e62620b))
* **metal-agent:** event-driven endpoint withdrawal/recovery on health transitions ([#662](https://github.com/defilantech/LLMKube/issues/662)) ([#771](https://github.com/defilantech/LLMKube/issues/771)) ([7848d18](https://github.com/defilantech/LLMKube/commit/7848d1803be8322855d2635954f2afbe4dbc3cef))
* **metal-agent:** honor spec.runtime per-CR for multi-runtime dispatch ([#525](https://github.com/defilantech/LLMKube/issues/525)) ([#783](https://github.com/defilantech/LLMKube/issues/783)) ([aa210c3](https://github.com/defilantech/LLMKube/commit/aa210c346f8ae06742fb29f3ecc0f9d7e9247528))
* **metrics:** TTFT histogram, request error counter, vLLM metrics flag ([#409](https://github.com/defilantech/LLMKube/issues/409)) ([#769](https://github.com/defilantech/LLMKube/issues/769)) ([563b0c9](https://github.com/defilantech/LLMKube/commit/563b0c9f96a176f471ca45d4aade64f0aa3022b6))


### Bug Fixes

* **controller:** make checkAcceleratorAvailability DRA-aware ([#754](https://github.com/defilantech/LLMKube/issues/754)) ([#776](https://github.com/defilantech/LLMKube/issues/776)) ([804063b](https://github.com/defilantech/LLMKube/commit/804063b8339b74a409fd7ee037390993b8de52a0))
* **controller:** preserve agent-written schedulingStatus on reconcile ([#643](https://github.com/defilantech/LLMKube/issues/643)) ([#774](https://github.com/defilantech/LLMKube/issues/774)) ([4321028](https://github.com/defilantech/LLMKube/commit/4321028391b609cadcad5584d95df304150ec451))
* **controller:** reconcileService updates existing Service on endpoint changes ([#720](https://github.com/defilantech/LLMKube/issues/720)) ([#773](https://github.com/defilantech/LLMKube/issues/773)) ([70222ae](https://github.com/defilantech/LLMKube/commit/70222aed216b38d042212f1553cac72024f8d48c))
* **metal-agent:** clear SchedulingStatus on memory-check-pass ([#777](https://github.com/defilantech/LLMKube/issues/777)) ([#779](https://github.com/defilantech/LLMKube/issues/779)) ([ed1c4eb](https://github.com/defilantech/LLMKube/commit/ed1c4eb2dc4012d56440c8225cff950f0055d7f4))
* **metal-agent:** prevent zero-byte stub files from failed model downloads ([#642](https://github.com/defilantech/LLMKube/issues/642)) ([#772](https://github.com/defilantech/LLMKube/issues/772)) ([d737e47](https://github.com/defilantech/LLMKube/commit/d737e47677798311754e56119e4ef570778e573e))

## [0.8.11](https://github.com/defilantech/LLMKube/compare/v0.8.10...v0.8.11) (2026-06-20)


### Features

* **ci:** publish Helm charts as OCI artifacts to GHCR ([#758](https://github.com/defilantech/LLMKube/issues/758)) ([da07772](https://github.com/defilantech/LLMKube/commit/da07772cdfdfb273afa3cdad6966d19763981d3f))
* **foreman:** add unit-test tier to coder gate ([#763](https://github.com/defilantech/LLMKube/issues/763)) ([dfb3d75](https://github.com/defilantech/LLMKube/commit/dfb3d757380075c489c485298b4c2efa21954e4d))
* **foreman:** per-Agent contextStrategy with session mode (stable cache-friendly prefix) ([#757](https://github.com/defilantech/LLMKube/issues/757)) ([3b1ea1c](https://github.com/defilantech/LLMKube/commit/3b1ea1c1e417424e76f39266501334680071eed8))


### Bug Fixes

* **ci:** pin GitHub Actions to commit SHAs in helm-chart workflow ([#751](https://github.com/defilantech/LLMKube/issues/751)) ([e1a5b9e](https://github.com/defilantech/LLMKube/commit/e1a5b9ebc47e0c4118c7a487e1a59f3fbf9739b4))
* **foreman:** scope coder-gate golangci-lint cache per workspace ([#760](https://github.com/defilantech/LLMKube/issues/760)) ([cee04e8](https://github.com/defilantech/LLMKube/commit/cee04e8d58b4e0a6cb5c548313c07c36cb7d6050))
* **router:** inject backend Model on dispatch so fallback chains degrade ([#766](https://github.com/defilantech/LLMKube/issues/766)) ([bee7f1c](https://github.com/defilantech/LLMKube/commit/bee7f1cf6f31526dca957937e69dcb1749715736))


### Documentation

* add Strix Halo node onboarding quickstart ([#724](https://github.com/defilantech/LLMKube/issues/724)) ([b1859f0](https://github.com/defilantech/LLMKube/commit/b1859f0e4f87d84eb3d9c79233b1aea45a24a3f7))

## [0.8.10](https://github.com/defilantech/LLMKube/compare/v0.8.9...v0.8.10) (2026-06-20)


### Bug Fixes

* **crd:** guard GPU resourceClaims/resourceName CEL rule against absent field ([#753](https://github.com/defilantech/LLMKube/issues/753)) ([26be1aa](https://github.com/defilantech/LLMKube/commit/26be1aa3fec2a13fd001098b8df91e1b91716e5d))

## [0.8.9](https://github.com/defilantech/LLMKube/compare/v0.8.8...v0.8.9) (2026-06-19)


### Features

* add vulkan accelerator enum and make readiness-check Vulkan-aware ([#735](https://github.com/defilantech/LLMKube/issues/735)) ([76cf370](https://github.com/defilantech/LLMKube/commit/76cf3706ac50fb3ac65ef8537c923e79ac6e3aa0))
* **cli:** add --node-port to pin a stable NodePort on InferenceService ([#737](https://github.com/defilantech/LLMKube/issues/737)) ([a6c1a03](https://github.com/defilantech/LLMKube/commit/a6c1a03002bdc8eac75c93c3ae36158a077fe9e5))
* **cli:** add `llmkube scale` subcommand to scale InferenceService replicas ([#736](https://github.com/defilantech/LLMKube/issues/736)) ([bdb89f8](https://github.com/defilantech/LLMKube/commit/bdb89f898bc31f00b714b05b4e2b5960b7db304d))
* **controller:** vendor-neutral DRA (resource.k8s.io/v1) scheduling for InferenceService ([#750](https://github.com/defilantech/LLMKube/issues/750)) ([6eb0f27](https://github.com/defilantech/LLMKube/commit/6eb0f27d9b2e4c06b41af5855e7135ca5d7edc02))
* **foreman:** deterministic coder verification gate with feedback loop ([#749](https://github.com/defilantech/LLMKube/issues/749)) ([8cf3295](https://github.com/defilantech/LLMKube/commit/8cf32956b4b448bb2ae37e4ff048e532e6668663))
* **foreman:** loop convergence forcing (EditFreeStreak + final-turns submit) ([#741](https://github.com/defilantech/LLMKube/issues/741)) ([cd3f068](https://github.com/defilantech/LLMKube/commit/cd3f068d7557a5e228696e87d7abe594ff549810))


### Bug Fixes

* **cli:** correct --node-port help text and add NodePort test coverage ([#742](https://github.com/defilantech/LLMKube/issues/742)) ([ef55900](https://github.com/defilantech/LLMKube/commit/ef55900d04002fef4999e294919428c62a6caded))
* **controller:** honor GPU resourceName override in checkAcceleratorAvailability ([#747](https://github.com/defilantech/LLMKube/issues/747)) ([5aa3152](https://github.com/defilantech/LLMKube/commit/5aa3152b062f9a5b5e459681c46360c295e43a28))
* **controller:** honor RouterRule.Timeout in gateway-mode AIGatewayRoute generation ([#748](https://github.com/defilantech/LLMKube/issues/748)) ([1978a72](https://github.com/defilantech/LLMKube/commit/1978a722d81be96b3f6e57bfeb7f41a2792cccf0))
* **foreman:** make install-foreman-agent produce a working plist ([#743](https://github.com/defilantech/LLMKube/issues/743)) ([b946fef](https://github.com/defilantech/LLMKube/commit/b946fef0e7c4c910554f5459ccf7308c1eaa9600))
* **foreman:** scope-overlap rail rescues honest paraphrase from false NO-GO ([#746](https://github.com/defilantech/LLMKube/issues/746)) ([00ae36e](https://github.com/defilantech/LLMKube/commit/00ae36e813f625b041f57c8c8a9ee90798a12d31))

## [0.8.8](https://github.com/defilantech/LLMKube/compare/v0.8.7...v0.8.8) (2026-06-17)


### Features

* AMD/Vulkan runtime image selection (hardware.gpu.runtime) ([#727](https://github.com/defilantech/LLMKube/issues/727)) ([1a4544f](https://github.com/defilantech/LLMKube/commit/1a4544fe54de673bb93443dd4971c35875cef017))
* **crd:** make GPU resource name configurable to support AMD/Vulkan/Intel scheduling ([#709](https://github.com/defilantech/LLMKube/issues/709)) ([c88becf](https://github.com/defilantech/LLMKube/commit/c88becfdd9d25a0763a16d536403c6615fcbb313))
* **gateway:** active HTTP health checks on the ModelRouter BTP for fast backend ejection ([#662](https://github.com/defilantech/LLMKube/issues/662)) ([#704](https://github.com/defilantech/LLMKube/issues/704)) ([ba99060](https://github.com/defilantech/LLMKube/commit/ba990606b1b9cc776d491337d8e85883e2e4b8bc))
* **gateway:** event-driven route-level ejection of unhealthy backends ([#662](https://github.com/defilantech/LLMKube/issues/662)) ([#706](https://github.com/defilantech/LLMKube/issues/706)) ([815f2bf](https://github.com/defilantech/LLMKube/commit/815f2bf9aed37e9098c23bc40941fe0b22347f1b))
* **gateway:** gateway-scoped audit access log + fail-loud auditLog in Gateway mode (2c) ([#703](https://github.com/defilantech/LLMKube/issues/703)) ([b874b5e](https://github.com/defilantech/LLMKube/commit/b874b5e0eda49f9ee152cd2ae1bb8d8a4c923bfd))
* **gateway:** header-only data-classification routing + fail-closed sensitive guard (2e-core) ([#707](https://github.com/defilantech/LLMKube/issues/707)) ([0249665](https://github.com/defilantech/LLMKube/commit/02496656d109fb5be33bf9ec93186b141610acca))
* **gateway:** InferenceService Envoy AI Gateway exposure (MVP) ([#692](https://github.com/defilantech/LLMKube/issues/692)) ([3b095dc](https://github.com/defilantech/LLMKube/commit/3b095dc7b0ce6cfb50f6691db4560cb9a4a47cca))
* **gateway:** ModelRouter dataPlane Gateway mode with cross-tier failover (2a) ([#693](https://github.com/defilantech/LLMKube/issues/693)) ([2842634](https://github.com/defilantech/LLMKube/commit/2842634b9059fbec67815f2b64c283f4c456162c))
* **gateway:** ModelRouter JWT authentication via SecurityPolicy (2d-core) ([#695](https://github.com/defilantech/LLMKube/issues/695)) ([73a2ea9](https://github.com/defilantech/LLMKube/commit/73a2ea9493b9c261a0e91c6c610d3820f088bc03))
* **gateway:** ModelRouter per-team model allowlists via SecurityPolicy authorization (2d.2) ([#702](https://github.com/defilantech/LLMKube/issues/702)) ([94428b4](https://github.com/defilantech/LLMKube/commit/94428b4df745e1ce1b163971a45a42be2aa92de8))
* **gateway:** ModelRouter token budgets and 429 enforcement (2b) ([#694](https://github.com/defilantech/LLMKube/issues/694)) ([627e85a](https://github.com/defilantech/LLMKube/commit/627e85ac1e9d46a0afbd35b186ae970fe749d168))
* **metal-agent:** withdraw endpoint when runtime is unhealthy ([#662](https://github.com/defilantech/LLMKube/issues/662)) ([#705](https://github.com/defilantech/LLMKube/issues/705)) ([5ed9395](https://github.com/defilantech/LLMKube/commit/5ed9395af9de8aa206c55a6a584d3c5a32a0b197))
* **selfupdate:** bound download size + GC old agent versions ([#690](https://github.com/defilantech/LLMKube/issues/690)) ([5205a62](https://github.com/defilantech/LLMKube/commit/5205a62f5158725816e7a6b0e300aa988b4b6ff2))
* **webhook:** ModelRouter validating webhook for apply-time honest-boundary rejection ([#708](https://github.com/defilantech/LLMKube/issues/708)) ([13d9321](https://github.com/defilantech/LLMKube/commit/13d93216f2c1ed7a6ce6862d359bb367d4f653c2))


### Bug Fixes

* **cache:** restore shared model cache as the default (perService becomes opt-in) ([#732](https://github.com/defilantech/LLMKube/issues/732)) ([44ab7dc](https://github.com/defilantech/LLMKube/commit/44ab7dc34687e44ca3b0ad9fdc20922b54e5cb86))
* per-node model cache so GPU on a second node can schedule ([#728](https://github.com/defilantech/LLMKube/issues/728)) ([#729](https://github.com/defilantech/LLMKube/issues/729)) ([79bccce](https://github.com/defilantech/LLMKube/commit/79bccce3b4445914d7d62b43c2cad0115e310d0d))


### Documentation

* DGX Spark (GB10) on MicroK8s setup guide ([#717](https://github.com/defilantech/LLMKube/issues/717)) ([bf7d7a7](https://github.com/defilantech/LLMKube/commit/bf7d7a7cc456b2f0aa7641c68af016567d4289c2))
* fix DGX Spark guide for ARM64 (GPU operator + GB10 image) ([#718](https://github.com/defilantech/LLMKube/issues/718)) ([45a4237](https://github.com/defilantech/LLMKube/commit/45a42372a763a888958c3e4a83ed188c1188c584))
* proposal for owned AMD/Vulkan runtime image and build pipeline ([#726](https://github.com/defilantech/LLMKube/issues/726)) ([3a1a150](https://github.com/defilantech/LLMKube/commit/3a1a150a1ae844951771ff3b25a2034ea1b5fe85))

## [0.8.7](https://github.com/defilantech/LLMKube/compare/v0.8.6...v0.8.7) (2026-06-14)


### Features

* **foreman:** validating webhook for Agent + AgenticTask CRs ([#685](https://github.com/defilantech/LLMKube/issues/685)) ([1935da7](https://github.com/defilantech/LLMKube/commit/1935da775dafcbb1ea25c3f6d763df42693163e7))


### Bug Fixes

* **foreman:** drop deprecated Result.Requeue in workload terminal test ([#688](https://github.com/defilantech/LLMKube/issues/688)) ([a97056c](https://github.com/defilantech/LLMKube/commit/a97056c89c27a14cd2fde0a63f2d4c49ce7c086b))

## [0.8.6](https://github.com/defilantech/LLMKube/compare/v0.8.5...v0.8.6) (2026-06-13)


### Bug Fixes

* **selfupdate:** resolve current symlink in managed-root detection ([#679](https://github.com/defilantech/LLMKube/issues/679)) ([a6e7223](https://github.com/defilantech/LLMKube/commit/a6e7223fabd5ee15fdf5905f803cfc90498734b1))

## [0.8.5](https://github.com/defilantech/LLMKube/compare/v0.8.4...v0.8.5) (2026-06-13)


### Features

* **foreman,agent:** report agent version to the cluster ([#672](https://github.com/defilantech/LLMKube/issues/672)) ([8860cb6](https://github.com/defilantech/LLMKube/commit/8860cb66f62d358e3b08351ffd9dbaf012e7fa77))
* **foreman,selfupdate:** agent self-update so approved AgentReleases roll the fleet ([#676](https://github.com/defilantech/LLMKube/issues/676)) ([5917715](https://github.com/defilantech/LLMKube/commit/59177153c3f4569fb0a48680e364077ff46431f3))
* **foreman:** add AgentRelease CRD (scaffold for managed fleet updates) ([#674](https://github.com/defilantech/LLMKube/issues/674)) ([55f7d9f](https://github.com/defilantech/LLMKube/commit/55f7d9f11899978fd42fadfda04b5f8530007118))
* **foreman:** AgentRelease rollout controller (staged, health-gated, approved) ([#675](https://github.com/defilantech/LLMKube/issues/675)) ([aa6bbd0](https://github.com/defilantech/LLMKube/commit/aa6bbd05ec156a0b4414825a7f47e3cb915a86cd))

## [0.8.4](https://github.com/defilantech/LLMKube/compare/v0.8.3...v0.8.4) (2026-06-12)


### Features

* **agent,controller:** heartbeat liveness for metal endpoint registrations ([#666](https://github.com/defilantech/LLMKube/issues/666)) ([6a8cfe8](https://github.com/defilantech/LLMKube/commit/6a8cfe8a2e8c4bbbb057e3efd9aa1d0f8e59e11c))


### Bug Fixes

* **agent:** retry endpoint registration with correct update semantics ([#664](https://github.com/defilantech/LLMKube/issues/664)) ([82cfc35](https://github.com/defilantech/LLMKube/commit/82cfc35894668600162605420c365508d135c49a))
* **foreman:** ERROR verdicts become terminal INCOMPLETE outcomes; watcher can no longer wedge ([#667](https://github.com/defilantech/LLMKube/issues/667)) ([46405b6](https://github.com/defilantech/LLMKube/commit/46405b67ea7646f367feb2bbd143e13f13b6b80e))
* **foreman:** expire stale claims so tasks survive agent death ([#669](https://github.com/defilantech/LLMKube/issues/669)) ([8af7ac6](https://github.com/defilantech/LLMKube/commit/8af7ac65e477967ff5f4f742409e3c434c5bfc7f))
* **foreman:** patchTerminal stands down when the task is no longer owned ([#670](https://github.com/defilantech/LLMKube/issues/670)) ([33af907](https://github.com/defilantech/LLMKube/commit/33af90773dc2afe7f4a581274874f33ce4e268c7))
* **foreman:** workspace reset survives read-only files from prior runs ([#671](https://github.com/defilantech/LLMKube/issues/671)) ([0a7de0d](https://github.com/defilantech/LLMKube/commit/0a7de0d1058f88a704fbb3ea28118d48e8db2345))

## [0.8.3](https://github.com/defilantech/LLMKube/compare/v0.8.2...v0.8.3) (2026-06-11)


### Features

* **foreman/loop:** support hybrid-thinking models via reasoning_content ([#651](https://github.com/defilantech/LLMKube/issues/651)) ([b178de2](https://github.com/defilantech/LLMKube/commit/b178de2ad7aaf225ec241ef2deb291cce92b10da))
* **foreman/reviewer:** computable scope-overlap check demotes drifted GO verdicts ([#648](https://github.com/defilantech/LLMKube/issues/648)) ([8afe0c1](https://github.com/defilantech/LLMKube/commit/8afe0c1813a3e4ffcb743b0821d54194aea9f40a))
* **foreman/reviewer:** demote unverifiable review verdicts to NO-GO ([#645](https://github.com/defilantech/LLMKube/issues/645)) ([188dacd](https://github.com/defilantech/LLMKube/commit/188dacd04dcb13d62fee660f55efa2dd855051ca))


### Bug Fixes

* **foreman/executor:** map reasoning-only exhaustion to INCOMPLETE ([#652](https://github.com/defilantech/LLMKube/issues/652)) ([d2f453e](https://github.com/defilantech/LLMKube/commit/d2f453ea8e2fab9aff94590d7937623b85c94a0a))
* **foreman/loop:** keep wire-stripped reasoning-only turns template-valid ([#653](https://github.com/defilantech/LLMKube/issues/653)) ([5ec14f6](https://github.com/defilantech/LLMKube/commit/5ec14f6fac87c708bf8fb94c33f96862c8740da7))

## [0.8.2](https://github.com/defilantech/LLMKube/compare/v0.8.1...v0.8.2) (2026-06-10)


### Features

* **controller:** add Model refreshPolicy + upstream drift detection ([#635](https://github.com/defilantech/LLMKube/issues/635)) ([7dc5b14](https://github.com/defilantech/LLMKube/commit/7dc5b1498a691ce5473e2ff459cb8afcf01124fd))
* **controller:** real accelerator availability check for Model status ([#230](https://github.com/defilantech/LLMKube/issues/230)) ([#610](https://github.com/defilantech/LLMKube/issues/610)) ([f8652fd](https://github.com/defilantech/LLMKube/commit/f8652fd796aae5cb5cf50556aa1fee96003fc569))
* **foreman:** add opt-in NetworkPolicy to the Foreman Helm chart ([#625](https://github.com/defilantech/LLMKube/issues/625)) ([8872b4d](https://github.com/defilantech/LLMKube/commit/8872b4da7fd7e16672b6da4f12c750b71e166e05))
* **foreman:** escalation-only reviewer emission on base NO-GO ([#639](https://github.com/defilantech/LLMKube/issues/639)) ([d596ce3](https://github.com/defilantech/LLMKube/commit/d596ce3ecbbbb15fbbd1589ec3b77234834f92db))
* **foreman:** Job execution strategy for remote-model coder/reviewer Agents ([#620](https://github.com/defilantech/LLMKube/issues/620)) ([#621](https://github.com/defilantech/LLMKube/issues/621)) ([2317ac0](https://github.com/defilantech/LLMKube/commit/2317ac03689c29d4acb8cd26c745e7da17f6f57f))
* **metal-agent:** stable host-side client proxy on a fixed port ([#406](https://github.com/defilantech/LLMKube/issues/406)) ([#608](https://github.com/defilantech/LLMKube/issues/608)) ([5b4ea34](https://github.com/defilantech/LLMKube/commit/5b4ea34acd1b4a4fbf1736aeb34c7e7b52324a5c))


### Bug Fixes

* bump Go toolchain to 1.26.4 to patch standard-library CVEs ([#626](https://github.com/defilantech/LLMKube/issues/626)) ([6d8fbb7](https://github.com/defilantech/LLMKube/commit/6d8fbb70f0b052b010856dd3ce152fd9a7f271ec))
* **controller:** recreate InferenceService Deployment on immutable selector change ([#606](https://github.com/defilantech/LLMKube/issues/606)) ([#607](https://github.com/defilantech/LLMKube/issues/607)) ([faf9151](https://github.com/defilantech/LLMKube/commit/faf915137fe5a5f9b9ec5204c77d1c13b30292a8))
* **controller:** revalidate http/https model sources in init container ([#636](https://github.com/defilantech/LLMKube/issues/636)) ([64812e5](https://github.com/defilantech/LLMKube/commit/64812e5a346c02b0a9c4a9581a9c745e505eea81))
* **foreman:** detect stale FleetNodes via heartbeat window ([#633](https://github.com/defilantech/LLMKube/issues/633)) ([515bdfe](https://github.com/defilantech/LLMKube/commit/515bdfeedcdf130ff216d8fb7e573f4a8a3c453f))
* **foreman:** harden the agent loop against false-negative INCOMPLETE terminations ([#623](https://github.com/defilantech/LLMKube/issues/623)) ([edc93e4](https://github.com/defilantech/LLMKube/commit/edc93e404c874da1e14811bed6b72ba7722b01d3))
* **foreman:** propagate Job-mode branch + commit SHA to AgenticTask status ([#634](https://github.com/defilantech/LLMKube/issues/634)) ([3586a14](https://github.com/defilantech/LLMKube/commit/3586a14154187cf718c2bcc7b44c069e84b44d6f))
* **metal-agent:** fail closed when memory admission check cannot complete ([#641](https://github.com/defilantech/LLMKube/issues/641)) ([cfe53aa](https://github.com/defilantech/LLMKube/commit/cfe53aa0282bc0e38f6a34dd7caeaa71de1b794e))


### Documentation

* **roadmap:** rewrite around the heterogeneous sovereign fleet thesis + milestones ([#632](https://github.com/defilantech/LLMKube/issues/632)) ([d98b257](https://github.com/defilantech/LLMKube/commit/d98b257d8ae4880448ecbb74cba566f2763b7fa8))

## [0.8.1](https://github.com/defilantech/LLMKube/compare/v0.8.0...v0.8.1) (2026-06-01)


### ⚠ BREAKING CHANGES

* **foreman:** Agent.spec.requestTimeoutSeconds changes meaning from a per-request HTTP timeout to a loop-wide wall-clock budget, and its default moves from 600 to 3600. The former per-request bound is now the new Agent.spec.requestTurnTimeoutSeconds (default 120). Re-apply your Agent CRs after upgrade so existing Agents pick up explicit values.

### Features

* **inferenceservice:** typed spec.ropeScaling for RoPE/YaRN context extension ([#507](https://github.com/defilantech/LLMKube/issues/507)) ([#600](https://github.com/defilantech/LLMKube/issues/600)) ([a554aee](https://github.com/defilantech/LLMKube/commit/a554aeef1e8f76d3d06d00b91ac779ef563c2000))


### Bug Fixes

* **foreman:** recover orphaned phase=Running tasks on agent restart ([#542](https://github.com/defilantech/LLMKube/issues/542)) ([#598](https://github.com/defilantech/LLMKube/issues/598)) ([6dd2c44](https://github.com/defilantech/LLMKube/commit/6dd2c445fbec7d85b32dec67169de8339b6ec0e5))
* **foreman:** split per-turn timeout from loop-wide budget ([#532](https://github.com/defilantech/LLMKube/issues/532)) ([#602](https://github.com/defilantech/LLMKube/issues/602)) ([41e7663](https://github.com/defilantech/LLMKube/commit/41e7663cb25a64a967feb56ec9de49be4ca98c9c))
* **foreman:** warm-path reviewer scheduling on macOS ([#578](https://github.com/defilantech/LLMKube/issues/578), [#579](https://github.com/defilantech/LLMKube/issues/579)) ([#597](https://github.com/defilantech/LLMKube/issues/597)) ([a94d1ef](https://github.com/defilantech/LLMKube/commit/a94d1efec9cb796a4e985eb92964925182a20ac0))
* **metal-agent:** prefer routable interface for host-IP auto-detect ([#526](https://github.com/defilantech/LLMKube/issues/526)) ([#599](https://github.com/defilantech/LLMKube/issues/599)) ([c780795](https://github.com/defilantech/LLMKube/commit/c78079503529538abf0c05aa283957b4e6b8d29a))


### Documentation

* **foreman:** absolute paths in overview README cross-refs (fix llmkube-web prerender) ([#596](https://github.com/defilantech/LLMKube/issues/596)) ([b5f6f94](https://github.com/defilantech/LLMKube/commit/b5f6f94b6a1cc031fc4a758ca8e2ab66b35a88e9))
* **foreman:** move docs/foreman to docs/site/foreman + register in site nav ([#594](https://github.com/defilantech/LLMKube/issues/594)) ([9fd85bb](https://github.com/defilantech/LLMKube/commit/9fd85bbe3f4bd2ec9135f71925ba677433b8adc2))


### Miscellaneous

* pin next release to 0.8.1 (Release-As) ([#605](https://github.com/defilantech/LLMKube/issues/605)) ([a876cc6](https://github.com/defilantech/LLMKube/commit/a876cc69ee7bda878b2f916a9e2fe1a28dfd6f50))

## [0.8.0](https://github.com/defilantech/LLMKube/compare/v0.7.12...v0.8.0) (2026-05-28)


### Features

* **foreman/api:** structured AgenticTaskFailureReason taxonomy ([#565](https://github.com/defilantech/LLMKube/issues/565)) ([6e72e85](https://github.com/defilantech/LLMKube/commit/6e72e8599a4e6d97e44e026cbc1194df064fc65c))
* **foreman/loop:** observation masking for context-window management ([#563](https://github.com/defilantech/LLMKube/issues/563)) ([d17c3e0](https://github.com/defilantech/LLMKube/commit/d17c3e070416354fdfcc9b39c125f79603abea0d))
* **foreman/loop:** stuck-loop detector with nudge-then-force protocol ([#544](https://github.com/defilantech/LLMKube/issues/544)) ([#569](https://github.com/defilantech/LLMKube/issues/569)) ([2172ece](https://github.com/defilantech/LLMKube/commit/2172ece33486c1a0a0c0b71cd4199b5d1f593854))
* **foreman/reviewer:** fetch_issue tool replaces gh issue view subshell ([#581](https://github.com/defilantech/LLMKube/issues/581)) ([0253e43](https://github.com/defilantech/LLMKube/commit/0253e430f02a6d96a8a1ff05597dfb926aa0d411))
* **foreman/tools:** distinguish whitelist-excluded from unknown tool calls ([#564](https://github.com/defilantech/LLMKube/issues/564)) ([089e9ca](https://github.com/defilantech/LLMKube/commit/089e9ca5dd0dcfa9513eda09634f5030d007a707))
* **foreman/v0.2:** hybrid cloud reviewer Agent + sovereignty toggles ([#553](https://github.com/defilantech/LLMKube/issues/553)) ([65a7cb8](https://github.com/defilantech/LLMKube/commit/65a7cb874a492e9eba28888a95c600ebf6789d44))
* **foreman/v0.2:** WorkloadSpec.reviewerAgentRefs (plural) + third pipeline stage ([#551](https://github.com/defilantech/LLMKube/issues/551)) ([831ae8c](https://github.com/defilantech/LLMKube/commit/831ae8c501e6485d234acbd0b1aab2b34af8faf5))
* **foreman:** add repo-map localization for coder Agents ([#560](https://github.com/defilantech/LLMKube/issues/560)) ([#566](https://github.com/defilantech/LLMKube/issues/566)) ([f6bf8c0](https://github.com/defilantech/LLMKube/commit/f6bf8c08db5466bd5ed222c8bc4d009d6ddb1c3a))
* **foreman:** AgenticTask branches include workload name ([#573](https://github.com/defilantech/LLMKube/issues/573)) ([#574](https://github.com/defilantech/LLMKube/issues/574)) ([2986906](https://github.com/defilantech/LLMKube/commit/2986906fc33b09932f03240bf0c1220b499e5a87))
* **foreman:** executor fetches GitHub issue body when payload prompt is empty ([#571](https://github.com/defilantech/LLMKube/issues/571)) ([#572](https://github.com/defilantech/LLMKube/issues/572)) ([2b5bd31](https://github.com/defilantech/LLMKube/commit/2b5bd318ad897c90679f2ac0e378a30e07a8425f))
* **foreman:** post-M4 stability follow-ups for v5-batch readiness ([#535](https://github.com/defilantech/LLMKube/issues/535)) ([a841612](https://github.com/defilantech/LLMKube/commit/a841612b07bc2cbc57cda2383a1846976bdfff22))
* **foreman:** v0.4 reviewer agent — tool-using reviewer with sharpened prompt + structured findings ([#575](https://github.com/defilantech/LLMKube/issues/575)) ([#576](https://github.com/defilantech/LLMKube/issues/576)) ([06091a9](https://github.com/defilantech/LLMKube/commit/06091a9e78922ee091a8cb9063732b4515626140))
* **foreman:** workspace-scoped bash + WORKSPACE_ROOT contract ([#567](https://github.com/defilantech/LLMKube/issues/567)) ([#568](https://github.com/defilantech/LLMKube/issues/568)) ([061eb41](https://github.com/defilantech/LLMKube/commit/061eb4100d3055d236fe04f42cb079dd08af9631))
* **gpu:** add Intel GPU (oneAPI/SYCL) support across controller, CLI, docs, and e2e ([#557](https://github.com/defilantech/LLMKube/issues/557)) ([741ef5d](https://github.com/defilantech/LLMKube/commit/741ef5db256055a66cda0532c9272f98b96b5568))
* **metal-agent:** InferenceService name allowlist for multi-Mac fleets ([#555](https://github.com/defilantech/LLMKube/issues/555)) ([67361f3](https://github.com/defilantech/LLMKube/commit/67361f385256766969fc65d5994e169609f597d4))


### Bug Fixes

* **foreman/build:** include gate_job_template.yaml in Docker context ([#554](https://github.com/defilantech/LLMKube/issues/554)) ([def535d](https://github.com/defilantech/LLMKube/commit/def535d3da8e28fa6d9eeba798d1885a182d176c))
* **foreman/executor:** resolve InferenceService port from live Endpoints, not stale install-time override ([#550](https://github.com/defilantech/LLMKube/issues/550)) ([4351608](https://github.com/defilantech/LLMKube/commit/43516088c7cd3af704e51e411b9e3bd657fd307c))
* **foreman/executor:** route reviewer-role GO through modelDecidedResult ([#545](https://github.com/defilantech/LLMKube/issues/545)) ([16943a5](https://github.com/defilantech/LLMKube/commit/16943a5814eeeb7bac897b2e816cea51f4004bc2))
* **foreman/loop:** force-terminate returns clean Terminal envelope ([#544](https://github.com/defilantech/LLMKube/issues/544) follow-up) ([#570](https://github.com/defilantech/LLMKube/issues/570)) ([19500c9](https://github.com/defilantech/LLMKube/commit/19500c9a36fb87d390eab93204ca6a69499705a6))
* **foreman/oai:** always emit content on non-assistant messages ([#562](https://github.com/defilantech/LLMKube/issues/562)) ([f6dc8e1](https://github.com/defilantech/LLMKube/commit/f6dc8e16f734808baa441540fa5e26f402c947bf))
* **foreman/reviewer:** ground-truth filesTouched + bump qwen maxTurns + tighten confabulation defenses ([#584](https://github.com/defilantech/LLMKube/issues/584)) ([b3e21f0](https://github.com/defilantech/LLMKube/commit/b3e21f096fd61be674dc10b7843a56898a6e094a))
* **foreman/reviewer:** ground-truth issueAsk + cap qwen Section D ([#587](https://github.com/defilantech/LLMKube/issues/587)) ([b66006c](https://github.com/defilantech/LLMKube/commit/b66006c8234b867fa60edbea0cc75004abcf00eb))
* **foreman/reviewer:** role-aware stuck-loop detector + non-empty reviewer user prompt (rerun-7 follow-up) ([#577](https://github.com/defilantech/LLMKube/issues/577)) ([19590f6](https://github.com/defilantech/LLMKube/commit/19590f64c08522fb1d924e23cb934fd47e376783))
* **foreman/tools:** cmd.WaitDelay + process-group kill so BashTool can't deadlock on grandchild-held pipes ([#547](https://github.com/defilantech/LLMKube/issues/547)) ([c12f6f8](https://github.com/defilantech/LLMKube/commit/c12f6f82674f5167fbc6022315c4b2530d3c7106))
* **foreman:** cascade + Workload rollup gate on phase AND verdict, not phase alone ([#548](https://github.com/defilantech/LLMKube/issues/548)) ([1b72a7c](https://github.com/defilantech/LLMKube/commit/1b72a7c9d4d5545eab74a2a15b2422335cd9ead4))


### Documentation

* **foreman:** v0.8.0 release-prep docs + README Foreman section ([#591](https://github.com/defilantech/LLMKube/issues/591)) ([5e41df1](https://github.com/defilantech/LLMKube/commit/5e41df1628311a416765468bf22a28dc2e9d0790))
* mention make lint-all in AGENTS.md and CONTRIBUTING.md ([#588](https://github.com/defilantech/LLMKube/issues/588)) ([39da983](https://github.com/defilantech/LLMKube/commit/39da983792b93a6ec13d0b44eab7ff71003115e5))


### Miscellaneous

* release Foreman debut as 0.8.0 (take 2) ([#593](https://github.com/defilantech/LLMKube/issues/593)) ([a8f0368](https://github.com/defilantech/LLMKube/commit/a8f036884c9c973af52162260315945e805f7fa2))

## [0.7.12](https://github.com/defilantech/LLMKube/compare/v0.7.11...v0.7.12) (2026-05-24)


### Features

* **foreman/m6:** Workload reconciler with stub planner (explicit pipeline + issue-batch shortcut) ([#533](https://github.com/defilantech/LLMKube/issues/533)) ([dbdcd46](https://github.com/defilantech/LLMKube/commit/dbdcd46e5ee1578f6b0be02ac3bf270d3935d7e1))


### Bug Fixes

* **foreman/m4:** gate Job honors payload.branch + clones from --git-remote-url ([#529](https://github.com/defilantech/LLMKube/issues/529)) ([905a269](https://github.com/defilantech/LLMKube/commit/905a2698a922641a8cd64ce6bd81e18ff7ecb339))
* **foreman:** chart wires --workspace-dir + tightens docs for native-mode required values ([#534](https://github.com/defilantech/LLMKube/issues/534)) ([1c43c69](https://github.com/defilantech/LLMKube/commit/1c43c69fa257ae1d2cb8aecada54831e815e2780))

## [0.7.11](https://github.com/defilantech/LLMKube/compare/v0.7.10...v0.7.11) (2026-05-23)


### Bug Fixes

* **foreman:** drop chart-level subchart dep on llmkube (unblock v0.7.11 chart-releaser) ([#519](https://github.com/defilantech/LLMKube/issues/519)) ([207ddc6](https://github.com/defilantech/LLMKube/commit/207ddc64f82e0243da96bdd73e1b5d949f991041))

## [0.7.10](https://github.com/defilantech/LLMKube/compare/v0.7.9...v0.7.10) (2026-05-23)


### Features

* add --llama-server-port for a fixed llama-server runtime port ([#499](https://github.com/defilantech/LLMKube/issues/499)) ([cc30b0d](https://github.com/defilantech/LLMKube/commit/cc30b0dbd5cc8493547f3a251c11ad02d391aa58))
* add make lint-all target for cross-arch linting ([#508](https://github.com/defilantech/LLMKube/issues/508)) ([f57dd5b](https://github.com/defilantech/LLMKube/commit/f57dd5b0f5178a9c5d6e284a7f1bd5e36f1b282d))
* capability-aware scheduler + AgenticTaskWatcher + stub executor (Foreman v0.1 M2) ([#504](https://github.com/defilantech/LLMKube/issues/504)) ([74b3d6e](https://github.com/defilantech/LLMKube/commit/74b3d6e1c47915ff1b7a6ac1025acd035f28cce7))
* **foreman:** gate-role Agent on a verifier node (M4) ([#518](https://github.com/defilantech/LLMKube/issues/518)) ([40a340e](https://github.com/defilantech/LLMKube/commit/40a340e09f3e1845b0ad5bc0b549985fd0d92ab1))
* **foreman:** native agent loop + Agent CRD + coder role on M5 Max (M3) ([#509](https://github.com/defilantech/LLMKube/issues/509)) ([6661343](https://github.com/defilantech/LLMKube/commit/666134373653bc621bf0c411db300f61f83f447b))
* scaffold Foreman as an opt-in add-on (M0 + M1) ([#501](https://github.com/defilantech/LLMKube/issues/501)) ([cd40491](https://github.com/defilantech/LLMKube/commit/cd404914cc443eab43e16a9dfbf32c47171d2960))


### Bug Fixes

* report Stopped phase when InferenceService.spec.replicas=0 on Metal path ([#498](https://github.com/defilantech/LLMKube/issues/498)) ([7787239](https://github.com/defilantech/LLMKube/commit/77872395deda1ec5ce89d61c12de62cedd998968))


### Documentation

* add AGENTS.md ([#496](https://github.com/defilantech/LLMKube/issues/496)) ([89d3766](https://github.com/defilantech/LLMKube/commit/89d3766ba5a43726fdf89b0fbe61caf7bacb75dc))
* bump broken bartowski phi-4-mini URL to renamed repo ([#514](https://github.com/defilantech/LLMKube/issues/514)) ([9f15d98](https://github.com/defilantech/LLMKube/commit/9f15d9891ddc32e118f55b378f8e357e2e7e4d14))
* **macos-metal:** derive curl port from Endpoints (follow-up to [#513](https://github.com/defilantech/LLMKube/issues/513)) ([#515](https://github.com/defilantech/LLMKube/issues/515)) ([83085c2](https://github.com/defilantech/LLMKube/commit/83085c218ff17c2e0a995110b090aca75aa9e99f))
* **macos-metal:** replace broken port-forward step with host-localhost curl ([#513](https://github.com/defilantech/LLMKube/issues/513)) ([0f7f7a7](https://github.com/defilantech/LLMKube/commit/0f7f7a7c903660272b53d52c6f5de18838237b5e))

## [0.7.9](https://github.com/defilantech/LLMKube/compare/v0.7.8...v0.7.9) (2026-05-18)


### Features

* add mlx-server runtime to the metal-agent ([#471](https://github.com/defilantech/LLMKube/issues/471)) ([8bf9808](https://github.com/defilantech/LLMKube/commit/8bf9808edc03f6d8485b21e390b9ad6fbad092df))
* add scale sub resource ([#474](https://github.com/defilantech/LLMKube/issues/474)) ([73419a5](https://github.com/defilantech/LLMKube/commit/73419a5efb6a2aee264a5787c1fa056d548e2137))


### Bug Fixes

* clear stale conditions when a model reaches Ready without a download ([#476](https://github.com/defilantech/LLMKube/issues/476)) ([06325b0](https://github.com/defilantech/LLMKube/commit/06325b0d72559b40625ef045840284903c897fac))
* inference PodMonitor selector matched no pods ([#481](https://github.com/defilantech/LLMKube/issues/481)) ([31ee4d6](https://github.com/defilantech/LLMKube/commit/31ee4d675016014306e7634935c04790dcfbd052))
* mark Metal local-path models Ready instead of stuck Copying ([#472](https://github.com/defilantech/LLMKube/issues/472)) ([c513c84](https://github.com/defilantech/LLMKube/commit/c513c84bd2a19253230b56e8d5bb11cca3778edd))
* metal-path InferenceService status and memory pre-flight ([#488](https://github.com/defilantech/LLMKube/issues/488)) ([98ef2c4](https://github.com/defilantech/LLMKube/commit/98ef2c4fb42e0d4aa248a8c77f6c09ae8c1058ac))
* point metal-agent mlx-server install hint at the Homebrew formula ([#477](https://github.com/defilantech/LLMKube/issues/477)) ([74b3333](https://github.com/defilantech/LLMKube/commit/74b33337f4abb5b54a1f1d9be216a4d5fc2e2c82))
* prevent concurrent runtime respawn in metal-agent ([#469](https://github.com/defilantech/LLMKube/issues/469)) ([f34640b](https://github.com/defilantech/LLMKube/commit/f34640b118e4a043888e2eb8bb20012c6f2d5b42))
* stop the operator fighting the HPA over Deployment replicas ([#485](https://github.com/defilantech/LLMKube/issues/485)) ([8fc70e2](https://github.com/defilantech/LLMKube/commit/8fc70e23cb6a8b699c7d853cf109d1e55561d6eb))


### Documentation

* add MAINTAINERS file and recommend private vulnerability reporting ([#479](https://github.com/defilantech/LLMKube/issues/479)) ([aaccb4d](https://github.com/defilantech/LLMKube/commit/aaccb4dea54b5cb52304b303b823a3531dfe5a4c))

## [0.7.8](https://github.com/defilantech/LLMKube/compare/v0.7.7...v0.7.8) (2026-05-14)


### Features

* configurable proxy + per-route/backend timeouts (closes [#457](https://github.com/defilantech/LLMKube/issues/457), [#458](https://github.com/defilantech/LLMKube/issues/458)) ([#461](https://github.com/defilantech/LLMKube/issues/461)) ([03d222a](https://github.com/defilantech/LLMKube/commit/03d222a759e5d639278ad554425efbd4bc1bac21))
* external provider URL defaults + cluster-wide LiteLLM URL (closes [#438](https://github.com/defilantech/LLMKube/issues/438)) ([#451](https://github.com/defilantech/LLMKube/issues/451)) ([26cd5ae](https://github.com/defilantech/LLMKube/commit/26cd5aebf517b006f9557a84051a13056ed88d82))
* Helm packaging, sample manifest, and concept doc for ModelRouter ([#448](https://github.com/defilantech/LLMKube/issues/448)) ([a513fdc](https://github.com/defilantech/LLMKube/commit/a513fdc44a66c741fd3c5bd4a464e80ebcc91338))
* ModelRouterReconciler skeleton with spec validation ([#445](https://github.com/defilantech/LLMKube/issues/445)) ([9b1a259](https://github.com/defilantech/LLMKube/commit/9b1a2597f6523c87f362c165626ea9a7512f2f15))
* reconcile router-proxy Deployment, Service, and ConfigMap ([#447](https://github.com/defilantech/LLMKube/issues/447)) ([856ecc3](https://github.com/defilantech/LLMKube/commit/856ecc3bae81f3038b287f29f78f51f0096e70e1))
* router-proxy binary with OpenAI streaming passthrough ([#446](https://github.com/defilantech/LLMKube/issues/446)) ([942d09a](https://github.com/defilantech/LLMKube/commit/942d09a6ab931dec8b92875610dc067622c3c5e6))
* router-proxy cluster e2e + runtime fail-closed 503 (closes [#430](https://github.com/defilantech/LLMKube/issues/430)) ([#450](https://github.com/defilantech/LLMKube/issues/450)) ([75151fa](https://github.com/defilantech/LLMKube/commit/75151fa1efcfb1b2e4febc47859245d8eaae033f))
* scaffold ModelRouter CRD types and deepcopy ([#442](https://github.com/defilantech/LLMKube/issues/442)) ([e6c60b3](https://github.com/defilantech/LLMKube/commit/e6c60b34bb0dd5167ff89fb9ae589168b7414d93))


### Bug Fixes

* close cloud-tier conns + drop local idle timeout (closes [#459](https://github.com/defilantech/LLMKube/issues/459)) ([#460](https://github.com/defilantech/LLMKube/issues/460)) ([173c26a](https://github.com/defilantech/LLMKube/commit/173c26a1c5663a0e8b4545a5a8cba7150e28e16e))
* don't quarantine backends on per-attempt context deadline (closes [#462](https://github.com/defilantech/LLMKube/issues/462)) ([#463](https://github.com/defilantech/LLMKube/issues/463)) ([80ef9c8](https://github.com/defilantech/LLMKube/commit/80ef9c81862ffb5e9e61594fffd12ee374f0d36a))
* **e2e:** unblock MicroShift SCC diagnostics + bump bootstrap timeout ([#466](https://github.com/defilantech/LLMKube/issues/466)) ([0c793b7](https://github.com/defilantech/LLMKube/commit/0c793b74b70a6f7baf1891f8e62cbbb59c3ff933))
* half-open circuit breaker on proxy + scale-to-zero status (closes [#452](https://github.com/defilantech/LLMKube/issues/452), [#453](https://github.com/defilantech/LLMKube/issues/453)) ([#454](https://github.com/defilantech/LLMKube/issues/454)) ([ac9302c](https://github.com/defilantech/LLMKube/commit/ac9302cb1b5ce90201276458aaf57b85b50cca2f))
* preserve external annotations on reconciler Deployment updates ([#468](https://github.com/defilantech/LLMKube/issues/468)) ([de580c1](https://github.com/defilantech/LLMKube/commit/de580c18f60e2239ee16027d77759ff7c729325d))


### Documentation

* add consumer-hardware model matrix guide ([#444](https://github.com/defilantech/LLMKube/issues/444)) ([dd07397](https://github.com/defilantech/LLMKube/commit/dd07397de39efd549581b5253ab2767bc442ba6c))
* **readme:** land ModelRouter prominently for the 0.7.8 release ([#464](https://github.com/defilantech/LLMKube/issues/464)) ([deb24bb](https://github.com/defilantech/LLMKube/commit/deb24bb572d85d40422e26c764a57f195642677a))
* **site:** air-gapped, OpenShift, macOS Metal guides + architecture refresh (Tier 1) ([#465](https://github.com/defilantech/LLMKube/issues/465)) ([5996a1e](https://github.com/defilantech/LLMKube/commit/5996a1e707df399070dcc67b3a6100b7c30cd8e5))
* **site:** drop stale "fifteen lines" claim in openshift-install Reference ([#467](https://github.com/defilantech/LLMKube/issues/467)) ([ec52ca8](https://github.com/defilantech/LLMKube/commit/ec52ca87da0644b839cb37d7a3db9cfbd1e5e4ac))

## [0.7.7](https://github.com/defilantech/LLMKube/compare/v0.7.6...v0.7.7) (2026-05-11)


### Features

* **agent:** vllm-swift runtime + TurboQuant passthrough ([#391](https://github.com/defilantech/LLMKube/issues/391)) ([#393](https://github.com/defilantech/LLMKube/issues/393)) ([2691e67](https://github.com/defilantech/LLMKube/commit/2691e67eb855e2af00b2767f046a34c1dfe07db7))
* **ci+chart:** make OpenShift a first-class deploy target (closes [#421](https://github.com/defilantech/LLMKube/issues/421)) ([#422](https://github.com/defilantech/LLMKube/issues/422)) ([798a13e](https://github.com/defilantech/LLMKube/commit/798a13e70d1b6a46e8dd08f448654fbed5f49de4))
* **crd:** add gpuMemoryUtilization and cpuOffloadGB to VLLMConfig ([#394](https://github.com/defilantech/LLMKube/issues/394)) ([6883f78](https://github.com/defilantech/LLMKube/commit/6883f784eae6813adb268ddb982b68d99faf45e7))
* **metal-agent:** emit Kubernetes events for memory-pressure transitions, evictions, skips, and respawn blocks (closes [#390](https://github.com/defilantech/LLMKube/issues/390)) ([#411](https://github.com/defilantech/LLMKube/issues/411)) ([e0d17d1](https://github.com/defilantech/LLMKube/commit/e0d17d1aa27246db1fda2d8bca4c90686d0fbc72))
* **observability:** runtime label on inference pods + recording rules + starter dashboard (refs [#409](https://github.com/defilantech/LLMKube/issues/409)) ([#410](https://github.com/defilantech/LLMKube/issues/410)) ([71743ed](https://github.com/defilantech/LLMKube/commit/71743ed7901d8f493a20583dea4c572d8d261304))


### Bug Fixes

* **controller:** default FSGroup to curl_group + Longhorn-backed e2e job (closes [#418](https://github.com/defilantech/LLMKube/issues/418), closes [#420](https://github.com/defilantech/LLMKube/issues/420)) ([adce90f](https://github.com/defilantech/LLMKube/commit/adce90fd46be37aa8ad6be566012b7c7dbd6d016))
* **controller:** stop hot-spinning on unreachable file:// model sources (closes [#405](https://github.com/defilantech/LLMKube/issues/405)) ([#412](https://github.com/defilantech/LLMKube/issues/412)) ([4ac6f57](https://github.com/defilantech/LLMKube/commit/4ac6f573910ea1f08aa41db2c3dbb374431a5490))


### Documentation

* add NVIDIA Blackwell B200 (sm_100) validation matrix (refs [#413](https://github.com/defilantech/LLMKube/issues/413)) ([#414](https://github.com/defilantech/LLMKube/issues/414)) ([bfda149](https://github.com/defilantech/LLMKube/commit/bfda149844c006d66acb5dcfc7c95909e2dfba8d))
* **operations:** seed runbooks index + first 2 entries (file:// hot-spin, metal-agent memory pressure) ([#417](https://github.com/defilantech/LLMKube/issues/417)) ([d3bce8d](https://github.com/defilantech/LLMKube/commit/d3bce8d7f1d481a5792135f5e46ffd5f77102334))
* port concepts/comparison to markdown (first Phase 1C content port) ([#403](https://github.com/defilantech/LLMKube/issues/403)) ([51c396b](https://github.com/defilantech/LLMKube/commit/51c396b9b0b66d2cabf985da4a3533347aefee76))
* **readme:** HN-launch readiness fixes (broken link, Apple Silicon CTA, quickstart memory) ([#401](https://github.com/defilantech/LLMKube/issues/401)) ([3e44bfb](https://github.com/defilantech/LLMKube/commit/3e44bfb8bcfb21ce950c3305b2ebca653645e191))
* refresh quickstart cast for v0.7.6 (HN launch) ([#404](https://github.com/defilantech/LLMKube/issues/404)) ([5abaddb](https://github.com/defilantech/LLMKube/commit/5abaddb99b994b393ac3201de4d363aba4d0f231))
* split docs/ into site/ and contributors/, prep for site rendering ([#396](https://github.com/defilantech/LLMKube/issues/396)) ([9299a31](https://github.com/defilantech/LLMKube/commit/9299a3168fef2f2afd1329a026f39486da42b8e6))
* **upgrade:** OpenShift / OKD / MicroShift installs must use `helm ... -f charts/llmkube/values-openshift.yaml` so restricted-v2 SCC can inject fsGroup from the namespace's allocated range ([adce90f](https://github.com/defilantech/LLMKube/commit/adce90fd46be37aa8ad6be566012b7c7dbd6d016))
* **upgrade:** operators using a custom `--init-container-image` whose user is not curl (uid=101 gid=102) should set `spec.podSecurityContext` on each InferenceService or pass `--default-fsgroup=<gid>` to the controller ([adce90f](https://github.com/defilantech/LLMKube/commit/adce90fd46be37aa8ad6be566012b7c7dbd6d016))
* **upgrade:** v0.7.7 rolls every InferenceService Pod once on first reconcile (Deployment template gains fsGroup=102 and the new `inference.llmkube.dev/runtime` label) ([adce90f](https://github.com/defilantech/LLMKube/commit/adce90fd46be37aa8ad6be566012b7c7dbd6d016))

## [0.7.6](https://github.com/defilantech/LLMKube/compare/v0.7.5...v0.7.6) (2026-05-03)


### Features

* **agent:** eviction safety floor + evictionProtection opt-out + late-spawn condition fix ([#186](https://github.com/defilantech/LLMKube/issues/186)) ([#384](https://github.com/defilantech/LLMKube/issues/384)) ([6544747](https://github.com/defilantech/LLMKube/commit/65447470c1fe4ca5291cb63eead12a3d9533de58))
* **agent:** memory-pressure eviction and respawn protection ([#186](https://github.com/defilantech/LLMKube/issues/186)) ([#382](https://github.com/defilantech/LLMKube/issues/382)) ([65a78b5](https://github.com/defilantech/LLMKube/commit/65a78b5409c1a98db8389516a7a8a2be3bc69dc2))
* **api:** add podAnnotations and podLabels passthrough (closes [#326](https://github.com/defilantech/LLMKube/issues/326)) ([#381](https://github.com/defilantech/LLMKube/issues/381)) ([baecd68](https://github.com/defilantech/LLMKube/commit/baecd687578f4fe240ce3d4a9d923908161fe10a))
* **api:** expose runtimeClassName on InferenceServiceSpec (closes [#375](https://github.com/defilantech/LLMKube/issues/375)) ([#380](https://github.com/defilantech/LLMKube/issues/380)) ([cc44ff5](https://github.com/defilantech/LLMKube/commit/cc44ff5e14ec0129cee411c5f704b28d15c6d38f))
* **crd:** add ParallelSlots support for vllm and fix llamacpp ([#340](https://github.com/defilantech/LLMKube/issues/340)) ([d81babb](https://github.com/defilantech/LLMKube/commit/d81babbc2b4ffb7437f6111e3b9e5e0c85ec66bc))


### Bug Fixes

* **catalog:** default phi-4-mini context to 8K (closes [#386](https://github.com/defilantech/LLMKube/issues/386)) ([#387](https://github.com/defilantech/LLMKube/issues/387)) ([7bcd685](https://github.com/defilantech/LLMKube/commit/7bcd6856837784c13f2534493ed059316f188db4))
* **controller:** drop model label from Deployment selector to make modelRef mutable (closes [#301](https://github.com/defilantech/LLMKube/issues/301)) ([#385](https://github.com/defilantech/LLMKube/issues/385)) ([a1de3bf](https://github.com/defilantech/LLMKube/commit/a1de3bf7801748666a0089bd44dbdc094a7e5244))
* derive metal InferenceService phase from Endpoints, not desiredReplicas (closes [#374](https://github.com/defilantech/LLMKube/issues/374)) ([#376](https://github.com/defilantech/LLMKube/issues/376)) ([350dafe](https://github.com/defilantech/LLMKube/commit/350dafe281d843527a53a10f4013a67b60d306cf))


### Documentation

* fix broken phi-3-mini command and dead benchmark link ([#369](https://github.com/defilantech/LLMKube/issues/369)) ([6a1fd58](https://github.com/defilantech/LLMKube/commit/6a1fd58b62966c3ee7c310c10859978bc75bf2df))
* HN launch prep README polish (llama.cpp credit, vLLM, KubeAI/llm-d) ([#371](https://github.com/defilantech/LLMKube/issues/371)) ([9d27774](https://github.com/defilantech/LLMKube/commit/9d277744d5fd3a59ea55888299f5c715e8e53523))

## [0.7.5](https://github.com/defilantech/LLMKube/compare/v0.7.4...v0.7.5) (2026-04-30)


### Bug Fixes

* **chart:** sync Helm CRDs from kubebuilder source and add CI guard ([#367](https://github.com/defilantech/LLMKube/issues/367)) ([73bd2b4](https://github.com/defilantech/LLMKube/commit/73bd2b40367a88c3f345fbcaf5872039505846d3))

## [0.7.4](https://github.com/defilantech/LLMKube/compare/v0.7.3...v0.7.4) (2026-04-29)


### Features

* **controller:** pin vLLM default image to v0.20.0 ([#362](https://github.com/defilantech/LLMKube/issues/362)) ([d2ae561](https://github.com/defilantech/LLMKube/commit/d2ae561bb4900a52e4e538cd10c19ca848d8e69b))


### Bug Fixes

* **controller:** defer HTTP(S) Model downloads to the workload init container ([#364](https://github.com/defilantech/LLMKube/issues/364)) ([469f542](https://github.com/defilantech/LLMKube/commit/469f5428d16a01a6f8f0797976f70e2f8ecba62b))

## [0.7.3](https://github.com/defilantech/LLMKube/compare/v0.7.2...v0.7.3) (2026-04-29)


### Features

* **agent:** cache-type-aware memory estimator + TurboQuant docs ([#355](https://github.com/defilantech/LLMKube/issues/355)) ([0697afd](https://github.com/defilantech/LLMKube/commit/0697afdc44939605bb76d0d0a8872c3d326ff934))
* **api:** add cacheTypeCustomK/V for non-enum llama.cpp KV cache types ([#351](https://github.com/defilantech/LLMKube/issues/351)) ([71bd762](https://github.com/defilantech/LLMKube/commit/71bd7622f983a1ea3a2b9032296346e094b753e1))
* **api:** add kvCacheCustomDtype for non-enum vLLM KV cache types ([#359](https://github.com/defilantech/LLMKube/issues/359)) ([5e796d0](https://github.com/defilantech/LLMKube/commit/5e796d0767e15a300dddd74819765c031aa5d7f4))


### Bug Fixes

* **agent:** respawn on InferenceService spec drift, honor replicas=0, and plumb full spec to llama-server flags ([#353](https://github.com/defilantech/LLMKube/issues/353)) ([ff54cad](https://github.com/defilantech/LLMKube/commit/ff54cad355096ce34d70aebf4fe114ab32745348))
* **controller:** use GGUF metadata name for downloaded model file basename ([#347](https://github.com/defilantech/LLMKube/issues/347)) ([e932c7a](https://github.com/defilantech/LLMKube/commit/e932c7ae1c484fab9f99202d9ea35a7c8abb3906))
* **vllm:** set enableServiceLinks=false on vLLM Pod spec ([#361](https://github.com/defilantech/LLMKube/issues/361)) ([01eb5c5](https://github.com/defilantech/LLMKube/commit/01eb5c5bcaf6740bc4054c5185cbcbdd17d2685f))
* **vllm:** use positional model argument instead of deprecated --model ([#360](https://github.com/defilantech/LLMKube/issues/360)) ([a17566c](https://github.com/defilantech/LLMKube/commit/a17566cc5211c2f2dea1a22dbe40c74031c486fa))

## [0.7.2](https://github.com/defilantech/LLMKube/compare/v0.7.1...v0.7.2) (2026-04-27)


### Features

* **agent:** expose Apple Silicon power gauges via powermetrics ([#334](https://github.com/defilantech/LLMKube/issues/334)) ([58a94a7](https://github.com/defilantech/LLMKube/commit/58a94a7729daf95162e3feca347b86e2119e51b5))
* **make:** one-command install-powermetrics-sudo + uninstall targets ([#336](https://github.com/defilantech/LLMKube/issues/336)) ([af48077](https://github.com/defilantech/LLMKube/commit/af4807795c605e97e532e2df38aec9566b8a1d25))


### Bug Fixes

* **agent:** make executor startup timeouts configurable; raise defaults to 120s ([#330](https://github.com/defilantech/LLMKube/issues/330)) ([5aa5fa2](https://github.com/defilantech/LLMKube/commit/5aa5fa2f5063bd14ff4396efec6ce93005dd8f5d))
* **agent:** reconcile orphaned Service+Endpoints on agent startup ([#332](https://github.com/defilantech/LLMKube/issues/332)) ([d88c541](https://github.com/defilantech/LLMKube/commit/d88c541e28badb179723dcb2c907646d2e833c49))

## [0.7.1](https://github.com/defilantech/LLMKube/compare/v0.7.0...v0.7.1) (2026-04-25)


### Features

* **agent:** pass Apple Silicon-optimized flags to llama-server ([#327](https://github.com/defilantech/LLMKube/issues/327)) ([a69ab6a](https://github.com/defilantech/LLMKube/commit/a69ab6a32c1039af53a2afcc8a8792e4932753eb))
* **chart:** add values.schema.json for Helm value validation ([#322](https://github.com/defilantech/LLMKube/issues/322)) ([1f8a34d](https://github.com/defilantech/LLMKube/commit/1f8a34d86077d0afb015db3d1567f7bd75f98bda))
* **crd:** extend InferenceService vLLM config for agentic-coding flags ([#306](https://github.com/defilantech/LLMKube/issues/306)) ([cb2aa6a](https://github.com/defilantech/LLMKube/commit/cb2aa6a8c8c921e3db11c71f9309b5b38dafb2df))
* **security:** supply-chain MVP — checksum install, govulncheck, gosec, codecov ([#310](https://github.com/defilantech/LLMKube/issues/310)) ([f17f59d](https://github.com/defilantech/LLMKube/commit/f17f59d7f3be20d9719591f0d70f9102968e8365))


### Bug Fixes

* **agent:** detect stalled K8s polling and exit for supervisor restart ([#328](https://github.com/defilantech/LLMKube/issues/328)) ([c0636cc](https://github.com/defilantech/LLMKube/commit/c0636cc7ede733f43c6d38c99c615fe4c1d73659))
* **agent:** let the kernel pick free ports for llama-server ([#321](https://github.com/defilantech/LLMKube/issues/321)) ([8111395](https://github.com/defilantech/LLMKube/commit/8111395c60f8f005082ace9efd7eb57b7f2b92d6))
* bump InferenceService spec.contextSize cap from 131072 to 2097152 ([#300](https://github.com/defilantech/LLMKube/issues/300)) ([a46a1bf](https://github.com/defilantech/LLMKube/commit/a46a1bf31ba222f66a7474d580500fe029640294))


### Documentation

* add ADOPTERS.md inviting public user listings ([#324](https://github.com/defilantech/LLMKube/issues/324)) ([871a0cb](https://github.com/defilantech/LLMKube/commit/871a0cb8b5eba4e1fb918dfab2c2d67938ad73c7))
* backfill ⚠ BREAKING CHANGES section into 0.7.0 changelog ([#296](https://github.com/defilantech/LLMKube/issues/296)) ([2ad4640](https://github.com/defilantech/LLMKube/commit/2ad4640c56b7592a5b3e7f337ec72444389012c9))

## [0.7.0](https://github.com/defilantech/LLMKube/compare/v0.6.0...v0.7.0) (2026-04-18)


### ⚠ BREAKING CHANGES

* **sharding:** `sharding.strategy: tensor` on a Model now correctly maps to llama.cpp's `--split-mode row` instead of silently falling back to `--split-mode layer`. Configs that set `strategy: tensor` expecting layer behavior may see performance regressions or new failure modes under concurrent load (particularly on consumer PCIe multi-GPU setups with quantized models). Explicitly set `strategy: layer` to retain the previous behavior. ([#291](https://github.com/defilantech/LLMKube/issues/291))
* **vllm:** InferenceService `spec.extraArgs` is now forwarded to the vLLM runtime. Previously `extraArgs` was silently ignored when `runtime: vllm`. Configs that placed llama.cpp-only flags in `extraArgs` on a vLLM InferenceService will start failing at pod startup. Audit any vLLM InferenceService that sets `extraArgs` before upgrading. ([#291](https://github.com/defilantech/LLMKube/issues/291))


### Features

* add hybrid GPU/CPU offloading support for MoE models ([#281](https://github.com/defilantech/LLMKube/issues/281)) ([2287f66](https://github.com/defilantech/LLMKube/commit/2287f664a638fc1a3e976ef63b902eb951adefa9))
* add tensor overrides and batch size controls for hybrid offloading ([#283](https://github.com/defilantech/LLMKube/issues/283)) ([8be4adc](https://github.com/defilantech/LLMKube/commit/8be4adc56bfb959049ae84a3da48cc4f8ab7d98e))
* expose additional runtime controls for llama.cpp and vllm ([#291](https://github.com/defilantech/LLMKube/issues/291)) ([2245718](https://github.com/defilantech/LLMKube/commit/2245718ae7790f399f96eb66e28717af3d593d97))
* recognize runtime-resolved sources (HF repo IDs) in Model controller ([#293](https://github.com/defilantech/LLMKube/issues/293)) ([953e8a7](https://github.com/defilantech/LLMKube/commit/953e8a7de00836cc82173de4b81e06a406d43772))


### Bug Fixes

* inherit runAsUser/runAsGroup from podSecurityContext  ([#274](https://github.com/defilantech/LLMKube/issues/274)) ([72b9b5c](https://github.com/defilantech/LLMKube/commit/72b9b5cb6260e918bc56bef0c0bf137d69216b85))


### Documentation

* surface breaking behavior changes for 0.7.0 ([#294](https://github.com/defilantech/LLMKube/issues/294)) ([e234a40](https://github.com/defilantech/LLMKube/commit/e234a400ca3a20cf43d6d29dbb9f7e361b577a00))

## [0.6.0](https://github.com/defilantech/LLMKube/compare/v0.5.3...v0.6.0) (2026-04-08)


### ⚠ BREAKING CHANGES

* update default CUDA image to server-cuda13 for Qwen3.5 and Blackwell support ([#262](https://github.com/defilantech/LLMKube/issues/262))

### Features

* add first-class PersonaPlex (Moshi) runtime backend ([#272](https://github.com/defilantech/LLMKube/issues/272)) ([2b1c948](https://github.com/defilantech/LLMKube/commit/2b1c948052806108ea2dea3debeea54379dc450b))
* add Grafana inference metrics dashboard ([#269](https://github.com/defilantech/LLMKube/issues/269)) ([be376c6](https://github.com/defilantech/LLMKube/commit/be376c67523cd2565997bd0cae886d19c2bc51ee))
* add HPA autoscaling for InferenceService ([#260](https://github.com/defilantech/LLMKube/issues/260)) ([2d16502](https://github.com/defilantech/LLMKube/commit/2d165023b3595a137104acff382474ee5498c5ba))
* add pluggable runtime backends for non-llama.cpp inference engines ([#271](https://github.com/defilantech/LLMKube/issues/271)) ([bb1576c](https://github.com/defilantech/LLMKube/commit/bb1576c04f685b5657aef7b0fb3f2c9a840fcd1a))
* add vLLM and TGI runtime backends with per-runtime HPA metrics ([#273](https://github.com/defilantech/LLMKube/issues/273)) ([441c7c7](https://github.com/defilantech/LLMKube/commit/441c7c77cf72936fd1cf31ea0b44914018579172))
* separate image registry from repository in Helm chart ([#268](https://github.com/defilantech/LLMKube/issues/268)) ([5c059a4](https://github.com/defilantech/LLMKube/commit/5c059a4f2b26227684c8e148793e259c05e82daa))
* support custom layer splits from GPUShardingSpec ([#267](https://github.com/defilantech/LLMKube/issues/267)) ([a37701c](https://github.com/defilantech/LLMKube/commit/a37701c0bd355b0e209650e8b52a243b61efcdc5))
* update default CUDA image to server-cuda13 for Qwen3.5 and Blackwell support ([#262](https://github.com/defilantech/LLMKube/issues/262)) ([cc9a95e](https://github.com/defilantech/LLMKube/commit/cc9a95eadd5077c6234f2cbf0518a804a303269a))

## [0.5.3](https://github.com/defilantech/LLMKube/compare/v0.5.2...v0.5.3) (2026-04-01)


### Features

* add KV cache type configuration and extraArgs escape hatch ([#256](https://github.com/defilantech/LLMKube/issues/256)) ([7a4b855](https://github.com/defilantech/LLMKube/commit/7a4b855d180cabd224b5121eff60298c3bfb24f2))
* add Ollama as runtime backend for Metal agent ([#258](https://github.com/defilantech/LLMKube/issues/258)) ([6148b89](https://github.com/defilantech/LLMKube/commit/6148b899547966bc18719bf73243af873d9064c4))
* add oMLX as alternative runtime backend for Metal agent ([#257](https://github.com/defilantech/LLMKube/issues/257)) ([eaf9045](https://github.com/defilantech/LLMKube/commit/eaf90451c9f15cdd02f6eafbef12eda81768b955))


### Bug Fixes

* improve Metal agent usability ([#254](https://github.com/defilantech/LLMKube/issues/254)) ([149c582](https://github.com/defilantech/LLMKube/commit/149c582305b32b9bd51f498d7e2a85aa0dead223))

## [0.5.2](https://github.com/defilantech/LLMKube/compare/v0.5.1...v0.5.2) (2026-03-27)


### Features

* add pod security context defaults and CRD overrides ([#239](https://github.com/defilantech/LLMKube/issues/239)) ([904432b](https://github.com/defilantech/LLMKube/commit/904432b4e4669d9a2efb44de43f2c49c4747d9b5))


### Documentation

* add CNCF/Kubernetes trademark disclaimer ([#246](https://github.com/defilantech/LLMKube/issues/246)) ([27a49eb](https://github.com/defilantech/LLMKube/commit/27a49eb46e37d53c028a53e46526ef9f408c9b7e))
* add Discord community link ([#236](https://github.com/defilantech/LLMKube/issues/236)) ([c0d499d](https://github.com/defilantech/LLMKube/commit/c0d499d0bf5cbac220851ec5da81a04d466d2bdc))
* add OpenShift troubleshooting to README ([#241](https://github.com/defilantech/LLMKube/issues/241)) ([47fd1b0](https://github.com/defilantech/LLMKube/commit/47fd1b039e64192e459ba50fc76f4d05293eabd8))

## [0.5.1](https://github.com/defilantech/LLMKube/compare/v0.5.0...v0.5.1) (2026-03-16)


### Features

* add memory pressure watchdog with runtime monitoring ([#216](https://github.com/defilantech/LLMKube/issues/216)) ([5fa6d54](https://github.com/defilantech/LLMKube/commit/5fa6d54f778e68f9bf4739fb232e05c9a9791971))
* add pvc:// model source and SHA256 integrity verification ([#229](https://github.com/defilantech/LLMKube/issues/229)) ([1b94f5d](https://github.com/defilantech/LLMKube/commit/1b94f5d560dd596315af76a40f377d6b8f34cedf))
* auto-detect llama-server from Homebrew paths on macOS ([#215](https://github.com/defilantech/LLMKube/issues/215)) ([a1e4302](https://github.com/defilantech/LLMKube/commit/a1e4302702e6531f2a6cbda9434d9613514cf15a))


### Bug Fixes

* controller metrics port declarations and ServiceMonitor consistency ([#214](https://github.com/defilantech/LLMKube/issues/214)) ([296ec99](https://github.com/defilantech/LLMKube/commit/296ec990c17484a823a6b4261ce409b894535e02))
* correct CHANGELOG entry from 0.4.21 to 0.5.0 ([#212](https://github.com/defilantech/LLMKube/issues/212)) ([f7f703a](https://github.com/defilantech/LLMKube/commit/f7f703ad32404a2a38aeab428ed76c37b0a8fcf8))
* quote job-level if expression to fix YAML parsing in helm-chart workflow ([8714b9f](https://github.com/defilantech/LLMKube/commit/8714b9fc60d7020be4c21d5764ced7b88bd7f97a))

## [0.5.0](https://github.com/defilantech/LLMKube/compare/v0.4.20...v0.5.0) (2026-03-04)


### Features

* add pre-flight memory validation for Metal agent ([#204](https://github.com/defilantech/LLMKube/issues/204)) ([ba252ef](https://github.com/defilantech/LLMKube/commit/ba252efc6bf6cfbf6f29866a249cd6543e03dfed))
* add health checks, metrics, and continuous monitoring to Metal agent ([#205](https://github.com/defilantech/LLMKube/issues/205)) ([a113fd1](https://github.com/defilantech/LLMKube/commit/a113fd1b5a8eb04608adbab719dee03c11d2fa54))
* add per-model memoryBudget and memoryFraction CRD fields ([#206](https://github.com/defilantech/LLMKube/issues/206)) ([e632369](https://github.com/defilantech/LLMKube/commit/e6323692ae35ea2e9bcd07abbc074b4393da2875))


### Bug Fixes

* **agent:** unregister service endpoints on metal process delete ([#168](https://github.com/defilantech/LLMKube/issues/168)) ([147b9bc](https://github.com/defilantech/LLMKube/commit/147b9bcb7a8ece1ecb2a91c62a249c591f9c9a07))
* enable controller metrics endpoint in Helm chart ([#195](https://github.com/defilantech/LLMKube/issues/195)) ([70940af](https://github.com/defilantech/LLMKube/commit/70940afc948dae5a1a6d1fc6d4e330908ec973dc))
* prevent model re-download of cached models after helm upgrade ([#203](https://github.com/defilantech/LLMKube/issues/203)) ([a8f9a88](https://github.com/defilantech/LLMKube/commit/a8f9a886f0e9d081505183f1d30d2a37ee050068))
* use Recreate strategy for GPU workloads to prevent rolling update deadlock ([#196](https://github.com/defilantech/LLMKube/issues/196)) ([2e45181](https://github.com/defilantech/LLMKube/commit/2e4518106cd9306b4d2a472fc9c9432d015a69c9))


### Documentation

* rewrite README for clarity, positioning, and growth ([#190](https://github.com/defilantech/LLMKube/issues/190)) ([a7fc152](https://github.com/defilantech/LLMKube/commit/a7fc15238fe5c3dfe698aa6acb05234fe8b51adc))

## [0.4.20](https://github.com/defilantech/LLMKube/compare/v0.4.19...v0.4.20) (2026-02-28)


### Features

* add license compliance scanning for GGUF models ([#188](https://github.com/defilantech/LLMKube/issues/188)) ([c26400a](https://github.com/defilantech/LLMKube/commit/c26400a319af79995dc7bd5b20843b9926f0cfb4))
* add Prometheus metrics, OpenTelemetry tracing, and inference observability ([#189](https://github.com/defilantech/LLMKube/issues/189)) ([c653ff1](https://github.com/defilantech/LLMKube/commit/c653ff1c2f1dbbe564a56cee22f34efc2d463049))
* add PVC inspection to cache list for orphaned entry detection ([#183](https://github.com/defilantech/LLMKube/issues/183)) ([2723d92](https://github.com/defilantech/LLMKube/commit/2723d92b7030472f8dd0ba57f5dd523356672d94))
* **agent:** add structured zap logging to metal agent ([#164](https://github.com/defilantech/LLMKube/issues/164)) ([e9d143c](https://github.com/defilantech/LLMKube/commit/e9d143c25e61438642ca2a033c3daaaf7e3a93d6))
* **deps:** upgrade to Kubernetes 1.35 and controller-runtime v0.23.1 ([#175](https://github.com/defilantech/LLMKube/issues/175)) ([3c323f4](https://github.com/defilantech/LLMKube/commit/3c323f46ecfb3ec1b1d4849ce4b9c710d7cc8658))


### Bug Fixes

* correct Metal quickstart docs for selectorless services ([#173](https://github.com/defilantech/LLMKube/issues/173)) ([89471ec](https://github.com/defilantech/LLMKube/commit/89471ec7f5569ca0192fb76c15bd408aa2986c1b))
* prevent command injection in init container shell commands ([#172](https://github.com/defilantech/LLMKube/issues/172)) ([3aa9cc3](https://github.com/defilantech/LLMKube/commit/3aa9cc30fce879f49ff42389fda5633c00d6396a))
* remove mutable latest tags and pin container images ([#174](https://github.com/defilantech/LLMKube/issues/174)) ([3c4569a](https://github.com/defilantech/LLMKube/commit/3c4569aafdb73127eeee2260e8d722459a91823a))


### Documentation

* add Apple Silicon Metal option to bug report template ([#169](https://github.com/defilantech/LLMKube/issues/169)) ([e7689d8](https://github.com/defilantech/LLMKube/commit/e7689d868f2034f4e60d37a813cb4c72bf2b3f37))

## [0.4.19](https://github.com/defilantech/LLMKube/compare/v0.4.18...v0.4.19) (2026-02-21)


### Features

* add --jinja flag for tool/function calling support ([#162](https://github.com/defilantech/LLMKube/issues/162)) ([47624ca](https://github.com/defilantech/LLMKube/commit/47624ca2e060aa0e1accd9950f3b15cd66312f46))

## [0.4.18](https://github.com/defilantech/LLMKube/compare/v0.4.17...v0.4.18) (2026-02-20)


### Bug Fixes

* **agent:** read contextSize from InferenceService CRD ([#160](https://github.com/defilantech/LLMKube/issues/160)) ([17f58d4](https://github.com/defilantech/LLMKube/commit/17f58d40c20c83d1cbb1d19e2ec473c18af7b218))


### Documentation

* update README and Metal Agent guide for remote K8s architecture ([#156](https://github.com/defilantech/LLMKube/issues/156)) ([79145b2](https://github.com/defilantech/LLMKube/commit/79145b2b2b59137d7792e0d1c3c36c1b78b9b460))

## [0.4.17](https://github.com/defilantech/LLMKube/compare/v0.4.16...v0.4.17) (2026-02-20)


### Bug Fixes

* **agent:** filter InferenceServices by Metal accelerator type ([#157](https://github.com/defilantech/LLMKube/issues/157)) ([5737bb7](https://github.com/defilantech/LLMKube/commit/5737bb7c1cc9510507f9fda91519ecbfd1ab3271))

## [0.4.16](https://github.com/defilantech/LLMKube/compare/v0.4.15...v0.4.16) (2026-02-20)


### Features

* **agent:** add --host-ip flag for remote K8s cluster support ([#155](https://github.com/defilantech/LLMKube/issues/155)) ([b425569](https://github.com/defilantech/LLMKube/commit/b42556919afe0089e6ebde1d35fe79459e116fcb))


### Documentation

* Add Metal Agent (Apple Silicon) support to README ([#151](https://github.com/defilantech/LLMKube/issues/151)) ([3579426](https://github.com/defilantech/LLMKube/commit/3579426ea24616f75104233f90fdb0b90b933070))

## [0.4.15](https://github.com/defilantech/LLMKube/compare/v0.4.14...v0.4.15) (2026-02-15)


### Bug Fixes

* **inference:** pass value to --flash-attn for newer llama.cpp versions ([#148](https://github.com/defilantech/LLMKube/issues/148)) ([25e08d0](https://github.com/defilantech/LLMKube/commit/25e08d0270241d15c0a1815c11e12627cfe5eb27))

## [0.4.14](https://github.com/defilantech/LLMKube/compare/v0.4.13...v0.4.14) (2026-02-15)


### Features

* **gguf:** add native Go GGUF parser with CRD integration and CLI inspect ([#140](https://github.com/defilantech/LLMKube/issues/140)) ([9d96ed4](https://github.com/defilantech/LLMKube/commit/9d96ed4b7e0409d14808f536b23f615b7e232004))
* **inference:** add flashAttention and contextSize to sample manifest ([914c929](https://github.com/defilantech/LLMKube/commit/914c929e0ae180bc7c1219582fc97e85398e47fe)), closes [#145](https://github.com/defilantech/LLMKube/issues/145)

## [0.4.13](https://github.com/defilantech/LLMKube/compare/v0.4.12...v0.4.13) (2026-02-07)


### Features

* **controller:** make init container image configurable ([#128](https://github.com/defilantech/LLMKube/issues/128)) ([38ccdf0](https://github.com/defilantech/LLMKube/commit/38ccdf0a1075373ce4b3e40d42ed6fe8e558f1ba))
* expose llama.cpp parallel slots in InferenceService CRD ([#133](https://github.com/defilantech/LLMKube/issues/133)) ([cae7b52](https://github.com/defilantech/LLMKube/commit/cae7b52d780bff34ce1a6c1cf86165859e171621))
* **helm:** add optional NetworkPolicy for controller manager ([#135](https://github.com/defilantech/LLMKube/issues/135)) ([8d61ce3](https://github.com/defilantech/LLMKube/commit/8d61ce3811a0d79be136ed078388a39310ad4f3a))
* update model catalog with DeepSeek R1 and refresh stale entries ([#131](https://github.com/defilantech/LLMKube/issues/131)) ([89eb5a6](https://github.com/defilantech/LLMKube/commit/89eb5a6e99b27f8dbc4847727bc0d72834919a70))

## [0.4.12](https://github.com/defilantech/LLMKube/compare/v0.4.11...v0.4.12) (2026-01-22)


### Features

* add custom CA support and fix deprecated image tags ([#124](https://github.com/defilantech/LLMKube/issues/124)) ([5ec912e](https://github.com/defilantech/LLMKube/commit/5ec912e1c80076cd9a7af0c9e9ad98fe92381a27))

## [0.4.11](https://github.com/defilantech/LLMKube/compare/v0.4.10...v0.4.11) (2026-01-22)


### Bug Fixes

* **cli:** use numeric comparison for version checking ([#109](https://github.com/defilantech/LLMKube/issues/109)) ([05e0025](https://github.com/defilantech/LLMKube/commit/05e00250b9be4e115e7be78bef84e08bd2d1d1b4))
* **controller:** use fully qualified image names for curl ([#121](https://github.com/defilantech/LLMKube/issues/121)) ([213660b](https://github.com/defilantech/LLMKube/commit/213660b031c6ea19d9abe5eeaab5de3abb5aebc8))

## [0.4.10](https://github.com/defilantech/LLMKube/compare/v0.4.9...v0.4.10) (2025-12-07)


### Features

* add 32B models to catalog with --context flag ([#88](https://github.com/defilantech/LLMKube/issues/88)) ([6c06602](https://github.com/defilantech/LLMKube/commit/6c066026b11c108072fab77c2d7b6ee61b432203))
* add air-gapped deployment support for local model paths ([#85](https://github.com/defilantech/LLMKube/issues/85)) ([31fe8d0](https://github.com/defilantech/LLMKube/commit/31fe8d051fa5be51626dc64c4bc56d05dbfade7a))
* add GPU observability config and Grafana dashboard ([#105](https://github.com/defilantech/LLMKube/issues/105)) ([571643f](https://github.com/defilantech/LLMKube/commit/571643fadcc6a106f360b8bd1ceec26360f62058))
* **cli:** add comprehensive benchmark test suites and sweeps ([#107](https://github.com/defilantech/LLMKube/issues/107)) ([323a28a](https://github.com/defilantech/LLMKube/commit/323a28ad8c1412fd1a09cd7e18cf7b42d7b55de8))
* **cli:** add stress testing mode to benchmark command ([#104](https://github.com/defilantech/LLMKube/issues/104)) ([530c82e](https://github.com/defilantech/LLMKube/commit/530c82e1f0c62b56a7accae9026ef8a6df7ce1cd))


### Documentation

* add community standards and security policy ([#92](https://github.com/defilantech/LLMKube/issues/92)) ([e7c9cad](https://github.com/defilantech/LLMKube/commit/e7c9cad0887cbe43bcc72c01500919b9268009a2))
* update documentation for v0.4.9 GPU scheduling features ([#83](https://github.com/defilantech/LLMKube/issues/83)) ([0934e8f](https://github.com/defilantech/LLMKube/commit/0934e8f605b3d65eee74fd2e62e8eeb3132f4518))

## [0.4.9](https://github.com/defilantech/LLMKube/compare/v0.4.8...v0.4.9) (2025-12-01)


### Features

* add GPU contention visibility, queue position, and priority classes ([#81](https://github.com/defilantech/LLMKube/issues/81)) ([c0220e5](https://github.com/defilantech/LLMKube/commit/c0220e5a392a5958d826eabdffe68bfce46f84d4))


### Documentation

* add getting started video to README ([#76](https://github.com/defilantech/LLMKube/issues/76)) ([ceb83d7](https://github.com/defilantech/LLMKube/commit/ceb83d79d4338eafcedb6cce583321bb6100bc7e))

## [0.4.8](https://github.com/defilantech/LLMKube/compare/v0.4.7...v0.4.8) (2025-11-27)


### Features

* Support configurable context size for llama.cpp server ([#73](https://github.com/defilantech/LLMKube/issues/73)) ([6f8e04b](https://github.com/defilantech/LLMKube/commit/6f8e04bc9c420772e65a5dffe1961ae9e92144a4))

## [0.4.7](https://github.com/defilantech/LLMKube/compare/v0.4.6...v0.4.7) (2025-11-26)


### Bug Fixes

* Don't mark Helm chart release as latest ([#70](https://github.com/defilantech/LLMKube/issues/70)) ([761b154](https://github.com/defilantech/LLMKube/commit/761b154a7f51fa38727c70f82dd8020b59807289))

## [0.4.6](https://github.com/defilantech/LLMKube/compare/v0.4.5...v0.4.6) (2025-11-26)


### Bug Fixes

* Set empty component to prevent llmkube- prefix in releases ([#68](https://github.com/defilantech/LLMKube/issues/68)) ([45b61c6](https://github.com/defilantech/LLMKube/commit/45b61c680e4880040f9cab36fbf28cb931686462))

## [0.4.5](https://github.com/defilantech/LLMKube/compare/v0.4.4...v0.4.5) (2025-11-26)


### Bug Fixes

* Clean up release process - single release with proper notes ([#66](https://github.com/defilantech/LLMKube/issues/66)) ([4deae85](https://github.com/defilantech/LLMKube/commit/4deae853cacc3aae86b5782c3e2ee79e18011f7e))

## [0.4.4](https://github.com/defilantech/LLMKube/compare/v0.4.3...v0.4.4) (2025-11-26)


### Bug Fixes

* Trigger GoReleaser and Helm release from Release Please workflow ([#64](https://github.com/defilantech/LLMKube/issues/64)) ([9a37a77](https://github.com/defilantech/LLMKube/commit/9a37a77e556d6f811cb6a090125a4a73e2e9c346))

## [0.4.3](https://github.com/defilantech/LLMKube/compare/v0.4.2...v0.4.3) (2025-11-26)


### Features

* Add benchmark command and reorganize documentation ([58307be](https://github.com/defilantech/LLMKube/commit/58307bece720644bbdf1e27026a90279b9009c51))
* Add benchmark command and reorganize documentation ([ac8888e](https://github.com/defilantech/LLMKube/commit/ac8888ea2ac41f90ebd6b529deea86b2fa67f24f)), closes [#6](https://github.com/defilantech/LLMKube/issues/6)
* Add Helm chart for easy installation ([5718804](https://github.com/defilantech/LLMKube/commit/5718804a33905a30393993156c8c0ec4a56d0538))
* Add Helm chart for easy installation with comprehensive CI testing ([3ea3bfd](https://github.com/defilantech/LLMKube/commit/3ea3bfd27ce864f7884f25ae9db65ed52eb68e01)), closes [#9](https://github.com/defilantech/LLMKube/issues/9)
* Add Metal GPU support for macOS (Apple Silicon) ([f673c26](https://github.com/defilantech/LLMKube/commit/f673c26bd4ac1a285dc7e72ffe6a930bc586b855)), closes [#33](https://github.com/defilantech/LLMKube/issues/33)
* Add model catalog with 10 pre-configured models ([404d722](https://github.com/defilantech/LLMKube/commit/404d722e70d3e885f1e437ebdadf38fe43c7689a))
* Add model catalog with 10 pre-configured models (Phase 1) ([0fd969a](https://github.com/defilantech/LLMKube/commit/0fd969a8268d47045f18771206036cc5d243ba3e))
* Add persistent model cache to avoid re-downloading ([83f844f](https://github.com/defilantech/LLMKube/commit/83f844f7b8ca18c2eed407b0f6995f2dc13e0965)), closes [#52](https://github.com/defilantech/LLMKube/issues/52)
* Add Release Please automation and version-agnostic docs ([dc2d54e](https://github.com/defilantech/LLMKube/commit/dc2d54ea15f936a62b6fa1d382c1f606d97a5610))
* **helm:** Add image digest support for production deployments ([a38801d](https://github.com/defilantech/LLMKube/commit/a38801dd61d5f6606209577744cc5376bf1eb626))
* Implement automatic port forwarding for benchmark command ([472b3ae](https://github.com/defilantech/LLMKube/commit/472b3ae74b73d1d55d5a8a2051625ed1c3834ad9))
* Multi-GPU support with layer-based sharding ([#47](https://github.com/defilantech/LLMKube/issues/47)) ([4797609](https://github.com/defilantech/LLMKube/commit/479760973eb811a0b7a71c711f52ca3d8695b761))
* Persistent model cache with per-namespace PVC support ([ab04261](https://github.com/defilantech/LLMKube/commit/ab0426161e3765e539e82ccbf864da943974f199))
* Set up Helm repository on GitHub Pages ([8d62737](https://github.com/defilantech/LLMKube/commit/8d62737931093e429b832f6f862457056fb80cb4))
* Support per-namespace model cache PVCs ([c3cb891](https://github.com/defilantech/LLMKube/commit/c3cb891dc74c3718f495068c98418d84c78b6da9))


### Bug Fixes

* Add cacheKey to CRD and restrict cache to llmkube-system namespace ([464c23d](https://github.com/defilantech/LLMKube/commit/464c23d07bffebcab8cda58d8ce8d00ad8d4ecba))
* Add CRD keep policy and improve security test reliability ([ff32296](https://github.com/defilantech/LLMKube/commit/ff32296a45174bdce6070844a68007e2c45cf3fe))
* Add Helm chart publishing to release workflow ([8baf9c4](https://github.com/defilantech/LLMKube/commit/8baf9c4b09ea27f8b229adb499582f83eff2e5be))
* Add Helm chart publishing to release workflow ([03bab72](https://github.com/defilantech/LLMKube/commit/03bab72a74496085b79e3c51838f9853ed674062))
* Add Homebrew archive IDs and v0.3.0 release notes ([cea933b](https://github.com/defilantech/LLMKube/commit/cea933beac2607122772d14184b35da04738b7f9))
* Address lint issues in benchmark command ([bf80610](https://github.com/defilantech/LLMKube/commit/bf806107c664425d9f8a4a3056600ba6ec95b34e))
* Address linter errors in catalog implementation ([8932e4f](https://github.com/defilantech/LLMKube/commit/8932e4fbb3fe8d1fea1fedba5bb18f3cd02808c8))
* Address linter issues in Metal agent code ([3f1f678](https://github.com/defilantech/LLMKube/commit/3f1f678502c985b04d48a1c8c8bc44ea68d8a542))
* **controller:** Add Model watch to InferenceService controller ([cb4e201](https://github.com/defilantech/LLMKube/commit/cb4e2019583a811fa98af1a446bd0df6b6c3cba2))
* Correct CLI binary path in E2E tests ([41af555](https://github.com/defilantech/LLMKube/commit/41af55589ba6b17f07119b50d96db9c39eac6ea3))
* Fix GoReleaser Homebrew tap configuration for v0.3.0 ([4e95c04](https://github.com/defilantech/LLMKube/commit/4e95c04718b83acf59fb4401bbb9c897e34b4a5c))
* Further increase Helm CI timeout and readiness probe delay ([5453d66](https://github.com/defilantech/LLMKube/commit/5453d66a21be60af17528724c4c760b7524c358f))
* Further increase Helm CI timeout and readiness probe delay ([fd577d3](https://github.com/defilantech/LLMKube/commit/fd577d3137da086346524f1802e47219feefa1fa))
* Handle resp.Body.Close error in version check (linter) ([fb3adf5](https://github.com/defilantech/LLMKube/commit/fb3adf57913744e08ebffb58af6877bd15fbeb93))
* Increase Helm chart CI timeout from 2m to 5m ([7a08b45](https://github.com/defilantech/LLMKube/commit/7a08b45a3f96fa85ec71f609d8c035c4a3e91db9))
* Increase Helm chart CI timeout from 2m to 5m ([ced2210](https://github.com/defilantech/LLMKube/commit/ced2210ea28d453fdac4c7346bc98f66684893b1))
* InferenceService stuck in Pending when Model becomes Ready ([4d20aec](https://github.com/defilantech/LLMKube/commit/4d20aec51760ed5fa6946a1be57045eee4b84593))
* Metal agent production fixes and testing improvements ([8744c7b](https://github.com/defilantech/LLMKube/commit/8744c7b54e23cbb77609a97340d9be9dd5da931c))
* Resolve Helm chart CI test failures ([9919696](https://github.com/defilantech/LLMKube/commit/99196961bf91e4c285182211a7a6fdec574ae7e7))
* Resolve staticcheck SA5011 lint errors and update CONTRIBUTING.md ([#60](https://github.com/defilantech/LLMKube/issues/60)) ([c0b5824](https://github.com/defilantech/LLMKube/commit/c0b5824fa3c42a547c1c760c7dbb5dd68bd4e89f))
* Sanitize Service names for DNS-1035 compliance (v0.3.3) ([db81990](https://github.com/defilantech/LLMKube/commit/db819902a121628c196899b9b449eeccf3be9394))
* Sanitize Service names to comply with DNS-1035 requirements ([b431986](https://github.com/defilantech/LLMKube/commit/b431986ceae6b383ee064bec595c922a42394a8e))
* Skip containerized Deployment for Metal accelerator and add version check ([d300e64](https://github.com/defilantech/LLMKube/commit/d300e64efb57a10917018c000d9d855d51d9dcc6))
* Skip containerized Deployment for Metal accelerator and add version check ([8dab955](https://github.com/defilantech/LLMKube/commit/8dab955a2d1e728fe8a9b1b2971a4906454d71c3))
* Suppress Endpoints API deprecation warnings ([e70a4b3](https://github.com/defilantech/LLMKube/commit/e70a4b391725a70a82d78d47a7d4f6d2b898dcc8))
* Update operator deployment to use correct container image ([00fee75](https://github.com/defilantech/LLMKube/commit/00fee7580b3661259e8c09491739a86f685da6e9))
* Update operator deployment to use correct container image ([4c67a78](https://github.com/defilantech/LLMKube/commit/4c67a7806232c687b7b2450660735d9265d507b8))
* Update version.go to 0.2.1 and add automation for future releases ([8dd613d](https://github.com/defilantech/LLMKube/commit/8dd613d7de88f93e150930ea11f6aad3760b792a))
* Update version.go to 0.2.1 and add automation for future releases ([2ff68bd](https://github.com/defilantech/LLMKube/commit/2ff68bdc0e40ab9ee8337403af649fda7354ad7c))
* Use simple v* tag format for releases ([#62](https://github.com/defilantech/LLMKube/issues/62)) ([bda9f19](https://github.com/defilantech/LLMKube/commit/bda9f19157e8ececd995e7488b751fdeb53cf144))
* Use workspace path for kubeconform validation ([fc066d8](https://github.com/defilantech/LLMKube/commit/fc066d8d0f9175382fa7cfab5f40c755739e175f))


### Documentation

* Add CLI option to quick start, keep kubectl as fallback ([f6829ee](https://github.com/defilantech/LLMKube/commit/f6829ee44a33e114921fbc60557f1268e144e22d))
* Add release notes for v0.3.2 ([177abf8](https://github.com/defilantech/LLMKube/commit/177abf812f220cd4a4b203a978a71c997bfdb5b6))
* Add release notes for v0.3.2 ([ca1bb12](https://github.com/defilantech/LLMKube/commit/ca1bb12f0e99392e91fb99a9e946138d8d466674))
* Add release notes for v0.4.0 ([144b960](https://github.com/defilantech/LLMKube/commit/144b9603fd6f96ef65d7ff83a2e72dc6c186a7ae))
* Add release notes for v0.4.0 ([a61321f](https://github.com/defilantech/LLMKube/commit/a61321f74add8fba1651b25b134e77468f8e8d43))
* Overhaul README and roadmap for public launch ([b42c17e](https://github.com/defilantech/LLMKube/commit/b42c17e1fd796b15976ea81d700f751da85041dc))
* Update binary download links to version 0.2.1 ([fad530a](https://github.com/defilantech/LLMKube/commit/fad530a58384787d480146bdac27f26256a04d82))
* Update binary download links to version 0.2.1 ([63bb0fa](https://github.com/defilantech/LLMKube/commit/63bb0fa1937afe0ff7fc0fe1c569b64459d360b7))
* Update Helm installation to use GitHub Pages repository ([477e037](https://github.com/defilantech/LLMKube/commit/477e037a41dca72347b98a3ed1995dbebb30189c))
* Update MODEL-CACHE.md for per-namespace PVC pattern ([0be3f46](https://github.com/defilantech/LLMKube/commit/0be3f4697fd249aba4e9120de93fe0d5942a3f90))

## [0.4.2](https://github.com/defilantech/LLMKube/compare/llmkubev0.4.1...llmkubev0.4.2) (2025-11-26)


### Bug Fixes

* Resolve staticcheck SA5011 lint errors and update CONTRIBUTING.md ([#60](https://github.com/defilantech/LLMKube/issues/60)) ([c0b5824](https://github.com/defilantech/LLMKube/commit/c0b5824fa3c42a547c1c760c7dbb5dd68bd4e89f))

## [0.4.1](https://github.com/defilantech/LLMKube/compare/llmkube-0.4.0...llmkubev0.4.1) (2025-11-26)


### Features

* Add benchmark command and reorganize documentation ([58307be](https://github.com/defilantech/LLMKube/commit/58307bece720644bbdf1e27026a90279b9009c51))
* Add benchmark command and reorganize documentation ([ac8888e](https://github.com/defilantech/LLMKube/commit/ac8888ea2ac41f90ebd6b529deea86b2fa67f24f)), closes [#6](https://github.com/defilantech/LLMKube/issues/6)
* Add persistent model cache to avoid re-downloading ([83f844f](https://github.com/defilantech/LLMKube/commit/83f844f7b8ca18c2eed407b0f6995f2dc13e0965)), closes [#52](https://github.com/defilantech/LLMKube/issues/52)
* Add Release Please automation and version-agnostic docs ([dc2d54e](https://github.com/defilantech/LLMKube/commit/dc2d54ea15f936a62b6fa1d382c1f606d97a5610))
* **helm:** Add image digest support for production deployments ([a38801d](https://github.com/defilantech/LLMKube/commit/a38801dd61d5f6606209577744cc5376bf1eb626))
* Implement automatic port forwarding for benchmark command ([472b3ae](https://github.com/defilantech/LLMKube/commit/472b3ae74b73d1d55d5a8a2051625ed1c3834ad9))
* Persistent model cache with per-namespace PVC support ([ab04261](https://github.com/defilantech/LLMKube/commit/ab0426161e3765e539e82ccbf864da943974f199))
* Support per-namespace model cache PVCs ([c3cb891](https://github.com/defilantech/LLMKube/commit/c3cb891dc74c3718f495068c98418d84c78b6da9))


### Bug Fixes

* Add cacheKey to CRD and restrict cache to llmkube-system namespace ([464c23d](https://github.com/defilantech/LLMKube/commit/464c23d07bffebcab8cda58d8ce8d00ad8d4ecba))
* Address lint issues in benchmark command ([bf80610](https://github.com/defilantech/LLMKube/commit/bf806107c664425d9f8a4a3056600ba6ec95b34e))


### Documentation

* Update MODEL-CACHE.md for per-namespace PVC pattern ([0be3f46](https://github.com/defilantech/LLMKube/commit/0be3f4697fd249aba4e9120de93fe0d5942a3f90))

## [0.3.0] - 2025-11-23

### Added

#### Metal GPU Support for macOS (Apple Silicon)
- **Native Metal GPU Acceleration**: Full support for Apple Silicon (M1/M2/M3/M4) GPUs
  - 60-120 tok/s generation on M4 Max (Llama 3.1 8B: 40-60 tok/s, Llama 3.2 3B: 80-120 tok/s)
  - Native llama-server processes with Metal GPU offloading
  - Hybrid architecture: Kubernetes orchestration + native Metal performance
- **Metal Agent**: Background daemon for macOS that manages llama-server processes
  - Watches InferenceService CRDs and spawns native processes
  - Automatic Service and Endpoints creation for cluster integration
  - Health checking and process lifecycle management
  - Configurable via LaunchAgent (deployment/macos/com.llmkube.metal-agent.plist)
- **Platform Detection**: Automatic detection of Metal availability and GPU capabilities
- **CLI Metal Support**: `--accelerator metal` flag for one-command Metal deployments
  - `llmkube deploy llama-3.1-8b --accelerator metal`
  - Automatic GPU layer configuration and optimization
- **Multi-Accelerator Support**: Unified CLI for CUDA (cloud) and Metal (local) deployments
  - Same Kubernetes CRDs work across both platforms
  - Test locally on Mac, deploy to cloud with same configs

#### Developer Experience
- **GoReleaser Configuration**: Multi-platform CLI builds for macOS, Linux, Windows
  - Separate Metal agent binary for macOS (Intel + Apple Silicon)
  - Automated release workflow with GitHub Actions
- **Metal Quick Start Guide**: Comprehensive guide at `examples/metal-quickstart/README.md`
  - Architecture diagrams and explanations
  - Step-by-step setup instructions
  - Troubleshooting and performance tuning
- **macOS Deployment Guide**: Production deployment instructions at `deployment/macos/README.md`

### Changed
- **Deploy Command**: Enhanced to support Metal accelerator alongside GPU flag
- **Service Registry**: Added support for manual Endpoints management to bridge native processes

### Fixed
- Endpoints API deprecation warnings (SA1019) with appropriate nolint directives
- Metal agent linter issues and production stability improvements

### Documentation
- New: `examples/metal-quickstart/README.md` - Metal GPU quick start guide
- New: `deployment/macos/README.md` - macOS deployment and setup
- New: `cmd/metal-agent/main.go` - Metal agent binary implementation
- New: `pkg/agent/` - Agent, executor, watcher, and registry implementations
- New: `internal/platform/detect.go` - Platform and GPU detection
- Updated: README with Metal support documentation

## [0.2.2] - 2025-11-23

### Added

#### Model Catalog (Phase 1)
- **Pre-configured Model Catalog**: 10 battle-tested LLM models with optimized settings
  - Small models (1-3B): Llama 3.2 3B, Phi-3 Mini
  - Medium models (7-8B): Llama 3.1 8B, Mistral 7B, Qwen 2.5 Coder 7B, DeepSeek Coder 6.7B, Gemma 2 9B
  - Large models (13B+): Qwen 2.5 14B, Mixtral 8x7B, Llama 3.1 70B
- **Catalog CLI Commands**:
  - `llmkube catalog list` - Browse all available models with specifications
  - `llmkube catalog info <model-id>` - View detailed model information
  - `llmkube catalog list --tag <tag>` - Filter models by tags (code, small, recommended, etc.)
- **One-Command Deployments**: Deploy catalog models without specifying source URLs
  - `llmkube deploy llama-3.1-8b --gpu` - No need to find GGUF URLs
  - Automatic application of optimized settings (quantization, resources, GPU layers)
  - Flag overrides still work for customization
- **Embedded Catalog**: YAML catalog embedded in CLI binary for offline usage

#### Developer Experience
- **Enhanced Deploy Command**: Made `--source` flag optional for catalog models
- **Smart Defaults**: Catalog models come with pre-configured CPU, memory, GPU layers, and quantization
- **Better Error Messages**: Helpful suggestions when model not found in catalog
- **Documentation Updates**: README showcases catalog feature prominently

### Changed
- **CLI Help Text**: Updated deploy command examples to highlight catalog usage
- **README**: Added catalog section to features and quick start

### Fixed
- Line length and linter compliance in catalog implementation
- E2E test binary path for catalog tests

### Documentation
- New: `pkg/cli/catalog.yaml` - Embedded model catalog with 10 models
- New: Comprehensive unit tests (13 test functions, 50+ test cases)
- New: E2E tests for catalog commands
- Updated: README with catalog usage examples
- Updated: Deploy command help text with catalog examples

## [0.2.0] - 2025-11-17

### Added

#### GPU Acceleration (Phase 0-1)
- **17x Performance Improvement**: GPU-accelerated inference on NVIDIA GPUs (L4, T4, A100, V100)
  - 64 tok/s generation on Llama 3.2 3B (vs 4.6 tok/s CPU)
  - 1,026 tok/s prompt processing (66x faster than CPU)
  - 0.6s total response time (17x faster than CPU's 10.3s)
- **Automatic GPU Scheduling**: GPU resource requests, tolerations, and node selectors configured automatically
- **GPU Layer Offloading**: Automatic detection and configuration of optimal GPU layer count
- **CLI GPU Support**: `--gpu` flag for one-command GPU deployments
- **Multi-GPU API**: Future-proof CRD design supporting up to 8 GPUs per model
- **GPU Configuration Flags**: `--gpu-count`, `--gpu-memory`, `--gpu-layers`, `--gpu-vendor`

#### Observability Stack (Phase 1)
- **Prometheus Integration**: Full kube-prometheus-stack deployment with ServiceMonitors
- **DCGM GPU Metrics**: 10+ GPU metrics (utilization, temperature, power, memory)
- **Grafana Dashboard**: Pre-built GPU monitoring dashboard (`config/grafana/llmkube-gpu-dashboard.json`)
  - 3 gauge panels: GPU utilization, temperature, power
  - 3 timeseries panels: Memory, utilization over time, power over time
  - Auto-refresh every 10 seconds
- **SLO Alert Rules**: Production-ready alerts for GPU health and service availability
  - GPUHighUtilization, GPUHighTemperature, GPUMemoryPressure, GPUPowerLimit
  - InferenceServiceDown, ControllerDown

#### Infrastructure & Testing
- **GKE GPU Cluster Terraform**: Complete GPU cluster setup with NVIDIA L4 GPUs
  - Spot instance support (~70% cost savings)
  - Auto-scale to 0 for cost optimization
  - NVIDIA GPU Operator installation
- **E2E Test Suite**: Comprehensive 8-test validation suite (`test/e2e/gpu_test.sh`)
  - GPU scheduling verification
  - Inference endpoint testing
  - GPU metrics validation
  - Alert rules validation
- **GPU Quickstart Example**: Complete working example (`examples/gpu-quickstart/`)
  - Model and InferenceService YAML files
  - Automated test script
  - Comprehensive documentation with troubleshooting

### Changed
- **Controller Image**: Updated to support GPU layer offloading automatically
- **CLI Deploy Command**: Enhanced with GPU-specific flags and auto-detection
- **Documentation**: Complete rewrite of README, launch materials, and performance benchmarks
- **Version**: Bumped from 0.1.0 to 0.2.0

### Fixed
- **GPU Layer Offloading**: Controller now correctly applies `--n-gpu-layers 99` for automatic offloading
- **CUDA Image Selection**: CLI automatically selects CUDA image when `--gpu` flag is set

### Performance
- **Llama 3.2 3B Q8_0 on NVIDIA L4**:
  - Generation: 64 tok/s (17x faster than CPU)
  - Prompt Processing: 1,026 tok/s (66x faster than CPU)
  - Total Response: 0.6s (17x faster than CPU)
  - GPU Layers: 29/29 (100% offloaded)
  - GPU Memory: 4.2GB / 24GB
  - Power: 35W
  - Temperature: 56-58°C

### Documentation
- New: `RELEASE_NOTES_v0.2.0.md` - Comprehensive v0.2.0 release notes
- New: `examples/gpu-quickstart/` - GPU deployment quickstart guide
- New: `config/grafana/llmkube-gpu-dashboard.json` - GPU monitoring dashboard
- New: `config/prometheus/llmkube-alerts.yaml` - SLO alert rules
- New: `test/e2e/gpu_test.sh` - E2E test suite
- Updated: `README.md` - GPU sections, performance benchmarks
- Updated: `ROADMAP.md` - Phase 0-1 completion status
- Updated: `LAUNCH_ANNOUNCEMENT.md` - GPU-focused launch messaging

### Known Limitations
- Single-GPU only (multi-GPU coming in Phase 2-3)
- NVIDIA GPUs only (AMD/Intel support planned for later sprints)
- GGUF format only (SafeTensors planned)
- Tested primarily on GKE/EKS (other K8s distributions should work)

## [0.1.0] - 2025-11-15

### Added
- **Kubernetes Operator**: Complete operator implementation with Kubebuilder
- **Model CRD**: Define LLM models with source URLs, quantization, and hardware requirements
- **InferenceService CRD**: Manage inference deployments with replicas and resources
- **Model Controller**: Automatic model download from HuggingFace and other HTTP sources
  - GGUF format support
  - Size calculation and validation
  - Path management and status tracking
- **InferenceService Controller**: Automatic deployment and service creation
  - Init containers for model downloading
  - Service creation (ClusterIP, NodePort, LoadBalancer)
  - OpenAI-compatible endpoint routing
- **CLI Tool**: Basic CRUD operations
  - `llmkube deploy` - Deploy models
  - `llmkube list` - List models and services
  - `llmkube status` - Check deployment status
  - `llmkube delete` - Remove deployments
  - `llmkube version` - Version information
- **Inference Runtime**: llama.cpp integration
  - Automatic model download via init containers
  - OpenAI-compatible API (`/v1/chat/completions`)
  - CPU inference support
  - Streaming and non-streaming responses

### Performance
- **TinyLlama 1.1B Q4_K_M on GKE CPU nodes**:
  - Model size: 637.8 MiB
  - Prompt processing: ~29 tokens/sec
  - Token generation: ~18.5 tokens/sec
  - Cold start (with download): ~5 seconds
  - Warm start: <1 second

### Documentation
- Initial `README.md` with installation and usage instructions
- `ROADMAP.md` with development plan
- API documentation for CRDs
- Architecture overview in README

---

**Release Links:**
- v0.2.0: Full release notes at [RELEASE_NOTES_v0.2.0.md](RELEASE_NOTES_v0.2.0.md)
- Repository: https://github.com/Defilan/LLMKube
- Issues: https://github.com/Defilan/LLMKube/issues
- Discussions: https://github.com/Defilan/LLMKube/discussions
