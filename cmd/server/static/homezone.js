// The traveller's prompt (ADR 0030).
//
// A Home Zone does not follow its owner abroad, on purpose: a briefing
// anchored to seven in the morning arrives at eight in the evening in
// Tokyo and is itself again on the flight home. That is the right default
// — following the traveller means a zone change mid-cycle, which can only
// ever deliver two episodes in one day or none — but it should not be
// silent. So when the browser is somewhere the account is not, the
// Dashboard says so and offers the change.
//
// The comparison has to happen here because the server never learns where
// a page is being read. With JS off nothing is revealed, which is correct:
// there would be no zone to offer switching to.
(function () {
  var banner = document.getElementById("zone-banner");
  if (!banner) return;

  var here = "";
  try {
    here = Intl.DateTimeFormat().resolvedOptions().timeZone || "";
  } catch (e) {
    return;
  }
  var home = banner.dataset.homeZone;
  if (!here || !home || here === home) return;

  var shown = document.getElementById("zone-browser");
  var field = document.getElementById("zone-switch-to");
  if (shown) shown.textContent = here;
  if (field) field.value = here;
  banner.hidden = false;
})();
