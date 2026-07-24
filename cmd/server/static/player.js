// The inline Player (ADR 0013): play/pause, a seekable scrubber, ±15s,
// and a speed cycle, built with no dependencies on top of the browser's
// own <audio> element. The server ships a working <audio controls>; this
// file takes the controls away and puts ours in their place, so the
// audio element remains the single source of truth and a page without
// JavaScript still plays.
//
// Resume Position and playback rate live in localStorage and are never
// sent anywhere — see CONTEXT.md before promoting either to the store.
(function () {
  "use strict";

  var STORE_POS = "pp:pos:";       // + owner/slug -> seconds
  var STORE_RATE = "pp:rate";      // shared across every player
  var RATES = [1, 1.25, 1.5, 2];
  var SKIP = 15;
  var SAVE_EVERY = 5;              // seconds of playback between writes
  var RESUME_FLOOR = 20;           // don't offer to resume the first 20s
  var RESUME_TAIL = 15;            // ...nor the last 15

  // Storage is a convenience, never a requirement: Safari in private
  // mode throws on write, and a thrown error here would take the whole
  // player down with it.
  function get(key) {
    try { return localStorage.getItem(key); } catch (e) { return null; }
  }
  function set(key, value) {
    try { localStorage.setItem(key, value); } catch (e) { /* ignore */ }
  }
  function drop(key) {
    try { localStorage.removeItem(key); } catch (e) { /* ignore */ }
  }

  // clock renders seconds as m:ss, or h:mm:ss past an hour.
  function clock(seconds) {
    if (!isFinite(seconds) || seconds < 0) seconds = 0;
    var s = Math.floor(seconds % 60);
    var m = Math.floor(seconds / 60) % 60;
    var h = Math.floor(seconds / 3600);
    var mm = h > 0 && m < 10 ? "0" + m : String(m);
    return (h > 0 ? h + ":" : "") + mm + ":" + (s < 10 ? "0" + s : s);
  }

  function icon(name) {
    var svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("class", "icon");
    svg.setAttribute("aria-hidden", "true");
    var use = document.createElementNS("http://www.w3.org/2000/svg", "use");
    use.setAttribute("href", "#" + name);
    svg.appendChild(use);
    return svg;
  }

  function button(cls, label, child) {
    var b = document.createElement("button");
    b.type = "button";
    b.className = cls;
    b.setAttribute("aria-label", label);
    b.title = label;
    b.appendChild(child);
    return b;
  }

  // Only one thing plays at a time: whoever starts pauses everyone else.
  var players = [];

  function build(box) {
    var audio = box.querySelector("audio");
    if (!audio) return;

    var key = STORE_POS + box.dataset.key;
    var total = parseInt(box.dataset.seconds, 10) || 0;
    var title = box.dataset.title || "";
    var cover = box.dataset.cover || "";
    var seekTo = null;   // applied once metadata arrives (preload="none")
    var lastSave = 0;

    // From here on the element is ours to drive.
    audio.controls = false;
    audio.preload = "none";

    var ui = document.createElement("div");
    ui.className = "player-ui";

    var play = button("pp-play", "Play", document.createTextNode("▶"));
    var back = button("pp-skip quiet", "Back 15 seconds", icon("i-back15"));
    var fwd = button("pp-skip quiet", "Forward 15 seconds", icon("i-fwd15"));

    var seek = document.createElement("input");
    seek.type = "range";
    seek.className = "pp-seek";
    seek.min = 0;
    seek.max = total > 0 ? total : 100;
    seek.step = 1;
    seek.value = 0;
    seek.setAttribute("aria-label", "Seek");
    // A range input is already keyboard- and screen-reader-shaped; the
    // one thing it cannot say for itself is that its unit is time.
    seek.setAttribute("aria-valuetext", "0:00");

    var time = document.createElement("span");
    time.className = "pp-time";
    time.textContent = "0:00 / " + (total > 0 ? clock(total) : "–:––");

    var rate = document.createElement("button");
    rate.type = "button";
    rate.className = "pp-rate quiet";
    rate.setAttribute("aria-label", "Playback speed");
    rate.title = "Playback speed";

    ui.append(play, back, fwd, seek, time, rate);

    // The resume offer is explicit rather than automatic: silently
    // starting someone eight minutes in is a worse surprise than a
    // button they can ignore.
    var resume = document.createElement("button");
    resume.type = "button";
    resume.className = "pp-resume quiet";
    resume.hidden = true;
    var resumeAt = parseFloat(get(key));
    if (isFinite(resumeAt) && resumeAt > RESUME_FLOOR &&
        (!total || resumeAt < total - RESUME_TAIL)) {
      resume.textContent = "Resume at " + clock(resumeAt);
      resume.hidden = false;
    }

    box.append(ui, resume);

    function applyRate() {
      var stored = parseFloat(get(STORE_RATE));
      var value = RATES.indexOf(stored) >= 0 ? stored : 1;
      audio.playbackRate = value;
      rate.textContent = value + "×";
    }
    applyRate();
    rate.addEventListener("click", function () {
      var next = RATES[(RATES.indexOf(audio.playbackRate) + 1) % RATES.length];
      set(STORE_RATE, String(next));
      applyRate();
    });

    function duration() {
      return isFinite(audio.duration) && audio.duration > 0 ? audio.duration : total;
    }

    function paintProgress() {
      var d = duration();
      var played = d > 0 ? (audio.currentTime / d) * 100 : 0;
      var buffered = 0;
      if (audio.buffered.length && d > 0) {
        buffered = (audio.buffered.end(audio.buffered.length - 1) / d) * 100;
      }
      // Two CSS custom properties drive the track's gradient; no layout
      // work, and the same bar shows played and buffered.
      box.style.setProperty("--played", played.toFixed(2) + "%");
      box.style.setProperty("--buffered", buffered.toFixed(2) + "%");
    }

    function paintTime() {
      var d = duration();
      time.textContent = clock(audio.currentTime) +
        " / " + (d > 0 ? clock(d) : "–:––");
      seek.setAttribute("aria-valuetext", clock(audio.currentTime));
    }

    function mediaSession() {
      if (!("mediaSession" in navigator)) return;
      try {
        navigator.mediaSession.metadata = new MediaMetadata({
          title: title,
          artwork: cover ? [{src: cover}] : [],
        });
        navigator.mediaSession.setActionHandler("play", function () { audio.play(); });
        navigator.mediaSession.setActionHandler("pause", function () { audio.pause(); });
        navigator.mediaSession.setActionHandler("seekbackward", function () { nudge(-SKIP); });
        navigator.mediaSession.setActionHandler("seekforward", function () { nudge(SKIP); });
      } catch (e) { /* unsupported: the tab still plays */ }
    }

    function nudge(delta) {
      var d = duration();
      var at = Math.max(0, Math.min(audio.currentTime + delta, d || Infinity));
      if (audio.readyState === 0) {
        seekTo = at;          // nothing loaded yet; land there on arrival
        audio.load();
      } else {
        audio.currentTime = at;
      }
      paintProgress();
      paintTime();
    }

    play.addEventListener("click", function () {
      if (audio.paused) audio.play(); else audio.pause();
    });
    back.addEventListener("click", function () { nudge(-SKIP); });
    fwd.addEventListener("click", function () { nudge(SKIP); });

    resume.addEventListener("click", function () {
      var at = parseFloat(get(key));
      if (isFinite(at)) {
        if (audio.readyState === 0) seekTo = at;
        else audio.currentTime = at;
      }
      resume.hidden = true;
      audio.play();
    });

    seek.addEventListener("input", function () {
      var at = parseFloat(seek.value);
      if (audio.readyState === 0) seekTo = at;
      else audio.currentTime = at;
      paintTime();
      paintProgress();
    });

    audio.addEventListener("loadedmetadata", function () {
      if (isFinite(audio.duration) && audio.duration > 0) {
        seek.max = audio.duration;
      }
      if (seekTo !== null) {
        audio.currentTime = seekTo;
        seekTo = null;
      }
      paintTime();
      paintProgress();
    });

    audio.addEventListener("play", function () {
      players.forEach(function (other) {
        if (other !== audio && !other.paused) other.pause();
      });
      play.textContent = "❚❚";
      play.setAttribute("aria-label", "Pause");
      play.title = "Pause";
      box.classList.add("playing");
      resume.hidden = true;
      mediaSession();
    });

    function stopped() {
      play.textContent = "▶";
      play.setAttribute("aria-label", "Play");
      play.title = "Play";
      box.classList.remove("playing");
    }
    audio.addEventListener("pause", stopped);

    audio.addEventListener("timeupdate", function () {
      if (!seek.matches(":active")) seek.value = audio.currentTime;
      paintTime();
      paintProgress();
      if (Math.abs(audio.currentTime - lastSave) >= SAVE_EVERY) {
        lastSave = audio.currentTime;
        set(key, String(Math.floor(audio.currentTime)));
      }
    });

    audio.addEventListener("progress", paintProgress);

    audio.addEventListener("ended", function () {
      stopped();
      drop(key);          // finished: nothing left to resume
      seek.value = 0;
      paintProgress();
    });

    audio.addEventListener("error", function () {
      stopped();
      time.textContent = "unavailable";
    });

    players.push(audio);
    paintProgress();
  }

  document.querySelectorAll(".player").forEach(build);
})();
