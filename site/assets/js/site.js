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
  button.setAttribute("aria-label", `Copy ${name ? name.textContent : source.textContent.trim()}`);
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

// The agent switcher rewrites the sign-in and start commands; without JavaScript they stay on Claude.
const agentButtons = [...document.querySelectorAll("[data-agent]")];
const pick = (button) => {
  if (button.getAttribute("aria-checked") === "true") return;
  const agent = button.dataset.agent;
  for (const other of agentButtons) {
    other.setAttribute("aria-checked", String(other === button));
    other.tabIndex = other === button ? 0 : -1;
  }
  for (const [step, command] of [["login", `coop login ${agent}`], ["run", `coop ${agent}`]]) {
    const cmd = document.querySelector(`[data-cmd="${step}"]`);
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
  const follow = () => {
    document.documentElement.classList.toggle("js-story", wide.matches);
    if (wide.matches) parts.forEach((part) => watch.observe(part));
    else watch.disconnect();
  };
  wide.addEventListener("change", follow);
  follow();
  document.documentElement.classList.add("story-ready");
}

// The feature scenes are drawn finished. Where motion is allowed and a scene has room to show whole,
// it steps back to where it started just before it scrolls into view, and replays the rest, once,
// when it is on screen. A scene nobody scrolls to (or prints) stays finished.
const sleep = (ms) => new Promise((done) => setTimeout(done, ms));

// The loop's live bar (internal/loop/bar.go): spinner, 20 cells (done cyan, active yellow), counts,
// what it is on, and the attempt's elapsed time.
const SPIN = [".[  ]", ">[  ]", "[.  ]", "[ * ]", "[  .]", "[  ]>", "[  ]."];
const CORNER = ["◰", "◳", "◲", "◱"]; // the one-column spinner coop tasks watch uses
const barCells = (done, doing, total) => {
  const d = Math.round((20 * done) / total), a = Math.round((20 * (done + doing)) / total) - d;
  return `<span class="bar-done">${"█".repeat(d)}</span><span class="bar-doing">${"█".repeat(a)}</span>${"░".repeat(20 - d - a)}`;
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
    fits: (card) => card.clientWidth >= 540,
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
    fits: (card) => card.clientWidth >= 540,
    reset(card) {
      for (const line of card.querySelectorAll(".line[data-beat]")) {
        line.classList.toggle("is-pending", line.dataset.beat !== "0");
        // a command is typed out when its turn comes; the first prompt waits, empty, for it
        if (line.classList.contains("cmdline")) {
          line.dataset.command ??= line.lastChild.textContent;
          line.lastChild.textContent = "";
        }
      }
    },
    async play(card) {
      const bar = card.querySelector(".live-bar"); // only the loop has one
      const beats = [];
      for (const line of card.querySelectorAll(".line[data-beat]")) (beats[line.dataset.beat] ??= []).push(line);
      let state = null, spin = 0, seconds = 0;
      // like the CLI, the bar fits the terminal's width by shortening what it is on
      card.classList.add("is-playing");
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
        const room = columns - `.[  ] [${"░".repeat(20)}] ${done}/3 done ·  · ${time}`.length;
        const fit = on.length > room ? `${on.slice(0, Math.max(room - 1, 1))}…` : on;
        bar.innerHTML = `${SPIN[spin % SPIN.length]} [${barCells(done, doing, 3)}] ${done}/3 done · ${fit} · ${time}`;
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
          if (line.dataset.command) {
            await sleep(500);
            for (const char of line.dataset.command) { line.lastChild.textContent += char; await sleep(35); }
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

// The docs: on wide screens the contents stay open beside the text and mark the section being read;
// on narrow ones they fold into a box above it, which closes again once you pick a section.
const side = document.querySelector(".docs-side");
if (side) {
  const fold = () => { side.open = wide.matches; };
  wide.addEventListener("change", fold);
  fold();
  const links = new Map([...side.querySelectorAll('a[href^="#"]')].map((link) => [link.hash.slice(1), link]));
  for (const link of links.values()) link.addEventListener("click", () => { if (!wide.matches) side.open = false; });
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
        const link = links.get(target.id), item = link.getBoundingClientRect(), pane = side.getBoundingClientRect();
        if (wide.matches && item.top < pane.top) side.scrollTop += item.top - pane.top;
        else if (wide.matches && item.bottom > pane.bottom) side.scrollTop += item.bottom - pane.bottom;
      }
    }, { rootMargin: "0px 0px -70% 0px" });
    for (const section of document.querySelectorAll(".doc-section[id]")) reading.observe(section);
  }
}
