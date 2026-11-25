# Security policy

CallStork sits at the edge of a phone system: it is the address a carrier or
other SIP peer sends calls to, and it decides which PBX gets each one. A flaw
can mean calls placed by someone who shouldn't be able to (toll fraud), calls
sent to the wrong place, or a phone system that stops taking calls. Please
report anything like that privately.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting: the repository's **Security**
tab › **Report a vulnerability**
(<https://github.com/chrisnock-personal/callstork/security/advisories/new>).
Only the maintainer sees the report.

Please don't open a public issue, pull request or discussion for it.

Useful to include: the commit, the config (with real numbers and addresses
removed), and the SIP messages that show the problem. You'll get a reply within
a week. Once a fix is merged, the advisory is published with credit, unless
you'd rather not be named.

Examples of what's in scope:

- getting a call routed from a source that no peer's `allow` list covers, or
  with a peer's `match_field` that should not apply to it;
- crafted SIP (headers, URIs, sizes, retransmissions, CANCEL/ACK races) that
  crashes or wedges the leaf, or makes it route a call it shouldn't;
- header or Request-URI injection through rule captures and rewrites;
- anything in the config checker that accepts a config which then behaves
  differently from what it reported.

## Supported versions

The project is in Phase 0: a Kamailio reference configuration and a test
harness, with the standalone program to come. Fixes go into `main` only.

| Version | Supported |
|---|---|
| `main` | Yes |
| Anything older | No |

## How it's built

- **Allowlist first:** a request is matched to a peer by its source address
  before anything else; everything else gets 403. Numbers no rule matches get
  404 without reaching a PBX.
- **A proxy, not a B2BUA:** `From`, `To`, caller ID and SDP are never
  rewritten; the leaf only carries signalling, so it never handles audio.
- **Guards:** `Max-Forwards` is enforced (483) and oversized UDP messages are
  refused (513).
- **CI** runs the unit tests, the full wire-scenario suite against the
  reference in both `Record-Route` modes, and `gitleaks` over the history on
  every push and pull request. Dependabot keeps the workflow actions current.
- **Repository:** secret scanning with push protection, and the default branch
  can't be deleted or force-pushed.

Out of scope: TLS and SIP authentication are not implemented yet (UDP and
IP-based peers only, see #10 and #11), so run the leaf on networks where that
is acceptable. The security of the PBXs, the carrier and the host itself.
