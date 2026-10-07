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

## Check understanding through ownership

After implementing and explaining a behavior, ask two or three focused questions
and end the turn. The questions should check whether the user understands the
gateway he is building and the code that makes it work. Do not include answers.

- Ground each question in this change. Ask the user to trace a request, predict a
  failure, diagnose a symptom, or adapt the behavior to a concrete requirement.
- Check both the system behavior and an important mechanism in the implementation.
  A useful answer connects what a caller observes to the code that causes it.
- Include the relevant inputs, state, and timing. Provide a short code excerpt or
  precise file reference when needed. Do not make the user guess hidden facts.
- Ask about consequences that require reasoning. Avoid definitions, API-name
  trivia, gotchas, and questions that merely repeat the explanation.
- Choose questions that reveal a meaningful misunderstanding, not questions that
  prove the agent knows more. Allow equivalent correct explanations and fixes.

When the user answers, say what is correct and identify specific gaps using the
code. Explain each gap with a concrete example. Ask a focused follow-up only when
it would resolve a substantial misunderstanding. Do not restart the entire quiz,
repeat equivalent questions, or turn minor imprecision into an endless loop.

Questions support learning; they are not permission gates. If the user explicitly
asks to code the next task, do it even if earlier answers are missing or incomplete.
Carry relevant gaps into the next explanation. If the user asks about the current
code, answer that question before moving on. Do not implement the next task merely
because the user answered the questions.
