// Live preview for generated strand cover art on the admin canon page.
// The server draws the art; this only keeps the <img> pointed at the right
// query while the admin types, so the words, colour and icon can be judged
// before anything is stored. Without JS the preview is still correct on
// load and the form still works — it just stops following the typing.
(function () {
  "use strict";

  function fieldValue(form, name) {
    // Two inputs can feed "text" (a strand's title and the art words); the
    // later one wins when filled, matching what the server does.
    var value = "";
    form.querySelectorAll('[data-art-field="' + name + '"]').forEach(function (el) {
      var v = (el.value || "").trim();
      if (v) value = v;
    });
    return value;
  }

  function refresh(form) {
    var img = form.querySelector("[data-art-target]");
    if (!img) return;
    var text = fieldValue(form, "text");
    if (!text) return; // nothing to draw; leave the last good preview up
    var q = new URLSearchParams({ text: text });
    ["accent", "icon"].forEach(function (name) {
      var v = fieldValue(form, name);
      if (v) q.set(name, v);
    });
    img.src = "/admin/strands/cover/preview?" + q.toString();
  }

  document.querySelectorAll("[data-art-preview]").forEach(function (form) {
    var timer = null;
    form.addEventListener("input", function () {
      // Debounced: every keystroke would be a render on the server.
      clearTimeout(timer);
      timer = setTimeout(function () { refresh(form); }, 300);
    });
    form.addEventListener("change", function () { refresh(form); });
  });
})();
