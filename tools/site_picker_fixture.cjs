// Execute the complete shipped script against the command DOM from each real page.
// This is picker/copy behavior proof, not a browser/layout qualification.
"use strict";
const fs = require("node:fs");
const vm = require("node:vm");
const assert = require("node:assert/strict");
const page = JSON.parse(fs.readFileSync(0, "utf8"));
let focused, selected, rejectClipboard = false, timerSequence = 0;
const copies = [], timers = new Map();
class Element {
  constructor(attrs = {}) {
    this.attrs = { ...attrs }; this.dataset = {}; this.handlers = new Map(); this.parentElement = null;
    for (const [key, value] of Object.entries(attrs)) if (key.startsWith("data-")) this.dataset[key.slice(5)] = value;
    this.tabIndex = Number(attrs.tabindex ?? 0); this.textContent = "";
    const classes = new Set((attrs.class ?? "").split(/\s+/));
    this.classList = { add: (x) => classes.add(x), remove: (x) => classes.delete(x), contains: (x) => classes.has(x) };
  }
  getAttribute(key) { return this.attrs[key]; }
  setAttribute(key, value) { this.attrs[key] = String(value); }
  addEventListener(type, fn) { if (!this.handlers.has(type)) this.handlers.set(type, []); this.handlers.get(type).push(fn); }
  dispatchEvent(event) { for (const fn of this.handlers.get(event.type) ?? []) fn(event); return !event.defaultPrevented; }
  async fire(type, event = {}) { event.type = type; await Promise.all((this.handlers.get(type) ?? []).map((fn) => fn(event))); }
  closest(selector) { assert.equal(selector, ".command"); return this.parentElement; }
  focus() { focused = this; }
}
const agents = page.agents.map((attrs) => new Element(attrs));
assert.deepEqual(agents.map((el) => el.dataset.agent), ["claude", "codex", "gemini", "grok"]);
const fields = page.fields.map((spec) => {
  const field = new Element({ class: "command" });
  field.source = new Element(); field.copy = new Element({ "data-copy": "" }); field.status = new Element({ "data-copy-status": "" });
  field.copy.parentElement = field;
  if (spec.step) {
    field.command = new Element({ "data-cmd": spec.step }); field.command.parentElement = field;
    field.command.textContent = spec.text;
    Object.defineProperty(field.source, "textContent", { get: () => spec.prefix + field.command.textContent + spec.suffix });
  } else field.source.textContent = spec.prefix + spec.text + spec.suffix;
  field.querySelector = (selector) => ({ "[data-copy-source]": field.source, "[data-copy-status]": field.status })[selector] ?? null;
  return field;
});
const commands = fields.filter((field) => field.command);
assert.deepEqual(commands.map((field) => field.command.dataset.cmd), page.steps);
const document = {
  querySelectorAll(selector) {
    if (selector === "[data-agent]") return agents;
    if (selector === "[data-copy]") return fields.map((field) => field.copy);
    // These pages' command buttons are already authored. Unrelated scene/docs
    // enhancements have no nodes in this small owner fixture.
    return [];
  },
  querySelector(selector) {
    const step = selector.match(/^\[data-cmd="(\w+)"\]$/)?.[1];
    return commands.find((field) => field.command.dataset.cmd === step)?.command ?? null;
  },
  getElementById() { return null; },
};
class Event { constructor(type) { this.type = type; this.defaultPrevented = false; } preventDefault() { this.defaultPrevented = true; } }
const context = vm.createContext({
  document, window: {}, location: { hash: "" }, Event,
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  navigator: { clipboard: { async writeText(text) { copies.push(text); if (rejectClipboard) throw Error("denied"); } } },
  getSelection: () => ({ selectAllChildren(el) { selected = el; } }),
  setTimeout: (fn, delay) => { const id = ++timerSequence; timers.set(id, { fn, delay }); return id; },
  clearTimeout: (id) => timers.delete(id),
});
vm.runInContext(fs.readFileSync(page.script, "utf8"), context, { filename: page.script, timeout: 1000 });
function checkChoice(index) {
  assert.equal(agents.filter((button) => button.getAttribute("aria-checked") === "true").length, 1);
  for (const [i, button] of agents.entries()) {
    assert.equal(button.getAttribute("aria-checked"), String(i === index));
    assert.equal(button.tabIndex, i === index ? 0 : -1);
  }
  const provider = agents[index].dataset.agent;
  const expected = { login: `coop login ${provider}`, run: `coop ${provider}`, loop: `coop loop ${provider}` };
  for (const field of commands) assert.equal(field.command.textContent.trim(), expected[field.command.dataset.cmd]);
}
async function main() {
  checkChoice(0);
  for (const [index, button] of agents.entries()) {
    await button.fire("click"); checkChoice(index);
    assert.equal(timers.size, 0, "selection must cancel earlier copy timers");
    for (const field of commands) {
      assert.equal(field.status.textContent, ""); assert.equal(field.copy.classList.contains("is-done"), false);
      await field.copy.fire("click");
      const provider = button.dataset.agent;
      const expected = { login: `coop login ${provider}`, run: `coop ${provider}`, loop: `coop loop ${provider}` };
      assert.equal(copies.at(-1), expected[field.command.dataset.cmd]);
      assert.equal(field.status.textContent, "Copied."); assert.equal(field.copy.classList.contains("is-done"), true);
      const timer = timers.get(timerSequence);
      assert.equal(timer.delay, 2000, "Copied expiry must stay two seconds");
      if (index === 0 && field === commands[0]) {
        timers.delete(timerSequence); timer.fn();
        assert.equal(field.status.textContent, ""); assert.equal(field.copy.classList.contains("is-done"), false);
      }
    }
  }
  for (const [key, from, to] of [["ArrowRight", 3, 0], ["ArrowDown", 3, 0], ["ArrowLeft", 0, 3], ["ArrowUp", 0, 3], ["Home", 3, 0], ["End", 0, 3]]) {
    await agents[from].fire("click");
    const event = new Event("keydown"); event.key = key;
    await agents[from].fire("keydown", event);
    assert.equal(event.defaultPrevented, true); assert.equal(focused, agents[to]); checkChoice(to);
  }
  const unrelated = new Event("keydown"); unrelated.key = "x";
  await agents[3].fire("keydown", unrelated);
  assert.equal(unrelated.defaultPrevented, false); assert.equal(focused, agents[3]); checkChoice(3);
  rejectClipboard = true;
  const field = commands[0]; await field.copy.fire("click");
  assert.equal(selected, field.source); assert.equal(field.classList.contains("copy-failed"), true);
  assert.equal(field.copy.classList.contains("is-done"), false);
  assert.equal(field.status.textContent, "Copy blocked. Press Cmd+C or Ctrl+C.");
  await agents[0].fire("click"); checkChoice(0);
  for (const field of commands) {
    assert.equal(field.status.textContent, ""); assert.equal(field.classList.contains("copy-failed"), false);
  }
  console.log("picker, clipboard, keyboard and reset controls passed");
}
main().catch((err) => { console.error(err); process.exitCode = 1; });
