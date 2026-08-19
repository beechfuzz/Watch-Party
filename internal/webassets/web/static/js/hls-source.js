// Manages the lifecycle of a single hls.js instance across repeated source
// loads, so a genuinely new source (e.g. a playlist item transition) never
// leaves a previous instance still attached to the <video> element.
//
// player.js used to do `const hls = new Hls(); hls.attachMedia(video);`
// fresh on every attachSource call, with no reference kept anywhere and no
// teardown of whatever instance was already attached. That worked for the
// very first load (nothing preceding it to conflict with) but broke on
// every subsequent playlist transition: the old instance was still alive,
// still holding its own MediaSource attached to the same <video> element,
// when a second instance called attachMedia on it. hls.js requires
// destroy() (which handles detaching internally) before a new instance
// takes over a media element it doesn't already own -- reusing an element
// across instances without that teardown is exactly what surfaces as a
// fatal `mediaSourceRequiresReset` error. See ARCHITECTURE.md's postmortem
// for this bug for the full trace.
//
// Extracted into its own module, dependency-injected on the Hls
// constructor, so this lifecycle rule is unit-testable without a DOM or
// the real hls.js library -- the same way playback-events.js separates
// pure logic from player.js's DOM-heavy code (see hls-source.test.mjs).

export function createHlsInstanceManager() {
  let current = null;

  function destroy() {
    if (current) {
      current.destroy();
      current = null;
    }
  }

  // Tears down any previously-attached instance, then constructs, wires,
  // and attaches a fresh one. A fatal error also tears down and reports via
  // onFatalError -- this only guarantees the broken instance doesn't linger
  // attempting further recovery on its own; it does not retry or rebuild
  // the stream itself, so the caller's onFatalError still surfaces the same
  // error state to the user it always did.
  function attach(HlsCtor, video, url, onFatalError) {
    destroy();
    const hls = new HlsCtor();
    current = hls;
    hls.on(HlsCtor.Events.ERROR, (_event, data) => {
      if (data.fatal) {
        destroy();
        onFatalError(data);
      }
    });
    hls.loadSource(url);
    hls.attachMedia(video);
    return hls;
  }

  return { attach, destroy };
}
