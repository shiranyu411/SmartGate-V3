# Contributor conventions

- Keep changes focused; use `feat:`, `fix:`, `test:`, `docs:`, `chore:`, `refactor:` or `ci:` commits.
- Update `docs/API_CONTRACT.md` in the same change when modifying an API.
- Follow `docs/ARCHITECTURE.md` for state and Worker ownership boundaries.
- Run the relevant backend/frontend checks before publishing source changes.
- Device releases require `make release-gate`, an isolated rehearsal, a private backup and the guarded cutover scripts.
- Never commit credentials, subscription URLs, live configurations, personal device details, raw evidence, backups or VM material.
- Never expose the management API to WAN or overwrite a running daemon directly.
- Keep generated output, caches and dependencies ignored.

This public repository contains an independent source snapshot. Deployment facts and private development history are not included.
