---
description: Draft a new RFC for a feature and stop for approval
argument-hint: <feature description in natural language>
---
Draft an RFC for this feature: $ARGUMENTS

Follow steps 1–2 of `<workflow_rules>` in CLAUDE.md. Write no source code and no plan file.

1. **Number.** Take the highest `NNN` in `docs/rfc/` and add one. Choose a short kebab-case slug.
2. **Research narrowly.** Read only the RFCs this one depends on and the code it touches.
   Use grep/find to locate, then read ranges.
3. **Write** `docs/rfc/NNN-slug.md`, with a header matching existing RFCs
   (`- **Status:** Proposed`, Author, Date, Depends on) and these sections:
   problem, proposed architecture, impacted JSON schema, security considerations,
   testing plan, rollout (phases the plan will mirror), open questions, and an empty
   `## 8. Implementation notes`. Map each architectural choice back to the Rosette.
4. **Security by construction.** State how the providers enforce hardening, and confirm the
   Specification gains no field that could ask for less security.
5. **Stop.** Summarise the RFC in a few lines, then ask exactly: "Do you approve this RFC?"
   Wait for the answer. After approval, the plan goes in `todo/NNN-slug.md` per `<todo_policy>`.
