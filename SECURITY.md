# Security policy

## Supported versions

| Version | Support |
|---------|---------|
| 3.x     | Best-effort community |

This project is provided **as-is**. There is no formal security response team.

## Hardening expectations

- Keep the management UI/API on a **trusted LAN**. Default bind is LAN-only; do not publish port 3000 to the Internet.
- Use a strong admin password; change any default/example credentials immediately.
- Treat subscription URLs as secrets.
- Prefer `scripts/deploy-safe.sh` for upgrades so rollback artifacts exist.

## Reporting

If you find a vulnerability, open a GitHub issue **without** exploit details if the impact is high, or contact the repository owner privately if a contact method is listed on the GitHub profile. No bounty program is offered.
