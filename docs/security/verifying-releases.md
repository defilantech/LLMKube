# Verifying LLMKube releases

Every released image is signed with cosign (keyless, Sigstore) and carries an
SLSA build provenance attestation and an SBOM attestation, all bound to the
`defilantech/LLMKube` release workflow identity.

## Verify the image signature

    cosign verify ghcr.io/defilantech/llmkube-controller:<version> \
      --certificate-identity-regexp '^https://github.com/defilantech/LLMKube/.github/workflows/release-please.yml@.*$' \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com

## Verify SLSA provenance and SBOM

The provenance and SBOM attestations are produced by `actions/attest-build-provenance`
and `actions/attest-sbom`, which write to the GitHub attestations store rather than
the cosign-native attestation format, so the GitHub CLI is the tool to verify them
with, not `cosign verify-attestation`.

    gh attestation verify oci://ghcr.io/defilantech/llmkube-controller:<version> \
      --owner defilantech

    gh attestation verify oci://ghcr.io/defilantech/llmkube-controller:<version> \
      --owner defilantech --predicate-type https://spdx.dev/Document/v2.3

The first command checks the SLSA build provenance attestation (the default
predicate type). The second checks the SBOM attestation, whose predicate type
is derived from the SPDX document version syft emits (`SPDX-2.3` as of this
writing); if a future release bumps that version, update the version suffix
above to match.

The same applies to llmkube-foreman-operator, llmkube-foreman-agent, and
llmkube-router-proxy. Release binary archives also carry provenance; verify with
`gh attestation verify <archive> --owner defilantech`.
