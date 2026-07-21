# tmux teardown guard

This hook is a best-effort safety interlock. Its primary job is to stop the
direct mistake that caused the original incident: a bare `tmux kill-server`
against the host's shared server. It also recognizes a number of common shell
and program-mediated forms so accidental indirection is harder.

It is not a security boundary. A developer session can invoke programs that
load commands or code from source files, Makefiles, repository configuration,
hooks, plugins, pagers, editors, transports, environment variables, and future
options. Completely modeling those semantics would mean reproducing a large
part of the developer toolchain, and an incomplete allowlist must not be
treated as proof that an allowed command is safe.

Accordingly:

- a denial means the command matched a known hazard or was outside the guard's
  approved best-effort shapes;
- an allow means only that no currently modeled hazard was found;
- a literal script, package, or source path is a compatibility shape, not file
  provenance: the guard neither reviews file contents nor tracks earlier writes;
- findings should improve the speed bump when the cost is reasonable, but a
  new bypass is not evidence that host isolation has been defeated;
- operators and reviewers must not relax around an allow decision.

The actual security boundary is containment: an agent that cannot reach the
host tmux socket cannot destroy the shared server, regardless of which shell
or developer-tool escape hatch it finds. That work is tracked in
[#2194](https://github.com/sachiniyer/agent-factory/issues/2194). Until it lands,
this hook reduces risk but does not eliminate it.
