---
name: commit
description: Inspect the current changes, verify them, and create a focused Git commit.
allowed_tools:
  - Read
  - Glob
  - Grep
  - Bash
mode: shared
---

Create a safe, focused Git commit for the current work. Use the user's arguments as intent or commit-message guidance: {{args}}

Follow this workflow:

1. Inspect `git status`, the relevant staged and unstaged diffs, and recent commit-message style. Preserve unrelated user changes.
2. Identify the files that belong to the requested change. Do not stage secrets, generated artifacts, unrelated edits, or files you have not reviewed.
3. Run the smallest relevant verification available for the changed code. Report and resolve failures that are in scope; do not claim tests passed unless they ran successfully.
4. Stage only the intended files and create one concise commit whose message explains the completed outcome.
5. Confirm the resulting commit identifier and summarize verification. Never amend, force-push, reset, or discard changes unless the user explicitly asks.
