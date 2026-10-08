#!/usr/bin/env bash
#
# Sandbox harness for scripts/upstream-sync.sh.
#
# Builds a synthetic "origin" and "upstream" (bare repos + a working clone) that
# reproduce every branch state the sync must survive, then runs the real script
# against them with DRY_RUN=0. Nothing touches GitHub.
#
#   scripts/upstream-sync-test.sh            # run all scenarios
#   scripts/upstream-sync-test.sh <name>     # run one scenario
#
set -uo pipefail

SCRIPT="${SCRIPT:-$(cd "$(dirname "$0")" && pwd)/upstream-sync.sh}"
[ -x "$SCRIPT" ] || [ -f "$SCRIPT" ] || { echo "cannot find $SCRIPT"; exit 2; }

ROOT="$(mktemp -d)"
PASS=0; FAIL=0
declare -a RESULTS

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
ok()   { PASS=$((PASS+1)); RESULTS+=("PASS  $*"); printf '  \033[32mPASS\033[0m %s\n' "$*"; }
bad()  { FAIL=$((FAIL+1)); RESULTS+=("FAIL  $*"); printf '  \033[31mFAIL\033[0m %s\n' "$*"; }
check(){ if [ "$2" = "$3" ]; then ok "$1 ($2)"; else bad "$1 — expected [$3] got [$2]"; fi; }

# --------------------------------------------------------------------------- #
# Fixture: a repo with a customisation commit, then optional upstream growth.
# --------------------------------------------------------------------------- #
seed() {
  local d="$1"
  git init -q --bare "$d/upstream.git"
  git init -q --bare "$d/origin.git"
  git clone -q "$d/upstream.git" "$d/seed"
  cd "$d/seed"
  git config user.email s@t; git config user.name s; git config commit.gpgsign false
  git checkout -q -b main
  mkdir -p src
  printf 'base\n'      > src/app.txt
  printf 'readme\n'    > README.md
  git add .; git commit -qm "chore: initial"
  git push -q origin main
}

# Publish the seeded upstream, create the fork's main, add a custom commit and
# custom/production.
publish_fork() {
  local d="$1"
  cd "$d/seed"
  git push -q "$d/upstream.git" main 2>/dev/null || true
  git clone -q "$d/upstream.git" "$d/fork"
  cd "$d/fork"
  git config user.email f@t; git config user.name f; git config commit.gpgsign false
  git checkout -q main
  git remote add upstream "$d/upstream.git"
  git remote set-url origin "$d/origin.git"
  git push -q origin main
  # a customisation commit on custom/production
  git checkout -q -b custom/production
  printf 'custom-local-change\n' > CUSTOM.md
  git add .; git commit -qm "feat(custom): local production tweak"
  git push -q origin custom/production
}

# Add an upstream commit (optionally touching a file the fork customised).
upstream_commit() {
  local d="$1" file="$2" content="$3" msg="$4"
  cd "$d/seed"
  git fetch -q "$d/upstream.git" main 2>/dev/null || true
  git checkout -q main
  git pull -q --ff-only origin main 2>/dev/null || true
  mkdir -p "$(dirname "$file")" 2>/dev/null || true
  printf '%s\n' "$content" > "$file"
  git add -A; git commit -qm "$msg"
  git push -q "$d/upstream.git" main
}

run_sync() {  # run_sync <dir> <expected_exit> <label> [extra env]
  local d="$1" expected="$2" label="$3"; shift 3
  cd "$d/fork"
  local out rc
  out="$(env FORK_DEFAULT_BRANCH=main CUSTOM_BRANCH=custom/production \
             UPSTREAM_REMOTE=upstream ORIGIN_REMOTE=origin \
             REPORT_FILE="$d/report.json" "$@" bash "$SCRIPT" 2>&1)"
  rc=$?
  printf '%s\n' "$out" | sed 's/^/    | /'
  LAST_OUT="$out"; LAST_RC="$rc"; LAST_DIR="$d"
  if [ "$rc" != "$expected" ]; then bad "$label — exit [$rc] expected [$expected]"; else ok "$label — exit $rc"; fi
}

# =========================================================================== #
# Scenario 1: no upstream delta — must be a pure no-op.
# =========================================================================== #
scenario_no_updates() {
  say "scenario: no upstream updates (expect no-op, exit 0, no push)"
  local d="$ROOT/s1"; mkdir -p "$d"
  seed "$d"; publish_fork "$d"
  local before after
  before="$(git -C "$d/fork" ls-remote origin | sort)"
  run_sync "$d" 0 "no-update run exits 0"
  after="$(git -C "$d/fork" ls-remote origin | sort)"
  check "origin unchanged (no push)" "$([ "$before" = "$after" ] && echo same || echo changed)" "same"
  check "verdict" "$(jq -r .verdict "$d/report.json")" "no-upstream-delta"
  check "custom/production untouched" \
        "$(git -C "$d/fork" rev-parse origin/custom/production)" \
        "$(git -C "$d/origin.git" rev-parse custom/production)"
}

# =========================================================================== #
# Scenario 2: upstream has a NEW commit (non-conflicting) — ff main, integrate,
# propose. Plus idempotency on the second run.
# =========================================================================== #
scenario_new_upstream_commit() {
  say "scenario: new upstream commit (clean) — ff + integration branch + report"
  local d="$ROOT/s2"; mkdir -p "$d"
  seed "$d"; publish_fork "$d"
  upstream_commit "$d" "src/feature.txt" "new-feature" "feat: upstream feature"
  upstream_commit "$d" "src/fix.txt" "bugfix" "fix: upstream bugfix"

  run_sync "$d" 0 "sync with 2 new upstream commits"
  check "verdict"            "$(jq -r .verdict "$d/report.json")" "ok"
  check "fork main == upstream tip" \
        "$(git -C "$d/fork" rev-parse origin/main)" \
        "$(git -C "$d/upstream.git" rev-parse main)"
  check "integration branch exists on origin" \
        "$(git -C "$d/fork" ls-remote --heads origin automation/upstream-integration | wc -l | tr -d ' ')" "1"
  check "integration contains upstream tip" \
        "$(git -C "$d/fork" merge-base --is-ancestor "$(git -C "$d/upstream.git" rev-parse main)" origin/automation/upstream-integration && echo yes || echo no)" "yes"
  check "custom/production NOT modified" \
        "$(git -C "$d/fork" rev-parse origin/custom/production)" \
        "$(git -C "$d/origin.git" rev-parse custom/production)"
  check "no PR url (no token / dry)" "$(jq -r .prUrl "$d/report.json")" ""
  check "upstream commits listed" "$(jq '.upstreamCommitsToIntegrate|length' "$d/report.json")" "2"

  # idempotency
  local sha_before sha_after
  sha_before="$(git -C "$d/fork" rev-parse origin/automation/upstream-integration)"
  run_sync "$d" 0 "second run is idempotent"
  sha_after="$(git -C "$d/fork" rev-parse origin/automation/upstream-integration)"
  check "integration branch unchanged by re-run" "$sha_after" "$sha_before"
  check "re-run verdict" "$(jq -r .verdict "$d/report.json")" "already-proposed"
}

# =========================================================================== #
# Scenario 3: upstream edits a file the fork customised — must report conflicts,
# push nothing, leave custom/production alone.
# =========================================================================== #
scenario_conflict() {
  say "scenario: conflicting change on a customised file (expect exit 20, no push)"
  local d="$ROOT/s3"; mkdir -p "$d"
  seed "$d"; publish_fork "$d"
  # fork customises CUSTOM.md on custom/production (done in publish_fork).
  # upstream now adds the SAME file with different content on main.
  upstream_commit "$d" "CUSTOM.md" "upstream-version-of-custom" "feat: upstream adds CUSTOM.md"

  local before
  before="$(git -C "$d/fork" ls-remote origin | sort)"
  run_sync "$d" 20 "conflicting upstream change exits 20"
  local after; after="$(git -C "$d/fork" ls-remote origin | sort)"
  # main IS fast-forwarded to upstream first (that is this script's contract and
  # it is safe). What must NOT change is custom/production, and the integration
  # branch must not appear.
  check "custom/production ref unchanged" \
        "$(git -C "$d/fork" rev-parse origin/custom/production)" \
        "$(git -C "$d/fork" rev-parse custom/production)"
  check "no integration branch pushed" \
        "$(git -C "$d/fork" ls-remote --heads origin automation/upstream-integration | wc -l | tr -d ' ')" "0"
  check "upgrade applied to main is ff (old main is ancestor of new)" \
        "$(git -C "$d/fork" merge-base --is-ancestor "$(printf '%s\n' "$before" | awk '/refs\/heads\/main/{print $1}')" origin/main && echo yes || echo no)" "yes"
  check "verdict" "$(jq -r .verdict "$d/report.json")" "conflicts"
  check "conflicting file reported" "$(jq -r '.conflictingFiles[]?' "$d/report.json" | grep -c CUSTOM.md)" "1"
  check "custom/production intact" \
        "$(git -C "$d/fork" rev-parse origin/custom/production)" \
        "$(git -C "$d/origin.git" rev-parse custom/production)"
  check "no stray MERGE_HEAD in worktree" \
        "$(test -e "$d/fork/.git/MERGE_HEAD" && echo present || echo absent)" "absent"
  check "worktree not left dirty" "$(git -C "$d/fork" status --porcelain | wc -l | tr -d ' ')" "0"
}

# =========================================================================== #
# Scenario 4: fork's default branch has DIVERGED from upstream (can't ff).
# =========================================================================== #
scenario_diverged() {
  say "scenario: fork default branch diverged (expect exit 10, no push)"
  local d="$ROOT/s4"; mkdir -p "$d"
  seed "$d"; publish_fork "$d"
  # Commit ON the fork's main that upstream does not have.
  cd "$d/fork"; git checkout -q main
  printf 'fork-only\n' > FORKONLY.txt; git add .; git commit -qm "chore(fork): local main commit"
  git push -q origin main
  # And upstream moves on independently.
  upstream_commit "$d" "src/other.txt" "up" "feat: upstream moves"

  local before; before="$(git -C "$d/fork" ls-remote origin main)"
  run_sync "$d" 10 "diverged fork default branch exits 10"
  local after; after="$(git -C "$d/fork" ls-remote origin main)"
  check "fork main not force-pushed / not rewritten" "$after" "$before"
  check "verdict" "$(jq -r .verdict "$d/report.json")" "diverged"
}

# =========================================================================== #
# Scenario 5: pre-existing integration branch behind, ahead, and diverged.
# =========================================================================== #
scenario_integration_states() {
  say "scenario: integration branch behind / ahead / diverged from custom/production"
  local d="$ROOT/s5"; mkdir -p "$d"
  seed "$d"; publish_fork "$d"
  upstream_commit "$d" "src/a.txt" "a" "feat: a"
  run_sync "$d" 0 "initial integrate (behind the new upstream)"
  local integ; integ="$(git -C "$d/fork" rev-parse origin/automation/upstream-integration)"

  # custom/production moves AHEAD (a local commit) -> integration now diverges.
  cd "$d/fork"; git checkout -q custom/production
  printf 'more custom\n' > MORE_CUSTOM.md; git add .; git commit -qm "feat(custom): more local work"
  git push -q origin custom/production
  check "integration is now ahead of custom (1 integ merge + 1 custom commit)" \
        "$(git -C "$d/fork" rev-list --count custom/production..origin/automation/upstream-integration)" "2"

  # A new upstream commit arrives; the sync must rebuild the integration branch
  # on top of the NEW custom/production tip and preserve the custom commit.
  upstream_commit "$d" "src/b.txt" "b" "feat: b"
  run_sync "$d" 0 "rebuild after custom moved ahead"
  check "verdict" "$(jq -r .verdict "$d/report.json")" "ok"
  check "custom commit preserved in integration" \
        "$(git -C "$d/fork" merge-base --is-ancestor custom/production origin/automation/upstream-integration && echo yes || echo no)" "yes"
  check "new upstream commit integrated" \
        "$(git -C "$d/fork" merge-base --is-ancestor "$(git -C "$d/upstream.git" rev-parse main)" origin/automation/upstream-integration && echo yes || echo no)" "yes"
  check "custom/production untouched throughout" \
        "$(git -C "$d/fork" rev-parse origin/custom/production)" \
        "$(git -C "$d/origin.git" rev-parse custom/production)"
}

# =========================================================================== #
# Scenario 6: DRY_RUN — must not push anything.
# =========================================================================== #
scenario_dry_run() {
  say "scenario: DRY_RUN=1 pushes nothing"
  local d="$ROOT/s6"; mkdir -p "$d"
  seed "$d"; publish_fork "$d"
  upstream_commit "$d" "src/c.txt" "c" "feat: c"
  local before; before="$(git -C "$d/fork" ls-remote origin | sort)"
  run_sync "$d" 0 "dry-run" DRY_RUN=1
  local after; after="$(git -C "$d/fork" ls-remote origin | sort)"
  check "origin untouched by dry-run" "$([ "$before" = "$after" ] && echo same || echo changed)" "same"
}

# --------------------------------------------------------------------------- #
ALL=(no_updates new_upstream_commit conflict diverged integration_states dry_run)
if [ $# -gt 0 ]; then ALL=("$@"); fi
for s in "${ALL[@]}"; do "scenario_$s"; done

say "summary"
printf '%s\n' "${RESULTS[@]}"
printf '\n  %d passed, %d failed   (workspace: %s)\n' "$PASS" "$FAIL" "$ROOT"
[ "$FAIL" -eq 0 ] || exit 1
