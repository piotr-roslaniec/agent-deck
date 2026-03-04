# Remote Session Security Model

This document describes the security boundaries and threat model for Agent Deck remote session features:
- `--host <name>` and `--host auto`
- `session adopt`
- Remote tmux probing and attach flows

## Scope and Security Boundary

Agent Deck runs as a local process and uses SSH to interact with remote hosts.
The primary trust boundary is between:
- Local machine state (`~/.agent-deck`, local tmux sessions, local web server)
- Remote host sessions (tmux state and shell output over SSH)

Key assumptions:
- You trust SSH server identities configured on your machine.
- You trust the remote host account you connect to.
- Remote hosts can still send untrusted terminal output, so output handling should be treated as untrusted input.

## Localhost-Only Services

Agent Deck services are designed to be loopback-only by default:
- Web UI defaults to `127.0.0.1:8420`.
- Claude SDK bridge binds to `127.0.0.1` on an ephemeral local port.
- Websocket connections are origin-checked against the serving host.

Implication:
- The default exposure is local-user only, not LAN/public network.
- If you override bind addresses, you expand exposure and should use explicit network controls.

## SSH Control Socket and Command Security

Remote session operations use SSH control sockets and conservative SSH flags:
- Control socket directory is created with restrictive permissions (`0700`).
- SSH control sessions use `ControlMaster=auto` and `ControlPersist=600`.
- Commands are run with `BatchMode=yes` and connection timeouts.

Host value hardening:
- `hosts.*.ssh_host` is validated when `config.toml` loads.
- Unsafe host formats (including option-injection-like patterns) are rejected.

Operational note:
- Remote command wrappers use shell quoting for command payloads, but host validation is still required to reduce injection and option confusion risk.

## Config File Protection Requirements

Agent Deck config path:
- `~/.agent-deck/config.toml`

Security requirements:
- Recommended file mode: `0600`
- Recommended directory mode: `0700`

Runtime behavior:
- Agent Deck warns at startup if `config.toml` is more permissive than owner-only.
- Config writes use atomic temp-file replace and write with restrictive mode.

Why this matters:
- Host definitions and remote defaults influence SSH destinations and automation behavior.
- World/group-readable config can leak infrastructure metadata and targeting details.

## Threat Model for Remote Adoption

`session adopt` imports an existing remote tmux session into local dashboard state.

Primary threats and mitigations:

| Threat | Mitigation | Residual Risk |
|---|---|---|
| Malicious `ssh_host` config value | Startup validation of host format; invalid values rejected | Compromised trusted host alias can still redirect if operator already trusts it |
| Remote command injection via adoption flow | SSH command construction uses explicit command structure and quoting | Remote tmux content remains untrusted data |
| Accidental destruction of remote session when deleting adopted entry | Delete path for adopted sessions removes local reference, does not kill remote tmux | Operator still needs to manage remote tmux lifecycle intentionally |
| Overexposure of local control interfaces | Local services default to loopback-only binds | Manual bind override can increase attack surface |
| Credential/config leakage | Permission checks and restrictive write modes for config/sockets | Local machine compromise bypasses these controls |

Additional boundary note:
- Workdir/status detection may capture remote pane output. Treat remote terminal output as untrusted content crossing into local process memory.

## Hardening Checklist

1. Keep `~/.agent-deck/config.toml` at `0600` and `~/.agent-deck` at `0700`.
2. Prefer SSH host aliases from `~/.ssh/config` instead of ad-hoc host strings.
3. Keep SSH host key verification enabled and maintain known_hosts hygiene.
4. Do not bind web features to non-loopback interfaces unless required.
5. Use dedicated low-privilege remote accounts for adopted/probed sessions.
6. Review remote host list periodically and remove unused entries.
7. Treat adopted remote sessions as externally controlled state.

## Quick Audit Commands

```bash
# Config permissions
ls -ld ~/.agent-deck
ls -l ~/.agent-deck/config.toml

# Lock down permissions
chmod 700 ~/.agent-deck
chmod 600 ~/.agent-deck/config.toml

# Verify configured hosts
agent-deck host list
```
