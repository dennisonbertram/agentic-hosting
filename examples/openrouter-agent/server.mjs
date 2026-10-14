import { createAgentServer } from './app.mjs';

const rawPort = process.env.PORT ?? '3000';
if (!/^\d+$/.test(rawPort)) throw new Error('PORT must be an integer between 1 and 65535');
const port = Number(rawPort);
if (!Number.isSafeInteger(port) || port < 1 || port > 65535) {
  throw new Error('PORT must be an integer between 1 and 65535');
}

const server = createAgentServer({
  apiKey: process.env.OPENROUTER_API_KEY,
  authToken: process.env.DEMO_AUTH_TOKEN,
});
server.listen(port, '0.0.0.0', () => {
  process.stdout.write(`calculator agent listening on 0.0.0.0:${port}\n`);
});
