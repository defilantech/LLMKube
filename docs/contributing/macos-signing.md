# Signing and notarizing the macOS binaries

The release signs every darwin binary it builds, and notarizes them when the
App Store Connect key is configured (#2084). This page is for maintainers: what
the pipeline does, the five secrets it reads from the `release` environment,
and how to create them.

## What the release does

GoReleaser runs `hack/macos-sign.sh` as a post-build hook for each darwin build,
on the Linux release runner, before anything is archived:

| Build                  | Binary                | Identifier                  | Info.plist template                              |
|------------------------|-----------------------|-----------------------------|--------------------------------------------------|
| `llmkube-cli`          | `llmkube`             | `dev.llmkube.cli`           | none (a Terminal tool needs no bundle identity)  |
| `llmkube-metal-agent`  | `llmkube-metal-agent` | `dev.llmkube.metal-agent`   | `deployment/macos/metal-agent.Info.plist.tmpl`   |

1. The template is rendered with the release version and embedded as a
   `__TEXT,__info_plist` section by `cmd/machoplist`. Apple's linker would do
   this with `-sectcreate`, which Go only reaches through cgo and a darwin
   linker; the release builds with `CGO_ENABLED=0` on Linux, so the tool edits
   the finished Mach-O instead (`internal/machoplist`).
2. [rcodesign](https://github.com/indygreg/apple-platform-rs) signs the
   binary with the identifier above and binds the embedded plist. The release
   job installs rcodesign 0.29.0 with `hack/install-rcodesign.sh`, pinned by
   version and SHA-256: a download that does not match fails the release, a
   download that fails only warns (and the hook then fails if signing is
   configured).

With the notary key set, the `notarize-macos` job runs after the release is
published: `hack/macos-notarize.sh` reads GoReleaser's `dist/artifacts.json`,
zips every darwin binary (four today), submits them to Apple's notary service in
one submission and waits up to an hour for the verdict. Notarization does not
change the binaries; Apple records a ticket for their signatures, so the
published assets become notarized when it is accepted. Running it after the
release keeps a slow notary queue from failing GoReleaser after release-please
has tagged the release. Until the job succeeds, a downloaded copy fails
Gatekeeper's check. If it fails or times out, use "Re-run failed jobs" on the
workflow run: only `notarize-macos` runs again, and it resubmits the same
binaries. A notary misconfiguration still fails before anything is published:
the `goreleaser` job checks the settings (`hack/macos-notarize.sh --preflight`)
before GoReleaser runs.

GoReleaser's own `notarize.macos` step is deliberately not used: it signs with
the file name as the identifier and does not bind the plist.

What the secrets select:

| Secrets set                          | Result                                                              |
|--------------------------------------|---------------------------------------------------------------------|
| none                                 | ad-hoc signature (identifier and plist bound), not notarized        |
| `MACOS_SIGN_P12`, `MACOS_SIGN_PASSWORD` | Developer ID signature, hardened runtime, secure timestamp       |
| all five                             | Developer ID signature, notarized                                   |
| any other combination                | the release fails with the missing variable named                   |
| anything short of all five, with `MACOS_SIGNING_REQUIRED=true` | the release fails before anything is published |

The ad-hoc fallback keeps forks and secret-less runs working: embedding the
plist breaks the Go linker's own ad-hoc signature, and macOS kills an arm64
binary whose signature does not verify, so the binary is re-signed either way.

## The secrets

Store them as secrets of the `release` environment on `defilantech/LLMKube`
(Settings, Environments, release), never as repository secrets. A repository
secret is readable by a workflow run on any branch, so anyone who can push a
branch, or a leaked token that can, could have a workflow read the signing
certificate. The environment allows deployments from `main` only, so a run
from any other branch gets empty values. The `goreleaser` and `notarize-macos`
jobs name it with `environment: release`; a job without that key reads
environment secrets as empty. The `.p12` and its password reach only the
GoReleaser step; the three notary values reach only the settings check before
it and the `notarize-macos` job.

Create the environment once, limited to `main`:

```bash
gh api -X PUT repos/defilantech/LLMKube/environments/release \
  -F 'deployment_branch_policy[protected_branches]=false' \
  -F 'deployment_branch_policy[custom_branch_policies]=true'
gh api -X POST repos/defilantech/LLMKube/environments/release/deployment-branch-policies \
  -f name=main -f type=branch
```

Because the `goreleaser` job uses the environment, a manual `workflow_dispatch`
dry run has to be started from `main`; GitHub refuses the job on any other
branch. A dry run signs nothing either way.

Once all five are stored (after the local dry run below), set the repository
variable `MACOS_SIGNING_REQUIRED` to `true` (Settings, Secrets and
variables, Actions, Variables, or
`gh variable set MACOS_SIGNING_REQUIRED --repo defilantech/LLMKube --body true`).
From then on a release fails, before any binary is signed, if any of the five
is empty, instead of shipping ad-hoc or un-notarized binaries. Forks and runs
without the variable keep the ad-hoc fallback.

| Secret                   | Value                                                         |
|--------------------------|---------------------------------------------------------------|
| `MACOS_SIGN_P12`         | base64 of the Developer ID Application certificate and key (`.p12`) |
| `MACOS_SIGN_PASSWORD`    | the `.p12` export password (required; it cannot be empty)     |
| `MACOS_NOTARY_KEY`       | base64 of the App Store Connect API key (`AuthKey_<KEYID>.p8`) |
| `MACOS_NOTARY_KEY_ID`    | the key's ID                                                  |
| `MACOS_NOTARY_ISSUER_ID` | the issuer ID shown above the keys list                       |

### Developer ID Application certificate

1. Create a Developer ID Application certificate in Certificates,
   Identifiers & Profiles (Apple reserves this for the account holder unless
   the account grants admins access). Choose **G2 Sub-CA**, not "Previous
   Sub-CA": a certificate under the previous intermediate expires with it on
   2027-02-01 22:12:15 UTC, however recently it was issued. Use a separate
   certificate for CI, so revoking it does not affect local signing. Check the
   expiry after installing it
   (`security find-certificate -c "Developer ID Application" -p | openssl x509 -noout -issuer -enddate`);
   the G2 certificate issued 2026-10-29 expires 2027-11-28.
2. In Keychain Access, find "Developer ID Application: <team>" under My
   Certificates, expand it to confirm the private key is there, and export
   both as a `.p12` with a strong password. Keychain Access writes it with
   `pbeWithSHA1And40BitRC2-CBC`, which rcodesign reads; OpenSSL 3 opens it only
   with `-legacy`.
3. Keep `developer-id.p12` until the dry run below passes.

### App Store Connect API key

1. In App Store Connect, Users and Access, Integrations, App Store Connect API,
   create a Team Key with the Developer role (enough for notarization).
2. Download `AuthKey_<KEYID>.p8`; Apple offers the download only once. Note the
   key ID and the issuer ID.

### Dry run on a Mac before storing anything

GoReleaser runs only after release-please has tagged a release, so a bad secret
found there leaves a tagged release without assets. Run the release's own hook
on a Mac first. It signs a freshly built metal-agent with the release's hook,
notarizes it with the release's notarize step (a real submission to Apple, a
few minutes) and checks it, with the values held only by a subshell. You need rcodesign 0.29.0: the
`apple-codesign-0.29.0-aarch64-apple-darwin.tar.gz` release asset (SHA-256
`d1a532150adaf90048260d76359261aa716abafc45c53c5dc18845029184334a`) or
`brew install rcodesign`.

```bash
cd <your LLMKube checkout>
mkdir -p /tmp/llmkube-sign-test
bin=/tmp/llmkube-sign-test/llmkube-metal-agent
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags '-s -w' \
  -o "$bin" ./cmd/metal-agent
(
  export MACOS_SIGNING_REQUIRED=true
  export MACOS_SIGN_P12="$(base64 -i developer-id.p12)"
  printf '.p12 password: '; read -rs MACOS_SIGN_PASSWORD; echo; export MACOS_SIGN_PASSWORD
  export MACOS_NOTARY_KEY="$(base64 -i AuthKey_<KEYID>.p8)"
  export MACOS_NOTARY_KEY_ID=<KEYID>
  export MACOS_NOTARY_ISSUER_ID=<issuer-id>
  export MACOS_SIGN_P12_SET=true
  ./hack/macos-sign.sh darwin "$bin" 0.0.0 dev.llmkube.metal-agent \
    deployment/macos/metal-agent.Info.plist.tmpl
  printf '[{"type":"Binary","goos":"darwin","path":"%s"}]' "$bin" >/tmp/llmkube-sign-test/artifacts.json
  ./hack/macos-notarize.sh /tmp/llmkube-sign-test/artifacts.json
)
codesign -dv --verbose=4 "$bin"
spctl --assess --type install -vv "$bin"
"$bin" --version
```

The hook should print `macos-sign: signed ... with the Developer ID certificate`
and the notarize step should end with `macos-notarize: notarized 1 darwin
binary`. `codesign` should show
`Identifier=dev.llmkube.metal-agent`, `Authority=Developer ID Application: ...`,
your `TeamIdentifier`, a `Timestamp`, the `runtime` flag and
`Info.plist entries=9`. `spctl` should print `accepted` and
`source=Notarized Developer ID`. Use `--type install`: `--type execute` assesses
app bundles and rejects any bare executable, notarized or not, with "does not
seem to be an app".

### rcodesign rejects the .p12

rcodesign 0.29.0 cannot read a `.p12` encrypted with PBES2/AES-256, the default
of OpenSSL 3, and reports it as
`Error: incorrect password given when decrypting PFX data` even when the
password is right; the hook then prints a hint pointing here. Check the format
with `openssl pkcs12 -in developer-id.p12 -info -noout` (Homebrew `openssl@3`;
the system `openssl` is LibreSSL): `PBES2, PBKDF2, AES-256-CBC` is the
unreadable kind. Re-encode it in the legacy format. The certificates, which are
public, go through a file; the decrypted private key only goes through a pipe:

```bash
openssl=$(brew --prefix openssl@3)/bin/openssl
(
  printf 'current .p12 password: '; read -rs P12_IN; echo; export P12_IN
  printf 'new .p12 password: '; read -rs P12_OUT; echo; export P12_OUT
  "$openssl" pkcs12 -in developer-id.p12 -nokeys -passin env:P12_IN -out developer-id-certs.pem
  "$openssl" pkcs12 -in developer-id.p12 -nocerts -nodes -passin env:P12_IN \
    | "$openssl" pkcs12 -export -legacy -inkey /dev/stdin -in developer-id-certs.pem \
        -out developer-id-legacy.p12 -passout env:P12_OUT
)
rm developer-id-certs.pem
```

`-inkey /dev/stdin` with the certificates from a file matters: given only the
pipe, `openssl pkcs12 -export` reads its input twice and fails with "Could not
find certificates". Run the dry run again with `developer-id-legacy.p12` and
the new password.

### Store the secrets

When the dry run passes, store the values:

```bash
base64 -i developer-id.p12 | tr -d '\n' | gh secret set MACOS_SIGN_P12 --env release --repo defilantech/LLMKube
gh secret set MACOS_SIGN_PASSWORD --env release --repo defilantech/LLMKube   # prompts for the value
base64 -i AuthKey_<KEYID>.p8 | tr -d '\n' | gh secret set MACOS_NOTARY_KEY --env release --repo defilantech/LLMKube
gh secret set MACOS_NOTARY_KEY_ID --env release --repo defilantech/LLMKube --body <KEYID>
gh secret set MACOS_NOTARY_ISSUER_ID --env release --repo defilantech/LLMKube --body <issuer-id>
gh secret list --env release --repo defilantech/LLMKube   # all five
```

Use the `.p12` file that passed the dry run (the `-legacy` one if you
re-encoded it). GitHub secrets cannot be read back, so keep the `.p12`, its
password and the `.p8` in a password manager before deleting the local files;
Apple offers the `.p8` download only once.

Set `MACOS_SIGNING_REQUIRED` only after the change that gives both jobs
`environment: release` is on `main`; before it, the jobs read the environment
secrets as empty and the release would fail.

The hook and the notarize step decode the secrets into a private temporary
directory, pass them to rcodesign as files, never print them, and remove the
directory when they exit. A process killed with SIGKILL (a job cancelled past
its grace period) cannot run that cleanup; the release jobs run on ephemeral
GitHub-hosted runners, which are discarded afterwards.

## Stapling and Gatekeeper

The release ships bare executables, and a notarization ticket can only be
stapled to an app bundle, disk image or installer package. Gatekeeper therefore
looks the ticket up online the first time a quarantined (downloaded) copy runs.
A notarized binary checks as:

```bash
spctl --assess --type install -vv llmkube-metal-agent   # accepted, source=Notarized Developer ID
codesign -dv --verbose=4 llmkube-metal-agent            # Identifier=dev.llmkube.metal-agent, Info.plist entries=9
```

## Local builds

- `make build-metal-agent` and `make install-metal-agent` do not sign or embed
  anything; the binary keeps the Go linker's ad-hoc signature.
- A GoReleaser snapshot runs the same hook:
  `go run github.com/goreleaser/goreleaser/v2@v2.19.0 release --snapshot --clean --skip=publish,docker,sbom`.
  With `rcodesign` on `PATH` and no secrets, the darwin binaries get the plist
  and an ad-hoc signature; without it the hook prints a warning and leaves them
  as built.
- `make test-scripts` runs `scripts/macos-sign_test.sh`, which checks the hook's
  secret handling with a stubbed rcodesign and, on macOS with rcodesign
  installed, signs a real binary; `scripts/macos-notarize_test.sh`, which
  checks the single submission and the notary settings; and
  `scripts/install-rcodesign_test.sh`, which checks that a checksum mismatch
  fails and a failed download only warns.

## Changing things

- The identifiers are the binaries' identity to macOS privacy settings; keep
  them stable. They must match in `.goreleaser.yaml`, the Info.plist templates
  and the `AssociatedBundleIdentifiers` of the launchd plists in
  `deployment/macos/`; `internal/machoplist/release_identity_test.go` checks
  that they do.
- The plist has to fit in the header padding the Go linker leaves, about 1680
  bytes on darwin/amd64 today. The same test caps each rendered template at
  1400 bytes and keeps the templates out of the release archives, which list
  `deployment/macos/` by pattern: a new file there needs a matching pattern.
- To update rcodesign, change the version and the SHA-256 of the
  `x86_64-unknown-linux-musl` tarball in `hack/install-rcodesign.sh`.
- Renew the Developer ID certificate before its expiry (see the certificate
  steps above for how to read it), then run the dry run with the new `.p12`
  and store it the same way. Signatures made before it expires stay valid:
  the hook adds a secure timestamp.
