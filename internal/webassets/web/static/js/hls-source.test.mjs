// Run with: node --test internal/webassets/web/static/js/hls-source.test.mjs
import test from "node:test";
import assert from "node:assert/strict";
import { createHlsInstanceManager } from "./hls-source.js";

// A minimal stand-in for hls.js's Hls class: tracks its own lifecycle calls
// so tests can assert on them without the real vendored library or a DOM.
function makeFakeHlsCtor() {
  const instances = [];
  class FakeHls {
    constructor() {
      this.destroyed = false;
      this.attachedMedia = null;
      this.loadedSource = null;
      this.errorHandler = null;
      instances.push(this);
    }
    on(event, handler) {
      if (event === FakeHls.Events.ERROR) this.errorHandler = handler;
    }
    loadSource(url) {
      this.loadedSource = url;
    }
    attachMedia(video) {
      this.attachedMedia = video;
    }
    destroy() {
      this.destroyed = true;
    }
  }
  FakeHls.Events = { ERROR: "hlsError" };
  FakeHls.instances = instances;
  return FakeHls;
}

test("attach: destroys the previous instance before constructing the next", () => {
  const FakeHls = makeFakeHlsCtor();
  const manager = createHlsInstanceManager();
  const video1 = {};
  const video2 = {};

  manager.attach(FakeHls, video1, "http://example/item1.m3u8", () => {});
  const first = FakeHls.instances[0];
  assert.equal(first.destroyed, false);
  assert.equal(first.attachedMedia, video1);

  manager.attach(FakeHls, video2, "http://example/item2.m3u8", () => {});
  const second = FakeHls.instances[1];
  assert.equal(first.destroyed, true, "previous instance must be destroyed before the next attach");
  assert.equal(second.destroyed, false);
  assert.equal(second.attachedMedia, video2);
  assert.equal(second.loadedSource, "http://example/item2.m3u8");
});

test("attach: a fatal error destroys the instance and calls onFatalError", () => {
  const FakeHls = makeFakeHlsCtor();
  const manager = createHlsInstanceManager();
  let reported = null;

  manager.attach(FakeHls, {}, "http://example/item1.m3u8", (data) => {
    reported = data;
  });
  const hls = FakeHls.instances[0];
  hls.errorHandler({}, { fatal: true, details: "mediaSourceRequiresReset" });

  assert.equal(hls.destroyed, true);
  assert.deepEqual(reported, { fatal: true, details: "mediaSourceRequiresReset" });
});

test("attach: a non-fatal error does not destroy the instance", () => {
  const FakeHls = makeFakeHlsCtor();
  const manager = createHlsInstanceManager();
  let called = false;

  manager.attach(FakeHls, {}, "http://example/item1.m3u8", () => {
    called = true;
  });
  const hls = FakeHls.instances[0];
  hls.errorHandler({}, { fatal: false, details: "bufferStalledError" });

  assert.equal(hls.destroyed, false);
  assert.equal(called, false);
});

test("destroy: a safe no-op when nothing is attached", () => {
  const manager = createHlsInstanceManager();
  assert.doesNotThrow(() => manager.destroy());
});

test("destroy: tears down whatever is currently attached, e.g. an idle transition", () => {
  const FakeHls = makeFakeHlsCtor();
  const manager = createHlsInstanceManager();

  manager.attach(FakeHls, {}, "http://example/item1.m3u8", () => {});
  const hls = FakeHls.instances[0];
  manager.destroy();

  assert.equal(hls.destroyed, true);
  // A second destroy() after the instance is already gone must not throw or
  // double-destroy anything.
  assert.doesNotThrow(() => manager.destroy());
});
