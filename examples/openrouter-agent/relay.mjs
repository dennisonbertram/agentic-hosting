import { createServer } from 'node:http';
import { timingSafeEqual } from 'node:crypto';
import { resolve4 } from 'node:dns/promises';
import { readFileSync } from 'node:fs';
import { BlockList, isIP } from 'node:net';
import { request as httpsRequest } from 'node:https';
import { fileURLToPath } from 'node:url';

export const PROVIDER_URL = 'https://openrouter.ai/api/v1/chat/completions';
export const PROVIDER_HOST = 'openrouter.ai';
export const PROVIDER_PATH = '/api/v1/chat/completions';
export const MODEL_ID = 'anthropic/claude-sonnet-5.5';
export const DEFAULT_KEY_FILE = '/run/secrets/openrouter-key';
export const DEFAULT_PORT = 8080;
export const MAX_BODY_BYTES = 8 * 1024;
export const MAX_PROVIDER_BODY_BYTES = 128 * 1024;
export const MAX_REQUESTS = 8;
export const MAX_OUTPUT_TOKENS = 2048;
export const PROVIDER_TIMEOUT_MS = 20_000;

const MAX_MESSAGES = 4;
const MAX_SYSTEM_CHARS = 512;
const MAX_USER_CHARS = 1000;
const MAX_ASSISTANT_CHARS = 512;
const MAX_TOOL_ARGUMENT_BYTES = 1024;
const MAX_REASONING_DETAILS_BYTES = 4096;
const allowedIPv4 = new BlockList();
for (const [subnet, prefix] of [
  ['0.0.0.0', 8],
  ['10.0.0.0', 8],
  ['100.64.0.0', 10],
  ['127.0.0.0', 8],
  ['169.254.0.0', 16],
  ['172.16.0.0', 12],
  ['192.0.0.0', 24],
  ['192.0.2.0', 24],
  ['192.88.99.0', 24],
  ['192.168.0.0', 16],
  ['198.18.0.0', 15],
  ['198.51.100.0', 24],
  ['203.0.113.0', 24],
  ['224.0.0.0', 4],
  ['240.0.0.0', 4],
]) {
  allowedIPv4.addSubnet(subnet, prefix, 'ipv4');
}

export class RelayError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

function isRecord(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function hasOnlyKeys(value, keys) {
  return isRecord(value) && Object.keys(value).every((key) => keys.includes(key));
}

function writeJson(response, status, value, headers = {}) {
  response.writeHead(status, {
    'Cache-Control': 'no-store',
    'Content-Type': 'application/json; charset=utf-8',
    'X-Content-Type-Options': 'nosniff',
    ...headers,
  });
  response.end(JSON.stringify(value));
}

function tokenMatches(authorization, expectedToken) {
  if (typeof authorization !== 'string') return false;
  const match = /^Bearer ([^ ]+)$/i.exec(authorization);
  if (!match) return false;
  const actual = Buffer.from(match[1]);
  const expected = Buffer.from(expectedToken);
  return actual.length === expected.length && actual.length > 0 && timingSafeEqual(actual, expected);
}

function readJsonBody(request) {
  return new Promise((resolve, reject) => {
    const contentLength = Number(request.headers['content-length']);
    if (Number.isFinite(contentLength) && contentLength > MAX_BODY_BYTES) {
      request.resume();
      reject(new RelayError(413, 'request body too large'));
      return;
    }

    const chunks = [];
    let bytes = 0;
    let settled = false;
    request.on('data', (chunk) => {
      if (settled) return;
      bytes += chunk.length;
      if (bytes > MAX_BODY_BYTES) {
        settled = true;
        chunks.length = 0;
        request.resume();
        reject(new RelayError(413, 'request body too large'));
        return;
      }
      chunks.push(chunk);
    });
    request.once('end', () => {
      if (settled) return;
      settled = true;
      try {
        resolve(JSON.parse(Buffer.concat(chunks).toString('utf8')));
      } catch {
        reject(new RelayError(400, 'request body must be valid JSON'));
      }
    });
    request.once('error', () => {
      if (settled) return;
      settled = true;
      reject(new RelayError(400, 'could not read request body'));
    });
    request.once('aborted', () => {
      if (settled) return;
      settled = true;
      reject(new RelayError(400, 'request was interrupted'));
    });
  });
}

function validateToolCall(message) {
  const calls = message.tool_calls;
  if (!Array.isArray(calls) || calls.length !== 1) {
    throw new RelayError(400, 'only one add_numbers tool call is allowed');
  }
  const call = calls[0];
  if (
    !hasOnlyKeys(call, ['id', 'type', 'function']) ||
    call.type !== 'function' ||
    typeof call.id !== 'string' ||
    !/^[A-Za-z0-9_-]{1,128}$/.test(call.id) ||
    !hasOnlyKeys(call.function, ['name', 'arguments']) ||
    call.function.name !== 'add_numbers' ||
    typeof call.function.arguments !== 'string' ||
    Buffer.byteLength(call.function.arguments) > MAX_TOOL_ARGUMENT_BYTES
  ) {
    throw new RelayError(400, 'unsupported tool call');
  }
  let args;
  try {
    args = JSON.parse(call.function.arguments);
  } catch {
    throw new RelayError(400, 'invalid tool arguments');
  }
  if (
    !hasOnlyKeys(args, ['a', 'b']) ||
    !Number.isFinite(args.a) ||
    !Number.isFinite(args.b) ||
    Math.abs(args.a) > 1_000_000 ||
    Math.abs(args.b) > 1_000_000
  ) {
    throw new RelayError(400, 'invalid tool arguments');
  }
  return call.id;
}

function validateMessages(messages) {
  if (!Array.isArray(messages) || (messages.length !== 2 && messages.length !== 4) || messages.length > MAX_MESSAGES) {
    throw new RelayError(400, 'invalid message count');
  }
  const [system, user] = messages;
  if (
    !hasOnlyKeys(system, ['role', 'content']) || system.role !== 'system' ||
    typeof system.content !== 'string' || system.content.length > MAX_SYSTEM_CHARS
  ) {
    throw new RelayError(400, 'invalid system message');
  }
  if (
    !hasOnlyKeys(user, ['role', 'content']) || user.role !== 'user' ||
    typeof user.content !== 'string' || user.content.trim().length === 0 || user.content.length > MAX_USER_CHARS
  ) {
    throw new RelayError(400, 'invalid user message');
  }

  const result = [{ role: 'system', content: system.content }, { role: 'user', content: user.content }];
  if (messages.length === 2) return result;

  const assistant = messages[2];
  if (
    !hasOnlyKeys(assistant, ['role', 'content', 'tool_calls', 'reasoning_details']) ||
    assistant.role !== 'assistant' ||
    (assistant.content !== null && typeof assistant.content !== 'string') ||
    (typeof assistant.content === 'string' && assistant.content.length > MAX_ASSISTANT_CHARS)
  ) {
    throw new RelayError(400, 'invalid assistant tool message');
  }
  if (assistant.reasoning_details !== undefined && Buffer.byteLength(JSON.stringify(assistant.reasoning_details)) > MAX_REASONING_DETAILS_BYTES) {
    throw new RelayError(400, 'reasoning details too large');
  }
  const toolCallID = validateToolCall(assistant);
  const tool = messages[3];
  if (
    !hasOnlyKeys(tool, ['role', 'tool_call_id', 'content']) ||
    tool.role !== 'tool' || tool.tool_call_id !== toolCallID ||
    typeof tool.content !== 'string' || !/^-?(?:\d+\.?\d*|\.\d+)$/.test(tool.content) ||
    !Number.isFinite(Number(tool.content))
  ) {
    throw new RelayError(400, 'invalid tool result message');
  }
  // Keep the provider-issued reasoning_details intact for the continuation turn.
  result.push(JSON.parse(JSON.stringify(assistant)), JSON.parse(JSON.stringify(tool)));
  return result;
}

const ADD_NUMBERS_TOOL = [{
  type: 'function',
  function: {
    name: 'add_numbers',
    description: 'Add two finite numbers, each between -1000000 and 1000000.',
    parameters: {
      type: 'object',
      properties: { a: { type: 'number' }, b: { type: 'number' } },
      required: ['a', 'b'],
      additionalProperties: false,
    },
  },
}];

function validatePayload(body) {
  const allowedFields = ['model', 'messages', 'tools', 'tool_choice', 'parallel_tool_calls', 'max_tokens', 'reasoning', 'stream'];
  if (!hasOnlyKeys(body, allowedFields) || body.model !== MODEL_ID || body.stream !== false || body.parallel_tool_calls !== false) {
    throw new RelayError(400, 'unsupported completion request');
  }
  if (
    !Array.isArray(body.tools) || body.tools.length !== 1 ||
    body.tools[0]?.type !== 'function' || body.tools[0]?.function?.name !== 'add_numbers'
  ) {
    throw new RelayError(400, 'only add_numbers is supported');
  }
  const isFirstRound = Array.isArray(body.messages) && body.messages.length === 2;
  const choice = body.tool_choice;
  if (isFirstRound) {
    if (
      !hasOnlyKeys(choice, ['type', 'function']) || choice.type !== 'function' ||
      !hasOnlyKeys(choice.function, ['name']) || choice.function.name !== 'add_numbers'
    ) throw new RelayError(400, 'unsupported tool choice');
  } else if (choice !== 'none') {
    throw new RelayError(400, 'unsupported tool choice');
  }
  if (
    !Number.isInteger(body.max_tokens) || body.max_tokens < 1 ||
    !isRecord(body.reasoning) || body.reasoning.effort !== 'low' || Object.keys(body.reasoning).some((key) => key !== 'effort')
  ) {
    throw new RelayError(400, 'invalid completion limits');
  }

  return {
    model: MODEL_ID,
    messages: validateMessages(body.messages),
    tools: ADD_NUMBERS_TOOL,
    tool_choice: isFirstRound ? { type: 'function', function: { name: 'add_numbers' } } : 'none',
    parallel_tool_calls: false,
    max_tokens: Math.min(body.max_tokens, MAX_OUTPUT_TOKENS),
    reasoning: { effort: 'low' },
    stream: false,
  };
}

export function isPublicIPv4(address) {
  return isIP(address) === 4 && !allowedIPv4.check(address, 'ipv4');
}

export async function resolvePublicIPv4(resolve4Impl = resolve4) {
  let addresses;
  try {
    addresses = await resolve4Impl(PROVIDER_HOST);
  } catch {
    throw new Error('provider DNS lookup failed');
  }
  if (!Array.isArray(addresses) || addresses.length === 0 || addresses.some((address) => !isPublicIPv4(address))) {
    throw new Error('provider DNS did not resolve to public IPv4 addresses');
  }
  return addresses[0];
}

export function createPinnedLookup(address) {
  if (!isPublicIPv4(address)) throw new Error('provider DNS did not resolve to a public IPv4 address');
  return (_hostname, options, callback) => {
    if (options?.all) {
      callback(null, [{ address, family: 4 }]);
    } else {
      callback(null, address, 4);
    }
  };
}

function waitWithSignal(promise, signal) {
  if (signal.aborted) return Promise.reject(new Error('provider request timed out'));
  return new Promise((resolve, reject) => {
    const cleanup = () => signal.removeEventListener('abort', onAbort);
    const onAbort = () => {
      cleanup();
      reject(new Error('provider request timed out'));
    };
    signal.addEventListener('abort', onAbort, { once: true });
    Promise.resolve(promise).then(
      (value) => { cleanup(); resolve(value); },
      (error) => { cleanup(); reject(error); },
    );
  });
}

function sendHTTPS({ apiKey, payload, address, signal, httpsRequestImpl = httpsRequest }) {
  return new Promise((resolve, reject) => {
    const body = JSON.stringify(payload);
    const request = httpsRequestImpl({
      hostname: PROVIDER_HOST,
      servername: PROVIDER_HOST,
      port: 443,
      path: PROVIDER_PATH,
      method: 'POST',
      rejectUnauthorized: true,
      lookup: createPinnedLookup(address),
      signal,
      headers: {
        Authorization: `Bearer ${apiKey}`,
        'Content-Type': 'application/json',
        'Content-Length': Buffer.byteLength(body),
      },
    }, (response) => {
      const chunks = [];
      let bytes = 0;
      response.on('data', (chunk) => {
        bytes += chunk.length;
        if (bytes > MAX_PROVIDER_BODY_BYTES) {
          request.destroy(new Error('provider response too large'));
          return;
        }
        chunks.push(chunk);
      });
      response.once('error', () => reject(new Error('provider request failed')));
      response.once('end', () => resolve({ statusCode: response.statusCode ?? 502, body: Buffer.concat(chunks).toString('utf8') }));
    });
    request.setTimeout(PROVIDER_TIMEOUT_MS, () => request.destroy(new Error('provider request timed out')));
    request.once('error', () => reject(new Error('provider request failed')));
    request.end(body);
  });
}

async function requestOpenRouter({
  apiKey,
  payload,
  resolve4Impl,
  httpsRequestImpl = httpsRequest,
  timeoutMs = PROVIDER_TIMEOUT_MS,
}) {
  const signal = AbortSignal.timeout(timeoutMs);
  const address = await waitWithSignal(resolvePublicIPv4(resolve4Impl), signal);
  return sendHTTPS({ apiKey, payload, address, signal, httpsRequestImpl });
}

function validateProviderResponse(result) {
  if (!result || result.statusCode < 200 || result.statusCode >= 300 || typeof result.body !== 'string') {
    throw new Error('provider request failed');
  }
  if (Buffer.byteLength(result.body, 'utf8') > MAX_PROVIDER_BODY_BYTES) {
    throw new Error('provider response too large');
  }
  let data;
  try {
    data = JSON.parse(result.body);
  } catch {
    throw new Error('provider returned invalid JSON');
  }
  if (!isRecord(data) || !Array.isArray(data.choices) || data.error) {
    throw new Error('provider returned invalid completion');
  }
  return result.body;
}

async function handleRequest(request, response, state) {
  if (request.method === 'GET' && request.url === '/health') {
    writeJson(response, 200, { status: 'ok' });
    return;
  }
  if (request.method !== 'POST' || request.url !== PROVIDER_PATH) {
    request.resume();
    writeJson(response, 404, { error: 'not found' });
    return;
  }
  if (!tokenMatches(request.headers.authorization, state.authToken)) {
    request.resume();
    writeJson(response, 401, { error: 'unauthorized' }, { 'WWW-Authenticate': 'Bearer' });
    return;
  }
  if (state.active || state.requestCount >= MAX_REQUESTS) {
    request.resume();
    writeJson(response, 429, { error: 'relay request limit reached' }, { 'Retry-After': '60' });
    return;
  }
  const contentType = request.headers['content-type'];
  if (typeof contentType !== 'string' || !/^application\/json(?:\s*;|$)/i.test(contentType)) {
    request.resume();
    writeJson(response, 415, { error: 'content type must be application/json' });
    return;
  }

  // Reserve the only in-flight slot before awaiting the body so simultaneous
  // requests cannot both pass the concurrency check.
  state.active = true;
  try {
    let payload;
    try {
      payload = validatePayload(await readJsonBody(request));
    } catch (error) {
      if (error instanceof RelayError) {
        writeJson(response, error.status, { error: error.message });
      } else {
        writeJson(response, 400, { error: 'invalid request' });
      }
      return;
    }

    state.requestCount += 1;
    const providerResult = await state.providerRequest({
      url: PROVIDER_URL,
      method: 'POST',
      headers: {
        Authorization: `Bearer ${state.apiKey}`,
        'Content-Type': 'application/json',
      },
      apiKey: state.apiKey,
      payload,
      resolve4Impl: state.resolve4Impl,
    });
    const body = validateProviderResponse(providerResult);
    response.writeHead(200, {
      'Cache-Control': 'no-store',
      'Content-Type': 'application/json; charset=utf-8',
      'X-Content-Type-Options': 'nosniff',
    });
    response.end(body);
  } catch {
    // Do not log provider request/response bodies, credentials, or relay tokens.
    writeJson(response, 502, { error: 'provider request failed' });
  } finally {
    state.active = false;
  }
}

export function createRelayServer({
  apiKey,
  authToken,
  providerRequest,
  resolve4Impl = resolve4,
  httpsRequestImpl = httpsRequest,
  providerTimeoutMs = PROVIDER_TIMEOUT_MS,
} = {}) {
  if (typeof apiKey !== 'string' || apiKey.trim().length < 20 || /[\r\n]/.test(apiKey)) {
    throw new Error('a valid provider key is required');
  }
  if (typeof authToken !== 'string' || authToken.length < 32 || /[\r\n]/.test(authToken)) {
    throw new Error('RELAY_AUTH_TOKEN must contain at least 32 characters');
  }
  const state = {
    apiKey: apiKey.trim(),
    authToken,
    providerRequest: providerRequest ?? ((options) => requestOpenRouter({
      ...options,
      resolve4Impl,
      httpsRequestImpl,
      timeoutMs: providerTimeoutMs,
    })),
    resolve4Impl,
    active: false,
    requestCount: 0,
  };
  const server = createServer((request, response) => {
    void handleRequest(request, response, state);
  });
  server.headersTimeout = 5_000;
  server.requestTimeout = 10_000;
  server.keepAliveTimeout = 5_000;
  server.maxHeadersCount = 24;
  return server;
}

function readConfiguration(env = process.env) {
  const authToken = env.RELAY_AUTH_TOKEN;
  if (typeof authToken !== 'string' || authToken.length < 32 || /[\r\n]/.test(authToken)) {
    throw new Error('RELAY_AUTH_TOKEN must contain at least 32 characters');
  }
  const keyPath = env.OPENROUTER_KEY_FILE || DEFAULT_KEY_FILE;
  let apiKey;
  try {
    apiKey = readFileSync(keyPath, 'utf8').trim();
  } catch {
    throw new Error('could not read provider key file');
  }
  if (apiKey.length < 20 || /[\r\n]/.test(apiKey)) {
    throw new Error('provider key file is empty or invalid');
  }
  const rawPort = env.PORT === undefined ? String(DEFAULT_PORT) : env.PORT;
  const port = Number(rawPort);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error('PORT must be an integer between 1 and 65535');
  }
  return { apiKey, authToken, port };
}

async function startRelay() {
  try {
    const { apiKey, authToken, port } = readConfiguration();
    const server = createRelayServer({ apiKey, authToken });
    server.listen(port, '0.0.0.0');
  } catch {
    process.stderr.write('relay startup failed\n');
    process.exitCode = 1;
  }
}

if (process.argv[1] && fileURLToPath(import.meta.url) === process.argv[1]) {
  void startRelay();
}
