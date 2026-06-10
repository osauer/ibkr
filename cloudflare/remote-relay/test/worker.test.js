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

test("connect returns 410 once the route expired", async () => {
  const state = fakeState({ expires_at: new Date(Date.now() - 1000).toISOString() });
  const session = new RelaySession(state, {});
  const res = await session.fetch(new Request("https://relay.example/api/connect"));
  assert.equal(res.status, 410);
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
  assert.match(setCookie, /Max-Age=604800/);
});
