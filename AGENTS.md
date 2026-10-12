# AGENTS.md

## Purpose

`llmgate` is an OpenAI-compatible LLM gateway in Go. Read
`docs/PROJECT_INTENT.md` before proposing or implementing work. It defines the
project's scope, phases, request flow, and completion criteria.

Abhishek is learning inference backend engineering by building this project.
The agent writes the code. Help him understand it well enough to explain how
his gateway works, defend its decisions, and reason about failures without the
agent present.

## Scope and decisions

- Follow the current phase in the intent document. Do not implement later phases
  early or expand the task without agreement.
- Use decisions already made in the intent document and `docs/decisions.md`.
  Choose routine implementation details yourself and explain consequential ones.
- Ask when an unresolved choice materially changes behavior, architecture, or
  scope. Give concrete options and a recommendation. Do not manufacture choices
  just to make the user participate.
- Challenge a mistaken design with a concrete failure or tradeoff. Do not assume
  familiarity with Go or inference infrastructure.

## Workflow and reviews

- Follow `go-conventions` for all Go code.
- Do not use the `code-review` skill.
- Do not commit or push unless Abhishek explicitly requests that action. Completing
  a coding task does not authorize a commit. When a push is requested, push to
  `main` unless instructed otherwise.

## Keep the code easy to follow

- Keep one clear construction path for the current gateway. Prefer one constructor
  with explicit dependencies and limits over variants for each development stage.
- Update callers and tests when an internal constructor changes. Do not keep old
  constructors solely to avoid updating tests or preserve obsolete project stages.
- Keep handler layers when they own distinct behavior, such as logging, validation,
  or proxying. Avoid wrappers and helpers that only add another step to follow.
- Make startup show how requests flow through the components. Use names that
  describe each component and separate assembly steps so readers can follow them.
- Keep the README reading guide aligned with the actual entry point and request
  flow. Remove comments that describe features as unfinished after they exist.
- Before adding a new abstraction, check whether it makes the main request path
  easier to understand. Prefer direct code when it only moves complexity elsewhere.

## Code comments

- Write comments that show how requests move through the system.
- When code uses `http.HandlerFunc`, `ReverseProxy`, request clones, context timeouts, or response flushers, state where the request comes from, what changes it, and where it goes next.

## Size each coding task for reading

Implement one small, complete behavior per task, including its necessary tests
and integration. Keep the change focused so the user can read it and trace one
understandable flow. Several functions or files are fine when they serve that
behavior. Do not impose a function or line limit.

If the request is larger, identify the first useful slice and state what remains.
Do not dump a whole feature or leave the slice broken merely to keep it small.
Explain when correctness requires a larger change and agree on its scope first.

Before editing, inspect the existing code and briefly state the behavior being
added, the relevant paths, how it fits the current phase, and how you will check
it. Proceed without asking for approval when the request and scope are clear.

## Project records

Keep measured gateway claims reproducible with a command. Record agreed
non-obvious design decisions in `docs/decisions.md`.

## Explain the completed change

Start with what the gateway can now do and why it needs that behavior. Give a
reading order through the changed code, then walk one concrete request or failure
through the flow. Connect inputs, state changes, outputs, and cleanup.

Explain the Go APIs and language behavior needed to follow that flow, alongside
the inference backend concepts it uses. Make shared state, cancellation, timing,
and ownership explicit when relevant. Explain the main tradeoff and what the
tests establish. Avoid narrating every line or giving a generic Go lecture.
