---
description: Verify the story, ship the code through a PR, and tick it off in Notion
---

Close out the story bound to this session.

## 1. Verify

1. Read `.claude/.current-story`. If missing or empty, ask which story this work belongs to — or `/groom` it if it was never on the backlog.
2. Fetch the story from Notion and re-read its **Done when**.
3. **Check the acceptance criteria honestly, one clause at a time.** Run the thing that verifies it — the tests, the script, the command the criteria name. Do not mark something Done because the code was written; mark it Done because you checked and the criteria hold.
4. If any clause is unmet: say which and why, set **Status** to `Blocked` if something external is in the way (otherwise leave it `In Progress`), write what happened into **Last session**, and stop. Do not ship it and do not tick it off.

## 2. Ship it

Read the `git` block in `.claude/launch-control.json` and follow it — the policy differs per repo and the differences are not cosmetic.

**Does this story involve a repo change?** Run `git status --porcelain`. If the tree is clean, there is nothing to ship — skip to step 3. (Plenty of stories are like this: an Xcode build, a dashboard setting, a filing.) Do not invent a commit to have something to push.

Otherwise:

5. **Branch.** `<lowercase story id>-<short kebab slug>`, e.g. `mun-14-app-store-screenshots`. If you are already on such a branch, stay on it. Never commit to `baseBranch` directly.
6. **Commit.** The story ID leads the subject: `MUN-14: shoot the 6.9-inch screenshot set`. Body says what changed and why, not what the files are. Follow this session's own commit-attribution convention for trailers.
   - Review `git status` before staging. Stage deliberately — never `git add -A` in **nightcrew** while a worker is running (`.nightcrew/mcp-*.json` holds a live worker credential), and check the `git.notes` for this repo before staging anywhere.
7. **Push and open a PR** against `baseBranch`. The PR body should carry the story ID, its **Done when** verbatim, and what you did to satisfy it — so the PR is reviewable by someone who has not read the story.
8. **Run any `extraChecks` whose `when` matches** — and read the `when` literally. These are deliberately narrow because they are expensive: muncheez's UI suite is ~10 minutes on a macOS runner, which is roughly ten times the per-minute cost of Linux, against a 2,000-minute monthly allowance. Its `when` says *before the archive*, not *on every PR*. Do not widen it to be thorough.
   - **Minutes are a budget.** Before triggering anything on macOS, or re-running a failed job more than once, say what it will cost and why it is worth it. If a run can wait for the archive, let it.
9. **Wait for CI**, up to `ciTimeoutMinutes`.

## 3. Merge — deliberately, not with `--auto`

**Do not use `gh pr merge --auto`.** It delegates the decision to branch protection, and budgetr has none — `--auto` there merges immediately regardless of what the checks say. Evaluate the rollup yourself:

10. Read the actual conclusions (`gh pr view <n> --json statusCheckRollup`, or `gh pr checks <n>`). Merge only if **every** check concluded successfully.
    - **If zero checks ran, refuse to merge.** An empty rollup is not a pass.
    - **Say which workflows ran, and whether they cover this diff.** Several repos path-filter their workflows, so a green rollup can mean "the checks that test this change never ran." In budgetr a PR touching `scripts/`, `Config/` or `project.yml` passes on gitleaks alone; that is not validation of a Swift or Terraform change. Name what ran before you merge on it.
    - A `skipped` required check is not a pass either. Say which check was skipped and why you think it is safe, or stop.
    - Never merge with `--admin` and never bypass a failing check. If CI is red, fix it or leave the PR open — those are the only two options.
11. **If `autoMerge` is false, or `mergeIsDeploy` is true, stop here even when everything is green.** Merging **concertFinder** deploys to production on the live app; that is Peter's call and his timing, not a session's. Set the story to `In Review`, record the PR URL in **Last session**, and tell him it is green and waiting.
12. Otherwise merge with `mergeMethod` and delete the branch.
13. If CI is still running when `ciTimeoutMinutes` elapses: do not wait longer and do not merge on optimism. Leave the PR open, set the story to `In Review`, record the PR URL, and say so.

## 4. Record

14. Set **Status** to `Done` — or leave it `In Review` if the PR is open per step 11 or 13. A story is not Done while its code is unmerged.
15. Write into **Last session**: today's date, one or two sentences on what was actually done, and the commit SHA and PR number.
16. If the work made a *different* story wrong — a trap discovered, a dependency that was mistaken, a step no longer needed — update that story too. A stale tracker is the problem this replaced.
17. Empty `.claude/.current-story` and `.claude/.nudged` (the mount may forbid deleting them; an empty file reads as unbound).
18. **Only if the story reached `Done`**, find what it unblocks: query the Stories data source filtered on **Blocked by** `relation_contains` this story's page URL. For each dependent, fetch its blockers; if every one is now Done and the dependent is in `Backlog`, promote it to `Ready`. Say which opened up. A story sitting in `In Review` unblocks nothing — its code is not on `main` yet.

## 5. Report

What was done, the PR and its state, what it unblocked, and what the next story would be.
