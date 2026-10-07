// Draft: the night story. render.py embeds the timeline, one state per part of the story (the hero,
// then each step): the time, how far through the queue the loop is, the task in hand and the account
// its agent runs on. The board keeps one row per task it shows and moves those rows between its places
// (the task in hand, the decision waiting, the next few, the newest finished, each list ending in the
// count of the rest). Scrolling forward plays a step one beat at a time: the task in hand finishes
// where it is, the decision folds or opens, the night runs on, then the step's own event. Without the
// script it shows morning.
(() => {
  const root = document.documentElement;
  const stage = document.querySelector(".night-stage");
  const data = document.getElementById("night-timeline");
  let deck = root.classList.contains("night-deck");
  // the head script picked the mode (none on a short screen, which shows the board at morning);
  // without IntersectionObserver it picked none and the page stays plain for good
  if (!stage || !data || !("IntersectionObserver" in window)) return;
  root.classList.add("night-ready");
  const playing = () => root.classList.contains("night-story") || root.classList.contains("night-deck");
  const { n, per, titles, commits, states } = JSON.parse(data.textContent);
  const board = stage.querySelector(".board");
  const lanes = Object.fromEntries(["now", "waiting", "next", "done"].map((name) => [name, board.querySelector(`.lane.${name}`)]));
  const cells = [...board.querySelectorAll(".cells li")];
  const headline = board.querySelector(".headline");
  const time = board.querySelector(".clock time");
  const track = board.querySelector(".night-track");
  const steps = [...document.querySelectorAll(".night-step")];
  const blank = document.getElementById("night-task").content.firstElementChild;
  const [kit, blankAccount] = document.getElementById("night-agent").content.children;
  const still = matchMedia("(prefers-reduced-motion: reduce)");
  const ease = "cubic-bezier(0.16, 1, 0.3, 1)";
  const hue = (name) => getComputedStyle(root).getPropertyValue(`--${name}`).trim();
  const ring = { todo: ["0 100", hue("amber")], active: ["28 72", hue("amber")], reopened: ["28 72", hue("cyan")],
    done: ["100 0", hue("success")], waiting: ["100 0", hue("warning")] };
  // each cell state's colour, read from the stylesheet once, so a cell can light from one to the next;
  // the cell borrowed for it goes back to its morning state, which a short screen keeps for good
  const tone = {}, morning = cells[0].dataset.state;
  for (const state of ["none", ...Object.keys(ring)]) { cells[0].dataset.state = state; tone[state] = getComputedStyle(cells[0]).backgroundColor; }
  cells[0].dataset.state = morning;

  const status = (s, i) => i >= (s.queued ?? n) ? "none" : i === s.waiting ? "waiting" : i === s.now ? (s.reopened ? "reopened" : "active") : i < s.done ? "done" : "todo";

  // one row per task, made the first time the board shows it, then moved for the rest of the night
  const rows = new Map([...board.querySelectorAll(".lane .task[data-task]")].map((el) => [Number(el.dataset.task), el]));
  const row = (i) => {
    if (!rows.has(i)) {
      const el = blank.cloneNode(true);
      el.dataset.task = i;
      el.querySelector(".title").textContent = titles[i] ?? "";
      el.querySelector(".commit").textContent = commits[i] ?? "";
      rows.set(i, el);
    }
    return rows.get(i);
  };
  // the count closing each list: "… 117 more"
  const rest = Object.fromEntries(["next", "done"].map((lane) => {
    const el = lanes[lane].querySelector(".rest") ?? Object.assign(document.createElement("li"), { className: "rest", innerHTML: '… <span class="count">0</span> more' });
    return [lane, { el, count: el.querySelector(".count") }];
  }));

  // Where everything stands in a state: the queue in order and the finished newest first, cut to the
  // rows that fit: two or three of each beside the story; on a phone, what the task in hand and the
  // decision leave of three, given to the finished tasks once there are any.
  const layout = (s) => {
    const next = [], done = [];
    for (let i = 0; i < n; i++) {
      const state = status(s, i);
      if (state === "todo") next.push(i);
      else if (state === "done" && i !== s.last) done.unshift(i);
    }
    if (s.last !== undefined) done.unshift(s.last);
    let room = { next: s.now === undefined && !done.length ? 3 : 2, done: 3 };
    if (deck) {
      const left = 3 - (s.now === undefined ? 0 : 1) - (s.waiting === undefined ? 0 : s.ask ? 2 : 1);
      room = done.length ? { next: 0, done: left } : { next: left, done: 0 };
    }
    // a count of one would take a row's room to say less than the row: show the row instead
    const cut = (list, k) => list.slice(0, k && list.length - k === 1 ? k + 1 : k);
    // just after tasks are added, the queue shows its end, where they landed
    const shown = { next: s.tail ? next.slice(-room.next) : cut(next, room.next), done: cut(done, room.done) };
    return { ...shown, more: { next: next.length - shown.next.length, done: done.length - shown.done.length } };
  };

  // minutes since 22:00, so the night counts forward across midnight
  const since = (hhmm) => { const [h, m] = hhmm.split(":").map(Number); return ((h + 2) % 24) * 60 + m; };
  const at = (t) => { const c = (t + 22 * 60) % (24 * 60); return `${String(Math.floor(c / 60)).padStart(2, "0")}:${String(c % 60).padStart(2, "0")}`; };
  const clock = (t) => { time.textContent = at(t); track.style.setProperty("--night", Math.min(1, t / 540).toFixed(3)); };
  // the clock and the counts run to their new values together, the night's time-lapse
  let tween = 0;
  const roll = (moving, pairs) => {
    cancelAnimationFrame(tween);
    if (!moving) { for (const [, to, set] of pairs) set(to); return; }
    const start = performance.now();
    const frame = (now) => {
      const k = Math.min(1, (now - start) / 1100), eased = 1 - (1 - k) ** 3;
      for (const [from, to, set] of pairs) set(Math.round(from + (to - from) * eased));
      if (k < 1) tween = requestAnimationFrame(frame);
    };
    tween = requestAnimationFrame(frame);
  };

  // A row's change of state, drawn on its icon: the ring starts turning, closes on a tick (and the
  // commit types itself out), asks, or turns back.
  const restate = (el, from, to) => {
    const icon = el.querySelector(".status");
    icon.querySelector(".arc").animate([{ strokeDasharray: ring[from][0], stroke: ring[from][1], opacity: from === "todo" ? 0 : 1 },
      { strokeDasharray: ring[to][0], stroke: ring[to][1], opacity: 1 }], { duration: 450, easing: ease });
    if (to === "done") {
      icon.querySelector(".tick").animate([{ strokeDashoffset: 100 }, { strokeDashoffset: 0 }], { duration: 350, delay: 200, easing: ease, fill: "backwards" });
      el.querySelector(".commit").animate([{ clipPath: "inset(0 100% 0 0)" }, { clipPath: "inset(0 0 0 0)" }], { duration: 420, delay: 350, easing: "steps(7, end)", fill: "backwards" });
    }
    if (from === "done") icon.querySelector(".tick").animate([{ opacity: 1 }, { opacity: 0 }], { duration: 250, easing: "ease-in" });
    if (to === "waiting") icon.querySelector(".ask").animate([{ opacity: 0, scale: 0.5 }, { opacity: 1, scale: 1 }], { duration: 500, delay: 300, easing: ease, fill: "backwards" });
    if (to === "reopened") icon.animate([{ rotate: "0deg" }, { rotate: "-360deg" }], { duration: 800, easing: ease });
  };

  // The agent on the task in hand. A new task gets a fresh one: its mark turns in and its line rises.
  // Every account shows, the one in use lit, each filling as its usage does from task to task. When
  // the one in use is spent it fills to the end, flashes and greys out, and the next lights up and
  // starts to fill; both stay in view, so a reader who looked away still sees what happened.
  let using = null;
  const look = {
    idle: { color: hue("dim"), boxShadow: "inset 0 0 0 1px rgb(255 255 255 / 14%)" },
    on: { color: hue("amber"), boxShadow: "inset 0 0 0 1px rgb(230 169 92 / 40%)" },
    out: { color: hue("danger"), boxShadow: `inset 0 0 0 1px ${hue("danger")}` },
  };
  const tint = { idle: "rgb(255 255 255 / 12%)", on: "rgb(230 169 92 / 26%)", out: "rgb(253 164 175 / 30%)" };
  const equip = (el, s, animate, fresh) => {
    let agent = el.querySelector(".agent");
    if (!agent) { agent = kit.cloneNode(true); el.querySelector(".title").after(agent); }
    const was = using, chips = Object.entries(s.use).map(([name, used]) => {
      const chip = blankAccount.cloneNode(true);
      chip.dataset.account = name;
      chip.classList.toggle("on", name === s.on);
      chip.querySelector(".name").textContent = name;
      chip.querySelector(".used").style.width = `${used}%`;
      return chip;
    });
    agent.querySelector(".accounts").replaceChildren(...chips);
    using = { on: s.on, use: s.use };
    if (!animate) return;
    if (fresh) {
      agent.animate([{ opacity: 0, transform: "translateY(6px)" }, { opacity: 1, transform: "none" }], { duration: 600, delay: 250, easing: ease, fill: "backwards" });
      agent.querySelector(".logo").animate([{ rotate: "-160deg", scale: 0.2 }, { rotate: "0deg", scale: 1 }], { duration: 900, delay: 250, easing: ease, fill: "backwards" });
    }
    for (const chip of chips) {
      const name = chip.dataset.account, fill = chip.querySelector(".used"), to = s.use[name], from = was?.use[name] ?? 0;
      if (s.handoff && was && name === was.on && name !== s.on) {
        fill.animate([{ width: `${from}%`, backgroundColor: tint.on, easing: "cubic-bezier(0.4, 0, 0.6, 1)" }, { width: "100%", backgroundColor: tint.on, offset: 0.4 },
          { width: "100%", backgroundColor: tint.out, offset: 0.48 }, { width: "100%", backgroundColor: tint.out, offset: 0.56 },
          { width: "100%", backgroundColor: tint.idle, offset: 0.68 }, { width: "100%", backgroundColor: tint.idle }], { duration: 2600 });
        chip.animate([look.on, { ...look.on, offset: 0.4 }, { ...look.out, offset: 0.48 }, { ...look.out, offset: 0.56 }, { ...look.idle, offset: 0.68 }, look.idle], { duration: 2600 });
      } else if (s.handoff && was && name === s.on && name !== was.on) {
        chip.animate([look.idle, { ...look.idle, offset: 0.58 }, { ...look.on, offset: 0.68 }, look.on], { duration: 2600 });
        fill.animate([{ width: `${from}%` }, { width: `${from}%`, offset: 0.66, easing: ease }, { width: `${to}%` }], { duration: 2600 });
      } else if (from !== to) {
        fill.animate([{ width: `${from}%` }, { width: `${to}%` }], { duration: 1100, delay: was ? 0 : 450, easing: ease, fill: "backwards" });
      }
    }
  };

  // One dot stands for a few tasks in queue order and shows the liveliest of them: one coming back
  // from the review, the one in hand, the decision waiting, any still to do, else done. Going forward
  // the dots light in queue order across the time-lapse.
  const rank = ["none", "done", "todo", "waiting", "active", "reopened"];
  const dot = (s, k) => {
    let best = rank[0];
    for (let i = k * per; i < Math.min(n, (k + 1) * per); i++) { const state = status(s, i); if (rank.indexOf(state) > rank.indexOf(best)) best = state; }
    return best;
  };
  const paint = (s, animate) => {
    const lit = [];
    cells.forEach((cell, k) => {
      const from = cell.dataset.state, to = dot(s, k);
      if (from === to) return;
      cell.dataset.state = to;
      if (animate) lit.push([cell, from, to]);
    });
    lit.forEach(([cell, from, to], k) => cell.animate([{ backgroundColor: tone[from], transform: "scale(0.4)" }, { backgroundColor: tone[to], transform: "none" }],
      { duration: 450, delay: lit.length > 1 ? (k / (lit.length - 1)) * 900 : 0, easing: ease, fill: "backwards" }));
  };
  // a light passing along the dots: the review going through the night's work, the sunrise
  const wave = (glow) => cells.forEach((cell, k) =>
    cell.animate([{ filter: "none" }, { filter: glow, offset: 0.45 }, { filter: "none" }], { duration: 650, delay: k * 16, easing: "ease-in-out" }));

  // The decision opens to its options or folds to one line. The list's height runs between the two,
  // so whatever sits under it moves with it.
  const fold = (el, open) => {
    const list = el.querySelector(".options"), from = list.getBoundingClientRect().height, padding = getComputedStyle(list).padding;
    el.toggleAttribute("data-open", open);
    const to = list.getBoundingClientRect().height, opened = getComputedStyle(list).padding;
    list.animate([{ height: `${from}px`, padding, opacity: open ? 0 : 1, visibility: "visible" },
      { height: `${to}px`, padding: opened, opacity: open ? 1 : 0, visibility: "visible" }], { duration: 550, easing: ease });
  };

  const box = (el) => el.getBoundingClientRect();
  const size = (el) => parseFloat(getComputedStyle(el).fontSize);
  let shown = -1, prev = null;
  const render = (s, part, moving, forward) => {
    const was = prev, plan = layout(s);
    const wanted = [s.now, s.waiting, ...plan.next, ...plan.done].filter((i) => i !== undefined);
    const keep = new Set(wanted);
    // where everything is before it moves, mid-flight included
    const tall = box(board).height;
    board.getAnimations().forEach((a) => a.cancel());
    const laneAt = Object.fromEntries(["waiting", "next", "done"].map((name) => [name, lanes[name].childElementCount ? box(lanes[name]) : null]));
    for (const name of ["waiting", "next", "done"]) lanes[name].getAnimations().forEach((a) => a.cancel());
    const before = new Map();
    for (const [i, el] of rows) {
      if (!el.isConnected) continue;
      before.set(i, { at: box(el), size: size(el), lane: el.parentElement.classList[1] });
      el.getAnimations().forEach((a) => a.cancel());
      el.classList.remove("moving");
    }
    const restAt = Object.fromEntries(["next", "done"].map((lane) => [lane, rest[lane].el.isConnected ? box(rest[lane].el) : null]));
    for (const lane of ["next", "done"]) rest[lane].el.getAnimations().forEach((a) => a.cancel());
    const asked = lanes.waiting.childElementCount > 0;
    // a row about to drop out of sight leaves a copy behind, to fade out toward the count it joins
    const ghosts = [];
    if (moving) {
      for (const [i, b] of before) {
        if (keep.has(i)) continue;
        const wrap = Object.assign(document.createElement("ol"), { className: `lane ${b.lane} ghost` });
        wrap.append(rows.get(i).cloneNode(true));
        ghosts.push({ i, wrap, b });
      }
    }

    board.dataset.part = part;
    lanes.now.replaceChildren(...(s.now === undefined ? [] : [row(s.now)]));
    lanes.waiting.replaceChildren(...(s.waiting === undefined ? [] : [row(s.waiting)]));
    for (const lane of ["next", "done"]) {
      const count = plan[lane].length && plan.more[lane] ? [rest[lane].el] : [];
      lanes[lane].replaceChildren(...(lane === "next" && s.tail ? [...count, ...plan[lane].map(row)] : [...plan[lane].map(row), ...count]));
    }
    for (const i of wanted) {
      const el = row(i), from = el.dataset.state, to = status(s, i);
      el.dataset.state = to;
      if (moving && forward && before.has(i) && from && from !== to) restate(el, from, to);
    }
    // the decision opens or folds at once, unless a step forward does it in a beat of its own
    if (s.waiting !== undefined && !(moving && forward && before.has(s.waiting))) row(s.waiting).toggleAttribute("data-open", Boolean(s.ask));
    if (s.now !== undefined) equip(row(s.now), s, moving && forward, !was || was.now !== s.now);
    paint(s, moving && forward);
    if (headline.textContent !== (s.headline ?? "")) {
      headline.textContent = s.headline ?? "";
      if (moving && forward) headline.animate([{ opacity: 0, transform: "translateY(8px)" }, { opacity: 1, transform: "none" }], { duration: 700, delay: 250, easing: ease, fill: "backwards" });
    }
    const to = since(s.clock), from = was ? since(was.clock) : to;
    roll(moving && to > from, [[from, to, clock],
      ...["next", "done"].map((lane) => [restAt[lane] ? Number(rest[lane].count.textContent) : 0, plan.more[lane], (v) => { rest[lane].count.textContent = v; }])]);
    board.toggleAttribute("data-day", to >= since("06:00"));
    prev = s;
    if (!moving) return;

    // Measured before anything moves: where each row and each list landed. A list whose box moved (the
    // decision's, the queue's, the finished ones') slides as a whole, and its rows slide within it.
    // Every row still showing slides from where it was and grows or shrinks to its new size, the
    // finished ones landing in the order they finished, over the rows they cross rather than through
    // them. A row leaving fades out where it was, toward the count it joins; a row arriving fades in
    // once it has gone. The decision arrives with its box; the task taken up comes out of the count it
    // was part of.
    const frame = box(board), arrivals = { now: 0, waiting: 0, next: 0, done: 0 };
    // the card grows or shrinks with what moves inside it, so nothing hangs past its edge on the way
    if (Math.abs(frame.height - tall) > 0.5) board.animate([{ height: `${tall}px` }, { height: `${frame.height}px` }], { duration: 750, easing: ease });
    const ends = new Map([...wanted.map((i) => [rows.get(i), box(rows.get(i))]), ...["next", "done"].filter((l) => rest[l].el.isConnected).map((l) => [rest[l].el, box(rest[l].el)])]);
    const shift = { now: 0 };
    for (const name of ["waiting", "next", "done"]) {
      const from = laneAt[name], to = lanes[name].childElementCount ? box(lanes[name]) : null;
      shift[name] = from && to ? from.top - to.top : 0;
    }
    for (const name of ["waiting", "next", "done"]) {
      if (shift[name]) lanes[name].animate([{ transform: `translateY(${shift[name]}px)` }, { transform: "none" }], { duration: 750, easing: ease });
    }
    const landing = wanted.filter((i) => before.get(i) && before.get(i).lane !== "done" && status(s, i) === "done").sort((a, b) => a - b);
    if (!asked && s.waiting !== undefined) {
      lanes.waiting.animate([{ opacity: 0, transform: "translateY(10px)" }, { opacity: 1, transform: "none" }], { duration: 650, delay: 250, easing: ease, fill: "backwards" });
    }
    const slide = (el, from, scale = 1, delay = 0, crossing = false) => {
      const end = ends.get(el), dx = from.left - end.left, dy = from.top - end.top - shift[el.parentElement.classList[1]];
      if (!dx && !dy && scale === 1) return;
      if (crossing) el.classList.add("moving");
      el.animate([{ transform: `translate(${dx}px, ${dy}px) scale(${scale})` }, { transform: "none" }], { duration: 750, delay, easing: ease, fill: "backwards" })
        .finished.then(() => el.classList.remove("moving"), () => {});
    };
    const handed = ghosts.some(({ b }) => b.lane === "now");
    for (const i of wanted) {
      const el = rows.get(i), b = before.get(i), state = status(s, i), lane = el.parentElement.classList[1];
      if (b) { slide(el, b.at, b.size / size(el), Math.max(0, landing.indexOf(i)) * 110, b.lane !== lane); continue; }
      if (lane === "waiting" && !asked) continue;
      const from = lane !== "now" ? null : state === "reopened" ? restAt.done : was && status(was, i) === "todo" ? restAt.next : null;
      if (from) {
        const end = ends.get(el);
        el.animate([{ opacity: 0, transform: `translate(${from.left - end.left}px, ${from.top - end.top}px) scale(0.75)` }, { opacity: 1, offset: 0.3 },
          { opacity: 1, transform: "none" }], { duration: 900, delay: handed ? 150 : 0, easing: ease, fill: "backwards" });
        continue;
      }
      if (s.tail && lane === "next") {
        // a task just added slides in at the end of the queue, one after another
        el.animate([{ opacity: 0, transform: "translateX(32px)", backgroundColor: "rgb(255 255 255 / 0%)" }, { opacity: 1, transform: "none", backgroundColor: "rgb(255 255 255 / 7%)", offset: 0.6 },
          { opacity: 1, transform: "none", backgroundColor: "rgb(255 255 255 / 0%)" }], { duration: 900, delay: 250 + (arrivals[lane]++) * 260, easing: ease, fill: "backwards" });
        continue;
      }
      el.animate([{ opacity: 0, transform: "translateY(10px)" }, { opacity: 1, transform: "none" }], { duration: 600, delay: 200 + (arrivals[lane]++) * 70, easing: ease, fill: "backwards" });
    }
    for (const lane of ["next", "done"]) {
      if (!rest[lane].el.isConnected) continue;
      if (restAt[lane]) slide(rest[lane].el, restAt[lane]);
      else rest[lane].el.animate([{ opacity: 0 }, { opacity: 1 }], { duration: 600, delay: 200 + arrivals[lane] * 70, easing: ease, fill: "backwards" });
    }
    for (const { i, wrap, b } of ghosts) {
      board.append(wrap);
      Object.assign(wrap.style, { left: `${b.at.left - frame.left}px`, top: `${b.at.top - frame.top}px`, width: `${b.at.width}px` });
      const state = status(s, i), into = state === "done" ? rest.done.el : state === "todo" ? rest.next.el : null;
      const toward = into?.isConnected ? Math.sign(box(into).top - b.at.top) * 12 : 8;
      wrap.animate([{ opacity: 1, transform: "none" }, { opacity: 0, transform: `translateY(${toward}px) scale(0.97)` }],
        { duration: 260, easing: "ease-in", fill: "both" }).finished.then(() => wrap.remove(), () => wrap.remove());
    }
  };

  // Scrolling forward, a step plays one beat at a time; scrolling back, or with reduced motion, the
  // board goes straight to the step. A new scroll cancels the beats still to come.
  let epoch = 0;
  const pause = (ms) => new Promise((done) => setTimeout(done, ms));
  const show = async (part) => {
    if (part === shown) return;
    const s = states[part], forward = shown !== -1 && part > shown, moving = shown !== -1 && !still.matches;
    const mine = ++epoch, live = () => epoch === mine;
    shown = part;
    if (!moving || !forward) { render(s, part, moving, forward); return; }
    // stepping into the review, the night first runs to its end with every task finished
    const review = s.reopened && !prev.reopened;
    const goal = review ? { ...s, now: undefined, reopened: undefined, headline: s.review } : s;
    // the task in hand finishes where it is: its agent steps away, its ring closes on a tick, its commit types out
    const held = prev.now === undefined ? null : rows.get(prev.now);
    if (held && status(goal, prev.now) === "done") {
      const agent = held.querySelector(".agent"), from = held.dataset.state;
      const away = agent?.animate([{ opacity: 1 }, { opacity: 0 }], { duration: 220, easing: "ease-in", fill: "forwards" });
      await pause(220);
      away?.cancel();
      if (!live()) return;
      held.dataset.state = "done";
      restate(held, from, "done");
      await pause(900);
      if (!live()) return;
    }
    // the decision folds to one line when the step isn't about it
    const asking = prev.waiting === undefined ? null : rows.get(prev.waiting);
    if (asking?.isConnected && asking.hasAttribute("data-open") && !goal.ask) {
      fold(asking, false);
      await pause(450);
      if (!live()) return;
    }
    render(goal, part, true, true);
    if (review) {
      // the review passes over the finished tasks and the cells, and turns one back where it stands
      await pause(1300);
      if (!live()) return;
      wave("brightness(1.8)");
      lanes.done.querySelectorAll(":scope > *").forEach((el, k) => el.animate([{ backgroundColor: "rgb(98 211 240 / 0%)" },
        { backgroundColor: "rgb(98 211 240 / 14%)", offset: 0.4 }, { backgroundColor: "rgb(98 211 240 / 0%)" }], { duration: 900, delay: k * 120, easing: "ease-in-out" }));
      await pause(1000);
      if (!live()) return;
      const back = rows.get(s.now);
      if (back.isConnected) {
        back.dataset.state = "reopened";
        restate(back, "done", "reopened");
        await pause(700);
        if (!live()) return;
      }
      render(s, part, true, true);
    }
    // the decision opens in full on the steps about it
    const asked = s.waiting === undefined ? null : rows.get(s.waiting);
    if (asked && s.ask && !asked.hasAttribute("data-open")) {
      await pause(800);
      if (!live()) return;
      fold(asked, true);
    }
    if (part === states.length - 1) {
      await pause(600);
      if (live()) wave("brightness(1.6) saturate(1.3)");
    }
  };

  // The part on screen is the last step past the reading line: the middle of the screen beside the
  // pinned board, or just under the board where it sticks to the top (once it has stuck there).
  let queued = false;
  const update = () => {
    queued = false;
    if (!playing()) return;
    const frame = stage.getBoundingClientRect();
    const line = deck ? frame.bottom + 32 : innerHeight * 0.55;
    let part = 0;
    steps.forEach((step, k) => { if (step.getBoundingClientRect().top < line) part = k + 1; });
    if (deck && frame.top > 1) part = 0;
    show(part);
  };
  addEventListener("scroll", () => { if (!queued) { queued = true; requestAnimationFrame(update); } }, { passive: true });
  addEventListener("resize", update);
  // The layout follows the screen, as the homepage's does: a wide screen pins the board beside the
  // story, a narrow one that is tall enough sticks it to the top, and a short one shows the plain
  // page. Crossing either line switches the mode and redraws the step on screen, without motion.
  const wide = matchMedia("(min-width: 1024px)"), tall = matchMedia("(min-height: 500px)");
  const relayout = () => {
    const mode = wide.matches ? "night-story" : tall.matches ? "night-deck" : "";
    root.classList.toggle("night-story", mode === "night-story");
    root.classList.toggle("night-deck", mode === "night-deck");
    deck = mode === "night-deck";
    epoch++;
    shown = -1;
    if (mode) update();
    else render(states.at(-1), states.length - 1, false, false);
  };
  wide.addEventListener("change", relayout);
  tall.addEventListener("change", relayout);
  update();
  // the queue arrives when the night opens on it: the dots one by one, then its first tasks
  if (shown === 0 && !still.matches) {
    cells.forEach((cell, i) => cell.animate([{ opacity: 0, transform: "scale(0.3)" }, { opacity: 1, transform: "none" }],
      { duration: 500, delay: 150 + i * 14, easing: ease, fill: "backwards" }));
    lanes.next.querySelectorAll(":scope > *").forEach((el, k) => el.animate([{ opacity: 0, transform: "translateY(8px)" }, { opacity: 1, transform: "none" }],
      { duration: 600, delay: 450 + k * 90, easing: ease, fill: "backwards" }));
  }
})();

// Safe to leave running: the sandbox builds itself once, a layer and its line at a time, the first time
// it comes into view. With reduced motion, or without the script, it's simply finished.
(() => {
  const safe = document.querySelector(".night-safe");
  const sandbox = safe?.querySelector(".night-sandbox");
  if (!sandbox) return;
  // the main page frames this picture with a camera; here, on a screen narrower than the picture, it
  // shrinks to the width it has
  const scene = sandbox.querySelector(".scene");
  const fit = () => {
    scene.style.zoom = "";
    const room = sandbox.clientWidth, wide = scene.scrollWidth;
    if (wide > room) scene.style.zoom = (room / wide).toFixed(3);
  };
  fit();
  addEventListener("resize", fit);
  const lines = [...safe.querySelectorAll(".night-safe-list li")];
  if (!("IntersectionObserver" in window) || matchMedia("(prefers-reduced-motion: reduce)").matches) return;
  for (const path of safe.querySelectorAll(".night-safe-list .icon path")) path.setAttribute("pathLength", "1");
  safe.classList.add("armed");
  sandbox.classList.add("armed");
  const watch = new IntersectionObserver(([entry]) => {
    if (!entry.isIntersecting) return;
    watch.disconnect();
    lines.forEach((line, k) => setTimeout(() => {
      sandbox.classList.add(`s${k + 1}`);
      line.classList.add("is-on");
    }, 400 + k * 1100));
  }, { threshold: 0.6 });
  watch.observe(sandbox);
})();
