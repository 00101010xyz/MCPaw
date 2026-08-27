# Security

MCPaw's threat model, controls, and defensive-programming posture are documented in full
in [`docs/ARCHITECTURE.md` §6 "Security considerations"](docs/ARCHITECTURE.md#6-security-considerations)
— secrets encryption, SSRF egress policy, authentication and session handling, CSRF,
injection defenses, resource exhaustion limits, and browser hardening. Read that section
before relying on this deployment for anything beyond local trial.

## Supported versions

MCPaw does not yet have tagged releases; security fixes land on the default branch.
Track that branch until a release process exists.

## Reporting a vulnerability

Please report suspected vulnerabilities privately rather than as a public issue. Open a
[GitHub security advisory](https://github.com/00101010xyz/mcpaw/security/advisories/new)
on this repository, or, if that is not available to you, contact the maintainer directly.
Include:

- The affected component and, if known, the file/function.
- Steps to reproduce, or a proof of concept.
- The impact you believe it has (what an attacker gains).

We'll acknowledge reports and aim to fix confirmed issues promptly; there is no bug bounty.

## A few things this is *not* protection against

- **A compromised master key.** Anyone holding `MCPAW_MASTER_KEY` (or the generated
  `master.key` file) can decrypt every stored credential. Treat it like any other root
  secret — a proper secret store, not a checked-in file.
- **An administrator account you don't trust.** Any admin can read connector manifests,
  configure egress policy, and issue tokens scoped to any instance. MCPaw authorizes
  *actions*, not intent.
- **Vulnerabilities in an upstream API you connect to.** MCPaw guards the request path
  (SSRF, injection, resource limits) but cannot vet what a third-party API does with a
  well-formed, correctly authorized request.
