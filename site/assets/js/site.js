"use strict";

// Everything here is an enhancement: without JavaScript the commands are plain selectable text
// (a <noscript> style hides the Copy buttons) and every section is in place.

// The docs mark each command and file you can copy; give each one the homepage's Copy button.
const COPY_ICONS = '<svg class="icon copy-idle" viewBox="0 0 24 24" aria-hidden="true"><rect x="8.5" y="8.5" width="11" height="11" rx="2.5"/>'
  + '<path d="M15.5 8.5v-2a2 2 0 0 0-2-2h-7a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h2"/></svg>'
  + '<svg class="icon copy-done" viewBox="0 0 24 24" aria-hidden="true"><path d="M5 12.5l4.5 4.5L19 7.5"/></svg>';
for (const field of document.querySelectorAll(".cmds li, .code-file")) {
  const source = field.querySelector("[data-copy-source]");
  const name = field.querySelector(".code-file-name");
  const button = Object.assign(document.createElement("button"), { className: "copy", type: "button", innerHTML: COPY_ICONS });
  button.dataset.copy = "";
  button.setAttribute("aria-label", `Copy ${source.dataset.copyLabel || (name ? name.textContent : source.textContent.trim())}`);
  const status = Object.assign(document.createElement("span"), { className: "copy-status" });
  status.setAttribute("role", "status");
  status.dataset.copyStatus = "";
  field.append(button, status);
}

// Copy buttons: "Copied" for two seconds. On failure, select the command and say so next to it until
// the next attempt.
for (const button of document.querySelectorAll("[data-copy]")) {
  const field = button.parentElement;
  const source = field.querySelector("[data-copy-source]");
  const status = field.querySelector("[data-copy-status]");
  let reset = 0;
  const clear = () => {
    clearTimeout(reset);
    button.classList.remove("is-done");
    field.classList.remove("copy-failed");
    status.textContent = "";
  };
  field.addEventListener("copy-reset", clear);
  button.addEventListener("click", async () => {
    const command = source.textContent.replace(/^\$\s*/, "").trim();
    clear();
    try {
      await navigator.clipboard.writeText(command);
      document.dispatchEvent(new CustomEvent("coop:copy-success", {
        detail: {
          kind: source.dataset.copyKind || field.dataset.copyKind ||
            (field.classList.contains("code-file") ? "configuration" : "command"),
        },
      }));
      button.classList.add("is-done");
      status.textContent = "Copied.";
      reset = setTimeout(() => {
        button.classList.remove("is-done");
        status.textContent = "";
      }, 2000);
    } catch {
      getSelection().selectAllChildren(source);
      field.classList.add("copy-failed");
      status.textContent = "Copy blocked. Press Cmd+C or Ctrl+C.";
    }
  });
}

// Platform output tabs. Without JavaScript, both labeled examples remain visible.
for (const group of document.querySelectorAll("[data-terminal-outputs]")) {
  const panels = [...group.querySelectorAll("[data-output-platform]")];
  const list = document.createElement("div");
  list.className = "terminal-output-tabs";
  list.setAttribute("role", "tablist");
  list.setAttribute("aria-label", "Example output by operating system");
  const buttons = panels.map((panel) => {
    const button = document.createElement("button");
    button.type = "button";
    button.id = `${panel.id}-tab`;
    button.textContent = panel.dataset.outputPlatform;
    button.setAttribute("role", "tab");
    button.setAttribute("aria-controls", panel.id);
    panel.setAttribute("role", "tabpanel");
    panel.setAttribute("aria-labelledby", button.id);
    panel.tabIndex = 0;
    list.append(button);
    return button;
  });
  const select = (index) => {
    buttons.forEach((button, i) => {
      button.setAttribute("aria-selected", String(i === index));
      button.tabIndex = i === index ? 0 : -1;
      panels[i].hidden = i !== index;
    });
  };
  buttons.forEach((button, index) => {
    button.addEventListener("click", () => select(index));
    button.addEventListener("keydown", (event) => {
      let next;
      if (event.key === "ArrowRight") next = (index + 1) % buttons.length;
      else if (event.key === "ArrowLeft") next = (index + buttons.length - 1) % buttons.length;
      else if (event.key === "Home") next = 0;
      else if (event.key === "End") next = buttons.length - 1;
      else return;
      event.preventDefault();
      select(next);
      buttons[next].focus();
    });
  });
  group.closest(".terminal-example").prepend(list);
  group.dataset.outputTabsReady = "";
  const platform = navigator.userAgentData?.platform || navigator.platform || navigator.userAgent;
  const preferred = /linux/i.test(platform) ? "Linux" : "macOS";
  const initial = panels.findIndex((panel) => panel.dataset.outputPlatform === preferred);
  select(initial < 0 ? 0 : initial);
}

// The agent switcher rewrites the sign-in and start commands (the Orchestrator page starts a loop);
// without JavaScript they stay on Claude.
const agentButtons = [...document.querySelectorAll("[data-agent]")];
const pick = (button) => {
  if (button.getAttribute("aria-checked") === "true") return;
  const agent = button.dataset.agent;
  for (const other of agentButtons) {
    other.setAttribute("aria-checked", String(other === button));
    other.tabIndex = other === button ? 0 : -1;
  }
  for (const [step, command] of [["login", `coop login ${agent}`], ["run", `coop ${agent}`], ["loop", `coop loop ${agent}`]]) {
    const cmd = document.querySelector(`[data-cmd="${step}"]`);
    if (!cmd) continue;
    cmd.textContent = command;
    cmd.closest(".command").dispatchEvent(new Event("copy-reset"));
  }
  if (matchMedia("(prefers-reduced-motion: no-preference)").matches) {
    for (const el of document.querySelectorAll("[data-cmd]")) {
      for (const running of el.getAnimations()) running.cancel();
      el.animate([{ opacity: 0 }, { opacity: 1 }], { duration: 200, easing: "ease-out" });
    }
  }
};
for (const button of agentButtons) {
  button.addEventListener("click", () => pick(button));
  // Arrow keys move the choice, as in any radio group; Home and End jump to the ends.
  button.addEventListener("keydown", (event) => {
    const i = agentButtons.indexOf(button);
    const next = { ArrowRight: i + 1, ArrowDown: i + 1, ArrowLeft: i - 1, ArrowUp: i - 1, Home: 0, End: agentButtons.length - 1 }[event.key];
    if (next === undefined) return;
    event.preventDefault();
    const target = agentButtons[(next + agentButtons.length) % agentButtons.length];
    pick(target);
    target.focus();
  });
}

// The story: on wide screens each step adds its layer to the pinned picture when the step reaches
// the middle of the screen. The inline script in <head> set the starting state before the first
// paint; below 1024px, or without this script, the picture stays finished.
const stage = document.querySelector(".stage");
const wide = matchMedia("(min-width: 1024px)");
if (stage && "IntersectionObserver" in window) {
  // A chapter of attacks keeps its text pinned while its attacks take turns under it. Scroll marks
  // spread over the chapter say whose turn it is; mark 0 is the chapter's opening, before the first.
  const chapters = [...document.querySelectorAll(".step-group")].map((group) => {
    const items = [...group.querySelectorAll(".attack")];
    // Each case's figure sits above the rail while its case is on screen, never inside the case's
    // block. A chapter gives every case a figure or none, since a case without one would leave the
    // space empty. The cases keep their own copies for the list.
    const own = items.map((item) => item.querySelector(":scope > .evidence"));
    const figures = own.every(Boolean) ? own.map((figure) => figure.cloneNode(true)) : [];
    if (figures.length) {
      const slot = Object.assign(document.createElement("div"), { className: "case-figures" });
      slot.append(...figures);
      group.querySelector(".attack-rail").before(slot);
    }
    const beats = [null, ...items].map((item, turn) => {
      const beat = document.createElement("span");
      beat.className = "beat";
      beat.style.setProperty("--k", turn);
      beat.dataset.turn = turn;
      beat.dataset.attack = item?.dataset.attack ?? "";
      beat.setAttribute("aria-hidden", "true");
      return group.appendChild(beat);
    });
    return { group, items, beats, figures };
  });
  const parts = [document.querySelector(".story-lead"), ...document.querySelectorAll(".step, .beat")];
  for (const chapter of chapters) [chapter.first, chapter.last] = [parts.indexOf(chapter.beats[0]), parts.indexOf(chapter.beats.at(-1))];
  // Steps 1 to 4 add the layers; every part after them keeps the layers on and may name an attack.
  const show = (step) => {
    for (let i = 1; i <= 4; i++) stage.classList.toggle(`s${i}`, i <= step);
    stage.dataset.attack = parts[step].dataset.attack ?? "";
    for (const { group, items, figures, first, last } of chapters) {
      // before a chapter nothing has played; past it, its last attack stays and scrolls away with it
      const turn = step < first ? 0 : step > last ? items.length : Number(parts[step].dataset.turn);
      group.dataset.at = turn;
      items.forEach((item, i) => {
        item.classList.toggle("is-active", i + 1 === turn);
        item.classList.toggle("is-past", i + 1 < turn);
      });
      // the chapter opens on its first case's figure, as the rail opens on its first case
      figures.forEach((figure, i) => figure.classList.toggle("is-active", i + 1 === Math.max(turn, 1)));
      // the rail runs the length of the attack on screen
      const shown = items[Math.max(turn, 1) - 1];
      group.querySelector(".attack-rail").style.setProperty("--rail", `${shown.offsetTop + shown.offsetHeight}px`);
    }
  };
  let current = 0;
  const watch = new IntersectionObserver((entries) => {
    for (const entry of entries) if (entry.isIntersecting) show((current = parts.indexOf(entry.target)));
  }, { rootMargin: "-50% 0px -50% 0px" });
  // a new width rewraps the attacks: measure the rail again
  addEventListener("resize", () => wide.matches && show(current));

  // Small screens: the story as a deck (the stylesheet's .js-deck block). Its cards are the steps,
  // each chapter's opening and the attacks; the card on screen is the last one whose top has reached
  // the picture's floor. The picture takes that card's state, and a camera frames what the card is
  // about: the whole picture, the project, the wires, or the entry an attack goes for and the wall
  // that stops it. A screen too short for both keeps the plain page.
  const roomy = matchMedia("(max-width: 1023px) and (min-height: 500px)");
  const scene = stage.querySelector(".scene");
  const SHOTS = {
    all: [".box", ".bus"], box: [".box"], zone: [".zone"], wires: [".secret", ".bus"],
    ssh: ['[data-hit="ssh"] > .row', ".zone > .row"], docs: ['[data-hit="docs"] > .row', ".zone > .row"],
    billing: ['[data-hit="billing"] > .row', ".bus"], aws: ['[data-hit="aws"] > .row', ".bus"],
  };
  const STEP_SHOTS = ["box", "box", "zone", "wires", "wires"];
  const ATTACK_SHOTS = { ssh: "ssh", issue: "billing", key: "wires", env: "wires", wipe: "docs", prod: "aws", push: "wires", supply: "aws" };
  const steps = [...document.querySelectorAll(".story-steps > .step:not(.step-chapter)")];
  // where a row of the picture sits in the scene, untouched by the camera's transform
  const place = (el) => {
    let [x, y] = [0, 0];
    for (let node = el; node && node !== scene; node = node.offsetParent) [x, y] = [x + node.offsetLeft, y + node.offsetTop];
    return [x, y, x + el.offsetWidth, y + el.offsetHeight];
  };
  const aim = (shot) => {
    const boxes = SHOTS[shot].map((selector) => place(scene.querySelector(selector)));
    const [x0, y0, x1, y1] = boxes.reduce((a, b) => [Math.min(a[0], b[0]), Math.min(a[1], b[1]), Math.max(a[2], b[2]), Math.max(a[3], b[3])]);
    const [w, h, pad] = [stage.clientWidth, stage.clientHeight, 16];
    // a phone's frame zooms in a little; a tablet's, more
    const s = Math.min(Math.max(1.2, Math.min(1.6, w / 500)), (w - 2 * pad) / (x1 - x0), (h - 2 * pad) / (y1 - y0));
    scene.style.transform = `translate(${(w - (x1 - x0) * s) / 2 - x0 * s}px, ${(h - (y1 - y0) * s) / 2 - y0 * s}px) scale(${s})`;
  };
  let cards = [], intros = [], onCard = null, queued = false;
  const turn = () => {
    queued = false;
    // until the picture pins, the sandbox stays open: the first card sits right under it
    const { top, bottom } = stage.getBoundingClientRect();
    const k = top > 1 ? -1 : cards.findLastIndex((card) => card.getBoundingClientRect().top <= bottom + 1);
    if (k === onCard) return;
    onCard = k;
    const card = cards[k];
    for (let i = 1; i <= 4; i++) stage.classList.toggle(`s${i}`, i <= (card?.deck.layers ?? 0));
    stage.dataset.attack = card?.dataset.attack ?? "";
    aim(card?.deck.shot ?? STEP_SHOTS[0]);
  };
  const onScroll = () => queued || (queued = requestAnimationFrame(turn));
  // the camera's frame and each card's height (where a tall card stops) change with the screen
  const fit = () => {
    for (const card of cards) card.style.setProperty("--h", `${card.offsetHeight}px`);
    onCard = null;
    turn();
  };
  const deck = (on) => {
    document.documentElement.classList.toggle("js-deck", on);
    if (on && !cards.length) {
      // a chapter's heading and opening line make one card
      for (const heading of document.querySelectorAll(".group-panel > h2")) {
        const intro = Object.assign(document.createElement("div"), { className: "deck-intro" });
        const line = heading.nextElementSibling;
        heading.before(intro);
        intro.append(heading, line);
        intros.push(intro);
      }
      // an attack is one of its chapter's cases: ticks count which, as the rail does on wide screens
      for (const list of document.querySelectorAll(".story-steps .attacks")) {
        const cases = [...list.children];
        cases.forEach((item, i) => {
          const ticks = Object.assign(document.createElement("div"), { className: "deck-ticks" });
          ticks.setAttribute("aria-hidden", "true");
          ticks.append(...cases.map((_, k) => Object.assign(document.createElement("span"), { className: k <= i ? "on" : "" })));
          item.prepend(ticks);
        });
      }
      cards = [...document.querySelectorAll(".story-steps .step, .deck-intro, .story-steps .attack")];
      for (const card of cards) {
        const step = steps.indexOf(card) + 1;
        card.deck = { layers: step || 4, shot: step ? STEP_SHOTS[step] : ATTACK_SHOTS[card.dataset.attack] ?? "all" };
      }
      // the first frame is placed, not flown to
      scene.style.transition = "none";
      fit();
      void scene.offsetWidth;
      scene.style.transition = "";
      addEventListener("scroll", onScroll, { passive: true });
      addEventListener("resize", fit);
      document.fonts?.ready.then(() => cards.length && fit());
    } else if (!on && cards.length) {
      removeEventListener("scroll", onScroll);
      removeEventListener("resize", fit);
      for (const intro of intros) intro.replaceWith(...intro.childNodes);
      for (const ticks of document.querySelectorAll(".deck-ticks")) ticks.remove();
      for (const card of cards) card.style.removeProperty("--h");
      [cards, intros, onCard] = [[], [], null];
      scene.style.transform = "";
    }
  };

  const follow = () => {
    document.documentElement.classList.toggle("js-story", wide.matches);
    if (wide.matches) parts.forEach((part) => watch.observe(part));
    else watch.disconnect();
    deck(!wide.matches && roomy.matches);
  };
  wide.addEventListener("change", follow);
  roomy.addEventListener("change", follow);
  follow();
  document.documentElement.classList.add("story-ready");
}

// The feature scenes are drawn finished. Where motion is allowed and a scene has room to show whole,
// it steps back to where it started just before it scrolls into view, and replays the rest, once,
// when it is on screen. A scene nobody scrolls to (or prints) stays finished.
const sleep = (ms) => new Promise((done) => setTimeout(done, ms));

// The loop's live bar (internal/loop/bar.go): spinner, 20 cells (done cyan, active yellow), counts,
// what it is on, and the attempt's elapsed time. A window narrower than any terminal the CLI draws in
// (a phone) gets fewer cells.
const SPIN = [".[  ]", ">[  ]", "[.  ]", "[ * ]", "[  .]", "[  ]>", "[  ]."];
const CORNER = ["◰", "◳", "◲", "◱"]; // the one-column spinner coop tasks watch uses
const barCells = (done, doing, total, cells = 20) => {
  const d = Math.round((cells * done) / total), a = Math.round((cells * (done + doing)) / total) - d;
  return `<span class="bar-done">${"█".repeat(d)}</span><span class="bar-doing">${"█".repeat(a)}</span>${"░".repeat(cells - d - a)}`;
};

// A usage meter as `coop usage` draws it: 10 cells, cyan, yellow from 80%, red at 100%.
const meter = (used) => {
  const full = Math.round(used / 10), tone = used >= 100 ? "full" : used >= 80 ? "high" : "ok";
  return `<span class="meter-used ${tone}">${"█".repeat(full)}</span>${"░".repeat(10 - full)}`;
};
const fill = (slot, used, state = "") => {
  slot.querySelector(".meter").innerHTML = meter(used);
  slot.querySelector(".pct").textContent = `${used}%`;
  slot.className = `slot ${state}`.trim();
  slot.querySelector(".state").textContent = { spent: "limit reached", live: "running" }[state] ?? "";
};

const SCENES = {
  watch: {
    // the board redraws as the loop works: subtasks tick, task after task lands in done; then the
    // finished tasks come back and it starts over. It runs only while it is on screen.
    fits: (card) => card.clientWidth >= 280,
    reset(card) {
      card.querySelector("code").innerHTML = card.querySelector("template").innerHTML;
    },
    async play(card) {
      const code = card.querySelector("code");
      const frames = [...card.querySelectorAll("template")].map((frame) => frame.innerHTML);
      let visible = true, spin = 0;
      new IntersectionObserver(([entry]) => { visible = entry.isIntersecting; }).observe(card);
      setInterval(() => {
        if (!visible) return;
        spin++;
        for (const mark of code.querySelectorAll(".spin")) mark.textContent = CORNER[spin % CORNER.length];
      }, 160);
      for (let next = 1; ; next = (next + 1) % frames.length) {
        do await sleep(1000); while (!visible);
        code.innerHTML = frames[next];
      }
    },
  },
  loop: {
    fits: (card) => card.clientWidth >= 280,
    reset(card) {
      for (const line of card.querySelectorAll(".line[data-beat]")) {
        line.classList.toggle("is-pending", line.dataset.beat !== "0");
        // a command is typed out when its turn comes; the first prompt waits, empty, for it
        if (line.classList.contains("cmdline")) {
          line.dataset.command ??= line.lastChild.textContent;
          line.lastChild.textContent = "";
        }
      }
      card.querySelector("code").scrollTop = 0;
    },
    async play(card) {
      const bar = card.querySelector(".live-bar"); // only the loop has one
      // as in a terminal, output fills the window from the top, then scrolls to keep the newest line
      const code = card.querySelector("code");
      const follow = () => { code.scrollTop = code.scrollHeight; };
      const beats = [];
      for (const line of card.querySelectorAll(".line[data-beat]")) (beats[line.dataset.beat] ??= []).push(line);
      let state = null, spin = 0, seconds = 0;
      // like the CLI, the bar fits the terminal's width by shortening what it is on
      card.classList.add("is-playing");
      follow();
      let columns = 0;
      if (bar) {
        const probe = Object.assign(document.createElement("span"), { textContent: "0".repeat(40) });
        bar.append(probe);
        columns = Math.floor(bar.clientWidth / (probe.getBoundingClientRect().width / 40));
        probe.remove();
      }
      const draw = () => {
        if (!bar || !state) return;
        const [done, doing, on] = state;
        const time = `${String(Math.floor(seconds / 60)).padStart(2, "0")}:${String(seconds % 60).padStart(2, "0")}`;
        // with no room left for it, the CLI drops what it is on rather than show a letter of it; still
        // too narrow, the bar takes fewer cells, so the line never runs past the window
        const fixed = `.[  ] [] ${done}/3 done · ${time}`.length, cells = Math.max(4, Math.min(20, columns - fixed));
        const room = columns - fixed - cells - 3;
        const fit = room < 2 ? "" : on.length > room ? `${on.slice(0, room - 1)}… · ` : `${on} · `;
        bar.innerHTML = `${SPIN[spin % SPIN.length]} [${barCells(done, doing, 3, cells)}] ${done}/3 done · ${fit}${time}`;
      };
      const ticker = setInterval(() => { spin++; seconds += 2; draw(); }, 110);
      // paced to be read: a command types out, its output scrolls in a line at a time, and each beat
      // stays up long enough to read before the next (a ✓ result a little longer); a beat can move the bar
      for (const lines of beats) {
        const next = lines[0].dataset.bar;
        if (next) {
          const [done, doing, ...on] = next.split(",");
          state = [Number(done), Number(doing), on.join(",")];
          seconds = 0;
          draw();
        }
        let read = 0;
        for (const line of lines) {
          line.classList.remove("is-pending");
          follow();
          if (line.dataset.command) {
            await sleep(500);
            for (const char of line.dataset.command) { line.lastChild.textContent += char; follow(); await sleep(35); }
          } else {
            if (!line.classList.contains("rule")) read += line.textContent.length;
            await sleep(60);
          }
        }
        await sleep(Math.min(500 + read * 20, 3000) + (lines.some((line) => line.querySelector(".ok")) ? 900 : 0));
      }
      await sleep(600);
      clearInterval(ticker);
      card.classList.remove("is-playing");
      follow();
    },
  },
  rotate: {
    reset(card) {
      const [work, home, ...rest] = card.querySelectorAll(".slot");
      fill(work, 60, "live");
      fill(home, 0);
      for (const slot of rest) fill(slot, 0);
    },
    async play(card) {
      const [work, home] = card.querySelectorAll(".slot");
      for (let used = 70; used <= 100; used += 10) { await sleep(260); fill(work, used, "live"); }
      await sleep(350);
      fill(work, 100, "spent");
      fill(home, 0, "live");
      for (let used = 10; used <= 40; used += 10) { await sleep(260); fill(home, used, "live"); }
    },
  },
  team: {
    // the lead keeps working its team: it consults or hands work to one role, then another, picked
    // at random (never the same twice running), for as long as the chart is on screen
    async play(card) {
      const org = card.querySelector(".org");
      const roles = [...card.querySelectorAll(".org-roles .seat")].map((seat) => seat.dataset.role);
      let visible = true, last = "";
      new IntersectionObserver(([entry]) => { visible = entry.isIntersecting; }).observe(card);
      for (;;) {
        do await sleep(700); while (!visible);
        const choices = roles.filter((role) => role !== last);
        const role = (last = choices[Math.floor(Math.random() * choices.length)]);
        const seat = card.querySelector(`[data-role="${role}"]`);
        org.dataset.asked = role;
        seat.classList.add("is-asked");
        await sleep(1400);
        delete org.dataset.asked;
        seat.classList.remove("is-asked");
      }
    },
  },
  services: {
    reset(card) {
      for (const svc of card.querySelectorAll(".svc")) svc.classList.add("is-pending");
    },
    async play(card) {
      for (const svc of card.querySelectorAll(".svc")) {
        await sleep(450);
        svc.classList.replace("is-pending", "is-starting");
        await sleep(700);
        svc.classList.remove("is-starting");
      }
    },
  },
};

SCENES.fork = SCENES.doctor = SCENES.loop; // the same replay, without a live bar

if ("IntersectionObserver" in window && matchMedia("(prefers-reduced-motion: no-preference)").matches) {
  const prepare = (card) => {
    if (card.dataset.staged) return;
    card.dataset.staged = "yes";
    SCENES[card.dataset.scene].reset?.(card);
  };
  // half a screen before a scene arrives, it steps back; once it is a fifth of the way up, it plays
  const near = new IntersectionObserver((entries) => {
    for (const { target, isIntersecting } of entries) if (isIntersecting) { near.unobserve(target); prepare(target); }
  }, { rootMargin: "0px 0px 50% 0px" });
  const seen = new IntersectionObserver((entries) => {
    for (const { target, isIntersecting } of entries) {
      if (!isIntersecting) continue;
      seen.unobserve(target);
      prepare(target);
      SCENES[target.dataset.scene].play(target);
    }
  }, { rootMargin: "0px 0px -20% 0px" });
  for (const card of document.querySelectorAll("[data-scene]")) {
    const scene = SCENES[card.dataset.scene];
    if (!scene || (scene.fits && !scene.fits(card))) continue;
    near.observe(card);
    seen.observe(card);
  }
}

// A deep link lands before the web font arrives, and the text that reflows above its target pushes
// the target away: once the font is in, land on it again.
const target = location.hash && document.getElementById(decodeURIComponent(location.hash.slice(1)));
if (target && document.fonts) document.fonts.ready.then(() => target.scrollIntoView({ behavior: "instant" }));

// Make local overflow discoverable without suggesting a scroll when everything fits.
// Without JS, the hints and keyboard-accessible regions remain available.
for (const frame of document.querySelectorAll(".reference-table, .reference-code-frame")) {
  const scroller = frame.querySelector(".table-scroll, pre.code");
  const hint = frame.querySelector(".reference-scroll-hint");
  const update = () => { hint.hidden = scroller.scrollWidth <= scroller.clientWidth + 1; };
  update();
  if ("ResizeObserver" in window) new ResizeObserver(update).observe(scroller);
  if (document.fonts) document.fonts.ready.then(update);
}

// The docs' contents: on wide screens they stay open beside the text and mark the section being read;
// on narrower ones they fold into a box above it, which closes again once you pick a section.
const side = document.querySelector(".docs-side");
if (side) {
  const besideText = matchMedia("(min-width: 1280px)");
  const fold = () => { side.open = besideText.matches; };
  besideText.addEventListener("change", fold);
  fold();
  const links = new Map([...side.querySelectorAll('a[href^="#"]')].map((link) => [link.hash.slice(1), link]));
  for (const link of links.values()) link.addEventListener("click", () => { if (!besideText.matches) side.open = false; });
  // Only one group of the contents is open at a time, so the list stays shorter than a laptop's
  // window: the group holding the section being read, until you open another by its name.
  const groups = [...side.querySelectorAll(".side-group")].map((label, i) => {
    const list = label.nextElementSibling;
    list.id ||= `contents-${i}`;
    const button = Object.assign(document.createElement("button"), { type: "button", textContent: label.textContent });
    button.setAttribute("aria-controls", list.id);
    label.replaceChildren(button);
    return { button, list };
  });
  const show = (open) => {
    for (const group of groups) {
      group.button.setAttribute("aria-expanded", String(group === open));
      group.list.hidden = group !== open;
    }
  };
  for (const group of groups) group.button.addEventListener("click", () => show(group.list.hidden ? group : null));
  const groupOf = (link) => groups.find((group) => group.list.contains(link));
  let readingGroup = groupOf(links.get(decodeURIComponent(location.hash.slice(1)))) ?? groups[0];
  show(readingGroup);
  if ("IntersectionObserver" in window) {
    const reading = new IntersectionObserver((entries) => {
      for (const { target, isIntersecting } of entries) {
        if (!isIntersecting || !links.has(target.id)) continue;
        for (const [id, link] of links) {
          if (id === target.id) link.setAttribute("aria-current", "location");
          else link.removeAttribute("aria-current");
        }
        // reading on into another group opens it in place of the last
        const group = groupOf(links.get(target.id));
        if (group !== readingGroup) show((readingGroup = group));
        // keep the marked entry in sight in the contents' own scroll, without moving the page
        if (!besideText.matches) continue;
        const item = links.get(target.id).getBoundingClientRect(), pane = side.getBoundingClientRect();
        // the list fades out over its last 32px, so stop short of that
        const top = pane.top + side.querySelector("summary").offsetHeight, bottom = Math.min(pane.bottom, innerHeight) - 32;
        if (item.top < top) side.scrollTop += item.top - top;
        else if (item.bottom > bottom) side.scrollTop += item.bottom - bottom;
      }
    }, { rootMargin: "0px 0px -70% 0px" });
    for (const section of document.querySelectorAll(".doc-section[id], .reference-content h3[id]")) reading.observe(section);
  }
}

// The docs menu: a column on wide screens, always open. On narrower ones it opens over the page from
// a bar at the top of the screen; a choice, Escape or a tap outside closes it again.
const menu = document.querySelector(".docs-nav");
if (menu) {
  const rail = menu.parentElement, list = menu.querySelector("nav"), current = menu.querySelector("[aria-current]");
  // keep this page's entry in sight in the list's own scroll, without moving the page: its group's
  // name lands at the top of the list, so the list never opens on half a row
  const showCurrent = () => {
    if (!current || !menu.open) return;
    const item = current.getBoundingClientRect(), pane = list.getBoundingClientRect(), bottom = Math.min(pane.bottom, innerHeight);
    if (item.top >= pane.top && item.bottom <= bottom - 32) return;
    const pages = current.parentElement.closest("li") ? current.parentElement.closest("li").closest("ul") : current.closest("ul");
    list.scrollTop += pages.previousElementSibling.getBoundingClientRect().top - pane.top - 16;
  };
  const fold = () => {
    menu.open = wide.matches;
    showCurrent();
  };
  wide.addEventListener("change", fold);
  fold();
  menu.addEventListener("toggle", showCurrent);
  // the bar shows its bottom edge only while it's pinned over the text
  new IntersectionObserver(([entry]) => rail.classList.toggle("stuck", entry.intersectionRatio < 1), { rootMargin: "-1px 0px 0px 0px", threshold: 1 }).observe(rail);
  menu.addEventListener("click", (event) => {
    if (!wide.matches && (event.target === menu || event.target.closest("a"))) menu.open = false;
  });
  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape" || wide.matches || !menu.open) return;
    menu.open = false;
    menu.querySelector("summary").focus();
  });
}

// The docs' search runs here in the browser, over the generated index of every section. The index is
// fetched the first time someone opens search. Without JavaScript the button stays hidden and the
// menu still reaches every page.
const searchButton = document.querySelector("[data-docs-search]");
if (searchButton && window.HTMLDialogElement) {
  const dialog = document.createElement("dialog");
  dialog.className = "docs-search";
  dialog.setAttribute("aria-label", "Search the docs");
  dialog.innerHTML = '<div class="docs-search-field"><svg class="icon" viewBox="0 0 24 24" aria-hidden="true" focusable="false"><circle cx="11" cy="11" r="6.5"/><path d="M16 16l4.5 4.5"/></svg>'
    + '<input type="search" aria-label="Search the docs" placeholder="Search the docs" autocomplete="off" spellcheck="false" enterkeyhint="go">'
    + '<button class="docs-search-close" type="button" aria-label="Close search"><svg class="icon" viewBox="0 0 24 24" aria-hidden="true" focusable="false"><path d="M6 6l12 12M18 6L6 18"/></svg></button></div>'
    + '<p class="docs-search-status" role="status"></p><ul class="docs-search-results" role="list"></ul>';
  document.body.append(dialog);
  const input = dialog.querySelector("input"), status = dialog.querySelector("[role=status]"), results = dialog.querySelector("ul");
  const plain = (text) => text.toLowerCase().replace(/’/g, "'");
  let entries = null, averageLength = 1, loading = null;

  // Words too common to say what someone is after, including what people call the docs themselves. A
  // query of nothing else still searches for them.
  const COMMON = new Set("a add an and are can do does doc docs documentation for from guide guides how i in is it manual my of on one or page pages run the three to tutorial two up use using what when where why with you your".split(" "));
  // What readers type for what the docs say another way.
  const ALSO = { setup: "set up", subcommand: "command", subcommands: "commands" };
  const escaped = (word) => word.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  // A match starts at the beginning of a word, so "zed" isn't in "authorized", and ignores the word's
  // ending, so "migration" finds "migrating". Typing "net" already finds "networking". A short root
  // keeps its letters: "prove" must not become "prov", which is most of "provider".
  const wordRoot = (word) => {
    const single = word.length > 3 ? word.replace(/(?<=[sxz]|ch|sh)es$|s$/, "") : word;
    const root = single.replace(/(?:ication|ation|ion|ing|ed)$/, "");
    const stem = root.length >= 4 ? root : single;
    return stem.length >= 6 ? stem.replace(/[ey]$/, "") : stem;
  };
  const termsOf = (query) => {
    const words = query.split(/\s+/).flatMap((word) => (ALSO[word] ?? word).split(" "));
    const meaningful = words.filter((word) => !COMMON.has(word));
    return (meaningful.length ? meaningful : words).map((word) => ({
      root: wordRoot(word),
      start: new RegExp("(?<![a-z0-9])" + escaped(wordRoot(word)), "g"),
      whole: new RegExp("(?<![a-z0-9])" + escaped(word) + "(?![a-z0-9])"),
    }));
  };
  // BM25, the usual ranking for text search: a rare word counts for more than a common one, and saying
  // a word more often counts for more, less so in a long section. A heading counts for much more
  // than the text. A section with every word beats one with only some, and one with some still shows,
  // since nobody knows which words the docs use. A page named by the query comes first, then a command
  // it names. Upgrade notes describe old versions, the worker API is for integrators, the overview
  // summarizes, and the command and settings references mention every topic in passing, so the pages
  // about the topic come before them.
  const rank = (terms, query) => {
    const rows = entries.map((entry) => ({ entry, points: 0, matched: 0, titled: false }));
    for (const term of terms) {
      const counts = entries.map((entry) => entry.lowText.match(term.start)?.length ?? 0);
      const having = counts.filter(Boolean).length;
      const rarity = Math.log(1 + (entries.length - having + 0.5) / (having + 0.5));
      rows.forEach((row, i) => {
        const inHeading = row.entry.lowHeading.search(term.start) >= 0, inPage = row.entry.lowPage.search(term.start) >= 0;
        if (!counts[i] && !inHeading && !inPage) return;
        const said = counts[i] * 2.2 / (counts[i] + 1.2 * (0.25 + 0.75 * row.entry.lowText.length / averageLength));
        const titled = term.whole.test(row.entry.lowHeading) ? 6 : inHeading ? 3 : 0;
        row.points += rarity * (said + titled + (inPage ? 3 : 0));
        row.titled ||= inHeading;
        row.matched++;
      });
    }
    for (const row of rows) {
      const { entry } = row;
      row.points *= (row.matched / terms.length) ** 2;
      if (entry.lowHeading === query) row.points += 60;
      else if (entry.lowHeading.startsWith(query) || entry.lowHeading.startsWith("coop " + query)) row.points += 20;
      else if (entry.lowHeading.includes(query)) row.points += 10;
      if (entry.lowText.includes(query)) row.points += 3;
      if (!entry.url.includes("#") && terms.every((term) => (entry.lowPage + " " + entry.lowHeading).search(term.start) >= 0)) row.points += 80;
      if (entry.url.startsWith("migrating.html")) row.points *= 0.5;
      else if (entry.url.startsWith("session-api.html")) row.points *= 0.7;
      else if (entry.url.startsWith("docs.html")) row.points *= 0.85;
      else if (/^(cli|configuration)\.html/.test(entry.url) && !row.titled) row.points *= 0.6;
    }
    return rows.filter((row) => row.matched).sort((a, b) => b.points - a.points);
  };
  const excerpt = (entry, terms) => {
    const hits = terms.map((term) => entry.lowText.search(term.start)).filter((at) => at >= 0);
    const at = hits.length ? Math.min(...hits) : 0;
    const start = at > 60 ? entry.text.indexOf(" ", at - 60) + 1 : 0;
    const end = start + 160;
    return (start ? "…" : "") + entry.text.slice(start, end) + (end < entry.text.length ? "…" : "");
  };
  const marked = (text, terms) => {
    const pattern = new RegExp("(" + terms.map((term) => "(?<![a-z0-9])" + escaped(term.root) + "[a-z0-9]*").join("|") + ")", "gi");
    return text.split(pattern).map((part, i) => {
      if (i % 2 === 0) return part;
      const mark = document.createElement("mark");
      mark.textContent = part;
      return mark;
    });
  };
  const span = (name, ...content) => {
    const el = document.createElement("span");
    el.className = name;
    el.append(...content);
    return el;
  };
  const render = () => {
    results.replaceChildren();
    if (!entries) return;
    status.classList.remove("sr-only");
    const typed = input.value.trim();
    if (!typed) {
      status.textContent = "Type a command, a setting or a topic.";
      return;
    }
    const query = plain(typed), terms = termsOf(query);
    // The walkthroughs for each agent share their steps: one row for a step leaves room for other pages.
    const steps = new Set();
    const found = rank(terms, query).filter(({ entry }) => {
      if (!entry.variant) return true;
      if (steps.has(entry.lowHeading)) return false;
      steps.add(entry.lowHeading);
      return true;
    });
    if (!found.length) {
      status.textContent = `Nothing matches “${typed}”. Try a command or a setting, or browse the docs menu.`;
      return;
    }
    // A screen reader hears the count; everyone else sees the results.
    status.textContent = `${Math.min(found.length, 20)} of ${found.length} results`;
    status.classList.add("sr-only");
    for (const { entry } of found.slice(0, 20)) {
      const link = Object.assign(document.createElement("a"), { href: entry.url });
      link.append(span("docs-search-where", entry.page), span("docs-search-heading", ...marked(entry.heading, terms)));
      if (entry.text) link.append(span("docs-search-snippet", ...marked(excerpt(entry, terms), terms)));
      const row = document.createElement("li");
      row.append(link);
      results.append(row);
    }
  };
  const load = () => {
    if (entries || loading) return;
    status.textContent = "Loading the search index…";
    loading = fetch("search-index.json")
      .then((response) => {
        if (!response.ok) throw new Error(String(response.status));
        return response.json();
      })
      .then((list) => {
        entries = list.map((entry) => ({ ...entry, lowHeading: plain(entry.heading), lowPage: plain(entry.page + " " + (entry.keywords ?? "")), lowText: plain(entry.text) }));
        averageLength = entries.reduce((sum, entry) => sum + entry.lowText.length, 0) / entries.length;
        render();
      })
      .catch(() => { status.textContent = "Search isn't available right now. Check your connection, or browse the docs menu."; })
      .finally(() => { loading = null; });
  };
  const open = () => {
    if (dialog.open) return;
    if (menu && !wide.matches) menu.open = false;
    dialog.showModal();
    input.select();
    load();
    render();
  };
  searchButton.querySelector("kbd").textContent = /Mac|iPhone|iPad/.test(navigator.platform) ? "⌘K" : "Ctrl K";
  searchButton.addEventListener("click", open);
  document.addEventListener("keydown", (event) => {
    const typing = event.target.closest?.("input, textarea, select, [contenteditable]");
    if ((event.key === "k" && (event.metaKey || event.ctrlKey)) || (event.key === "/" && !typing && !event.metaKey && !event.ctrlKey && !event.altKey)) {
      event.preventDefault();
      open();
    }
  });
  input.addEventListener("input", render);
  // A click on the backdrop lands on the dialog itself. Following a result closes it too, even one on this page.
  dialog.addEventListener("click", (event) => {
    if (event.target === dialog || event.target.closest("a, .docs-search-close")) dialog.close();
  });
  dialog.addEventListener("keydown", (event) => {
    const links = [...results.querySelectorAll("a")];
    const at = links.indexOf(document.activeElement);
    // By itself, Escape in a search field only clears the text and leaves the dialog open.
    if (event.key === "Escape") {
      event.preventDefault();
      dialog.close();
    } else if (event.key === "ArrowDown" || event.key === "ArrowUp") {
      event.preventDefault();
      const next = event.key === "ArrowDown" ? Math.min(at + 1, links.length - 1) : at - 1;
      (next < 0 ? input : links[next] ?? input).focus();
    } else if (event.key === "Enter" && document.activeElement === input && links.length) {
      event.preventDefault();
      links[0].click();
    } else if (event.key.length === 1 && at >= 0 && !event.metaKey && !event.ctrlKey) {
      input.focus();
    }
  });
}
