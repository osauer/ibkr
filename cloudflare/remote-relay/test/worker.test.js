import assert from "node:assert/strict";
import test from "node:test";

import worker, { RelaySession, __test } from "../src/worker.js";

const DAY_MS = 24 * 60 * 60 * 1000;

function fakeState(initial = {}) {
  const data = new Map(Object.entries(initial));
  return {
    data,
    storage: {
      async get(key) {
        return data.get(key);
      },
      async put(key, value) {
        data.set(key, value);
      },
    },
  };
}

function fakeEnv() {
  const sessions = new Map();
  return {
    sessions,
    env: {
      RELAY_SESSION: {
        idFromName: (name) => name,
        get: (name) => {
          if (!sessions.has(name)) sessions.set(name, new RelaySession(fakeState(), {}));
          return sessions.get(name);
        },
      },
    },
  };
}

test("requestPath preserves path and query", () => {
  assert.equal(
    __test.requestPath("https://remote.osauer.dev/pair.html?remote=r1&pair=p1"),
    "/pair.html?remote=r1&pair=p1",
  );
});

test("route cookie round trips route id", () => {
  const cookie = __test.routeCookie("r_abc123");
  assert.match(cookie, /Secure/);
  assert.match(cookie, /HttpOnly/);
  assert.equal(__test.readCookie(cookie, "ibkr_remote_route"), "r_abc123");
});

test("headerMap removes hop by hop headers", () => {
  const headers = new Headers({
    Connection: "upgrade",
    Cookie: "a=b",
    Upgrade: "websocket",
  });
  assert.deepEqual(__test.headerMap(headers), { cookie: ["a=b"] });
});

test("connect rejects a route with no registered token", async () => {
  const state = fakeState({ expires_at: new Date(Date.now() - 1000).toISOString() });
  const session = new RelaySession(state, {});
  const res = await session.fetch(new Request("https://relay.example/api/connect"));
  assert.equal(res.status, 401);
});

test("connect with the connector token revives an expired route", async () => {
  const state = fakeState({
    connector_token: "tok",
    expires_at: new Date(Date.now() - 1000).toISOString(),
  });
  const session = new RelaySession(state, {});
  const before = Date.now();
  // No websocket upgrade so the test stays inside Node: the token check and
  // TTL slide run before the upgrade requirement.
  const res = await session.fetch(
    new Request("https://relay.example/api/connect", {
      headers: { Authorization: "Bearer tok" },
    }),
  );
  assert.equal(res.status, 426);
  const next = Date.parse(state.data.get("expires_at"));
  assert.ok(next >= before + 7 * DAY_MS - 1000, `expired route was not revived: ${next}`);
});

test("connect does not slide the TTL for a bad connector token", async () => {
  const expiry = new Date(Date.now() + 60 * 1000).toISOString();
  const state = fakeState({ connector_token: "good", expires_at: expiry });
  const session = new RelaySession(state, {});
  const res = await session.fetch(
    new Request("https://relay.example/api/connect", {
      headers: { Upgrade: "websocket", Authorization: "Bearer bad" },
    }),
  );
  assert.equal(res.status, 401);
  assert.equal(state.data.get("expires_at"), expiry);
});

test("slideExpiry renews the route for a full TTL window", async () => {
  const stale = new Date(Date.now() + 60 * 1000).toISOString();
  const state = fakeState({ expires_at: stale });
  const session = new RelaySession(state, {});
  const before = Date.now();
  await session.slideExpiry();
  const next = Date.parse(state.data.get("expires_at"));
  assert.ok(next >= before + 7 * DAY_MS - 1000, `expires_at ${next} is not ~7 days out`);
  assert.ok(next > Date.parse(stale), "slideExpiry did not extend the window");
});

test("cookie-addressed requests refresh the route cookie", async () => {
  const env = {
    RELAY_SESSION: {
      idFromName: (name) => name,
      get: () => ({ fetch: async () => new Response("ok") }),
    },
  };
  const req = new Request("https://relay.example/api/bootstrap", {
    headers: { Cookie: "ibkr_remote_route=r_abc" },
  });
  const res = await worker.fetch(req, env);
  assert.equal(res.status, 200);
  const setCookie = res.headers.get("Set-Cookie") || "";
  assert.match(setCookie, /ibkr_remote_route=r_abc/);
  assert.match(setCookie, /Max-Age=34560000/);
});

test("register can resume an existing route with the connector token", async () => {
  const { env } = fakeEnv();
  const first = await worker.fetch(new Request("https://relay.example/api/register", { method: "POST" }), env);
  assert.equal(first.status, 200);
  const route = await first.json();
  const resumed = await worker.fetch(new Request("https://relay.example/api/register", {
    method: "POST",
    body: JSON.stringify({
      route_id: route.route_id,
      connector_token: route.connector_token,
    }),
  }), env);
  assert.equal(resumed.status, 200);
  const body = await resumed.json();
  assert.equal(body.route_id, route.route_id);
  assert.equal(body.connector_token, route.connector_token);
});

test("register rejects resume with the wrong connector token", async () => {
  const { env } = fakeEnv();
  const first = await worker.fetch(new Request("https://relay.example/api/register", { method: "POST" }), env);
  assert.equal(first.status, 200);
  const route = await first.json();
  const resumed = await worker.fetch(new Request("https://relay.example/api/register", {
    method: "POST",
    body: JSON.stringify({
      route_id: route.route_id,
      connector_token: "wrong",
    }),
  }), env);
  assert.equal(resumed.status, 401);
});

test("internal register revives an expired route on token-matched resume", async () => {
  const renewed = new Date(Date.now() + 7 * DAY_MS).toISOString();
  const state = fakeState({
    connector_token: "tok",
    expires_at: new Date(Date.now() - 1000).toISOString(),
  });
  const session = new RelaySession(state, {});
  const res = await session.fetch(new Request("https://relay.example/internal/register", {
    method: "POST",
    body: JSON.stringify({ token: "tok", expires_at: renewed, resume: true }),
  }));
  assert.equal(res.status, 200);
  assert.equal(state.data.get("expires_at"), renewed);
});

test("resume against empty storage adopts the presented token", async () => {
  const state = fakeState({});
  const session = new RelaySession(state, {});
  const res = await session.fetch(new Request("https://relay.example/internal/register", {
    method: "POST",
    body: JSON.stringify({
      token: "tok",
      expires_at: new Date(Date.now() + 7 * DAY_MS).toISOString(),
      resume: true,
    }),
  }));
  assert.equal(res.status, 200);
  assert.equal(state.data.get("connector_token"), "tok");
});

test("navigation without a route serves the localStorage recovery page", async () => {
  const { env } = fakeEnv();
  const res = await worker.fetch(new Request("https://relay.example/", {
    headers: { Accept: "text/html,application/xhtml+xml" },
  }), env);
  assert.equal(res.status, 200);
  assert.match(res.headers.get("Content-Type") || "", /text\/html/);
  assert.match(await res.text(), /ibkrRemoteRoute/);
});

test("API request without a route still gets JSON 400", async () => {
  const { env } = fakeEnv();
  const res = await worker.fetch(new Request("https://relay.example/api/bootstrap"), env);
  assert.equal(res.status, 400);
  assert.match(res.headers.get("Content-Type") || "", /application\/json/);
});

test("navigation to an offline route serves the auto-retry page with cookie refresh", async () => {
  const env = {
    RELAY_SESSION: {
      idFromName: (name) => name,
      get: () => ({
        fetch: async () => new Response(JSON.stringify({ error: "Mac relay connector offline" }), {
          status: 503,
          headers: { "Content-Type": "application/json" },
        }),
      }),
    },
  };
  const res = await worker.fetch(new Request("https://relay.example/", {
    headers: { Accept: "text/html", Cookie: "ibkr_remote_route=r_abc" },
  }), env);
  assert.equal(res.status, 503);
  assert.match(res.headers.get("Content-Type") || "", /text\/html/);
  assert.match(await res.text(), /Retrying automatically/);
  assert.match(res.headers.get("Set-Cookie") || "", /ibkr_remote_route=r_abc/);
});
