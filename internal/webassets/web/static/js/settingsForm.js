// Shared wiring for the Party Settings form's four end-of-media fields
// (auto_advance, show_next_dialog, autoplay_enabled, autoplay_delay_seconds)
// -- used by both the create-party dialog (index.html/app.js) and the
// post-creation settings dialog (party.html/player.js), which are otherwise
// two independent page entry points with no shared component system.
//
// show_next_dialog and the autoplay controls are disabled (and irrelevant)
// whenever Auto-Advance is off, per the confirmed field behavior; the
// autoplay delay input is additionally disabled whenever "Auto-play after"
// itself is unchecked, since the delay still applies to the paused-load
// case but editing it while the countdown-outcome checkbox is off would be
// misleading about what's being configured.
export function wireSettingsForm(ids) {
  const autoAdvance = document.getElementById(ids.autoAdvance);
  const showNextDialog = document.getElementById(ids.showNextDialog);
  const autoplayEnabled = document.getElementById(ids.autoplayEnabled);
  const autoplayDelay = document.getElementById(ids.autoplayDelay);

  function updateDependentDisabled() {
    const dependentsDisabled = !autoAdvance.checked;
    showNextDialog.disabled = dependentsDisabled;
    autoplayEnabled.disabled = dependentsDisabled;
    autoplayDelay.disabled = dependentsDisabled || !autoplayEnabled.checked;
  }
  autoAdvance.addEventListener("change", updateDependentDisabled);
  autoplayEnabled.addEventListener("change", updateDependentDisabled);
  updateDependentDisabled();

  return {
    read() {
      return {
        auto_advance: autoAdvance.checked,
        show_next_dialog: showNextDialog.checked,
        autoplay_enabled: autoplayEnabled.checked,
        autoplay_delay_seconds: parseInt(autoplayDelay.value, 10) || 5,
      };
    },
    write(settings) {
      autoAdvance.checked = settings.auto_advance;
      showNextDialog.checked = settings.show_next_dialog;
      autoplayEnabled.checked = settings.autoplay_enabled;
      autoplayDelay.value = settings.autoplay_delay_seconds;
      updateDependentDisabled();
    },
    // Re-syncs disabled state after something other than a user click
    // changed the checkboxes -- e.g. a native <form>.reset(), which
    // restores checked/value attributes but fires no change event.
    resync() {
      updateDependentDisabled();
    },
  };
}
