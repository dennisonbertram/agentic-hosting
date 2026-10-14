# OpenRouter calculator agent

This is a small, stateless Node 24 service that demonstrates one authenticated
chat turn using the pinned OpenRouter model `anthropic/claude-sonnet-5.5`. It
uses Node's built-in HTTP, fetch, and test APIs; it has no runtime dependencies,
database, file writes, or dynamic code execution. The only tool is a bounded
function that adds two finite numbers.

## Run locally

Set `DEMO_AUTH_TOKEN` to a private random token of at least 32 characters.
For local direct-provider testing, also set `OPENROUTER_API_KEY` to an
OpenRouter API key. The service does not print either value. Then run:

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

In relay mode, set `PROVIDER_RELAY_TOKEN` instead of `OPENROUTER_API_KEY`. The
service sends its fixed OpenRouter-compatible request to
`http://openrouter-demo-relay:8080/api/v1/chat/completions` using that token.
It rejects a configuration that also supplies `OPENROUTER_API_KEY`; the actual
provider key belongs only in the relay service. This URL is fixed in the app,
not caller-configurable. The relay must be attached to a network where the
`openrouter-demo-relay` service name resolves.

If the gVisor app cannot resolve that Docker alias, an operator may set
`PROVIDER_RELAY_IP` to the relay container's current IPv4 address. The app
accepts only `172.20.0.0/14` addresses whose final octet is 2 through 254, and
still fixes the port to `8080` and the path to `/api/v1/chat/completions`.
This setting requires `PROVIDER_RELAY_TOKEN`, is not available to chat callers,
and cannot select a hostname, URL, or alternate port. Verify the relay
container's current address before configuring it because the address may
change after recreation. This workaround addresses Docker alias resolution
only; the observed app-to-relay health check does not prove a live model
completion.

The included Dockerfile uses the official Node 24.15.0 Alpine multi-platform
image pinned by manifest digest and runs as the image's unprivileged `node`
user. It works with a read-only root filesystem because the app writes no
files. Build it from the repository root with
`docker build -f examples/openrouter-agent/Dockerfile -t openrouter-agent:local examples/openrouter-agent`.

## Hosting limitation

This example is prepared for local testing. The current agentic-hosting tenant
network blocks outbound provider connections, so a deployed instance cannot
reach OpenRouter yet. Do not present it as a working hosted agent until a
narrow, reviewed provider-egress path is implemented and a real authenticated
request has been observed. The automated tool round-trip uses a mock provider;
it makes no paid model request.
