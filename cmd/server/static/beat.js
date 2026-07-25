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

  if (freshness) freshness.addEventListener("change", syncTimeless);
  if (recur) recur.addEventListener("change", syncInterval);
  syncTimeless();
  syncInterval();
})();
