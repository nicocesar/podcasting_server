package main

import "html/template"

// The page wears the station's own clothes: the riso two-ink palette and
// Fraunces, lifted from cmd/server/static/style.css including its
// three-state theme handling, so a casting decision is made looking at
// the same surface the station is made of. The font travels as a data URI
// because this file is opened from disk, with no server to fetch /static
// from.
//
// It is a tool, not a document — operated with headphones on, one row at
// a time — so the information design carries it: one clip audible at a
// time, the playing row marked ON AIR (the one place the red ink is
// allowed), and the decision you make while listening turning directly
// into the Go the table needs.
var pageTemplate = template.Must(template.New("audition").Parse(`<!doctype html>
<html lang="{{.Language}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Casting call · {{.Language}}</title>
<style>
@font-face {
  font-family: "Fraunces";
  {{.FontCSS}}
  font-weight: 100 900;
  font-style: normal;
  font-display: swap;
}

:root {
  --bg: #f5efe2;
  --card: #fdfaf2;
  --text: #241f16;
  --muted: #6e6757;
  --border: #ddd4c0;
  --accent: #2b45c4;
  --accent-down: #1f35a3;
  --accent-ink: #ffffff;
  --live: #bc3227;
  --live-ink: #ffffff;
  --live-glow: rgba(188, 50, 39, 0.4);
  --shadow: 4px 4px 0 rgba(36, 31, 22, 0.07);
  --font-display: "Fraunces", Georgia, "Times New Roman", serif;
  --font-body: system-ui, -apple-system, "Segoe UI", sans-serif;
  --font-mono: ui-monospace, "SF Mono", "Cascadia Code", Menlo, Consolas, monospace;
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    --bg: #17130c;
    --card: #211c12;
    --text: #ede5d2;
    --muted: #a49a83;
    --border: #3a3323;
    --accent: #a3b3ff;
    --accent-down: #bcc8ff;
    --accent-ink: #141b3f;
    --live: #ff7a6b;
    --live-ink: #3d0e08;
    --live-glow: rgba(255, 122, 107, 0.35);
    --shadow: 4px 4px 0 rgba(0, 0, 0, 0.25);
  }
}
:root[data-theme="dark"] {
  --bg: #17130c;
  --card: #211c12;
  --text: #ede5d2;
  --muted: #a49a83;
  --border: #3a3323;
  --accent: #a3b3ff;
  --accent-down: #bcc8ff;
  --accent-ink: #141b3f;
  --live: #ff7a6b;
  --live-ink: #3d0e08;
  --live-glow: rgba(255, 122, 107, 0.35);
  --shadow: 4px 4px 0 rgba(0, 0, 0, 0.25);
}

* { box-sizing: border-box; }
body {
  margin: 0;
  padding: 0 1.25rem 7rem;
  background: var(--bg);
  color: var(--text);
  font: 16px/1.55 var(--font-body);
}
main { max-width: 54rem; margin: 0 auto; }

header { padding: 3rem 0 2rem; }
h1 {
  font-family: var(--font-display);
  font-weight: 600;
  font-size: clamp(2rem, 5vw, 2.9rem);
  line-height: 1.05;
  margin: 0 0 0.5rem;
  text-wrap: balance;
}
.standfirst { margin: 0; max-width: 46rem; color: var(--muted); }
.standfirst strong { color: var(--text); font-weight: 600; }

.slot { margin-top: 3rem; }
.slot-head {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 0.5rem 0.85rem;
  border-bottom: 2px solid var(--text);
  padding-bottom: 0.5rem;
}
.slot-head h2 {
  font-family: var(--font-mono);
  font-size: 1.05rem;
  font-weight: 600;
  margin: 0;
  letter-spacing: -0.01em;
}
.need {
  font-size: 0.75rem;
  text-transform: uppercase;
  letter-spacing: 0.09em;
  color: var(--muted);
}
.note { margin: 0.75rem 0 1.25rem; color: var(--muted); max-width: 44rem; }

.takes { display: flex; flex-direction: column; gap: 0.75rem; }

.take {
  display: grid;
  grid-template-columns: auto 1fr auto;
  align-items: start;
  gap: 0 1rem;
  padding: 0.9rem 1rem;
  background: var(--card);
  border: 1px solid var(--border);
  border-radius: 10px;
  box-shadow: var(--shadow);
}
.take.playing { border-color: var(--live); box-shadow: 0 0 0 3px var(--live-glow); }
.take.kept { border-color: var(--accent); }

.play {
  grid-row: 1 / span 3;
  width: 2.75rem;
  height: 2.75rem;
  border-radius: 50%;
  border: 2px solid var(--accent);
  background: transparent;
  color: var(--accent);
  font: inherit;
  font-size: 0.9rem;
  cursor: pointer;
  display: grid;
  place-items: center;
  flex: none;
}
.play:hover { background: var(--accent); color: var(--accent-ink); }
.take.playing .play { border-color: var(--live); background: var(--live); color: var(--live-ink); }
.play:focus-visible, .keep:focus-visible, .bar button:focus-visible {
  outline: 3px solid var(--accent);
  outline-offset: 2px;
}

.name { font-weight: 600; }
.tags {
  display: flex;
  flex-wrap: wrap;
  gap: 0.35rem;
  margin: 0.35rem 0;
}
.tag {
  font-size: 0.72rem;
  text-transform: uppercase;
  letter-spacing: 0.07em;
  padding: 0.1rem 0.45rem;
  border: 1px solid var(--border);
  border-radius: 999px;
  color: var(--muted);
}
.desc {
  grid-column: 2;
  margin: 0.15rem 0 0;
  font-size: 0.9rem;
  color: var(--muted);
}
.vid {
  grid-column: 2;
  margin-top: 0.5rem;
  font-family: var(--font-mono);
  font-size: 0.78rem;
  color: var(--muted);
  word-break: break-all;
}
.keep {
  grid-row: 1 / span 3;
  align-self: center;
  font: inherit;
  font-size: 0.85rem;
  padding: 0.45rem 0.9rem;
  border-radius: 999px;
  border: 1px solid var(--border);
  background: transparent;
  color: var(--muted);
  cursor: pointer;
  white-space: nowrap;
}
.keep:hover { border-color: var(--accent); color: var(--accent); }
.take.kept .keep { background: var(--accent); border-color: var(--accent); color: var(--accent-ink); }

.failed {
  margin-top: 1.25rem;
  padding: 0.9rem 1rem;
  border: 1px dashed var(--border);
  border-radius: 10px;
  font-size: 0.88rem;
  color: var(--muted);
}
.failed h3 {
  margin: 0 0 0.5rem;
  font-size: 0.75rem;
  text-transform: uppercase;
  letter-spacing: 0.09em;
  color: var(--live);
}
.failed li { margin-bottom: 0.25rem; }
.failed code { font-family: var(--font-mono); font-size: 0.8rem; }

.bar {
  position: fixed;
  left: 0; right: 0; bottom: 0;
  background: var(--card);
  border-top: 2px solid var(--text);
  padding: 0.85rem 1.25rem;
}
.bar-inner {
  max-width: 54rem;
  margin: 0 auto;
  display: flex;
  align-items: center;
  gap: 1rem;
  flex-wrap: wrap;
}
.count { font-variant-numeric: tabular-nums; }
.bar button {
  font: inherit;
  padding: 0.5rem 1rem;
  border-radius: 8px;
  border: 1px solid var(--accent);
  background: var(--accent);
  color: var(--accent-ink);
  cursor: pointer;
}
.bar button:disabled { opacity: 0.45; cursor: default; }
.bar button.ghost { background: transparent; color: var(--accent); }

.out {
  max-width: 54rem;
  margin: 0.85rem auto 0;
  padding: 0.85rem 1rem;
  background: var(--bg);
  border: 1px solid var(--border);
  border-radius: 8px;
  font-family: var(--font-mono);
  font-size: 0.8rem;
  white-space: pre;
  overflow-x: auto;
}
footer {
  margin-top: 3.5rem;
  padding-top: 1.25rem;
  border-top: 1px solid var(--border);
  font-size: 0.85rem;
  color: var(--muted);
}
@media (prefers-reduced-motion: reduce) {
  * { transition: none !important; animation: none !important; }
}
@media (max-width: 34rem) {
  .take { grid-template-columns: auto 1fr; }
  .keep { grid-row: auto; grid-column: 2; justify-self: start; margin-top: 0.6rem; }
}
</style>
</head>
<body>
<main>
  <header>
    <h1>Casting call · {{.Language}}</h1>
    <p class="standfirst">Every clip is the station's pinned narrator setting up, then the
    candidate answering — rendered through <strong>text-to-dialogue</strong> on
    <strong>eleven_v3</strong>, the same endpoint and model an episode uses. Listen for
    whether the candidate sounds like a <em>different person</em> from the narrator, not
    just whether the voice is good. Keep the ones that do; the button at the bottom writes
    the Go.</p>
  </header>

  {{range $slot := .Slots}}
  <section class="slot">
    <div class="slot-head">
      <h2>{{.Label}}</h2>
      <span class="need">role {{.Role}}{{if .Register}} · register {{.Register}}{{end}}</span>
    </div>
    <p class="note">{{.Note}}</p>

    <div class="takes">
      {{range .Takes}}
      <div class="take" data-voice="{{.VoiceID}}" data-name="{{.CleanName}}"
           data-role="{{$slot.Role}}" data-register="{{.Gender}}">
        <button class="play" aria-label="Play {{.CleanName}}">▶</button>
        <div>
          <div class="name">{{.Name}}</div>
          <div class="tags">
            {{with .Age}}<span class="tag">{{.}}</span>{{end}}
            {{with .Gender}}<span class="tag">{{.}}</span>{{end}}
            {{with .Accent}}<span class="tag">{{.}}</span>{{end}}
            {{with .Descriptive}}<span class="tag">{{.}}</span>{{end}}
            {{with .UseCase}}<span class="tag">{{.}}</span>{{end}}
          </div>
        </div>
        <button class="keep" aria-pressed="false">Keep</button>
        {{with .Description}}<p class="desc">{{.}}</p>{{end}}
        <div class="vid">{{.VoiceID}}</div>
        <audio preload="none" src="{{.Audio}}"></audio>
      </div>
      {{end}}
    </div>

    {{if .Failed}}
    <div class="failed">
      <h3>Refused by text-to-dialogue</h3>
      <ul>
        {{range .Failed}}<li>{{.Name}} — <code>{{.Err}}</code></li>{{end}}
      </ul>
      <p style="margin:0.5rem 0 0">Not candidates: the story path can only cast what this
      endpoint will render.</p>
    </div>
    {{end}}
  </section>
  {{end}}

  <footer>
    <p>Generated {{.Generated}} · {{.Cost}} characters billed to ElevenLabs ·
    candidates already in <code>storyVoices</code> are excluded.</p>
  </footer>
</main>

<div class="bar">
  <div class="bar-inner">
    <span class="count" id="count">Nothing kept yet</span>
    <button id="copy" disabled>Copy Go lines</button>
    <button id="show" class="ghost" disabled>Show</button>
  </div>
  <pre class="out" id="out" hidden></pre>
</div>

<script>
// Carried in the page rather than read off <html lang>: the page is also
// published as an artifact, where the surrounding document is not ours.
const LANG = "{{.Language}}";

// One clip at a time. Two voices talking over each other is the opposite
// of an audition.
let playing = null;

function stop() {
  if (!playing) return;
  playing.audio.pause();
  playing.audio.currentTime = 0;
  playing.row.classList.remove("playing");
  playing.row.querySelector(".play").textContent = "▶";
  playing = null;
}

document.querySelectorAll(".take").forEach((row) => {
  const audio = row.querySelector("audio");
  const play = row.querySelector(".play");
  const keep = row.querySelector(".keep");

  play.addEventListener("click", () => {
    const wasPlaying = playing && playing.row === row;
    stop();
    if (wasPlaying) return;
    playing = {row, audio};
    row.classList.add("playing");
    play.textContent = "■";
    audio.play();
  });
  audio.addEventListener("ended", stop);

  keep.addEventListener("click", () => {
    const kept = row.classList.toggle("kept");
    keep.setAttribute("aria-pressed", String(kept));
    keep.textContent = kept ? "Kept" : "Keep";
    refresh();
  });
});

function goLines() {
  return [...document.querySelectorAll(".take.kept")].map((row) =>
    '\t{Role: "' + row.dataset.role +
    '", Language: "' + LANG +
    '", Gender: "' + row.dataset.register +
    '", Eleven: "' + row.dataset.voice +
    '", Name: "' + row.dataset.name + '"},'
  ).join("\n");
}

function refresh() {
  const n = document.querySelectorAll(".take.kept").length;
  document.getElementById("count").textContent =
    n === 0 ? "Nothing kept yet" : n + (n === 1 ? " voice kept" : " voices kept");
  document.getElementById("copy").disabled = n === 0;
  document.getElementById("show").disabled = n === 0;
  const out = document.getElementById("out");
  if (!out.hidden) out.textContent = goLines();
}

document.getElementById("copy").addEventListener("click", async () => {
  const btn = document.getElementById("copy");
  try {
    await navigator.clipboard.writeText(goLines());
    btn.textContent = "Copied";
  } catch (e) {
    // Clipboard is blocked in some contexts; showing the text is the
    // fallback that always works.
    document.getElementById("out").hidden = false;
    btn.textContent = "Select below";
  }
  refresh();
  setTimeout(() => { btn.textContent = "Copy Go lines"; }, 2000);
});

document.getElementById("show").addEventListener("click", () => {
  const out = document.getElementById("out");
  out.hidden = !out.hidden;
  document.getElementById("show").textContent = out.hidden ? "Show" : "Hide";
  refresh();
});
</script>
</body>
</html>
`))
