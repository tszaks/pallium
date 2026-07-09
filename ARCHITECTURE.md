# Architecture: one kernel, many services

This is a ruling, not a suggestion — it governs how new capabilities get added to Pallium, including everything after this doc. Agents and contributors extending Pallium should read this before adding a table, a CLI surface, or a cross-cutting capability.

## The ruling

Pallium is one kernel with distinct services on top:

- **Repo intelligence** — index, explain, neighbors, risk, review, handoff
- **Session awareness** — sessions, decisions, handoff
- **Workflows** — the flagship orchestration engine
- **Loops** — bounded, named cycles that tick a workflow repeatedly
- **Agent teams** — peer agents coordinating over a shared task board and mailbox
- **Adoption layer** — how an agent discovers and adopts Pallium (PALLIUM.md, `pallium agents`, `pallium start`)

The kernel is the shared substrate every service sits on: the SQLite store, provider dispatch, worktree/patch machinery, budgets, leases, the run journal. The kernel is never split across services. The services are never blurred into each other.

This does **not** mean separate binaries, separate repos, or separate databases — one install and one database is a deliberate product feature (see the philosophy section below for why). It means enforced boundaries on top of shared machinery, not an unstructured pile of features that happen to share a `main()`.

## Why: modular monolith by intent

Pallium is a modular monolith on purpose: separate but cohesive, modular but together.

- **High cohesion inside each service** — a service knows its own job completely: its own tables, its own lifecycle vocabulary, its own CLI verbs, its own docs section. Nothing about loops' stagnation-counter logic lives in the team code, and nothing about a team's task board lives in loop code.
- **Low coupling between services** — front doors only. A loop runs a workflow through the exact same `workflow run` path a human or a trigger would use, not a private shortcut into workflow internals. A workflow that wants to convene a team goes through the team API, not through team_store.go's own functions directly.
- **One deployable whole** — one binary, one database, one install. The together-ness is a product feature (a single agent invocation, `pallium ...`, reaches every service without juggling connections or processes), not an accident of not having split things up yet.

Practical implications when a judgment call isn't spelled out by a specific rule below:

- If a piece of code needs to know which OTHER service is calling it, that's a coupling smell — push the capability down into the kernel, or pass it through the front door instead.
- If two services need the same helper, it belongs in the kernel, not copy-pasted into both (drift) or imported across (coupling).
- Building a new capability? First question is "which service owns this?" — and if the honest answer is "sort of several," that's either a kernel primitive in disguise, or two features that got designed as if they were one.
- Modularity decays one convenient shortcut at a time, which is why enforcement here is lints and structure, not intentions and good manners. When a shortcut fails loudly (a build-failing test, a compile error from a type boundary), that is the system working as designed, not friction to route around.

## Enforcement (not aspiration — checked mechanically)

1. **A service owns its own persistent state.** Loops introduced this as a build-failing lint (`TestServiceStateOwnership` in `internal/workflow/`, same spirit as the existing `no_hardcoded_provider_test.go` tripwire for provider exec calls): a service's SQLite tables are referenced in a query from exactly one file — that service's own store file (`team_store.go` for `team_*`, `loop_store.go` for `workflow_loops`). A regression back to some other file reaching into a service's tables directly fails this test. Scope is honest, not retroactive: `workflow_runs`/`workflow_agents`/`workflow_triggers`/`workflow_decisions`/`workflow_gates` predate this lint and are legitimately split across `store.go` plus their own files already — extend the lint's map as each of those gets its own cleanup, don't loosen the rule for new tables to match old debt.
2. **Loops never squat on a workflow_runs row.** Milestone 1's most consequential architecture decision: a loop is its own first-class entity (`workflow_loops`) that spawns a FRESH per-tick child workflow run, linked back by `Run.LoopName`, rather than reusing one run across ticks. This was settled over an alternative (one persistent run per loop) specifically because reuse would have required salting the agent-call resume cache with a cycle number to prevent tick N+1 from replaying tick N's stale cached output — and would eventually collide with a workflow run's default 1000-agent LIFETIME cap purely from ticking, independent of how much work any single cycle actually does. `loop status` aggregates a loop's history via `Store.RunsByLoop`, a query that lives in `store.go` (the owner of `workflow_runs`) — loops still never queries that table directly.
3. **Composition through front doors only.** A loop runs a workflow via the exact CLI-shaped composition `workflow trigger run` already established (build a `run` argv, call the same function `cmd/workflow.go`'s dispatcher calls) — not a parallel run-creation path. A workflow that convenes a team goes through the team API. If a service needs a capability another service has, that is a kernel candidate to evaluate, never an excuse to import across.
4. **Shared kernel helpers, not per-service copies.** `openPalliumStore`/`resolvePalliumDBPath` (the `PALLIUM_TEST_DB` safety net) is one function every service's CLI commands call, not a team-flavored copy and a loop-flavored copy independently drifting. `StableHash` (deterministic content hashing) is a single kernel utility used by script-hash change detection, `verify.untilGreen`'s stall signature, AND loops' own stagnation signature — three services, one implementation.

## Extending this

New capability? Ask "which service owns this?" first, before writing any code:

- If the answer is a clean single service, build it there, following that service's own existing conventions (its store file's schema/CAS patterns, its own CLI file, its own docs section in PALLIUM.md's service map).
- If the answer touches the SQLite connection, provider dispatch, worktrees, budgets, leases, or the run journal directly, it's very likely a kernel change — make it in the kernel, exposed to every service uniformly, not duplicated per service.
- If the honest answer is "two services, sort of" — stop and reconsider whether this is actually one feature mis-scoped as two, or a kernel primitive both services should call.

This ruling applied to Loops Milestone 1 and is expected to apply to Agent Teams Milestone 2 and everything after.
