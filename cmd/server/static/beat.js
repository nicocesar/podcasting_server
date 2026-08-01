// Progressive enhancement for the Beat controls on /me/generate.
//
// Two rules, both cosmetic. The server enforces the real ones: a Timeless
// briefing may not recur, and a template that picks its own cadence must
// send a valid interval. With JS off the form still works — you just see
// an offer that comes back as an error instead of one that quietly
// withdraws itself.
(function () {
  var box = document.getElementById("beat-box");
  if (!box) return;
  var recur = document.getElementById("recur");
  var interval = document.getElementById("interval");
  var freshness = document.getElementById("freshness");

  // A Timeless topic isn't tied to the news, so there is no window to
  // repeat on — the whole offer disappears rather than sitting there
  // waiting to be rejected.
  function syncTimeless() {
    if (!freshness || box.dataset.derives !== "1") return;
    var timeless = freshness.value === "0";
    box.hidden = timeless;
    if (timeless && recur) recur.checked = false;
  }

  // The cadence only means something once you've asked to repeat.
  function syncInterval() {
    if (!interval || !recur) return;
    var row = interval.previousElementSibling; // its <label>
    interval.hidden = !recur.checked;
    if (row && row.tagName === "LABEL") row.hidden = !recur.checked;
  }

  // The browser is the only thing that knows where its owner is, and it
  // is asked exactly once: the server uses this to give a User their
  // first Home Zone and never to change one they already have. A phone
  // in Tokyo for a week means its owner is travelling, not that their
  // mornings moved (ADR 0030).
  //
  // With JS off there is no zone and the server says so plainly rather
  // than guessing — which is why the time field is optional.
  function reportZone() {
    var field = document.getElementById("browser_zone");
    if (!field) return;
    try {
      field.value = Intl.DateTimeFormat().resolvedOptions().timeZone || "";
    } catch (e) {
      field.value = "";
    }
    // Name the zone on the form when the account has none yet, so the
    // first Anchor is not set blind.
    var note = document.getElementById("beat-zone-note");
    if (note && note.hidden && field.value) {
      note.textContent = "Times are " + field.value + " — we'll remember that as your home timezone.";
      note.hidden = false;
    }
  }

  // A time of day only means something once you've asked to repeat.
  function syncFireAt() {
    var fireAt = document.getElementById("fire_at");
    if (!fireAt || !recur) return;
    var label = fireAt.previousElementSibling;
    var note = document.getElementById("beat-zone-note");
    fireAt.hidden = !recur.checked;
    if (label && label.tagName === "LABEL") label.hidden = !recur.checked;
    if (note) note.hidden = !recur.checked || !note.textContent.trim();
  }

  if (freshness) freshness.addEventListener("change", syncTimeless);
  if (recur) {
    recur.addEventListener("change", syncInterval);
    recur.addEventListener("change", syncFireAt);
  }
  reportZone();
  syncTimeless();
  syncInterval();
  syncFireAt();
})();
