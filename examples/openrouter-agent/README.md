# OpenRouter calculator agent

This is a small, stateless Node 24 service that demonstrates one authenticated
chat turn using the pinned OpenRouter model `anthropic/claude-sonnet-5.5`. It
uses Node's built-in HTTP, fetch, and test APIs; it has no runtime dependencies,
database, file writes, or dynamic code execution. The only tool is a bounded
function that adds two finite numbers.

## Run locally

Set `OPENROUTER_API_KEY` to an OpenRouter API key and `DEMO_AUTH_TOKEN` to a
private random token of at least 32 characters. Keep both values server-side.
The service does not print either value. Then run:

```sh
npm test
npm run build
npm start
```

The server listens on `0.0.0.0:3000` by default, or on the integer port in
`PORT`. `GET /health` is unauthenticated. `POST /chat` requires
`Authorization: Bearer <DEMO_AUTH_TOKEN>` and a JSON body such as
`{"message":"Add 19 and 23, then explain the result."}`. The reply includes
the model's short answer and the local calculator result. Requests are limited
to 4 KiB and provider calls to 25 seconds; provider errors return a generic
502 response.

For Nixpacks, the package declares Node `24.x`, has no install dependencies,
and supplies `build` and `start` scripts. Configure the two environment values
as encrypted service variables. Do not place the OpenRouter key in client code
or send it to callers.

## Hosting limitation

This example is prepared for local testing. The current agentic-hosting tenant
network blocks outbound provider connections, so a deployed instance cannot
reach OpenRouter yet. Do not present it as a working hosted agent until a
narrow, reviewed provider-egress path is implemented and a real authenticated
request has been observed. The automated tool round-trip uses a mock provider;
it makes no paid model request.
