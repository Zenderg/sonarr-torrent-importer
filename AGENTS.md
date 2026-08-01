# Working rules

This document is the source of truth for repository-wide working rules for coding agents. Product decisions and user-facing guidance belong in `docs/` and `README.md`; subsystem-specific implementation guidance should live beside the relevant code when it is introduced.

Work only on the main branch.

Do not create or switch to new branches unless the user explicitly asks for a new branch.

Use subagents at your discretion whenever they are useful.

Use conventional commit prefixes for commit messages so releases can be summarized cleanly: `feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:`, or `build:`.

Do not write tests for everything by default. Write only genuinely useful tests; the goal is verification, not testing for its own sake or covering everything. Before adding a test, first ask whether that location truly needs one.

Implement fallback mechanics only with explicit user permission. Avoid fallbacks when possible because they make debugging harder. If a fallback would be a clear future advantage rather than a likely maintenance problem, stop and ask the user before adding it.

When the user says they found a bug, first investigate and identify the exact problem, then implement a fix only after the user approves it. It is acceptable to ask the user to do something manually or inspect something when you cannot do it yourself.

When working from a GitHub issue, validate that the issue describes a real, current problem before fixing it. If the issue is invalid, already fixed, unreproducible, or outside project scope, comment on the issue with the finding and close it instead of opening a PR.

Record newly discovered project knowledge where future agents can find it. Use README files, focused docs, or concise code comments when appropriate. If the knowledge does not fit an existing document's stated purpose, create a new focused document instead of diluting an unrelated one.

Use Docker Compose to run the application. Do not start the project locally outside Docker Compose. Local commands are fine for tests, checks, scripts, and other tasks that are not application startup.

## Documentation Maintenance

Keep documentation organized by ownership and purpose, not by convenience.

User-facing documents, such as `README.md` and `SECURITY.md`, should stay reader-facing. Do not add agent-only purpose boilerplate there unless it also helps the intended human reader.

Internal and agent-facing documentation should start with a short purpose statement explaining what the document is the source of truth for, what belongs in it, and what should be documented somewhere else.

Before adding information to an existing document, read its opening purpose statement and make sure the new content belongs there.

Create a new focused document when the information introduces a distinct long-lived topic, workflow, subsystem, contract, or troubleshooting area that does not clearly fit an existing document.

Do not append unrelated discoveries to a nearby or familiar document just because it already exists.

Prefer updating an existing document when the new information clarifies, corrects, or extends that document's stated purpose.

Avoid turning docs into chronological work logs. Temporary plans, investigation notes, and shipped implementation details should be removed, condensed, or moved into durable product, architecture, development, or decision documentation as appropriate.

When adding or changing docs, preserve links between related documents so future agents can find the right source of truth.
