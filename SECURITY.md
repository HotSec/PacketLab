# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability in PacketLab, please report it privately so it can be addressed before public disclosure.

**Preferred:** [GitHub Security Advisory](https://github.com/HotSec/PacketLab/security/advisories) — click **Report a vulnerability** and fill out the form. This is the fastest path and lets us coordinate a fix and a CVE if appropriate.

**Alternative:** Open a private issue or email the maintainer directly.

Please include:
- Affected version(s)
- A description of the vulnerability and its impact
- Steps to reproduce (PoC preferred)
- Suggested fix (optional)

## Disclosure Policy

We follow coordinated disclosure:
1. You report the vulnerability privately.
2. We acknowledge within 5 business days.
3. We work on a fix and a patched release.
4. We publish the advisory (and request a CVE) once a fix is available, giving credit to the reporter.

## Security Notes

- The API is authenticated by default: if no `--api-token` / `PACKETLAB_API_TOKEN` is provided, a random token is generated at startup and written to `~/.packetlab/token`.
- State-changing API endpoints require the `X-Requested-With: XMLHttpRequest` header (CSRF protection).
- The API binds to `127.0.0.1` by default; exposing it to the LAN requires explicit `--api-host` and a strong token.

## Supported Versions

| Version | Supported |
|---------|-----------|
| latest (main) | ✅ |
| < 0.1.1 | ❌ (fixed in 0.1.1) |
