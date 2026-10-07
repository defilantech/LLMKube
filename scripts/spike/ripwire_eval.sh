#!/usr/bin/env bash
# Spike collateral (feat/ripwire_spike). Not product code; delete after the
# decision.
#
# Measures strict file@K localization for two rankers on llmkube's own merged
#-issue ground truth:
#   - ripwire  : `ripwire . --for=<query> --format=candidates` (call-graph)
#   - repomap  : pkg/foreman/agent/repomap Walk+ScoreFiles   (lexical)
#
# Ground truth is the set of non-test .go files changed by the fixing commit
# referenced as (#N) in the commit subject. The query is the real GitHub
# issue/PR body for N, fetched with gh, so neither ranker is handed the answer
# via the commit subject. Both rankers receive byte-identical query text.
#
# Usage: scripts/spike/ripwire_eval.sh [NCASES] [WORKSPACE]
#
# Falsification hooks (run these before trusting any number):
#   SPIKE_FORCE_QUERY="<unrelated text>"  -> both rankers must score ~0 hits.
#   SPIKE_GOLD_FROM_RELEASE=1             -> gold set is HEAD's changed files
#                                            for every case; every score must
#                                            collapse (proves gold is read per
#                                            case, not constant).
set -u

NCASES="${1:-30}"
WS="${2:-.}"
export PATH="$HOME/.local/bin:$PATH"

if ! command -v ripwire >/dev/null 2>&1; then
  echo "ripwire not on PATH" >&2; exit 2
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "== building repomap ranker ==" >&2
( cd "$WS" && go build -o "$TMP/repomaprank" ./scripts/spike/repomaprank/main.go ) || exit 1

release_gold="$TMP/release_gold.txt"
git -C "$WS" show --name-only --format= HEAD \
  | grep -E '\.go$' | grep -vE '_test\.go$' | sort -u > "$release_gold"

# ---- collect usable cases: subject refs (#N) and the commit changed prod .go
# bash 3.2 (macOS default): no mapfile, so read into an indexed array.
CASES=()
while IFS= read -r line; do CASES+=("$line"); done < <(
  git -C "$WS" log --format='%H %s' -n 600 \
  | while read -r sha subj; do
      num=$(printf '%s' "$subj" | grep -oE '\(#[0-9]+\)' | head -1 | tr -dc '0-9')
      [ -z "$num" ] && continue
      prod=$(git -C "$WS" show --name-only --format= "$sha" 2>/dev/null \
             | grep -E '\.go$' | grep -vE '_test\.go$' | sort -u)
      [ -z "$prod" ] && continue
      echo "$num $sha"
    done | head -n "$NCASES"
)

printf 'case\tissue\tgold_n\trip@5\trepo@5\trip@10\trepo@10\teasy\n'
tot_g=0; t5=0; r5=0; t10=0; r10=0

for entry in "${CASES[@]}"; do
  num="${entry%% *}"; sha="${entry##* }"

  if [ -n "${SPIKE_GOLD_FROM_RELEASE:-}" ]; then
    gold="$release_gold"
  else
    gold=$(git -C "$WS" show --name-only --format= "$sha" 2>/dev/null \
           | grep -E '\.go$' | grep -vE '_test\.go$' | sort -u)
  fi
  [ -z "$gold" ] && continue
  gold_n=$(printf '%s\n' "$gold" | wc -l | tr -d ' ')

  if [ -n "${SPIKE_FORCE_QUERY:-}" ]; then
    query="$SPIKE_FORCE_QUERY"
  else
    query=$(gh pr view "$num" --json title,body -q '.title + "\n" + .body' 2>/dev/null \
            || gh issue view "$num" --json title,body -q '.title + "\n" + .body' 2>/dev/null)
  fi
  # Collapse whitespace and cap length so both rankers see identical text.
  query=$(printf '%s' "$query" | tr '\n\t' '  ' | tr -s ' ' | cut -c1-1500)

  # ripwire ranked files (dedupe paths preserving rank order)
  rip_files=$(ripwire "$WS" --for="$query" --format=candidates 2>/dev/null \
              | grep -oE 'p="[^"]+"' | cut -d'"' -f2 | awk '!s[$0]++')
  # repomap ranked files
  repo_files=$("$TMP/repomaprank" "$WS" "$query" 2>/dev/null | cut -f2)

  hit() { # $1=ranked list  $2=K
    local k="$2" n=0
    while IFS= read -r f; do
      [ -z "$f" ] && continue
      if printf '%s\n' "$gold" | grep -qxF "$f"; then n=$((n+1)); fi
      k=$((k-1)); [ "$k" -le 0 ] && break
    done <<< "$1"
    echo "$n"
  }

  t5n=$(hit "$rip_files" 5);  r5n=$(hit "$repo_files" 5)
  t10n=$(hit "$rip_files" 10); r10n=$(hit "$repo_files" 10)

  easy="no"
  while IFS= read -r g; do
    base=$(basename "$g")
    case "$query" in *"$base"*) easy="yes"; break;; esac
  done <<< "$gold"

  printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
    "$sha" "$num" "$gold_n" "$t5n" "$r5n" "$t10n" "$r10n" "$easy"

  tot_g=$((tot_g+gold_n)); t5=$((t5+t5n)); r5=$((r5+r5n)); t10=$((t10+t10n)); r10=$((r10+r10n))
done

echo "== totals across ${#CASES[@]} candidate cases =="
printf 'gold_files_total\t%s\n' "$tot_g"
printf 'ripwire_hits@5\t%s\n' "$t5"
printf 'repomap_hits@5\t%s\n' "$r5"
printf 'ripwire_hits@10\t%s\n' "$t10"
printf 'repomap_hits@10\t%s\n' "$r10"
