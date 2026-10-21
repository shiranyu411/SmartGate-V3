# SmartGate frontend

React 19 + TypeScript + Vite + Tailwind management UI (“书房” visual language).

## Develop

```sh
npm install
npm run dev
```

Point the dev proxy/API base at a running SmartGate daemon (see `docs/API_CONTRACT.md`). Production builds are same-origin with the daemon static file server.

## Build

```sh
npm run build
# output: frontend/dist → packaged into release as www/smartgate
```

## Test / lint

```sh
npm run test
npm run lint
```

## Rules

- Do not hand-edit `frontend/dist/`.
- API shapes live in `docs/API_CONTRACT.md`.
- Auth token key: `localStorage["smartgate_token"]`.
