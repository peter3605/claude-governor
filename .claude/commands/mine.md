---
description: Show the launch blockers only Peter can do, longest lead time first
---

Report the work that a Claude Code session **cannot** do — the tasks that need a human at a keyboard, a credit card, a login, or a third party. These are the ones that actually gate every launch, and nothing else in this system surfaces them.

1. Read `.claude/launch-control.json` for the Notion URLs.
2. Query the **Your turn** view (`views.yourTurn`) in **view mode**.
3. Split the results into two groups and report them in this order:

   **Clocks — start these, then walk away.** Anything with `Gating = External`. These have a lead time you cannot compress, and every day one sits unstarted is a day added to the end of the project. Show the Story ID, the name, the **Lead time**, and whether anything is still blocking it.

   **Your desk — things you can finish today.** Anything with `Gating = Self-serve` whose blockers are all Done. Order by Estimate, smallest first, so the quick ones are visible.

4. Then say plainly which single item, if started today, buys back the most calendar time. Do not hedge across five things — name one.

If the user passed a project key, narrow to that project. Otherwise report across all projects, because the long-lead items rarely live in the repo you happen to be sitting in.

Do not offer to do these tasks. Some are physically impossible from a terminal, and pretending otherwise wastes the user's time. Where a task has a preparable part — a draft, a checklist, a script, an answer to a form question — offer that specifically.
