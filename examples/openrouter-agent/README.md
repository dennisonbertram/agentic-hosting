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
not caller-configurable. The normal Docker service alias did not resolve from
the gVisor app during Mac mini testing; the tested setup used
`PROVIDER_RELAY_IP` instead.

If the gVisor app cannot resolve that Docker alias, an operator may set
`PROVIDER_RELAY_IP` to the relay container's current IPv4 address. The app
accepts only `172.20.0.0/14` addresses whose final octet is 2 through 254, and
still fixes the port to `8080` and the path to `/api/v1/chat/completions`.
This setting requires `PROVIDER_RELAY_TOKEN`, is not available to chat callers,
and cannot select a hostname, URL, or alternate port. Verify the relay
container's current address before configuring it because the address may
change after recreation. This workaround addresses Docker alias resolution
only; confirm a live model completion separately as described below.

The included Dockerfile uses the official Node 24.15.0 Alpine multi-platform
image pinned by manifest digest and runs as the image's unprivileged `node`
user. It works with a read-only root filesystem because the app writes no
files. Build it from the repository root with
`docker build -f examples/openrouter-agent/Dockerfile -t openrouter-agent:local examples/openrouter-agent`.

## Mac mini hosted example

The example has completed one live local hosted-model turn on the Mac mini
Lima/ARM64 setup. The app image ran as `node` under `runsc`; the relay also ran
under `runsc` with a read-only root filesystem and no added capabilities. The
authenticated request returned HTTP 200 in 2.60 seconds, answered “19 + 23 =
42,” and recorded the `add_numbers` result `42`. The request used the pinned
model ID above and the fixed OpenRouter Chat Completions endpoint. The
[Mac mini guide](../../docs/ops/macmini.md) covers the Lima VM and platform
setup; the manual recipe below covers this separately managed app/relay setup
and the local request.

This is one successful local request, not evidence of a public ngrok URL,
arbitrary agent compatibility, or a managed app-and-relay lifecycle. The relay
is created separately from the platform-managed app. Its internal link and
relay-only outbound network must be restored if either container or its
network is replaced. Do not attach the app to the relay's outbound network.

The relay uses the gVisor-reachable direct IPv4 address in `PROVIDER_RELAY_IP`
because the Docker service-name alias did not resolve from this gVisor app. The
address is limited to the tenant Docker pool and must be inspected again after
the relay is recreated. The relay's read-only resolver bind mount with
`1.1.1.1` and `1.0.0.1` fixed a DNS failure in this specific VM run; treat it as
an observed workaround, not a general DNS recommendation.

The OpenRouter key belongs only in the relay's read-only
`/run/secrets/openrouter-key` mount. The app receives a separate
`PROVIDER_RELAY_TOKEN`; callers authenticate with a third token via
`DEMO_AUTH_TOKEN`. Keep all three private and do not print them. The current
OpenRouter [Chat Completions API](https://openrouter.ai/docs/api-reference/chat-completion)
requires a Bearer API key and accepts the app's fixed request at
`https://openrouter.ai/api/v1/chat/completions`.

### Manual setup sequence

This describes the existing app service and a separately run relay; it is not
a one-command installer. The relay `docker run` invocation was reconstructed
from the inspected live container settings and was not replayed verbatim on a
fresh VM. Review the flags and inspect the resulting containers before sending
a provider request.

1. Use the committed source already staged in the Lima guest, or archive
   `examples/openrouter-agent` from the intended commit into a private guest
   directory. Build the app with its `Dockerfile` for `linux/arm64`, tag it
   `127.0.0.1:5000/ah/openrouter-agent:<commit>`, and push it to the guest's
   loopback registry. Build `Dockerfile.relay` as a local relay image. Inspect
   both images for `linux/arm64` and user `node`.
2. Copy the OpenRouter key from the Mac's private
   `~/.config/agentic-hosting/openrouter-api-key` file into the guest. Keep the
   guest runtime directory root-owned and mode `0700`; install the key at
   `/root/ah-openrouter-demo/provider-key`, owned by UID 1000 and mode `0400`.
   The relay mounts that file read-only at `/run/secrets/openrouter-key`.
3. Create separate 32-byte random hex files for `DEMO_AUTH_TOKEN` and
   `PROVIDER_RELAY_TOKEN` in the guest's root-only runtime directory. Put the
   latter in the relay env file as `RELAY_AUTH_TOKEN`; its value must match the
   app's `PROVIDER_RELAY_TOKEN`. Keep the provider key only in its mounted file.
   If a generator adds a final newline, strip exactly that one newline when
   loading each token. Keep files private and never print values or request bodies.
4. Inspect existing Docker networks before choosing two unused `/24` subnets
   from the configured `172.20.0.0/14` tenant pool. The QA run used
   `172.20.7.0/24` for an internal app-relay link and `172.20.8.0/24` for a
   normal outbound bridge. It used relay addresses `172.20.7.3` and
   `172.20.8.2`, respectively. Those addresses are examples from that VM and
   may conflict elsewhere.
5. Use `docker create` for the relay with `--runtime=runsc --user=node
   --read-only --cap-drop=ALL --security-opt=no-new-privileges:true`, 256 MiB
   memory, 128 PIDs, its `RELAY_AUTH_TOKEN` env file, and read-only key and
   resolver mounts. Publish no ports and mount no Docker socket. Attach the
   stopped relay to both networks before starting it: connect it to the
   internal link, then the outbound bridge with gateway priority 1. Attach no
   other container to the outbound bridge, then run `docker start` on relay.
6. The tested resolver file contained `nameserver 1.1.1.1`,
   `nameserver 1.0.0.1`, and `options timeout:2 attempts:1`. Bind it read-only
   at `/etc/resolv.conf` in the relay. This fixed DNS for the tested gVisor
   relay; treat it as a specific workaround, not a general resolver policy.
7. Create the app through `POST /v1/services` using the pushed image, port
   `3000`, and environment values `DEMO_AUTH_TOKEN`, `PROVIDER_RELAY_TOKEN`,
   and `PROVIDER_RELAY_IP` set to the relay's current internal-link address.
   The platform runs the app with `runsc`, a read-only root, all capabilities
   dropped, and no-new-privileges. Do not give the app the OpenRouter key or
   attach it to the outbound bridge.
8. After the API reports the service `running`, inspect its container ID,
   connect that app container to the internal link, and restart only that app
   container. The app can retain stale network state until restarted after
   the relay is attached. From inside the app, request
   `http://<relay-ip>:8080/health`; continue only after HTTP 200.
9. From the Mac, send `POST http://127.0.0.1:18080/chat` with
   `Host: calculator-agent.apps.<base-domain>`, an
   `Authorization: Bearer <DEMO_AUTH_TOKEN>` header from a mode-`0600` curl
   config file, and body `{"message":"Add 19 and 23, then explain the result."}`.
   The QA request returned HTTP 200 and the calculator result 42. This local
   route does not test ngrok, public DNS, or public TLS.
10. The platform does not manage the relay container or its extra networks.
    After replacing the relay or either network, inspect its new IP, restore
    the internal and relay-only outbound attachments, update
    `PROVIDER_RELAY_IP`, and restart the app. Remove only the demo service,
    relay container, and named demo networks when cleaning up.

## Hosted deployment status

Failures keep the public response generic and write a small structured record
to stderr with a constant failure code and, where applicable, an upstream HTTP
status or allowlisted finish reason. The record never includes prompts,
credentials, provider response bodies, or raw provider error messages. The
automated tests use a mock provider; the separate single live request above is
the only hosted-model completion verified for this example so far.
