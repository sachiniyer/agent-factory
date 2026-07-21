# tmux teardown guardrail

This hook is a best-effort safety interlock. Its primary job is to stop the
direct mistake that caused the original incident: a bare `tmux kill-server`
against the host's shared server. It recognizes common shell-mediated forms and
denies unmodeled tmux verbs even when they target a scoped server.

It is not a security boundary. A developer session can invoke shells and tools
that load commands or code from files, configuration, hooks, plugins,
environment variables, and future options. A string-level guard cannot fully
model those execution surfaces, and an allow decision must not be treated as
proof that a command is safe.

Accordingly:

- a denial means the command matched a known hazard or was outside a modeled
  guardrail shape;
- an allow means only that no currently modeled hazard was found;
- reviewers and operators must not relax a real isolation control because this
  hook allowed a command.

The actual security boundary is containment: an agent that cannot reach the
host tmux socket cannot destroy the shared server regardless of which shell or
developer-tool escape hatch it finds. That work is tracked in
[#2194](https://github.com/sachiniyer/agent-factory/issues/2194). Until it lands,
this hook reduces accidental risk but does not eliminate it.
