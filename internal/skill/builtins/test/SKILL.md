---
name: test
description: Discover and run the relevant tests, then report evidence and remaining failures.
allowed_tools:
  - Read
  - Glob
  - Grep
  - Bash
mode: isolated
history: 1
---

Test the requested scope independently: {{args}}

Follow this workflow:

1. Inspect the repository and changed files to identify the actual language, build system, and closest relevant test targets.
2. Start with focused tests that give fast, specific feedback. Expand to broader tests only when justified by the affected surface.
3. Capture the commands actually run and their exit results. Do not hide, reinterpret, or claim success for tests that did not execute.
4. When a test fails, distinguish product failures from environment or dependency failures and provide the most useful concise evidence. Do not edit implementation or tests in this independent run.
5. Return a self-contained final summary listing passed commands, failed commands, and any meaningful coverage gaps.
