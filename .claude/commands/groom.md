---
description: File a newly discovered item into the Launch Control backlog
argument-hint: <what you found>
---

File **$ARGUMENTS** into the Launch Control backlog.

1. Read `.claude/launch-control.json` for the project page ID, the stories data source, and the story-ID prefix.
2. Decide whether this is a **Launch Blocker** (users cannot be served without it), **Backlog** (better product, does not block shipping), or a **Chore**. Say which and why, in one line.
3. Search the Stories database first. If something equivalent exists, update that story rather than creating a duplicate.
4. Allocate the next free Story ID for this prefix. Launch blockers take plain numbers (`CF-22`); backlog items take the `-B` series (`CF-B12`).
5. Create the page with: Name (short, imperative), Story ID, Project relation, Type, Epic, Seq, Estimate, Gating, **Done when**, **Agent can do this**, and **Notes and traps** if you learned something worth not re-learning. If it depends on other stories, set the **Blocked by** relation to their pages — it is a relation, not text, so pass page URLs.
6. Set **Status** to `Ready` if nothing blocks it, `Backlog` otherwise.
7. Report the new story ID and where it lands in the sequence.

Two things to get right rather than fast. **Done when** must be checkable by someone who was not in this conversation — a command that exits 0, a file that contains a value, a screen that shows a thing. Restating the title is not acceptance criteria. And **Agent can do this** must be honest: uncheck it if the task needs a GUI, a login, a payment, a physical device, or a third party, so it lands in `/mine` rather than being offered to a session that cannot do it.

Estimates: XS under an hour, S 1-3 hours, M half a day, L 1-2 days, XL 3+ days.
