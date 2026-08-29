# Subagent model routing

Orchestration, planning, and final judgment stay in the top-level session (best model class); discrete work is distributed to the cheapest model class that does the task well, via the agents defined here:

- `scout` (Haiku) — bulk mechanical sweeps, inventories, usage searches. Fan out in parallel.
- `implementer` (Sonnet) — scoped, well-specified implementation with a clear definition of done.
- `analyst` (Opus) — deep single-topic reasoning: root-cause, performance, design trade-offs.
- `verifier` (Opus) — adversarial verification of findings and fixes; escalate to the session model for security-critical verification.

For ad hoc spawns that don't fit a named agent, pass the Agent tool's `model` override using the same mapping. Don't run execution-heavy work in the orchestrating session when a subagent can carry it.
