#!/usr/bin/env bash
#
# upstream-sync.sh — safe, idempotent, non-destructive upstream synchronisation
# for the 9router-go fork.
#
# WHAT IT DOES
#   1. Fast-forwards the fork's DEFAULT branch (e.g. main) to upstream's default
#      branch, and only when that is a true fast-forward.
#   2. Builds/refreshes an INTEGRATION branch that layers upstream's default
#      branch on top of the customisation branch, WITHOUT touching the
#      customisation branch. Diverged (conflicting) files are reported, never
#      resolved.
#   3. Opens a pull request from the integration branch into the customisation
#      branch, or refreshes the existing one (idempotent — never a duplicate).
#
# SAFETY CONTRACT (do not weaken)
#   * Never force-pushes. `git push` refuses any non-fast-forward, by design.
#   * Never writes to the customisation branch (custom/production). It is only
#     ever read (rev-parse / rev-list / merge-tree) and used as a PR base.
#   * Never resolves a merge conflict and never overwrites upstream-side files
#     with ours or vice-versa. A conflict is reported and the run stops before
#     pushing that state.
#   * Integration-branch history is ephemeral and may be rebuilt, so it is pushed
#     with --force-with-lease (a guarded force). Nothing else is ever forced.
#   * Idempotent: running twice changes nothing the second time.
#
# EXIT CODES
#   0  completed (including "nothing to do")
#   10 upstream diverged from the fork default branch — human action required
#   20 merge conflicts between upstream and the customisation branch — human action
#   30 configuration/remote error
#   40 PR could not be created and the token looked insufficient
#
set -uo pipefail

# --------------------------------------------------------------------------- #
# Configuration (all overridable by env — makes the script locally testable)
# --------------------------------------------------------------------------- #
FORK_DEFAULT_BRANCH="${FORK_DEFAULT_BRANCH:-main}"
CUSTOM_BRANCH="${CUSTOM_BRANCH:-custom/production}"
INTEGRATION_BRANCH="${INTEGRATION_BRANCH:-automation/upstream-integration}"
UPSTREAM_REMOTE="${UPSTREAM_REMOTE:-upstream}"
ORIGIN_REMOTE="${ORIGIN_REMOTE:-origin}"
UPSTREAM_DEFAULT_BRANCH="${UPSTREAM_DEFAULT_BRANCH:-}"
DRY_RUN="${DRY_RUN:-0}"
REPORT_FILE="${REPORT_FILE:-/tmp/upstream-sync-report.json}"
PR_TITLE_PREFIX="${PR_TITLE_PREFIX:-chore(upstream):}"
GIT_USER_NAME="${GIT_USER_NAME:-github-actions[bot]}"
GIT_USER_EMAIL="${GIT_USER_EMAIL:-41898282+github-actions[bot]@users.noreply.github.com}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# --------------------------------------------------------------------------- #
# Helpers
# --------------------------------------------------------------------------- #
log()  { printf '%s\n' "$*"; }
info() { printf ':: %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
have_ref() { git rev-parse --verify --quiet "$1" >/dev/null 2>&1; }

# Exit code the script will finish with; 0 unless a later step sets it.
EXIT_CODE=0
SUMMARY_MD=""

# Append a line to the human summary (shown on stdout and in the PR body).
add_summary() { SUMMARY_MD="${SUMMARY_MD}${1}"$'\n'; }

# classify_merge BASE TIP
#   Sets MT_RESULT to clean|conflict|error and MT_CONFLICTS to the sorted list of
#   conflicting paths. Uses `git merge-tree --write-tree`, which performs the
#   merge in-memory: no working tree, no checkout, nothing written. It cannot
#   touch the customisation branch.
classify_merge() {
  local base="$1" tip="$2" out rc
  out="$(git merge-tree --write-tree "$base" "$tip" 2>&1)"; rc=$?
  MT_CONFLICTS=""
  case "$rc" in
    0) MT_RESULT="clean" ;;
    1) MT_RESULT="conflict"
       # Machine-readable prelude: "<mode> <oid> <stage>\t<path>" for stage 1/2/3.
       MT_CONFLICTS="$(printf '%s\n' "$out" \
         | awk -F'\t' '/^[0-7]{6} [0-9a-f]+ [123]\t/ {print $2}' | sort -u)" ;;
    *) MT_RESULT="error"
       warn "git merge-tree failed (rc=$rc): $out" ;;
  esac
}

# push_ref_if_changed LOCAL_REF REMOTE REMOTE_REF PUSH_ARGS...
#   Pushes only when the local ref differs from the remote's current value, so a
#   no-op run performs no push.
push_ref_if_changed() {
  local local_ref="$1" remote="$2" remote_ref="$3"; shift 3
  local local_sha remote_sha
  local_sha="$(git rev-parse "$local_ref")"
  remote_sha="$(git ls-remote "$remote" "refs/heads/$remote_ref" 2>/dev/null | awk '{print $1}')"
  if [ "$local_sha" = "$remote_sha" ]; then
    info "  $remote_ref already at ${local_sha:0:8} — no push needed"
    return 0
  fi
  if [ "$DRY_RUN" = "1" ]; then
    info "  [dry-run] would push $local_ref -> $remote/$remote_ref ($local_sha)"
    return 0
  fi
  info "  pushing $local_ref -> $remote/$remote_ref (${local_sha:0:8})"
  git push "$@" "$remote" "$local_ref:refs/heads/$remote_ref"
}

write_report() {
  local verdict="$1" pr_url="$2" conflicts_json="$3" commits_json="$4" notes="$5"
  jq -n \
    --arg fork_default "$FORK_DEFAULT_BRANCH" \
    --arg custom "$CUSTOM_BRANCH" \
    --arg integration "$INTEGRATION_BRANCH" \
    --arg upstream_sha "$UPSTREAM_SHA" \
    --arg custom_sha "$CUSTOM_SHA" \
    --arg verdict "$verdict" \
    --arg pr_url "$pr_url" \
    --argjson conflicts "$conflicts_json" \
    --argjson upstream_commits "$commits_json" \
    --arg run_url "${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY:-}/actions/runs/${GITHUB_RUN_ID:-}" \
    --arg notes "$notes" \
    '{forkDefaultBranch:$fork_default, customBranch:$custom, integrationBranch:$integration,
      upstreamSha:$upstream_sha, customSha:$custom_sha, verdict:$verdict,
      prUrl:$pr_url, conflictingFiles:$conflicts, upstreamCommitsToIntegrate:$upstream_commits,
      runUrl:$run_url, notes:$notes}' > "$REPORT_FILE"
  log "report: $REPORT_FILE"
  cat "$REPORT_FILE"
}

# --------------------------------------------------------------------------- #
# 0. Sanity: the remotes and branches this script depends on must exist.
# --------------------------------------------------------------------------- #
mkdir -p /tmp
export GIT_TERMINAL_PROMPT=0

git rev-parse --git-dir >/dev/null 2>&1 || { warn "not inside a git repository"; exit 30; }

for r in "$UPSTREAM_REMOTE" "$ORIGIN_REMOTE"; do
  git remote get-url "$r" >/dev/null 2>&1 || { warn "remote '$r' is not configured"; exit 30; }
done

# Identity for the integration merge commit. Set on the repo (bot identity is
# fine for a throwaway integration branch) unless it is already configured.
if [ -z "$(git config user.email || true)" ]; then
  git config user.name  "$GIT_USER_NAME"
  git config user.email "$GIT_USER_EMAIL"
fi

info "fetching $UPSTREAM_REMOTE and $ORIGIN_REMOTE"
if ! git fetch --prune --tags "$UPSTREAM_REMOTE" 2>&1 | sed 's/^/  /'; then exit 30; fi
if ! git fetch --prune "$ORIGIN_REMOTE" 2>&1 | sed 's/^/  /'; then exit 30; fi

# Resolve upstream's default branch rather than assuming it.
if [ -z "$UPSTREAM_DEFAULT_BRANCH" ]; then
  UPSTREAM_DEFAULT_BRANCH="$(git symbolic-ref --short "refs/remotes/$UPSTREAM_REMOTE/HEAD" 2>/dev/null | sed "s#^$UPSTREAM_REMOTE/##")"
fi
[ -n "$UPSTREAM_DEFAULT_BRANCH" ] || UPSTREAM_DEFAULT_BRANCH="$FORK_DEFAULT_BRANCH"
UPSTREAM_REF="$UPSTREAM_REMOTE/$UPSTREAM_DEFAULT_BRANCH"
have_ref "$UPSTREAM_REF" || { warn "cannot resolve upstream ref '$UPSTREAM_REF'"; exit 30; }

CUSTOM_REF="$FORK_DEFAULT_BRANCH"
[ "$CUSTOM_BRANCH" != "$FORK_DEFAULT_BRANCH" ] && CUSTOM_REF="$ORIGIN_REMOTE/$CUSTOM_BRANCH"
have_ref "$CUSTOM_REF" || { warn "cannot resolve customisation branch '$CUSTOM_BRANCH' ($CUSTOM_REF)"; exit 30; }

UPSTREAM_SHA="$(git rev-parse "$UPSTREAM_REF")"
CUSTOM_SHA="$(git rev-parse "$CUSTOM_REF")"
log ""
info "upstream  $UPSTREAM_REF = ${UPSTREAM_SHA:0:10}"
info "fork      $FORK_DEFAULT_BRANCH = $(git rev-parse "$ORIGIN_REMOTE/$FORK_DEFAULT_BRANCH" 2>/dev/null | cut -c1-10 || echo '(absent)')"
info "custom    $CUSTOM_BRANCH = ${CUSTOM_SHA:0:10}"
log ""

# --------------------------------------------------------------------------- #
# 1. Fast-forward the fork's default branch to upstream — ff-only, never forced.
# --------------------------------------------------------------------------- #
FORK_DEFAULT_REF="$ORIGIN_REMOTE/$FORK_DEFAULT_BRANCH"

# Diverged means: upstream does not contain the fork default tip. Merging would
# require rewriting or a merge commit in a branch this tool must not rewrite.
if have_ref "$FORK_DEFAULT_REF" \
   && ! git merge-base --is-ancestor "$FORK_DEFAULT_REF" "$UPSTREAM_SHA"; then
  warn "'$FORK_DEFAULT_BRANCH' has diverged from '$UPSTREAM_REF' (fork default is not an ancestor of upstream)."
  add_summary "- **DIVERGED**: \`$FORK_DEFAULT_BRANCH\` is not an ancestor of \`$UPSTREAM_DEFAULT_BRANCH\`. Automatic sync stopped; fast-forward is impossible without rewriting history."
  write_report "diverged" "" '[]' '[]' "fork default branch diverged from upstream; refusing to non-fast-forward"
  log "$SUMMARY_MD"
  exit 10
fi

git checkout -q -B "$FORK_DEFAULT_BRANCH" "$FORK_DEFAULT_REF" 2>/dev/null \
  || git checkout -q -B "$FORK_DEFAULT_BRANCH" "$UPSTREAM_SHA"

if [ "$(git rev-parse HEAD)" = "$UPSTREAM_SHA" ]; then
  info "step 1: $FORK_DEFAULT_BRANCH already at upstream tip — nothing to sync"
  NEW_UPSTREAM=0
else
  # --ff-only refuses anything but a fast-forward; it cannot rewrite history.
  git merge --ff-only "$UPSTREAM_SHA"
  info "step 1: fast-forwarded $FORK_DEFAULT_BRANCH to ${UPSTREAM_SHA:0:10}"
  push_ref_if_changed "$FORK_DEFAULT_BRANCH" "$ORIGIN_REMOTE" "$FORK_DEFAULT_BRANCH"
  NEW_UPSTREAM=1
fi

# --------------------------------------------------------------------------- #
# 2. What does upstream hold that the customisation branch does not?
# --------------------------------------------------------------------------- #
if git merge-base --is-ancestor "$UPSTREAM_SHA" "$CUSTOM_SHA"; then
  info "step 2: $CUSTOM_BRANCH already contains upstream ${UPSTREAM_SHA:0:10} — nothing to integrate"
  add_summary "- Upstream \`${UPSTREAM_SHA:0:10}\` is already contained in \`$CUSTOM_BRANCH\`. Nothing to integrate."
  write_report "no-upstream-delta" "" '[]' '[]' "custom branch already contains upstream tip"
  log "$SUMMARY_MD"
  exit 0
fi

mapfile -t UP_COMMITS < <(git rev-list --no-merges --reverse "$CUSTOM_SHA..$UPSTREAM_SHA")
UP_COUNT="${#UP_COMMITS[@]}"
info "step 2: $UP_COUNT upstream commit(s) not yet in $CUSTOM_BRANCH"

# --------------------------------------------------------------------------- #
# 3. Build the integration branch = upstream tip on top of the customisation.
#    Built from explicit merges so repeated runs converge instead of stacking
#    merge commits. The customisation branch itself is never modified.
# --------------------------------------------------------------------------- #
git checkout -q -B "$INTEGRATION_BRANCH" "$CUSTOM_SHA"

# 3a. Reuse an existing integration merge when this exact upstream tip is already
#     merged, so a nightly re-run neither rebuilds the branch nor reopens a PR.
EXISTING_MERGE=""
if have_ref "$ORIGIN_REMOTE/$INTEGRATION_BRANCH"; then
  # Newest merge on the ancestry path custom -> integration (rev-list is newest
  # first). If it does not already contain this upstream tip, it is discarded and
  # the branch is rebuilt below.
  EXISTING_MERGE="$(git rev-list --ancestry-path --merges "$CUSTOM_SHA..$ORIGIN_REMOTE/$INTEGRATION_BRANCH" 2>/dev/null | head -1 || true)"
fi

MERGE_TREE_SHA=""
REUSED_MERGE=0
if [ -n "$EXISTING_MERGE" ]; then
  # Fast-forward our local integration branch to the known-good published one.
  git merge --ff-only "$EXISTING_MERGE" >/dev/null 2>&1 || true
  if git merge-base --is-ancestor "$UPSTREAM_SHA" HEAD; then
    info "step 3: integration branch already merges upstream ${UPSTREAM_SHA:0:10} — reusing"
    REUSED_MERGE=1
  else
    EXISTING_MERGE=""
  fi
fi

if [ -z "$EXISTING_MERGE" ]; then
  info "step 3: merging upstream ${UPSTREAM_SHA:0:10} into $INTEGRATION_BRANCH"
  # --no-ff keeps the upstream tip as an explicit merge parent even if the merge
  # could fast-forward, so fast-forward detection stays unambiguous.
  if ! git merge --no-ff --no-edit --no-commit "$UPSTREAM_SHA" >/dev/null 2>&1; then
    :
  fi
  if git ls-files --unmerged | grep -q .; then
    CONFLICTS="$(git diff --name-only --diff-filter=U | sort -u)"
    warn "merge conflicts between upstream and '$CUSTOM_BRANCH':"
    while IFS= read -r f; do [ -n "$f" ] && printf '   %s\n' "$f" >&2; done <<< "$CONFLICTS"
    git merge --abort >/dev/null 2>&1 || git reset --hard -q "$CUSTOM_SHA"
    add_summary "- **CONFLICTS**: upstream \`${UPSTREAM_SHA:0:10}\` conflicts with \`$CUSTOM_BRANCH\` on the files below. Nothing was pushed and nothing was resolved."
    while IFS= read -r f; do [ -n "$f" ] && add_summary "  - \`$f\`"; done <<< "$CONFLICTS"
    # Build the JSON array from the same list. mapfile keeps paths containing
    # spaces intact; jq --args takes them positionally so no word-splitting.
    # Note: no `local` here — this is the script body, not a function.
    mapfile -t cf_arr <<< "$CONFLICTS"
    CF_JSON="$(jq -n --args '[$ARGS.positional[] | select(length > 0)]' "${cf_arr[@]}")"
    write_report "conflicts" "" "$CF_JSON" '[]' "merge conflicts; unresolved, nothing pushed"
    log "$SUMMARY_MD"
    exit 20
  fi
  git commit -q --no-edit -m "chore(upstream): merge ${UPSTREAM_DEFAULT_BRANCH}@${UPSTREAM_SHA:0:10} into ${CUSTOM_BRANCH}

Automated integration of upstream ${UPSTREAM_SHA:0:10}.
Merge conflicts: none. Requires human review before merging into ${CUSTOM_BRANCH}."
  MERGE_TREE_SHA="$(git rev-parse HEAD)"

  # Non-fatal information: would the merge conflict? Already answered above for
  # the merge we just made, but report it explicitly for the PR body.
  classify_merge "$CUSTOM_SHA" "$UPSTREAM_SHA"
  if [ "$MT_RESULT" = "conflict" ]; then
    warn "merge-tree reported conflicts (merge succeeded, so these were resolved automatically):"
    printf '   %s\n' $MT_CONFLICTS >&2
  fi

  push_ref_if_changed "$INTEGRATION_BRANCH" "$ORIGIN_REMOTE" "$INTEGRATION_BRANCH" --force-with-lease
fi

# --------------------------------------------------------------------------- #
# 4. Open or refresh the pull request. Idempotent: one PR per branch.
# --------------------------------------------------------------------------- #
BRANCH_COMMITS_JSON="$(printf '%s\n' "${UP_COMMITS[@]}" | jq -R 'select(length>0)' | jq -s 'map({sha:.[0:10]})')"
CONFLICTS_JSON='[]'

# Commits that reached the integration branch but are not yet in the custom branch.
BEHIND="$(git rev-list --no-merges --reverse "$CUSTOM_SHA..$INTEGRATION_BRANCH" 2>/dev/null | wc -l | tr -d ' ')"

if [ "$BEHIND" = "0" ]; then
  info "step 4: integration branch holds no commits beyond $CUSTOM_BRANCH — nothing to propose"
  add_summary "- Nothing to propose: \`$INTEGRATION_BRANCH\` adds no commits beyond \`$CUSTOM_BRANCH\`."
  write_report "no-delta" "" "$CONFLICTS_JSON" "$BRANCH_COMMITS_JSON" "integration adds no commits"
  log "$SUMMARY_MD"
  exit 0
fi

PR_URL=""
PR_STATE=""
if command -v gh >/dev/null 2>&1 && [ -n "${GH_TOKEN:-}" ]; then
  PR_STATE="$(gh pr view "$INTEGRATION_BRANCH" --json url,state --jq '"\(.state) \(.url)"' 2>/dev/null || true)"
fi

build_pr_body() {
  cat <<EOF
Automated proposal to bring upstream \`$UPSTREAM_DEFAULT_BRANCH\` into \`$CUSTOM_BRANCH\`.

**Do not merge without review.** This PR was opened by automation and requires
human approval. It contains only upstream commits; customisation files were not
rewritten.

### Upstream commit range
\`${CUSTOM_SHA:0:10}..${UPSTREAM_SHA:0:10}\` on \`$UPSTREAM_DEFAULT_BRANCH\` — $UP_COUNT commit(s) not yet in \`$CUSTOM_BRANCH\`.

### Upstream commits
$(printf '%s\n' "${UP_COMMITS[@]}" | while read -r c; do [ -n "$c" ] && printf -- '- [`%s`](%s/commit/%s) %s\n' "${c:0:10}" "${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY:-}" "$c" "$(git log -1 --format=%s "$c")"; done)

### Merge result
$( [ "$MT_RESULT" = "conflict" ] && echo "Conflicts were detected by \`merge-tree\`; the merge that produced this branch resolved differences automatically. Read the diff carefully — do not blanket-accept." || echo "Clean merge: no conflicting files were detected." )

### Deliberately NOT done
- Not merged automatically.
- \`$CUSTOM_BRANCH\` not modified.
- No conflicting file overwritten, and no side chosen.
- Nothing deployed.

$(cat <<'NOTE'
### Reviewer checklist
- [ ] Read every upstream commit above; check for behaviour changes that affect local customisations.
- [ ] Confirm locally modified files are intact (see docs/UPSTREAM_SYNC.md → "Verifying customisations survived").
- [ ] CI on this PR is green.
- [ ] Then merge — "Create a merge commit" or "Squash", never "Rebase".
NOTE
)
EOF
}

if [ -n "$PR_STATE" ]; then
  case "$PR_STATE" in
    OPEN\ *) PR_URL="${PR_STATE#OPEN }"; info "step 4: refreshing existing PR $PR_URL"
             if [ "$DRY_RUN" != "1" ]; then gh pr edit "$INTEGRATION_BRANCH" --body "$(build_pr_body)" >/dev/null || warn "could not refresh PR body"; fi ;;
    MERGED\ *) PR_URL="${PR_STATE#MERGED }"; info "step 4: PR already MERGED ($PR_URL) — reopening the proposal is a human decision"
               add_summary "- A previous proposal (\`$PR_URL\`) was already merged but \`$CUSTOM_BRANCH\` still lacks these commits. A human must decide." ;;
    CLOSED\ *) PR_URL="${PR_STATE#CLOSED }"; warn "existing PR $PR_URL is CLOSED; not creating a duplicate"
               add_summary "- PR \`$PR_URL\` exists and is **closed**. Not creating a duplicate — reopen it or merge the integration branch manually." ;;
  esac
elif [ "$DRY_RUN" = "1" ]; then
  info "step 4: [dry-run] would open a PR $INTEGRATION_BRANCH -> $CUSTOM_BRANCH"
else
  if command -v gh >/dev/null 2>&1 && [ -n "${GH_TOKEN:-}" ]; then
    if PR_URL="$(gh pr create --base "$CUSTOM_BRANCH" --head "$INTEGRATION_BRANCH" \
                   --title "${PR_TITLE_PREFIX} integrate ${UPSTREAM_DEFAULT_BRANCH}@${UPSTREAM_SHA:0:10} (${UP_COUNT} commits)" \
                   --body "$(build_pr_body)" 2>&1)"; then
      info "step 4: opened PR $PR_URL"
    else
      warn "gh pr create failed: $PR_URL"
      add_summary "- **PR creation failed** (\`gh pr create\`). The integration branch \`$INTEGRATION_BRANCH\` is pushed and ready; open the PR manually, or grant the workflow \`pull-requests: write\`."
      PR_URL=""
      EXIT_CODE=40
    fi
  else
    add_summary "- **No PR opened**: \`gh\` or \`GH_TOKEN\` unavailable. Branch \`$INTEGRATION_BRANCH\` is pushed; open the PR manually."
    PR_URL=""
  fi
fi

VERDICT="ok"
NOTES="integration branch pushed; awaiting human review"
if [ "$REUSED_MERGE" = "1" ]; then
  VERDICT="already-proposed"
  NOTES="integration branch already carried this upstream tip; no change made"
fi

write_report "$VERDICT" "$PR_URL" "$CONFLICTS_JSON" "$BRANCH_COMMITS_JSON" "$NOTES"
log "$SUMMARY_MD"
exit "$EXIT_CODE"