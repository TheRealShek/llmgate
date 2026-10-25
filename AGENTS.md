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
- Before adding a new abstraction, check whether it makes the main request path
  easier to understand. Prefer direct code when it only moves complexity elsewhere.
- Keep reusable error definitions in the owning package's `error.go` and use them
  from callers. HTTP handlers use `api.WriteError` for the shared response format.

## Code comments

- Write comments that show how requests move through the system.

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

## Documentation responsibilities

Keep documents small and give each explanation one home. Link to it elsewhere.
Each document has a specific job:

| Document | Owns | Leave elsewhere |
| --- | --- | --- |
| [README.md](README.md) | Project summary, brief current status, minimal quick start, system map, and guide links. | Detailed setup, configuration tables, code walkthroughs, and feature plans. |
| [Architecture](docs/architecture.md) | Implemented request flow, package responsibilities and boundaries, code reading order, state ownership, failure behavior, and cleanup. | Setup commands, configuration defaults, decision history, and planned components. |
| [Manual testing](docs/manual-testing.md) | Setup, configuration reference, operational checks, test commands, expected results, and prerequisites. | Architecture walkthroughs, design rationale, and measurement reports. |
| [Project intent](docs/PROJECT_INTENT.md) | Goals, non-goals, agreed scope, phases, brief current checkpoint, target architecture, and completion criteria. | Session logs, detailed implementation walkthroughs, and measured results. |
| [Decisions](docs/decisions.md) | Agreed non-obvious choices, alternatives, tradeoffs, and reasons. | Repeated feature descriptions, setup instructions, and task progress. |
| [Benchmarks](docs/benchmarks.md) | Measurements, workload and environment, reproduction commands, raw-data references, limitations, and labeled historical results. | General setup instructions, unrelated test procedures, and unmeasured claims. |
| [AGENTS.md](AGENTS.md) | Repository-specific agent workflow, scope, learning goals, and documentation responsibilities. | Language conventions covered by `go-conventions`, product documentation, and progress logs. |

### Keep documents small

- Update existing sections first. Add a document or diagram only for a distinct
  reader question that does not fit its existing home. Avoid separate progress,
  planning, or per-feature documents.
- Summarize only enough to orient the reader, then link to the owning document.
  The README quick start is a short example after setup, not a second setup guide.
- Replace stale explanations and remove superseded duplication. Keep historical
  measurements labeled and identify decisions superseded by later choices.
- Keep package descriptions to responsibility, important boundaries, and a linked
  code entry point. Avoid inventories of files, functions, or types. Keep the code
  reading order focused on one request.
- Use short sections. Let prose explain what diagrams cannot show instead of
  repeating the walkthrough. Preserve failure boundaries, shared versus local
  state, cancellation, and ownership. Do not cut those details to meet an
  arbitrary length limit.

### Update and verify

- Update affected documents in the same change. Check architecture for changes to
  responsibilities, request order, dependencies, trust boundaries, state ownership,
  cancellation, or cleanup. Check manual testing for changes to setup,
  configuration, commands, or expected results.
- Update intent for agreed scope or phase changes, decisions for consequential
  agreed choices, and benchmarks for new or corrected measurements. Update the
  README when its summary, quick start, system map, or navigation changes.
- Describe implemented behavior from the code and relevant tests. Keep planned
  behavior in the intent document and distinguish unverified behavior from checked
  results. Every performance claim needs a measurement and reproduction command.
- Private helper refactors need no documentation change unless they break a code
  link or make an explanation inaccurate. Keep entry-point links current.
- Give each diagram one question and a clear scope. Treat `.tldraw` files as
  authoring sources only; never link readers to them. Export a light-mode image
  with a white background and embed that image in Markdown, refreshing it when
  its TLDraw source changes. Use purposeful connection labels. Keep service
  boundaries separate from handler order, and distinguish cancellation from
  request flow when both appear.
- Before finishing, check affected claims, commands, defaults, and links against
  their sources. Preview changed diagrams for rendering and readability. Report
  checks that could not run.

## Explain the completed change

Start with what the gateway can now do and why it needs that behavior. Give a
reading order through the changed code, then walk one concrete request or failure
through the flow. Connect inputs, state changes, outputs, and cleanup.

Explain the Go APIs and language behavior needed to follow that flow, alongside
the inference backend concepts it uses. Make shared state, cancellation, timing,
and ownership explicit when relevant. Explain the main tradeoff and what the
tests establish. Avoid narrating every line or giving a generic Go lecture.
