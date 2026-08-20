// Run with: node --test internal/webassets/web/static/js/sidebar-collapse.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import {
  defaultCollapsedState,
  loadCollapsedState,
  serializeCollapsedState,
} from "./sidebar-collapse.js";

test("defaultCollapsedState: starts expanded", () => {
  assert.equal(defaultCollapsedState(), false);
});

test("loadCollapsedState: falls back to the default when nothing is stored", () => {
  assert.equal(loadCollapsedState(null), defaultCollapsedState());
  assert.equal(loadCollapsedState(""), defaultCollapsedState());
  assert.equal(loadCollapsedState(undefined), defaultCollapsedState());
});

test("loadCollapsedState: falls back to the default on malformed/foreign values", () => {
  assert.equal(loadCollapsedState("not a boolean"), defaultCollapsedState());
  assert.equal(loadCollapsedState("1"), defaultCollapsedState());
  assert.equal(loadCollapsedState("TRUE"), defaultCollapsedState());
});

test("loadCollapsedState: reads back exactly what serializeCollapsedState writes", () => {
  assert.equal(loadCollapsedState("true"), true);
  assert.equal(loadCollapsedState("false"), false);
});

test("serializeCollapsedState + loadCollapsedState round-trip", () => {
  assert.equal(loadCollapsedState(serializeCollapsedState(true)), true);
  assert.equal(loadCollapsedState(serializeCollapsedState(false)), false);
});
