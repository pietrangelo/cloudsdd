---
description: Execute the next unchecked item of an RFC plan under TDD, then hand off
argument-hint: <rfc-number, e.g. 021>
---
Execute exactly one item of the plan for RFC $ARGUMENTS, following `<todo_policy>` and `<tdd_policy>` in CLAUDE.md.

1. **Locate.** If no RFC number was given, find the plans that still have open items with
   `grep -l '^- \[ \]' todo/*.md`. If exactly one matches, use it. Otherwise list the
   matches and stop.
2. **Read narrowly.** Read only `todo/$ARGUMENTS-*.md` and the matching `docs/rfc/$ARGUMENTS-*.md`,
   plus the files the next item names and what they import. Read no other plan.
3. **Gate.** If the RFC's `**Status:**` line does not say Approved, stop and ask:
   "Do you approve this RFC?" If the plan file does not exist yet, stop and say it must be written first.
4. **Pick.** Take the first `- [ ]` item, top to bottom. If it is wrong, blocked or unnecessary,
   amend it in the plan with the reason and stop. Never skip it silently, and never do unlisted work.
5. **Rationale.** Before any code, output a `<DesignRationale>` of two or three sentences naming
   the Rosette dimensions the code satisfies.
6. **Execute.**
   - *Test item* — red: write the test, run it, and confirm it fails for the intended reason
     and no other. The package may stay red until the next item.
   - *Implementation item* — green with the smallest change; then adversarial: break each
     guarantee the tests claim, confirm a test fails with its own diagnostic, and restore the
     file byte-identical; then refactor with the tests green.
7. **Verify.** For an implementation item, run `go test ./... -count=1`, `scripts/coverage-gate.sh`,
   `gosec ./...` and `govulncheck ./...`. Fix what they report. If a tool is not installed,
   say so; don't let silence imply it passed.
8. **Record.** Check off `- [x]` only on a verified result, in one line. Put the reasoning
   behind any decision in the RFC's `## 8. Implementation notes`.
9. **Hand off.** Report what is checked, what is next and what is blocked. End with
   "Item done, next step is `/clear`." Then stop.
