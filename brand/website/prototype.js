"use strict";

const examples = {
  secret: { command: "cat .env", output: "Empty file. Secret contents are hidden." },
  code: { command: "cat src/app.ts", output: "Source is readable. The agent can work here." }
};

document.querySelectorAll("button[data-example]").forEach((button) => {
  button.addEventListener("click", () => {
    const selected = examples[button.dataset.example];
    document.querySelectorAll("button[data-example]").forEach((item) => {
      item.setAttribute("aria-pressed", String(item === button));
    });
    const command = document.getElementById("example-command");
    command.replaceChildren();
    const prompt = document.createElement("span");
    prompt.className = "prompt";
    prompt.textContent = "$";
    command.append(prompt, ` ${selected.command}`);
    document.getElementById("example-output").textContent = selected.output;
  });
});

document.querySelectorAll("button[data-agent]").forEach((button) => {
  button.addEventListener("click", () => {
    document.querySelectorAll("button[data-agent]").forEach((item) => {
      item.setAttribute("aria-pressed", String(item === button));
    });
    document.getElementById("agent-name").textContent = button.dataset.agent;
  });
});

document.querySelectorAll(".example-controls, .agent-buttons").forEach((controls) => {
  controls.hidden = false;
});
