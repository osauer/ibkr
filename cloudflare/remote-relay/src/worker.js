const ROUTE_COOKIE = "ibkr_remote_route";
const CONNECTOR_TOKEN_KEY = "connector_token";
const EXPIRES_AT_KEY = "expires_at";
const ROUTE_TTL_MS = 7 * 24 * 60 * 60 * 1000;

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname === "/healthz") {
      return json({ ok: true, service: "ibkr-remote-relay" });
    }
    if (url.pathname === "/api/register" && request.method === "POST") {
      return registerRoute(request, env);
    }
    if (url.pathname === "/api/connect") {
      const routeID = url.searchParams.get("route_id") || "";
      if (!routeID) return json({ error: "route_id required" }, 400);
      return routeStub(env, routeID).fetch(request);
    }

    const routeID = url.searchParams.get("remote") || readCookie(request.headers.get("Cookie") || "", ROUTE_COOKIE);
    if (!routeID) return json({ error: "remote route required" }, 400);
    const response = await routeStub(env, routeID).fetch(request);
    if (!url.searchParams.get("remote")) return response;
    const routed = new Response(response.body, {
      status: response.status,
      statusText: response.statusText,
      headers: response.headers,
    });
    routed.headers.append("Set-Cookie", routeCookie(routeID));
    return routed;
  },
};

export class RelaySession {
  constructor(state, env) {
    this.state = state;
    this.env = env;
    this.connector = null;
    this.inflight = new Map();
  }

  async fetch(request) {
    const url = new URL(request.url);
    if (url.pathname === "/internal/register" && request.method === "POST") {
      const body = await request.json();
      await this.state.storage.put(CONNECTOR_TOKEN_KEY, body.token);
      await this.state.storage.put(EXPIRES_AT_KEY, body.expires_at);
      return json({ ok: true });
    }
    if (url.pathname === "/api/connect") {
      return this.connect(request);
    }
    return this.forward(request);
  }

  async connect(request) {
    if (await this.expired()) return json({ error: "route expired" }, 410);
    if (request.headers.get("Upgrade") !== "websocket") {
      return json({ error: "websocket upgrade required" }, 426);
    }
    const expected = await this.state.storage.get(CONNECTOR_TOKEN_KEY);
    const token = connectorToken(request);
    if (!expected || token !== expected) {
      return json({ error: "unauthorized" }, 401);
    }
    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);
    server.accept();
    if (this.connector) this.connector.close(1012, "connector replaced");
    this.connector = server;
    server.addEventListener("message", (event) => this.handleConnectorMessage(event.data));
    server.addEventListener("close", () => this.connectorClosed(server));
    server.addEventListener("error", () => this.connectorClosed(server));
    return new Response(null, { status: 101, webSocket: client });
  }

  async forward(request) {
    if (await this.expired()) return json({ error: "route expired" }, 410);
    if (!this.connector) return json({ error: "Mac relay connector offline" }, 503);
    const id = crypto.randomUUID();
    const body = await request.arrayBuffer();
    const { readable, writable } = new TransformStream();
    const writer = writable.getWriter();
    const start = new Promise((resolve) => {
      this.inflight.set(id, { resolve, readable, writer, started: false });
    });
    try {
      this.connector.send(JSON.stringify({
        type: "request",
        id,
        method: request.method,
        path: requestPath(request.url),
        headers: headerMap(request.headers),
        body: bytesToBase64(new Uint8Array(body)),
      }));
    } catch {
      this.inflight.delete(id);
      return json({ error: "Mac relay connector unavailable" }, 503);
    }
    return start;
  }

  async handleConnectorMessage(raw) {
    let frame;
    try {
      frame = JSON.parse(raw);
    } catch {
      return;
    }
    const pending = this.inflight.get(frame.id);
    if (!pending) return;
    if (frame.type === "response_start") {
      const headers = new Headers();
      for (const [key, values] of Object.entries(frame.headers || {})) {
        for (const value of values) headers.append(key, value);
      }
      pending.started = true;
      pending.resolve(new Response(pending.readable, {
        status: frame.status || 200,
        headers,
      }));
      return;
    }
    if (frame.type === "response_chunk") {
      await pending.writer.write(base64ToBytes(frame.body || ""));
      return;
    }
    if (frame.type === "response_end") {
      await pending.writer.close();
      this.inflight.delete(frame.id);
      return;
    }
    if (frame.type === "response_error") {
      const status = frame.status || 502;
      const body = JSON.stringify({ error: frame.error || "relay error" });
      if (!pending.started) {
        pending.resolve(new Response(body, {
          status,
          headers: { "Content-Type": "application/json" },
        }));
      } else {
        await pending.writer.abort(frame.error || "relay error");
      }
      this.inflight.delete(frame.id);
    }
  }

  connectorClosed(connector) {
    if (this.connector === connector) this.connector = null;
    for (const [id, pending] of this.inflight) {
      if (!pending.started) {
        pending.resolve(json({ error: "Mac relay connector disconnected" }, 503));
      } else {
        pending.writer.abort("Mac relay connector disconnected");
      }
      this.inflight.delete(id);
    }
  }

  async expired() {
    const raw = await this.state.storage.get(EXPIRES_AT_KEY);
    return !!raw && Date.parse(raw) <= Date.now();
  }
}

async function registerRoute(request, env) {
  await request.arrayBuffer();
  const routeID = `r_${randomToken(18)}`;
  const token = randomToken(32);
  const expiresAt = new Date(Date.now() + ROUTE_TTL_MS).toISOString();
  const origin = new URL(request.url).origin;
  const stub = routeStub(env, routeID);
  await stub.fetch(new Request(`${origin}/internal/register`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token, expires_at: expiresAt }),
  }));
  return json({
    route_id: routeID,
    public_url: origin,
    connector_url: `${origin.replace("https://", "wss://").replace("http://", "ws://")}/api/connect?route_id=${encodeURIComponent(routeID)}&token=${encodeURIComponent(token)}`,
    connector_token: token,
    expires_at: expiresAt,
  });
}

function routeStub(env, routeID) {
  return env.RELAY_SESSION.get(env.RELAY_SESSION.idFromName(routeID));
}

function json(body, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function requestPath(rawURL) {
  const url = new URL(rawURL);
  return `${url.pathname}${url.search}`;
}

function headerMap(headers) {
  const out = {};
  for (const [key, value] of headers) {
    if (skipHeader(key)) continue;
    if (!out[key]) out[key] = [];
    out[key].push(value);
  }
  return out;
}

function skipHeader(key) {
  return ["connection", "upgrade", "transfer-encoding"].includes(key.toLowerCase());
}

function connectorToken(request) {
  const auth = request.headers.get("Authorization") || "";
  if (auth.startsWith("Bearer ")) return auth.slice("Bearer ".length).trim();
  return new URL(request.url).searchParams.get("token") || "";
}

function routeCookie(routeID) {
  return `${ROUTE_COOKIE}=${encodeURIComponent(routeID)}; Path=/; Max-Age=604800; Secure; HttpOnly; SameSite=Lax`;
}

function readCookie(raw, name) {
  for (const part of raw.split(";")) {
    const [key, value] = part.trim().split("=");
    if (key === name) return decodeURIComponent(value || "");
  }
  return "";
}

function randomToken(byteLength) {
  const bytes = new Uint8Array(byteLength);
  crypto.getRandomValues(bytes);
  return bytesToBase64URL(bytes);
}

function bytesToBase64URL(bytes) {
  return bytesToBase64(bytes).replaceAll("+", "-").replaceAll("/", "_").replaceAll("=", "");
}

function bytesToBase64(bytes) {
  let binary = "";
  for (let i = 0; i < bytes.length; i += 0x8000) {
    binary += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  }
  return btoa(binary);
}

function base64ToBytes(raw) {
  const binary = atob(raw);
  const out = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) out[i] = binary.charCodeAt(i);
  return out;
}

export const __test = {
  requestPath,
  headerMap,
  readCookie,
  routeCookie,
};
