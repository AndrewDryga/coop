<!-- The GitHub release body: a short, plain-English summary of the release named below. The
     release skill rewrites this file in each finalize commit, and the release workflow publishes
     its section as is (tools/releasenotes). CHANGELOG.md keeps every change. -->

## 13.1.0

Coop 13.1.0 closes several ways a box could reach outside itself, gives each project its own tools and download cache, and adds support for SELinux hosts and older Linux kernels.

- Each project with a `.tool-versions` gets its tools built into its own image, and each project has its own download cache, so one project's box can no longer change the tools or modules another project runs.
- Coop's Git commands refuse pointers an agent planted in `.git` to reach another repository on your computer, while worktrees, submodules and shared clones keep working.
- Editor sessions never let a box run commands or touch files on your computer through the editor, filtered Compose services can no longer reach your computer, and kernel protection now covers Unix sockets in hidden folders.
- Coop runs on SELinux hosts such as Fedora and RHEL, starts on kernels too old for kernel protection by falling back to mount protection, and `coop setup apparmor` confines its Linux protection helper on AppArmor hosts.
- A launch now says when it uses mount protection, and dependencies are up to date.

Upgrading:

- Offline runs of a project with a `.tool-versions` need `coop build` once. The old `coop-asdf` and `coop-cache` volumes are no longer used; `docker volume rm coop-asdf coop-cache` removes them. `COOP_NO_ASDF` is retired.
- A checkout made with `git init --separate-git-dir` is refused until its Git folder moves back into `.git`; the message gives the commands.
- Filtered runs with Compose services need Docker Engine 28 or later.

[Full changelog](https://github.com/AndrewDryga/coop/compare/v13.0.0...v13.1.0)
