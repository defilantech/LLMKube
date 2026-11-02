#!/usr/bin/env bash
# Comment on every issue a release fixed, and on every outside contributor's PR
# it shipped, so the people who reported or wrote the change hear that it is
# out without watching the changelog.
#
# What counts as shipped: the squash-merged PRs between the previous v* tag and
# this one, found by the "(#N)" suffix of each commit subject. An issue counts
# as fixed when one of those PRs closed it with a closing keyword ("Fixes #N"),
# as GitHub records it in closingIssuesReferences. Mentions and "refs #N" do
# not count, and neither do issues that were reopened after the fix.
#
# Each comment carries a hidden marker for the tag, and an issue or PR that
# already has it is skipped, so a rerun posts nothing twice.
#
# Usage: notify-released-issues.sh <tag> [--dry-run]
#   <tag> is an operator release tag such as v0.10.16. Run from a checkout with
#   full history and tags. Needs GH_TOKEN with issues and pull-requests write
#   (read is enough for --dry-run). GITHUB_REPOSITORY defaults to
#   defilantech/LLMKube.
set -euo pipefail

TAG="${1:?usage: notify-released-issues.sh <tag> [--dry-run]}"
DRY_RUN=false
[ "${2:-}" = "--dry-run" ] && DRY_RUN=true
REPO="${GITHUB_REPOSITORY:-defilantech/LLMKube}"
OWNER="${REPO%%/*}"
NAME="${REPO##*/}"

# Only operator releases: the chart and Foreman tags (llmkube-*, foreman-*)
# point at the same commit and would repeat every comment.
[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "not an operator release tag: $TAG" >&2; exit 2; }
git rev-parse -q --verify "refs/tags/$TAG" >/dev/null || { echo "tag $TAG not found; fetch tags first" >&2; exit 2; }
PREV=$(git describe --tags --abbrev=0 --match 'v[0-9]*' "${TAG}^")
URL="https://github.com/${REPO}/releases/tag/${TAG}"
MARKER="<!-- llmkube-release-notify ${TAG} -->"

# The release PR is left out: its changelog repeats "fixes #N" from the commits,
# so GitHub records it as closing those issues too.
mapfile -t PRS < <(git log --format=%s "${PREV}..${TAG}" | grep -v '^chore: release ' |
  sed -nE 's/.*\(#([0-9]+)\)$/\1/p' | sort -un)
echo "${TAG}: ${#PRS[@]} PRs since ${PREV}"

# commented <number> succeeds when the issue or PR already has this tag's marker.
# grep -c reads to the end: grep -q would exit at the first match and, under
# pipefail, the SIGPIPE it leaves gh would read as "not commented" and post twice.
commented() {
  gh api "repos/${REPO}/issues/$1/comments" --paginate --jq '.[].body' | grep -cF "$MARKER" >/dev/null
}

# post <number> <body> comments once, or prints the comment under --dry-run.
post() {
  local n="$1" body="$2"
  if commented "$n"; then
    echo "#${n}: already notified for ${TAG}"
    return
  fi
  if $DRY_RUN; then
    printf '#%s would get:\n%s\n\n' "$n" "$body"
    return
  fi
  gh api "repos/${REPO}/issues/${n}/comments" -f body="$body" --jq '"#'"$n"': " + .html_url'
}

# links "1 2 3" prints "#1, #2 and #3".
links() {
  local -a a
  read -ra a <<<"$1"
  local out="#${a[0]}" i
  for ((i = 1; i < ${#a[@]}; i++)); do
    if ((i == ${#a[@]} - 1)); then out+=" and #${a[i]}"; else out+=", #${a[i]}"; fi
  done
  echo "$out"
}

declare -A FIXED_BY=()
# shellcheck disable=SC2016 # GraphQL variables, not shell expansions.
QUERY='query($owner: String!, $name: String!, $pr: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $pr) {
      merged
      authorAssociation
      author { __typename }
      closingIssuesReferences(first: 25) {
        nodes { number state repository { nameWithOwner } }
      }
    }
  }
}'

for pr in "${PRS[@]}"; do
  if ! info=$(gh api graphql -F owner="$OWNER" -F name="$NAME" -F pr="$pr" -f query="$QUERY" \
    --jq '.data.repository.pullRequest // empty' 2>&1); then
    echo "::warning::#${pr}: lookup failed, skipped: ${info}"
    continue
  fi
  [ -n "$info" ] || { echo "#${pr}: not a pull request, skipped"; continue; }
  [ "$(jq -r .merged <<<"$info")" = true ] || continue

  while read -r issue; do
    [ -n "$issue" ] && FIXED_BY[$issue]="${FIXED_BY[$issue]:-} ${pr}"
  done < <(jq -r --arg repo "$REPO" \
    '.closingIssuesReferences.nodes[] | select(.repository.nameWithOwner == $repo and .state == "CLOSED") | .number' <<<"$info")

  # Maintainers and bots already know; thank everyone else on their PR.
  case "$(jq -r '.authorAssociation + "/" + (.author.__typename // "Ghost")' <<<"$info")" in
    OWNER/* | MEMBER/* | COLLABORATOR/* | */Bot | */Ghost) ;;
    *) post "$pr" "This shipped in [LLMKube ${TAG}](${URL}). Thank you for the contribution!

${MARKER}" ;;
  esac
done

for issue in $(printf '%s\n' "${!FIXED_BY[@]}" | sort -n); do
  post "$issue" "The fix for this shipped in [LLMKube ${TAG}](${URL}) with $(links "${FIXED_BY[$issue]# }"). The release notes have the full list of changes and the upgrade steps.

${MARKER}"
done
