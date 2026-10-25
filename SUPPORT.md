# Support Policy

## Supported release line

| Version line | Status | Notes |
| --- | --- | --- |
| `master` (source install) | Supported | Has every fix and security protection |
| `v3.3.x` | Supported | Current release line; includes all protections described in [SECURITY.md](SECURITY.md#release-status--发布状态) |
| `v3.2.x` | Unsupported | Lacks the protections added after v3.2.0, see [SECURITY.md](SECURITY.md#release-status--发布状态); upgrade to v3.3.0 |
| `< v3.2.0` | Unsupported | No backports |

## What support means

- Critical bug fixes
- Security fixes
- Release pipeline reliability fixes

Feature work is prioritized on the latest release.

## Upgrade recommendation

Always upgrade to the latest patch release in the active line.

## End of support

When a new minor line is published (for example `v3.3.x`), previous minor lines
may be marked unsupported.
