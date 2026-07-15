#!/usr/bin/env bash
#
# foreman-finalize.sh — turn a GO'd Foreman agentic-coding branch into a clean,
# single-commit pull request against the base repo.
#
# It rebases the work onto current base (by re-applying the branch's source
# changes and REGENERATING derived artifacts, never copying the branch's
# possibly-stale generated files), drift-checks the regen, squashes to one
# commit with an issue-derived conventional-commit subject, and opens the PR.
#
# Motivation: a Foreman `branchStrategy: reset` revise commits the whole tree as
# one commit whose message reflects only the last fix, and its base can lag the
# real base so its generated files (zz_generated deepcopy, CRDs) are stale.
# Finalizing by hand is error-prone; this codifies it.
#
# Usage:
#   scripts/foreman-finalize.sh --branch <fork-branch> [options]
#
# Options:
#   --branch <ref>       GO'd branch on the fork (required),
#                        e.g. foreman/build-1098-s3-source/issue-1098
#   --issue <N>          Issue number (default: parsed from trailing issue-N)
#   --base <remote/br>   Base to rebase onto and PR into (default: upstream/main)
#   --fork <remote>      Remote holding the Foreman branch (default: origin)
#   --repo <owner/name>  Base repo slug for gh (default: derived from base remote)
#   --fork-owner <owner> Fork owner for the PR head (default: derived from fork remote)
#   --full-test          Also run `make test` (envtest) locally
#   --message-file <f>   Use this file as the commit message (overrides derivation)
#   --pr-body-file <f>   Use this file as the PR body (overrides the template)
#   --dry-run            Do everything locally through the squash; print the push
#                        and `gh pr create` commands instead of executing them
#   -h, --help           Show this help
#
set -euo pipefail

if ((BASH_VERSINFO[0] < 4)); then
	echo "ERROR: bash 4+ required (mapfile); on macOS: brew install bash" >&2
	exit 1
fi

# ---------------------------------------------------------------------------
# defaults + arg parsing
# ---------------------------------------------------------------------------
BASE="upstream/main"
FORK="origin"
BRANCH=""
ISSUE=""
REPO=""
FORK_OWNER=""
FULL_TEST=0
DRY_RUN=0
MESSAGE_FILE=""
PR_BODY_FILE=""

SRC_REF="refs/finalize/src" # temp local ref for the fetched fork branch
FINAL_BRANCH=""
ORIG_REF=""

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; }

die() {
	echo "ERROR: $*" >&2
	exit 1
}
info() { echo ">> $*"; }

while [[ $# -gt 0 ]]; do
	case "$1" in
	--branch)
		BRANCH="${2:-}"
		shift 2
		;;
	--issue)
		ISSUE="${2:-}"
		shift 2
		;;
	--base)
		BASE="${2:-}"
		shift 2
		;;
	--fork)
		FORK="${2:-}"
		shift 2
		;;
	--repo)
		REPO="${2:-}"
		shift 2
		;;
	--fork-owner)
		FORK_OWNER="${2:-}"
		shift 2
		;;
	--message-file)
		MESSAGE_FILE="${2:-}"
		shift 2
		;;
	--pr-body-file)
		PR_BODY_FILE="${2:-}"
		shift 2
		;;
	--full-test)
		FULL_TEST=1
		shift
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*) die "unknown argument: $1 (see --help)" ;;
	esac
done

[[ -n "$BRANCH" ]] || die "--branch is required (see --help)"

BASE_REMOTE="${BASE%%/*}"
BASE_BRANCH="${BASE#*/}"
[[ -n "$BASE_REMOTE" && -n "$BASE_BRANCH" && "$BASE_REMOTE" != "$BASE" ]] ||
	die "--base must be <remote>/<branch>, got: $BASE"

# derive issue from the trailing issue-<N> segment if not supplied
if [[ -z "$ISSUE" ]]; then
	ISSUE="${BRANCH##*issue-}"
fi
[[ "$ISSUE" =~ ^[0-9]+$ ]] || die "could not determine a numeric issue (got '$ISSUE'); pass --issue"

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

# is_generated: paths whose contents come from `make manifests/generate/chart-crds`.
# These are regenerated on base rather than copied from the branch.
is_generated() {
	case "$1" in
	*zz_generated*.go) return 0 ;;
	config/crd/bases/*) return 0 ;;
	charts/*/templates/crds/*) return 0 ;;
	config/rbac/role.yaml) return 0 ;;
	*) return 1 ;;
	esac
}

# remote_slug: extract owner/name from a git remote URL (https or ssh form).
remote_slug() {
	local url
	url="$(git remote get-url "$1")"
	url="${url%.git}"
	url="${url#git@*:}"      # git@github.com:owner/name -> owner/name
	url="${url#https://*/}"  # https://github.com/owner/name -> owner/name
	echo "$url"
}

cleanup() {
	local rc=$?
	git update-ref -d "$SRC_REF" 2>/dev/null || true
	if [[ -n "$ORIG_REF" ]]; then
		local now
		now="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo '')"
		if [[ "$now" != "$ORIG_REF" ]]; then
			git checkout --quiet "$ORIG_REF" 2>/dev/null || true
		fi
	fi
	return $rc
}
trap cleanup EXIT

# regen runs the repo's codegen targets. foreman-chart-crds regenerates the
# Foreman chart CRDs via a SEPARATE target that only exists in repos shipping
# Foreman; guard on its presence so this stays portable and so a Foreman-CRD
# change does not silently drift (the chart copy is not covered by chart-crds).
regen() {
	make manifests >/dev/null
	make generate >/dev/null
	make chart-crds >/dev/null
	if make -n foreman-chart-crds >/dev/null 2>&1; then
		make foreman-chart-crds >/dev/null
	fi
}

# ---------------------------------------------------------------------------
# 1. preflight
# ---------------------------------------------------------------------------
cd "$(git rev-parse --show-toplevel)"
ORIG_REF="$(git rev-parse --abbrev-ref HEAD)"

command -v gh >/dev/null || die "gh (GitHub CLI) is required"
gh auth status >/dev/null 2>&1 || die "gh is not authenticated (run: gh auth login)"

# A fully clean tree is required: the assemble step below stages with `git add -A`,
# so a stray untracked file would otherwise be swept into the finalize commit.
# (Gitignored build artifacts are not reported by --porcelain and are fine.)
[[ -z "$(git status --porcelain)" ]] ||
	die "working tree is not clean (uncommitted or untracked files present); commit, stash, or remove them first — untracked files would otherwise be swept into the finalize commit"

info "fetching base ($BASE) and fork branch ($FORK/$BRANCH)"
git fetch --quiet "$BASE_REMOTE" "$BASE_BRANCH"
git fetch --quiet "$FORK" "refs/heads/$BRANCH:$SRC_REF" ||
	die "branch '$BRANCH' not found on remote '$FORK'"

[[ -n "$REPO" ]] || REPO="$(remote_slug "$BASE_REMOTE")"
[[ -n "$FORK_OWNER" ]] || FORK_OWNER="$(remote_slug "$FORK")"
FORK_OWNER="${FORK_OWNER%%/*}"

# ---------------------------------------------------------------------------
# 2. issue metadata -> conventional-commit subject
# ---------------------------------------------------------------------------
info "reading issue #$ISSUE from $REPO"
issue_json="$(gh issue view "$ISSUE" --repo "$REPO" --json title,labels,state 2>/dev/null)" ||
	die "could not read issue #$ISSUE from $REPO"

issue_title="$(jq -r '.title' <<<"$issue_json")"
issue_state="$(jq -r '.state' <<<"$issue_json")"
labels="$(jq -r '.labels[].name' <<<"$issue_json")"

[[ "$issue_state" == "OPEN" ]] || info "warning: issue #$ISSUE is $issue_state"

# type: title prefix wins, then labels, else default feat (with a warning)
ctype=""
case "$issue_title" in
'[FEATURE]'*) ctype="feat" ;;
'[BUG]'*) ctype="fix" ;;
esac
if [[ -z "$ctype" ]]; then
	if grep -qx 'kind/feature' <<<"$labels"; then
		ctype="feat"
	elif grep -qx 'bug' <<<"$labels"; then
		ctype="fix"
	else
		ctype="feat"
		info "warning: could not infer commit type from issue; defaulting to '$ctype'"
	fi
fi

# scope from a component/* label, if any
cscope=""
while IFS= read -r l; do
	case "$l" in
	component/*)
		cscope="${l#component/}"
		break
		;;
	esac
done <<<"$labels"

# clean title: strip the [FEATURE]/[BUG] prefix and surrounding space
clean_title="$issue_title"
clean_title="${clean_title#'[FEATURE]'}"
clean_title="${clean_title#'[BUG]'}"
clean_title="${clean_title#"${clean_title%%[![:space:]]*}"}" # ltrim

if [[ -n "$cscope" ]]; then
	SUBJECT="${ctype}(${cscope}): ${clean_title} (#${ISSUE})"
else
	SUBJECT="${ctype}: ${clean_title} (#${ISSUE})"
fi
info "commit subject: $SUBJECT"

# ---------------------------------------------------------------------------
# 3. classify changed files + overlap guard
# ---------------------------------------------------------------------------
MB="$(git merge-base "$BASE_REMOTE/$BASE_BRANCH" "$SRC_REF")"
mapfile -t CHANGED < <(git diff --name-only "$MB" "$SRC_REF")
[[ ${#CHANGED[@]} -gt 0 ]] || die "branch has no changes vs its merge-base with $BASE"

SOURCE=()
overlap=()
for f in "${CHANGED[@]}"; do
	is_generated "$f" && continue
	SOURCE+=("$f")
	# if base also moved this file since the merge-base, a blind copy would clobber it
	if [[ -n "$(git diff --name-only "$MB" "$BASE_REMOTE/$BASE_BRANCH" -- "$f")" ]]; then
		overlap+=("$f")
	fi
done

[[ ${#SOURCE[@]} -gt 0 ]] || die "branch changed only generated files; nothing to finalize"

if [[ ${#overlap[@]} -gt 0 ]]; then
	printf 'ERROR: these source files changed on both %s and the branch since the merge-base;\n' "$BASE" >&2
	printf '       a manual rebase is needed (this script will not silently clobber):\n' >&2
	printf '         %s\n' "${overlap[@]}" >&2
	exit 1
fi

# ---------------------------------------------------------------------------
# 4. assemble on a fresh finalize branch off base
# ---------------------------------------------------------------------------
slug="$(tr '[:upper:]' '[:lower:]' <<<"$clean_title" | tr -c 'a-z0-9' '-' | sed -E 's/-+/-/g; s/^-|-$//g' | cut -c1-40)"
slug="${slug%-}"
FINAL_BRANCH="finalize/${ISSUE}-${slug}"

info "assembling $FINAL_BRANCH off $BASE_REMOTE/$BASE_BRANCH (${#SOURCE[@]} source file(s))"
git checkout --quiet -B "$FINAL_BRANCH" "$BASE_REMOTE/$BASE_BRANCH"

for f in "${SOURCE[@]}"; do
	if git cat-file -e "$SRC_REF:$f" 2>/dev/null; then
		git checkout --quiet "$SRC_REF" -- "$f"
	else
		# file was deleted on the branch
		git rm --quiet -f --ignore-unmatch "$f" >/dev/null
	fi
done

info "regenerating derived artifacts"
regen

# ---------------------------------------------------------------------------
# 5. verify (hard stops)
# ---------------------------------------------------------------------------
git add -A

info "checking codegen idempotency"
regen
git diff --quiet || die "codegen is not idempotent (a second regen produced changes); investigate before finalizing"

info "go build ./..."
go build ./...
info "go vet ./..."
go vet ./...
info "make validate-samples"
make validate-samples >/dev/null
if [[ "$FULL_TEST" == "1" ]]; then
	info "make test (envtest)"
	make test
fi

# ---------------------------------------------------------------------------
# 6. commit (single, DCO-signed)
# ---------------------------------------------------------------------------
msg_file="$(mktemp)"
if [[ -n "$MESSAGE_FILE" ]]; then
	cat "$MESSAGE_FILE" >"$msg_file"
else
	{
		echo "$SUBJECT"
		echo
		# carry the branch commit body, dropping any Fixes/Signed-off-by lines
		# (git's default message cleanup trims the leftover blank lines)
		git log -1 --format=%b "$SRC_REF" |
			grep -viE '^(Fixes|Closes|Signed-off-by):' || true
		echo
		echo "Fixes #${ISSUE}"
	} >"$msg_file"
fi

git commit --quiet -s -F "$msg_file"
rm -f "$msg_file"
git log -1 --format=%b | grep -q '^Signed-off-by:' || die "commit is missing a Signed-off-by trailer"

# PR title tracks the ACTUAL commit subject, so --message-file drives both the
# commit and the PR title (a partial slice can say "engine" / use Refs, not the
# derived full-issue subject).
PR_TITLE="$(git log -1 --format=%s HEAD)"

# ---------------------------------------------------------------------------
# 7. PR body
# ---------------------------------------------------------------------------
pr_body_file="$(mktemp)"
if [[ -n "$PR_BODY_FILE" ]]; then
	cat "$PR_BODY_FILE" >"$pr_body_file"
else
	{
		echo "## What"
		echo
		echo "$clean_title (#${ISSUE})."
		echo
		echo "## How"
		echo
		git show -s --format=%b HEAD | grep -viE '^(Fixes|Closes|Signed-off-by):' || true
		echo
		echo "## Testing"
		echo
		echo "- \`go build\` / \`go vet\` clean; \`make validate-samples\` passes."
		echo "- CRD/codegen regeneration (manifests, generate, chart-crds, foreman-chart-crds) produces no drift."
		if [[ "$FULL_TEST" == "1" ]]; then
			echo "- \`make test\` (envtest) passes locally."
		else
			echo "- Full envtest suite passed in the Foreman gate (GO)."
		fi
		echo
		echo "## AI assistance"
		echo
		echo "Implemented via the Foreman agentic-coding harness, then rebased onto" \
			"current \`${BASE_BRANCH}\` and verified by the author. Band-3 disclosure" \
			"per the project's AI-assisted contribution policy."
		echo
		echo "Fixes #${ISSUE}"
	} >"$pr_body_file"
fi

# ---------------------------------------------------------------------------
# 8. push + open PR (or dry-run)
# ---------------------------------------------------------------------------
if [[ "$DRY_RUN" == "1" ]]; then
	echo
	info "DRY RUN — nothing pushed. Planned actions:"
	echo "  git push -u $FORK $FINAL_BRANCH"
	echo "  gh pr create --repo $REPO --base $BASE_BRANCH --head ${FORK_OWNER}:${FINAL_BRANCH} \\"
	echo "    --title \"$PR_TITLE\" --body-file <generated>"
	echo
	echo "----- commit message -----"
	git log -1 --format='%B' HEAD
	echo "----- PR body -----"
	cat "$pr_body_file"
	rm -f "$pr_body_file"
	exit 0
fi

info "pushing $FINAL_BRANCH to $FORK"
git push --quiet -u "$FORK" "$FINAL_BRANCH"

info "opening PR into $REPO ($BASE_BRANCH)"
pr_url="$(gh pr create --repo "$REPO" --base "$BASE_BRANCH" \
	--head "${FORK_OWNER}:${FINAL_BRANCH}" \
	--title "$PR_TITLE" --body-file "$pr_body_file")"
rm -f "$pr_body_file"

echo
info "PR opened: $pr_url"
