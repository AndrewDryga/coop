<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 14.0.0

Coop 14.0.0 is the result of a review of the whole project: several hundred fixes across boxes, the loop, tasks, forks, filtered networking and the remote session service, plus a set of stricter defaults.

- Security: a session's output schema can no longer make a worker read local files, the Codex credential broker no longer forwards the client's analytics uploads with your token, and with eBPF protection a box can no longer rename a file onto a secret name such as `.env`.
- Text attachments no longer show up as untracked files in a writable session (a 13.2.1 regression).
- Stricter behavior you may notice: `coop build` refuses while `COOP_IMAGE` is set; `project.yaml` can only lower the process cap; an agent's `coop tasks claim` over a person's claim needs `--force`; Grok and Gemini launches refuse an MCP header that names an unset variable; Gemini no longer reads the pre-v11 profile settings file; a few commands changed exit codes. MIGRATING.md lists each one with what to do.
- `coop update` verifies the release signature with cosign when it is installed, and the website and installer deploy only after CI passes.
- The loop, forks and editor sessions fix many wrong messages, stuck states and slow paths: fork merge checks read Git in a few runs instead of thousands, signing no longer checks files out, and a fork made from a preset reopens with it, in the editor too.
- The session API answers failed prepares and checkpoint refusals with real error codes, pages through every session, and records why an agent stopped.

Coop is now built with Go 1.27.4. The next launch rebuilds the helper images; filtered projects may ask you to run `coop net setup` once, and tools that save a new secret file by temp-file-then-rename inside a box must write it in a hidden folder.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v13.2.1...v14.0.0)

