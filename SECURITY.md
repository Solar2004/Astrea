# Security model

**Astrea grants an AI agent full control of the machine the agent runs on: it
can run arbitrary commands, read and write any file, move the mouse, type, and
mount new HTTP endpoints. This is by design.** Treat an exposed agent exactly
like an unrestricted remote shell on that machine.

## Threat model & layers

Astrea is intended to run over a **private Tailscale tailnet**, never on the
public internet. Defense in depth, outermost first:

1. **WireGuard transport (Tailscale).** All traffic is encrypted and the peer
   set is restricted to your own devices. The agent should bind to its tailnet
   IP (e.g. `100.x.y.z:7700`), not `0.0.0.0`, so it is never reachable off-net.
2. **Bearer token.** Every control-channel request and capability query
   requires `Authorization: Bearer <token>`, compared in constant time. The
   installer generates a 256-bit token per machine.
3. **Authorization guard.** Optional `allow`/`deny` method lists in the agent
   config restrict which capabilities are callable (`"task.*"`-style
   wildcards supported). AI-defined tasks are checked per step, so a macro
   cannot reach a capability the guard denies.
4. **Kill switch.** `admin.lock` denies every non-admin capability instantly;
   `admin.stop` shuts the agent down. Both are callable by the operator through
   Hermes.
5. **Audit log.** Every invocation — including each step of a task — is
   appended as JSON to `audit.log` in the agent data directory (method,
   arguments truncated to 2 KiB, timestamp, success/error).

## Operator responsibilities

- Keep the token secret. It is written to `config.json` (mode 0600) — do not
  commit it or paste it into logs.
- Do **not** bind the agent to a public interface or forward a port to it.
- The `/dyn/<name>` dynamic endpoints are served **without** the control token
  so local tools can consume Hermes-built APIs; anyone on the tailnet can reach
  them. Only create endpoints you are comfortable exposing tailnet-wide.

## Reporting

This is a personal project. If you find a vulnerability, open a private security
advisory on the GitHub repository.
