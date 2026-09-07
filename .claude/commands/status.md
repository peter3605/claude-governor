---
description: Show where every project stands in Launch Control
---

Report the state of the backlog.

1. Read `.claude/launch-control.json` for the Notion URLs.
2. Query, in **view mode**: **In Progress**, **PRs waiting on you**, **Waiting on someone else**, **Ready for Claude Code**, and **Your turn**.
3. Report, briefly:
   - Anything **In Progress**, and whether this session is bound to one (`.claude/.current-story`).
   - Anything **In Review** — a green PR sitting unmerged. Give the PR link from **Last session**.
     concertFinder never auto-merges, so its PRs always land here and wait for Peter.
   - Anything waiting on a third party, with its lead time — these are where waiting costs real days.
   - The next three **Ready** stories for THIS repo, by Seq.
   - The next two items from **Your turn** — the things only Peter can do.
   - Launch blockers remaining vs done for this project, as one line.

If the user passed a project key, report on that project instead of this repo.
