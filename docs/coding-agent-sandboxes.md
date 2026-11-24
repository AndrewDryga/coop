# Claude Code, Codex, Gemini and Grok sandboxes compared

## What does each CLI actually isolate?

An agent fixing a test needs your checkout. On our Mac, Claude’s shell and Codex’s workspace sandbox could also read a key file under `~/.ssh`. In co:op, the agent could edit the checkout while that key stayed out of reach.

| Agent | Native sandbox | **With co:op** |
| --- | --- | --- |
| [Claude Code ](https://coop.dryga.com/claude-code-sandbox.html) | If you block Bash from reading `.env`, check Claude’s `Read` tool too. It has its own permissions. | co:op hides `.env` from both tools by default. |
| [Codex CLI ](https://coop.dryga.com/codex-sandbox.html) | Workspace-write blocks network access by default. On our Mac it could still read files outside the checkout. | You choose which folders to share. Other repos and SSH keys stay outside by default. |
| [Gemini CLI ](https://coop.dryga.com/gemini-cli-sandbox.html) | Both macOS Seatbelt profiles we tested could read `.env` and reach `example.com`. Gemini’s Docker sandbox uses different rules. | You approve sites for one run or the whole project. The network receipt shows which sites the agent contacted. |
| [Grok Build CLI ](https://coop.dryga.com/grok-cli-sandbox.html) | Sandboxing is off by default. In [xAI’s strict mode](https://docs.x.ai/build/features/sandbox), shell commands can reach the network on macOS. Linux blocks them. | The project’s file and network rules apply on both systems. |

## What if the agent installs a malicious package?

When your agent installs an npm dependency, the package can run code of its own. In a supply chain attack, even an update to a package you already use can introduce a malicious install script. [When npm runs install scripts](https://docs.npmjs.com/cli/v11/using-npm/scripts/#npm-install)

In our filtered co:op sandbox, an install script could read the README and write to the checkout. It couldn’t read the contents of `.env.coop-demo`. Your SSH keys and real provider credentials stay outside the container by default, and the script inherits the project’s network rules. **Review changes to your source code after an install.** The script can edit the same files your agent can.

> **How we tested it**
>
> We installed a local npm package on a Mac. Its install script checked which files it could read and wrote the results to the checkout. The network receipt showed no allowed external connections. [Package install test results](../examples/sandbox/2026-11-24-native-profiles.txt)

Claude’s native Bash sandbox also restricts programs it starts, including npm. co:op applies the same project rules whichever of the four agents installs the package. [Claude’s sandbox rules](https://code.claude.com/docs/en/sandboxing#what-the-sandbox-restricts)

## Can an agent break out of its sandbox?

An agent can leak a credential without escaping if the sandbox lets it read the file and contact the destination.

Any coding agent’s sandbox can have bugs. For example, see [Anthropic’s network sandbox fix (CVE-2025-66479)](https://github.com/anthropics/sandbox-runtime/security/advisories/GHSA-9gqj-5w7c-vx47) and the sandbox escapes fixed in [Claude Code (CVE-2026-55607)](https://github.com/anthropics/claude-code/security/advisories/GHSA-7835-87q9-rgvv) and [Codex CLI (CVE-2026-19592)](https://www.cve.org/CVERecord?id=CVE-2026-19592).

## What are you approving when a command starts with SSH?

Your agent asks to check a service on staging. Here’s the command in the approval prompt:

```sh
ssh staging.example /bin/sh -c "python3 healthcheck.py"
```

If you choose “always allow,” could the agent ask SSH to run a different script tomorrow? In our tests, Claude Code and Gemini offered rules broad enough to cover a different server, too.

| Agent | What the CLI offers to remember | Would changing the filename to `diskcheck.py` match? |
| --- | --- | --- |
| [Claude Code](https://code.claude.com/docs/en/permissions#bash) *2.1.312* | The terminal showed “Yes, and don’t ask again for: `ssh *`”. | **Yes.** The rule names neither the host nor the script. |
| [Codex CLI](https://github.com/openai/codex/blob/29eb79e9866b3262a777fdf8195c1b9e63684c91/codex-rs/tui/src/bottom_pane/approval_overlay.rs#L877-L892) *0.170.0* | “Yes, and don't ask again for commands that start with” `ssh staging.example /bin/sh -c 'python3 healthcheck.py'`. | **No.** The rule includes the filename. But if the agent adds `--verbose` after the closing quote, the command still matches. |
| [Gemini CLI](https://github.com/google-gemini/gemini-cli/blob/v0.67.0/packages/cli/src/ui/components/messages/ToolConfirmationMessage.tsx#L350-L378) *0.50.0 and 0.67.0* | “Allow for this session” remembers `ssh`. “Allow this command for all future sessions” remembers the same prefix if you enable permanent approvals. | **Yes.** Both choices cover other SSH commands. The second lasts beyond this session and is off by default. |
| [Grok Build](https://github.com/xai-org/grok-build/blob/2bdd1d6a6369de0e8c68132ea4539e9abd9e14a8/crates/codegen/xai-grok-workspace/src/permission/grants_tests.rs#L462-L484) *1.0.45* | The suggested rule is “Always allow: `ssh staging.example /bin/sh -c "python3 healthcheck.py"`”. It applies in this worktree. | **No.** Grok matches the whole command. Its separate always-approve mode applies to other tools as well. |
| **co:op** *Filtered project* | You choose which sites the project can reach before launch. The agent works inside the container without per-command approval prompts. | **SSH is blocked by default.** Your SSH keys and SSH-agent socket stay on your computer. |

Even an approval that names `healthcheck.py` will still match if a later release changes what the script does.

> **How we tested it**
>
> We captured the Claude and Codex terminal prompts. Claude’s programmatic interface suggested the exact command for the same request. For Gemini and Grok, we checked the code that builds the prompts and rules offline. [Full menu choices and test results](../examples/sandbox/2026-11-24-native-profiles.txt)

### Keep SSH keys and production secrets out of the sandbox

A malicious instruction in a bug report could trick an agent into posting a token from your `.env` file in a public issue. Anyone who finds that token could use it until it expires or you revoke it. [How prompt injection works](https://genai.owasp.org/llmrisk/llm01-prompt-injection/)

Command output can also put a secret in the conversation sent to your model provider. Depending on your plan and settings, that conversation may be used for training. Researchers have recovered some memorized training text by querying a model. If a token appears in a chat or issue, revoke it and issue a new one. [OpenAI’s data policy](https://help.openai.com/en/articles/5722486-how-your-data-is-used-to-improve-model-performance) · [Anthropic’s data policy](https://privacy.claude.com/en/articles/10023580-is-my-data-used-for-model-training) · [Training data study](https://arxiv.org/abs/2012.07805)

Keep SSH agent forwarding off. A forwarded SSH agent can let a coding agent log in as you even when your key stays on your laptop. co:op keeps both the key and the agent socket outside the container by default. [How co:op handles SSH keys](box.md#ssh-keys-and-agent-forwarding)

If staging looks unhealthy, [emisar](https://emisar.dev/) can give the agent access to container logs without your SSH key. You can require approval for a restart and see the host and exact command before you decide. [Walk through a service investigation](https://emisar.dev/docs/investigate-a-service)

### What else can a saved approval cover?

- **Another package.** A Codex rule for `cargo install` allowed both `cargo install cargo-insta` and `cargo install ripgrep`. [OpenAI’s approval test](https://github.com/openai/codex/blob/29eb79e9866b3262a777fdf8195c1b9e63684c91/codex-rs/core/src/exec_policy_tests.rs#L1137-L1158)
- **Writing a file.** Gemini allowed `git status` but asked before `git status > status.txt`. In co:op, the command wrote the file to our checkout. The neighboring folder stayed out of reach. [Gemini’s redirection check](https://github.com/google-gemini/gemini-cli/blob/v0.50.0/packages/core/src/policy/policy-engine.ts#L414-L425)

## What we could read, write and reach on a Mac

The project held four files that often contain secrets (`.env`, `.env.local`, `.ssh/id_ed25519` and `client.pem`). All files held harmless text, including the `id_ed25519` file we placed in a temporary folder under the Mac’s own `~/.ssh` directory.

| Sandbox | Project secrets | Key in `~/.ssh` | README and `.env.example` |
| --- | --- | --- | --- |
| Claude Code *Bash sandbox* | Read all four | Read | Read both |
| Codex CLI *Workspace profile* | Read all four | Read | Read both |
| Gemini CLI *permissive-open* | Read all four | Read | Read both |
| Gemini CLI *strict-open* | Read all four | Couldn’t read | Read both |
| **co:op** *Filtered* | Couldn’t read any | Couldn’t read | Read both |

> **How we tested it**
>
> We put a 20-byte `.env.coop-demo` file beside a 21-byte README and asked Claude to count the bytes in each file using Bash. In co:op, Bash reported 21 bytes for the README and “Operation not permitted” for the secret. After we blocked the secret in Claude’s Bash sandbox settings, its shell also lost access. [See the commands and output](box.md#what-claude-code-can-read-on-your-mac) · [Which Claude tools run outside Bash](https://code.claude.com/docs/en/sandboxing#what-runs-outside-the-sandbox)

### Can a Bash or Node.js script reach the internet?

We requested the headers from `https://example.com/` twice, once with curl and once with Node’s built-in `fetch`.

| Sandbox | curl from Bash | Node.js fetch |
| --- | --- | --- |
| Mac without a sandbox | HTTP 200 | HTTP 200 |
| Claude Code *No domains allowed* | Blocked | Failed to resolve the hostname |
| Codex CLI *Workspace profile* | Failed to resolve the hostname | Failed to resolve the hostname |
| Gemini CLI *Both Mac profiles* | HTTP 200 | HTTP 200 |
| **co:op** *No sites approved* | Blocked | Blocked |
| **co:op** *One site approved* | HTTP 200 | HTTP 200 |

Both Gemini profiles allow outgoing connections. In co:op, the receipt showed blocked DNS lookups on the first run and two allowed TLS connections on the second. Approving the website still left all four project secrets hidden.

### What about files outside the checkout?

| Sandbox | Read a neighboring file | Write in the checkout | Write in the host folder next to it |
| --- | --- | --- | --- |
| Codex CLI *Workspace profile* | Read | Allowed | Denied |
| Gemini CLI *permissive-open* | Read | Allowed | Denied |
| Gemini CLI *strict-open* | Denied | Allowed | Denied |
| **co:op** *Filtered* | Outside the container | Allowed | Outside the container |

> **How we tested it**
>
> We ran these checks on macOS 15.6.1 on November 24, 2026, using Claude Code 2.1.312’s Bash tool, Codex CLI 0.170.0’s workspace helper, Gemini CLI 0.50.0’s Seatbelt profiles and `coop run` in Docker. The Mac’s Grok CLI required sign-in, so the Grok row uses xAI’s documentation. [Test settings and results](../examples/sandbox/2026-11-24-native-profiles.txt)

## When a test needs the npm registry

If npm reports `EAI_AGAIN` in a filtered co:op session, check whether the sandbox blocked the registry. In our test, `coop net blocked registry.npmjs.org` showed exactly that. We gave npm access for one run and asked for a package version.

```sh
coop run --allow-domain registry.npmjs.org -- npm view is-number@7.0.0 version
```

npm returned `7.0.0` without installing the package. The network receipt showed connections to `registry.npmjs.org:443`. Afterward, `coop net check https://registry.npmjs.org` confirmed that the registry was still blocked for the project. [See the network checks and output](../examples/sandbox/2026-11-24-native-profiles.txt)

Approving a registry gives the agent access to other packages there, too. If only the build needs dependencies, install them in your project image and keep the registry closed during agent sessions. [Build a co:op project image](box.md#your-own-image-agentdockerfile)

## How the agent signs in without getting your key

The CLI gets a placeholder. A separate container checks each provider request and adds the real credential before sending it. Code in the agent container can still make approved requests that count against your account. [How co:op handles provider credentials](networking.md#provider-credentials-stay-outside-the-box)

Files the agent can read can still reach its provider. Add private files such as `customers.csv` to `.coopignore`. Cloud connectors on your provider account keep their own access. [Choose which files to hide](box.md#hiding-secrets)

[Try the file and network checks in a disposable project](https://coop.dryga.com/sandbox-guide.html).
