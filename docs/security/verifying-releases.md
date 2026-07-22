# Verifying LLMKube releases

Every released image is signed with cosign (keyless, Sigstore) and carries an
SLSA build provenance attestation and an SBOM attestation, all bound to the
`defilantech/llmkube` release workflow identity.

## Verify the image signature

    cosign verify ghcr.io/defilantech/llmkube-controller:<version> \
      --certificate-identity-regexp '^https://github.com/defilantech/llmkube/.github/workflows/release-please.yml@.*$' \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com

## Verify SLSA provenance and SBOM

    cosign verify-attestation ghcr.io/defilantech/llmkube-controller:<version> \
      --type slsaprovenance \
      --certificate-identity-regexp '^https://github.com/defilantech/llmkube/.github/workflows/release-please.yml@.*$' \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com

    gh attestation verify oci://ghcr.io/defilantech/llmkube-controller:<version> \
      --owner defilantech

The same applies to llmkube-foreman-operator, llmkube-foreman-agent, and
llmkube-router-proxy. Release binary archives also carry provenance; verify with
`gh attestation verify <archive> --owner defilantech`.
