# Upstream synchronisation

How this fork stays current with [`luqman-v1/9router-go`](https://github.com/luqman-v1/9router-go)
without ever risking the customisations on `custom/production`.

- [Repository architecture](#repository-architecture)
- [Purpose of each branch](#purpose-of-each-branch)
- [The automated sync](#the-automated-sync)
- [Manual synchronisation](#manual-synchronisation)
- [Resolving a conflict](#resolving-a-conflict)
- [Verifying customisations survived](#verifying-customisations-survived)
- [CI](#ci)
- [Rollback](#rollback)
- [Security](#security)
- [Pausing or disabling automation](#pausing-or-disabling-automation)
- [Maintenance checklist](#maintenance-checklist)
- [How Hermes handles future upstream updates](#how-hermes-handles-future-upstream-updates)

## Repository architecture

```
                luqman-v1/9router-go          (upstream, read-only for us)
                        │
                        │  fetch (public, no credential)
                        ▼
   dawamr/9router-go-with-restricted-model    (origin — the fork)
        ├── main                              mirror of upstream; never diverges
        ├── custom/production                 the branch that carries local work
        └── automation/upstream-integration   ephemeral; upstream merged onto custom
                        │
                        │  pull request (human approves)
                        ▼
                  custom/production
```

Remotes in a clone:

| Remote | URL | Role |
|---|---|---|
| `origin` | `git@github.com:dawamr/9router-go-with-restricted-model.git` | the fork; pushes go here |
| `upstream` | `https://github.com/luqman-v1/9router-go.git` | read-only source of truth |

## Purpose of each branch

| Branch | Purpose | Automation may write? |
|---|---|---|
| `main` | Identical to upstream's `main`. Fast-forwarded only. | Yes — fast-forward only |
| `custom/production` | Local customisations. The only branch that represents what we run. | **Never** |
| `automation/upstream-integration` | Throwaway. Upstream merged on top of `custom/production`, proposed by PR. | Yes — rebuilt each run |

Nothing is deployed automatically by any of this. Deployment is a separate,
manual step.

## The automated sync

`.github/workflows/upstream-sync.yml` runs daily at 02:17 UTC, and on demand via
**Actions → Upstream sync → Run workflow** (tick *dry_run* to report without
pushing). It calls `scripts/upstream-sync.sh`, which does four things:

1. **Fetch** `upstream` and `origin`.
2. **Fast-forward `main`** to upstream's default branch — but only if that is a
   true fast-forward. If `main` has diverged, the run stops with exit `10` and
   pushes nothing.
3. **Build `automation/upstream-integration`** by merging upstream's tip onto
   `custom/production`. A merge that conflicts stops the run with exit `20`,
   reports the conflicting paths, and pushes nothing. `custom/production` is
   never the merge target of a write — always the *first parent* of a throwaway
   branch.
4. **Open or refresh the PR** into `custom/production`. One branch → at most one
   open PR, so re-runs edit rather than duplicate.

Exit codes (they surface in the job summary and turn the run red):

| Code | Meaning | Human action |
|---|---|---|
| 0 | Done, or nothing to do | none |
| 10 | `main` diverged from upstream | inspect; history rewrite is a human call |
| 20 | Upstream conflicts with `custom/production` | resolve on a branch, see below |
| 30 | Configuration/remote error | fix remotes |
| 40 | Branch pushed but PR creation failed | open the PR manually |

**Why one workflow and not two.** GitHub does not create workflow runs for events
raised by the built-in `GITHUB_TOKEN`, so a push made with that token cannot start
a `push`-triggered workflow. A second workflow chained on the integration
branch's push would silently never fire. The PR is therefore opened inside the
same job. The one event `GITHUB_TOKEN` *can* raise that produces runs is
`pull_request` opened/synchronize/reopened, in an **approval-required** state; if
GitHub gates our PR's CI, one *Approve workflows to run* click releases it.

## Manual synchronisation

Do exactly what the workflow does, locally:

```bash
git fetch upstream origin
bash scripts/upstream-sync.sh          # honours the env vars below
DRY_RUN=1 bash scripts/upstream-sync.sh   # report only, push nothing
```

Overridable environment (it is what makes the script testable):

| Variable | Default |
|---|---|
| `FORK_DEFAULT_BRANCH` | `main` |
| `CUSTOM_BRANCH` | `custom/production` |
| `INTEGRATION_BRANCH` | `automation/upstream-integration` |
| `UPSTREAM_REMOTE` / `ORIGIN_REMOTE` | `upstream` / `origin` |
| `DRY_RUN` | `0` |

Run the sandbox suite to confirm the engine behaves before trusting it:

```bash
bash scripts/upstream-sync-test.sh      # 36 assertions, synthetic repos, no network
```

## Resolving a conflict

The automation refuses to resolve conflicts, so exit `20` means a human decides.
The conflict is between upstream and **your** customisation — the tool will not
guess which side wins.

```bash
git fetch upstream origin
git switch -c resolve/upstream-$(date +%Y%m%d) custom/production
git merge upstream/main          # reproduce the conflict locally
git status                       # the conflicting paths are listed here
# edit each file by hand — keep the custom behaviour, take the upstream fix, or combine
git add <files> && git commit
```

Rules:

- **Do not** reach for `git checkout --theirs` / `--ours`. Both discard work, and
  `--theirs` silently throws away the local customisation.
- Land the resolution the normal way: branch → PR → review → merge. Then the
  next sync run finds nothing left to integrate.
- If the resolution changes custom behaviour, that is a product decision, not a
  git decision.

## Verifying customisations survived

A PR from the integration branch should contain **upstream commits only**. Check
before merging:

```bash
git fetch origin
git log --oneline origin/custom/production..origin/automation/upstream-integration
# every commit here that is NOT "chore(upstream): merge ..." should exist upstream:
git log --oneline upstream/main | grep -F "$(git rev-parse --short <sha>)"
```

And confirm the customisation commits are still present on the target branch:

```bash
git log --oneline origin/custom/production | head -20   # your commits still there
git merge-base --is-ancestor <your-custom-sha> origin/automation/upstream-integration && echo "preserved"
```

Local customisation commits are never dropped by a merge commit, because a merge
keeps both parents. The only way to lose them is a force-push or a rebase, which
the automation never performs.

## CI

`.github/workflows/custom-ci.yml` validates the branch under test:

| Job | Covers |
|---|---|
| `go` | module integrity, `go mod tidy` drift, `go vet`, `go test`, `go build`, PGO assertion |
| `race` | `go test -race` |
| `integration` | `go vet -tags=integration` + the offline integration suite |
| `web` | locked install, production build, `bun test`, `oxlint`, svelte-check ratchet |
| `automation` | shell syntax + the 36-assertion sync sandbox |

The upstream `ci.yml` and `release.yml` still exist and still run; `custom-ci.yml`
adds gates rather than replacing them.

Common failures:

- **`go.mod or go.sum is not tidy`** → run `go mod tidy` and commit the result.
- **PGO assertion** → `cmd/9router-go/default.pgo` was renamed, ignored, or a
  `-pgo=off` flag appeared; nothing else would have failed.
- **svelte-check ratchet** → either a genuine unresolved identifier (fix it) or
  the error count grew past `web/scripts/svelte-check-baseline.json`. Never raise
  the baseline to make it pass; shrink it.
- **A workflow never ran** → `workflow_dispatch` and `schedule` only fire when the
  workflow file exists on the **default branch**. Merge it to `main` first.
- **The integration PR's CI sits unapproved** → GitHub raised the
  `pull_request` event from `GITHUB_TOKEN`; a user with write access clicks
  *Approve workflows to run*.

## Rollback

Everything the automation touches is recoverable, and it never rewrites history.

- **Bad automation change** (a workflow or script): revert it on the setup branch
  and merge; automation is additive.
- **Integration branch is wrong**: delete it. It is regenerated from
  `custom/production` + upstream on the next run:
  `git push origin --delete automation/upstream-integration`.
- **A bad merge landed on `custom/production`** after a review: do **not**
  rewrite the branch. Add a revert commit:
  ```bash
  git switch custom/production
  git revert -m 1 <merge-sha>     # -m 1 keeps the custom/production parent
  ```
  Then push normally.
- **Automation pushed something wrong to `main`**: `main` is fast-forward-only,
  so `git push --force-with-lease origin <good-sha>:main` is safe and is the only
  place a force is ever justified. It is still a human decision.

## Security

- No secrets are used. This fork has no repository secrets configured, and the
  workflows reference none. Fork PRs receive a read-only token and no secrets.
- `pull_request_target` is deliberately not used anywhere.
- Every third-party action is pinned to a full commit SHA, with the version in a
  trailing comment. CI verifies the pin matches the tag it claims.
- Permissions are least-privilege: `contents: read` for CI, and
  `contents: write` + `pull-requests: write` only for the sync (the minimum that
  can push a branch and open a PR).
- The sync script never prints a token, never disables TLS verification, and
  builds remotes from `git remote` entries rather than URLs containing
  credentials.
- Upstream scripts are not executed. The sync only performs `git` operations on
  the tree; it runs no code that upstream commits introduce.

## Pausing or disabling automation

- **Pause the schedule**: Actions → Upstream sync → ⋯ → *Disable workflow*. The
  `workflow_dispatch` trigger also stops; re-enable to resume.
- **Stop only the schedule, keep manual runs**: comment out the `schedule:` block
  and merge.
- **Stop the PR from being opened**: run manually with *dry_run* checked, or set
  `INTEGRATION_BRANCH` to a branch you inspect without a PR.
- **Fork-wide**: Settings → Actions → *Disable actions*.

## Maintenance checklist

- [ ] Weekly: read the open upstream-integration PR; merge it, or close it with a
      note explaining why.
- [ ] When a sync run goes red: read the job summary; exit 10 and 20 both need you.
- [ ] After merging an update: rebuild and redeploy, then verify (below).
- [ ] Monthly: confirm `main` still fast-forwards (a red exit-10 run means
      someone committed to `main` directly).
- [ ] On a new action version: update the pin SHA *and* the trailing comment.

Deploy on this host (see the `9router-go-deploy` skill for the full runbook):

```bash
systemctl --user status 9router-go.service
~/.local/bin/9router-go-redeploy        # pull, build, restart, verify
curl -s https://9router.s2.dawam.dev/health   # {"status":"ok"}
```

## Required repository settings

Two settings are **not** yet applied on the fork and both need an owner decision.

### 1. Default branch (required for the daily schedule)

`on: schedule` runs the workflow **from the repository's default branch**, so the
sync file must live there. The fork's default branch is currently `main`, which
must stay a pure fast-forward mirror of upstream — and a fast-forward to upstream
would *delete* the automation files from `main`. The two requirements conflict.

`on: workflow_dispatch` does **not** have this restriction: it has been verified
working from `automation/upstream-sync-setup` against this fork. So:

- **Manual sync works today** — Actions → Upstream sync → *Run workflow*, or
  `gh workflow run upstream-sync.yml --ref <branch> -f dry_run=true`.
- **Automatic daily sync needs `custom/production` as the default branch**:
  Settings → General → Default branch → `custom/production`. That is the right
  long-term choice for this fork anyway: unqualified PRs then target
  `custom/production`, and `main` is freed to be a pure upstream mirror.

### 2. Branch protection on `custom/production`

Neither branch is currently protected (no rulesets, no branch protection). A
ruleset with the following shape is recommended — it enforces the guarantee the
automation already implements, at the repository level:

| Rule | Setting |
|---|---|
| Require a pull request before merging | on |
| Required approvals | 1 |
| Dismiss stale approvals on new commits | on |
| Require status checks to pass | on — `Go — integrity, vet, test, build`, `Go — race detector`, `Go — integration suite`, `Web — build, test, lint, svelte-check`, `Automation — sync engine sandbox` |
| Require branches to be up to date before merging | on |
| Block force pushes | on |
| Restrict deletions | on |

Deliberately **not** included: any auto-merge or auto-deploy rule. Nothing in this
repository may deploy without a human.

## How Hermes handles future upstream updates

When asked to bring upstream changes in, Hermes should:

1. Run `DRY_RUN=1 bash scripts/upstream-sync.sh` **first** and report the verdict
   (`no-upstream-delta`, `ok`, `diverged`, `conflicts`) before changing anything.
2. Never resolve a conflict on its own initiative, and never use
   `git checkout --theirs` / `--ours`. Present the conflicting files, the
   competing changes, and a proposed resolution, then wait for approval.
3. Never merge the integration PR, never force-push, and never touch
   `custom/production` without explicit approval.
4. Run `bash scripts/upstream-sync-test.sh` after modifying anything under
   `scripts/` or `.github/workflows/`.
5. Treat a change to `custom/production` as a product decision requiring the
   operator's approval, and a change to deployment as a separate, explicitly
   authorised step.
