import assert from "node:assert/strict";
import test from "node:test";

import { __test } from "../src/worker.js";

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
