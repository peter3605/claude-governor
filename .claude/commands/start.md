---
description: Claim a Launch Control story and bind this session to it
argument-hint: <STORY-ID>
---

Bind this session to story **$ARGUMENTS**.

1. Read `.claude/launch-control.json` for the Notion URLs.
2. Fetch the story from Notion by its Story ID (search the Stories database, or query the data source filtered on `Story ID`). If it does not exist, say so and stop — do not invent one.
3. Check its **Depends on**. If any dependency is not Done, warn clearly and ask whether to proceed anyway before doing anything else.
4. Set the story's **Status** to `In Progress` via `update_page`.
5. Write the story ID to `.claude/.current-story` (one line, just the ID) and remove `.claude/.nudged` if present.
6. Create a working branch if the repo convention calls for one, named with the story ID (e.g. `mun-01-run-full-suite`).
7. Read out the story's **Done when** and **Notes and traps** verbatim, then state your plan for satisfying the Done-when criteria.

Every commit in this session should carry the story ID in its subject line, e.g. `MUN-01: run the full suite at HEAD`.
