# Dev note — #66 Release v0.1.0 checklist and tag

- **Issue:** #66 (epic #12, milestone M5, `area: demo`, `priority: P1`)
- **Branch:** release/v0.1.0 — the workflow runner created
  `feature/66-release-v0-1-0`; it is renamed to `release/v0.1.0` before
  push (maintainer decision #2, CONTRIBUTING.md:12, README.md:218)
- **Date:** 2026-11-04

> **Provenance:** both maintainer decisions were read from the issue #66
> comments this session (`gh issue view 66 --comments`): (1) **the release
> runs NOW** — the video link ships via the documented GitHub Release
> fallback, and the final YouTube-unlisted URL is swapped in with a small
> commit after the maintainer records and #64 closes; (2) **the branch is
> renamed to `release/v0.1.0` before push** (main accepts merges only from
> `release/*` or `hotfix/*`). The same comment assesses the open added
> issues (#110, #112, #115, #118) as skip-safe and records the conscious
> deferral of #60/#61. The fallback-hosting pattern lives only in draft
> PR #124 — `docs/demo/video-script.md` does not exist on `develop`
> (verified: `ls` fails) — so it is referenced everywhere as the text
> "PR #124", never as a repo path that would 404 in the v0.1.0 tree.

## What changed

- **`CHANGELOG.md` (new)** — first release entry `## [0.1.0] - 2026-11-02`
  in Keep a Changelog style (header conventions + `### Added` + link
  reference to the GitHub Release):
  - *Added* — the M0–M4 procure-to-pay capability set summarized from the
    README capability-to-evidence matrix (README.md:163–202) and dev-notes
    013–063: platform/delivery, identity/RBAC/tenancy, vendor master,
    requisitions & approvals, PO & GRN, invoice & 3-way match, async
    platform & jobs, documents/history/audit, web experience,
    observability & performance, demo & docs.
  - *Known gaps / deferred* — honest section: #60 and #61 deferred by
    maintainer decision; #64 video URL pending recording (MP4 +
    `demo-captions.en.srt` ship as GitHub Release assets — the
    fallback-hosting pattern of PR #124, referenced as text, not a path);
    #65 tunnel runbook missing; #110/#112/#115/#118 open and skip-safe;
    smaller per-capability pendings (Redis read-model cache, cancel
    REST/UI surface, notifications UI, email stub, k8s D4) left pointed
    at the README matrix instead of restated.
- **`docs/dev-notes/66-release-v0-1-0.md` (new)** — this note.

No other file is touched: README, the video package, compose/Makefile/
deploy/scripts/seed belong to #64/#65/#62/#63 scope, not #66.

## Decisions

- **Release date recorded as 2026-11-02** — the date of the maintainer
  go-ahead decisions and of the M5 artifact work (dev-notes 063, PR #124),
  as pinned by the implementation plan. If the maintainer prefers the
  calendar date the tag is pushed, this is a one-line edit before merge.
- **"PR #124" is cited as text, never as a repo path.** The
  fallback-hosting section exists only inside draft PR #124; linking
  `docs/demo/video-script.md` from CHANGELOG or release notes would 404
  in the v0.1.0 tree. `docs/demo-script.md` (which does exist on
  `develop`, verified) is safe to link and is linked.
- **Video handling in the release notes** (for the script's
  `gh release create v0.1.0`): the notes link the README and the
  `docs/demo-script.md` runbook, and state that the MP4 +
  `demo-captions.en.srt` attach as assets of the release itself with the
  chapter list pasted into the notes (the fallback-hosting pattern of
  PR #124), with the final YouTube-unlisted URL swapped in via a small
  commit once #64 closes — the release does not wait on the recording.
- **Merge strategy: merge commits, not squash.** CONTRIBUTING.md:43 —
  "Release merges keep a merge commit". Both the release PR
  (`release/v0.1.0` → `main`) and the back-merge (`main` → `develop`) are
  merged with an explicit `gh pr merge --merge`. Squashing the back-merge
  would synthesize a commit that duplicates CHANGELOG/dev-note while
  leaving both trees identical — it would pass a diff-empty check and
  still break the Git Flow history model, so it is prohibited explicitly.
- **Issue closure rides the back-merge PR.** The repo default branch is
  `develop` (verified: `gh repo view --json defaultBranchRef`), so
  "Closes #66" only auto-closes when the back-merge PR (base `develop`)
  merges; the release PR (base `main`) therefore uses "Part of #66". If
  auto-close does not fire, close manually with a completion comment.
- **Git Flow is procedural, not technical, right now:** both `main` and
  `develop` return 404 "Branch not protected" from the branch-protection
  API (verified this session) — so the discipline is: never push directly
  to either branch; annotated tag `v0.1.0` (the repo's first —
  `git tag -l` is empty) is created on the release merge commit on `main`
  after the release PR merges, then pushed.
- **Milestone M5 is not closed here** — closing the GitHub milestone is a
  maintainer action; recorded as a follow-up.

## Evidence

All checks run this session on `feature/66-release-v0-1-0`
(clean tree except untracked `.zcode/`):

- `test -f CHANGELOG.md` → **missing before this PR**; `git status` after
  writing → exactly `CHANGELOG.md` + `docs/dev-notes/66-release-v0-1-0.md`
  new, nothing else modified.
- `ls docs/demo/video-script.md` → "No such file or directory";
  `test -f docs/demo-script.md` → exists (the linked runbook is real).
- `gh issue view 66 --comments` → both maintainer decisions read:
  release runs now with the GitHub Release video fallback + URL swap
  after #64; rename branch to `release/v0.1.0` before push; #110/#112/
  #115/#118 skip-safe; #60/#61 consciously deferred.
- `gh pr list --state open` → only draft PR #124
  (`feature/64-demo-video-en-subtitles`); `gh pr view 124` + `gh pr diff
  124` → §Fallback hosting read verbatim (attach MP4 + SRT as GitHub
  Release asset, chapter list into release notes).
- `gh repo view --json defaultBranchRef` → `develop`.
- `gh api repos/luksluki/eprocurement` → `allow_merge_commit: true`.
- `gh api repos/.../branches/main/protection` and `.../develop/protection`
  → 404 "Branch not protected" for both.
- `git fetch origin && git rev-list --count origin/main..origin/develop`
  → 53 (the release carries 53 commits to `main`); `git tag -l` → empty
  (v0.1.0 is the first tag).
- `grep -c -i -E "release|tag|v0\.1\.0" docs/prd/prd-v0.2.md` → **0**
  matches (the release contract is issue #66 + CONTRIBUTING.md + README,
  which is what this note follows).
- Gates (docs-only change, full set still run per AGENTS.md step 5):
  - `go vet ./...` → clean; `go build ./...` → clean.
  - `go test ./...` → **40 packages ok, 0 FAIL** (DB-dependent
    integration tests auto-skip locally without `TEST_DATABASE_URL`;
    CI runs them).
  - `pnpm install --frozen-lockfile` → up to date, 5 workspace projects,
    pnpm v12.8.1.
  - `pnpm build` → turbo: **3/3 tasks successful**;
    `pnpm lint` → **4/4 successful**; `pnpm test` → **6/6 successful**.
  - The workflow script re-runs the full gate set after review before
    committing.
- Not run here (out of this step's scope, executed by the workflow after
  review): branch rename, push, release PR → `main`, CI wait, merge-commit
  merge, annotated tag `v0.1.0`, GitHub Release with video-fallback notes,
  back-merge PR → `develop` carrying "Closes #66", issue-closure check.

## Follow-ups

- **#64** — record + upload the video, attach MP4 + `demo-captions.en.srt`
  to Release v0.1.0, swap the YouTube-unlisted URL in with a small commit.
- **#65** — live-demo tunnel runbook (last open M5 artifact besides the
  video).
- **#60 / #61** — resilience and load tests consciously deferred past
  v0.1.0.
- Maintainer actions: close milestone **M5** on GitHub; optionally close
  the Project board column. Not done by the agent.
- **#110 / #112 / #115 / #118** — open product/testability follow-ups
  assessed skip-safe for this release.
