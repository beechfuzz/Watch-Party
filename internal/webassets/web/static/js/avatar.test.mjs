// Run with: node --test internal/webassets/web/static/js/avatar.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import { createAvatarResolver, initials } from "./avatar.js";

test("initials: falls back to \"?\" for empty/null/undefined names", () => {
  for (const name of ["", undefined, null]) {
    assert.equal(initials(name), "?");
  }
});

test("initials: uses the first character of a trimmed name, uppercased", () => {
  assert.equal(initials("bob"), "B");
  assert.equal(initials("  Alice  "), "A");
  assert.equal(initials("zoe smith"), "Z");
});

test("createAvatarResolver: resolves a successful lookup to displayName/avatarUrl", async () => {
  const resolver = createAvatarResolver(async () => ({ display_name: "Bobby", avatar_url: "https://emby.example/avatar.png" }));
  const identity = await resolver.resolve("user-bob");
  assert.deepEqual(identity, { displayName: "Bobby", avatarUrl: "https://emby.example/avatar.png" });
});

test("createAvatarResolver: falls back to the raw id and an empty avatarUrl on rejection, never throws", async () => {
  const resolver = createAvatarResolver(async () => {
    throw new Error("emby unreachable");
  });
  const identity = await resolver.resolve("user-bob");
  assert.deepEqual(identity, { displayName: "user-bob", avatarUrl: "" });
});

test("createAvatarResolver: a resolved lookup for one id is cached, not refetched", async () => {
  let calls = 0;
  const resolver = createAvatarResolver(async (id) => {
    calls++;
    return { display_name: id, avatar_url: "" };
  });
  await resolver.resolve("user-bob");
  await resolver.resolve("user-bob");
  await resolver.resolve("user-bob");
  assert.equal(calls, 1);
});

test("createAvatarResolver: concurrent lookups for the same unresolved id share one in-flight request", async () => {
  let calls = 0;
  let releaseFetch;
  const gate = new Promise((resolve) => { releaseFetch = resolve; });
  const resolver = createAvatarResolver(async (id) => {
    calls++;
    await gate;
    return { display_name: id, avatar_url: "" };
  });

  const p1 = resolver.resolve("user-bob");
  const p2 = resolver.resolve("user-bob");
  releaseFetch();
  const [i1, i2] = await Promise.all([p1, p2]);

  assert.equal(calls, 1);
  assert.deepEqual(i1, i2);
});

test("createAvatarResolver: distinct ids are fetched and cached independently", async () => {
  const calls = [];
  const resolver = createAvatarResolver(async (id) => {
    calls.push(id);
    return { display_name: id, avatar_url: "" };
  });
  const bob = await resolver.resolve("user-bob");
  const alice = await resolver.resolve("user-alice");
  assert.deepEqual(calls, ["user-bob", "user-alice"]);
  assert.equal(bob.displayName, "user-bob");
  assert.equal(alice.displayName, "user-alice");
});

test("createAvatarResolver: missing display_name/avatar_url in the response fall back to the id / empty string", async () => {
  const resolver = createAvatarResolver(async () => ({}));
  const identity = await resolver.resolve("user-bob");
  assert.deepEqual(identity, { displayName: "user-bob", avatarUrl: "" });
});
