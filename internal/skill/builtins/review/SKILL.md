---
name: review
description: Review the current change independently and report actionable findings by severity.
allowed_tools:
  - Read
  - Glob
  - Grep
  - Bash
mode: isolated
history: 1
---

Perform an independent code review for this target: {{args}}

Review only; do not modify files, stage changes, or create commits. Inspect the relevant diff and surrounding implementation, then verify concrete concerns with focused read-only commands or tests when useful.

Prioritize correctness, regressions, security, data loss, concurrency, compatibility, and missing tests. Report findings from highest to lowest severity. For every finding, provide a precise file and location, explain the observable failure mode, and suggest the smallest appropriate correction. Avoid speculative style comments.

If no actionable issue is found, say so explicitly and mention any residual risk or verification gap. Your final reply is the review summary returned to the main conversation, so make it self-contained and concise.
