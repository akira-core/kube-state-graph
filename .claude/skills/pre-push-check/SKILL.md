---
name: pre-push-check
description: Run the full local CI mirror (vet, lint, vuln, test, docs drift, mocks drift, containment) and report each gate. Use before pushing a branch.
disable-model-invocation: true
---

# Pre-push check

Run every gate the GitHub Actions pipeline (`.github/workflows/ci.yml`) runs,
plus `go vet`, and report each one separately. `make ci` stops at the first
failure; this skill does not, so one run shows every broken gate.

## Steps

1. Run each gate from the repository root as its own command, continuing past
   failures. Run them in this order, cheapest first:

   | Gate | Command | CI job |
   |---|---|---|
   | vet | `make vet` | — |
   | route containment | `make check-route-containment` | route-containment |
   | parser containment | `make check-parser-containment` | check |
   | lint | `make lint` | lint |
   | vuln | `make vuln` | vuln |
   | mocks drift | `make verify-mocks` | mocks-drift |
   | docs drift | `make check-docs` | docs-drift |
   | test | `make test` | test |

   `make verify-mocks` and `make check-docs` regenerate files before diffing.
   If the working tree was clean before the run and a drift gate fails, the
   regenerated files are the fix — leave them in place and say so.

2. Integration tests need Docker and skip themselves without it. Note in the
   report whether Docker was available, since a local pass without Docker
   does not cover `internal/integration/`.

3. Report a table of gate → pass / fail. For each failure, quote the relevant
   output and name the fix (`make mocks`, `make docs`, the lint finding, the
   vulnerable module and fixed version, the failing test).

Do not fix anything, commit, or push unless asked. This skill only reports.
