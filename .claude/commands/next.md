---
description: Pull the next story to work on from the Launch Control backlog in Notion
---

Pick up the next piece of work for this project.

1. Read `.claude/launch-control.json` for the project key, the story-ID prefix, and the Notion URLs.
2. If `.claude/.current-story` exists and is non-empty, a story is already bound. Say which, and stop — offer `/done` or an explicit `/start <other-id>` rather than silently switching.
3. Query the **Ready for Claude Code** view (`views.ready`) with `query_data_sources` in **view mode**. View mode is unquota'd on this plan; SQL mode is metered, so do not reach for it.
4. Keep only rows whose **Story ID** starts with this repo's prefix, unless the user named another project.
5. Take the first row by **Seq**. `Status = Ready` already means unblocked — `/done` promotes dependents from Backlog to Ready as their blockers close — but verify before starting: if the row's **Blocked by** relation is non-empty, fetch each linked page and confirm every one is Done. If any is not, that is a bug in the board: fix the story's Status to Backlog, say so, and move to the next candidate.
6. Present the story: Story ID, name, Estimate, **Done when**, and **Notes and traps** in full. Read the traps out — that field is where the expensive mistakes are recorded, not decoration.
7. Ask before starting. On confirmation, run the `/start` flow for that story.

An argument narrows the candidates first — a project key (`muncheez`), an epic (`Store listing`), or an estimate (`XS`).

This command only ever returns work a session can actually do. For the tasks that need you — an Xcode build, a login, a legal filing — run `/mine`.
