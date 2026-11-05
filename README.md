# Herdr customizations

```
herdr/
├── README.md        what each feature does, where its files go, what it needs
├── install.sh       ./install.sh            installs everything
│                    ./install.sh zed-button  installs only the features you name
├── layout/dev.json  the shared `dev` layout (a copy of `main` that uses the three mods)
└── features/
    ├── devcontainers/
    │   ├── mods/devcontainers/   the panel and the background sync
    │   ├── mods/dc-ports/        the Ports panel (port forwarding)
    │   ├── herdr-dc/             herdr-dc, a static Go helper → ~/.local/lib/herdr + ~/.local/bin
    │   ├── bin/herdr-dc-via      docker exec bridge → ~/.local/bin
    │   └── keys.json             ctrl-alt-shift-d, added to your keys.json
    ├── vscode-tasks-status/
    │   ├── mods/dev-status/      status bar with the VS Code task buttons
    │   └── upstream.diff         changes compared to the shipped workspace-status
    └── zed-button/
        ├── mods/dev-tabs/        tab bar with the Zed button (and zed.svg)
        ├── bin/herdr-zed-open    focus-or-open Zed helper → ~/.local/bin
        └── upstream.diff         changes compared to the shipped tabs
```

Three additions to the Herdr app, all shown in a custom `dev` layout. Copies of
the files that live in `~/.config/herdr-app/` (plus scripts in `~/.local/bin/`
and the `herdr-dc` builds in `~/.local/lib/herdr/`), so they can be put
back after a reinstall or a reset.

```sh
./install.sh                     # everything
./install.sh devcontainers zed-button   # only some features
```

Then pick the layout: `ctrl-shift-l` → **dev**. `install.sh` runs
`herdr-app check` on each mod it installs. Herdr regenerates `gpui-kit.d.ts`
and `jsconfig.json` in each mod folder on its own, so those aren't kept here.

## Layout — `layout/dev.json` → `layouts/dev.json`

Cloned from the shipped `main` layout (v0.1.9). Changes:
- tab bar uses `dev-tabs` instead of `tabs`
- status bar uses `dev-status` instead of `workspace-status`
- right panel adds **Devcontainers** and **Ports** sections next to Files and Changes
- a headless floating `devcontainers` block (`id: devcontainers-sync`,
  `props.headless`) so the start sync runs, and a space on a stopped container
  wakes it, even while the panel is closed

## Features

### 1. Devcontainers panel — `features/devcontainers/`

| File | Installed to |
|---|---|
| `mods/devcontainers/` | `mods/devcontainers/` |
| `mods/dc-ports/` | `mods/dc-ports/` |
| `herdr-dc/` (Go) | built to `~/.local/lib/herdr/herdr-dc-linux-{amd64,arm64}`, host's linked as `~/.local/bin/herdr-dc` |
| `bin/herdr-dc-via` | `~/.local/bin/herdr-dc-via` |
| `keys.json` | merged into `keys.json` (`ctrl-alt-shift-d` → `devcontainers.refresh`) |

Turns each devcontainer into a herdr-core machine, reached with
`herdr-dc-via` (docker exec) instead of ssh. docker exec skips shell
profiles, so the bridge sources the env file `herdr-dc attach` writes (below),
or, before there is one, takes `PATH` from the container user's login shell
(`$SHELL -lc env`, falling back to adding `~/.local/bin`). That way agents
installed in their own folders, like `claude` and `opencode`, show as
installed in the new-tab menu.

Every running container it added gets a **`herdr-dc attach <id>`**, an hc
background terminal named `dc-attach-<name>` (labelled `dc.container=<id>`),
started by the syncs and replaced when it ended or the container was rebuilt.
It does what VS Code's in-container server does, for as long as the container
runs:
- writes `env` next to the binary in the container: the user's environment as
  `userEnvProbe` finds it (once per attach), `remoteEnv` from the metadata
  (`${localEnv:…}`, `${containerEnv:…}`), `SSH_AUTH_SOCK` and `HERDR_DC=1`;
- relays the **SSH agent**: `in serve` listens on `<dir>/run/ssh-agent.sock`
  in the container, and each connection is passed to the host's agent over its
  own `docker exec -i … in attach <n>`. The host agent is looked up per
  connection: `ssh.agentSocket`, `$SSH_AUTH_SOCK`, the login shell's, then the
  GNOME keyring, systemd, gpg-agent and 1Password paths. Skipped when the
  container sets its own `SSH_AUTH_SOCK`.
- copies the host's **git config** (`~/.gitconfig`, `~/.config/git/config`)
  into the remote user's home, marked as ours and copied again on every
  attach, so host edits arrive without a rebuild. A file the image or dotfiles
  wrote is left alone unless it only has `[filter]`/`[safe]` sections (kept,
  above the host's). Credential helpers and `http.sslBackend` are dropped from
  the copy, and `core.editor`, `core.sshCommand` and `gpg.*.program` when their
  program isn't in the container; `gpg.ssh.allowedSignersFile` is copied too;
- sets git's **credential helper** to `herdr-dc git credential`, which asks the
  host's `git credential fill|approve|reject` over the channel (`rpc.sock`);
  without a channel, or a host credential, git just prompts as usual;
- adds the workspace repo to `safe.directory` when another user owns it;
- copies `~/.ssh/known_hosts` when the container has `ssh` and none of its own;
- relays **GPG**: the container's `agent-socket` becomes a channel listener
  passed to the host's `agent-extra-socket` (started with `gpgconf --launch`
  when it isn't there), so signing uses the host's keys and pinentry. The
  container's own agent is stopped, `no-autostart` goes in its `gpg.conf`, and
  the host's public keys and ownertrust are imported when it has no keyring
  (with keyboxd on both sides, its socket is relayed instead). Skipped when the
  container has no `gpg`/`gpgconf` or private keys of its own.

`herdr-dc status <id>` shows what's set up and which host agents are used;
`hc read dc-attach-<name>` shows the attach's log.

Lists host folders with a devcontainer config, and lets you create (`devcontainer up`), start and open
them. Syncs at start, when the panel opens, on Refresh and on the key; nothing
watches containers in the background (a 500 ms timer reads the layout to notice
the panel opening; it never runs docker). After start, stop, delete or create it
syncs every 2 s until the container gets there, the action fails or a minute
passes (for create, a minute from the end of the build). Containers changed
outside Herdr show as they were until the next sync, except that selecting a
space whose machine went away syncs once.

Needs: `docker`, and `@devcontainers/cli`
(`npm i -g --prefix ~/.local @devcontainers/cli`).

#### Ports panel — `mods/dc-ports/`, `herdr-dc/`

Lists the listening TCP ports of every running devcontainer (process name, and
the bind address when it's loopback only), and forwards one to this computer
when you click **Forward**. Nothing is forwarded on its own, and ports are only
scanned when the panel opens or on Refresh (a 500 ms timer reads the layout to
notice the panel reopening; it never runs docker). A forward takes the same
port on `127.0.0.1` when it's free, a random one otherwise; click
`localhost:N` to open it in the browser, `×` to stop it.

Like VS Code shipping its server, `herdr-dc` copies itself into the
container (piped through `docker exec`, again whenever its version changes),
since containers rarely have socat or nc. It goes in `/tmp/.herdr-dc/`, or
`/var/tmp/.herdr-dc/` or the remote user's `~/.cache/herdr-dc/` when `/tmp` is
`noexec`; `/tmp/.herdr-dc/dir` says which.
- `ports scan <id>` lists ports from `/proc/net/tcp{,6}` (skips Docker's DNS on
  `127.0.0.11` and herdr-core's own sockets).
- `ports forward <id> <port>` serves localhost and connects to the container's IP.
  A server on loopback only can't be reached that way, so it starts
  `in relay` inside the container (`docker exec -i`), listening on the
  container's IP; the relay ends when the forward does (stdin closes).

Each forward is an hc background terminal named `pf-<container id>-<port>`
that labels itself `dc.host_port=<port>`, so forwards outlive the app and the
panel rebuilds its list from `hc ls`. A scan stops forwards whose container is
no longer running.

Needs: `go` to build (`install.sh` skips it with a warning otherwise; it
rebuilds only when the source changes).

#### Configuration — `~/.config/herdr-dc/config.json`

Optional; a missing file or key means the default.
- `dockerPath` (`"docker"`): the docker CLI to use, e.g. `podman` or a full
  path when `docker` isn't on the app's `PATH`. Used by `herdr-dc`,
  `herdr-dc-via`, both mods (read once, at app start) and `devcontainer up
  --docker-path`.
- `ssh.agent` (`true`): relay the host's SSH agent into containers.
- `git.copyConfig` (`true`), `git.safeDirectory` (`true`): the git steps above.
- `git.credentialHelper` (`"global"`): where to set ours, `global`, `system`
  (as root) or `none`.
- `ssh.knownHosts` (`true`): copy `known_hosts`.
- `gpg.agent` (`true`): relay the host's gpg-agent and import its public keys.
- `ssh.agentSocket` (`""`): the host agent's socket, when it's somewhere the
  search above doesn't look (1Password, Secretive, KeePassXC…).

### 2. VS Code tasks in the status bar — `features/vscode-tasks-status/`

| File | Installed to |
|---|---|
| `mods/dev-status/` | `mods/dev-status/` |
| `upstream.diff` | — (changes compared to shipped `workspace-status`) |

The status bar plus a button for each entry in `VsCodeTaskButtons.tasks`
(from the Task Buttons extension) in the project's `.vscode/settings.json`.
Each button runs its task from `.vscode/tasks.json` in a tab named after the
button, on the space's machine (inside the devcontainer when it's one).
Clicking again selects the tab while the task runs, and runs it again once it
has finished. Buttons that don't fit go behind "+N".

### 3. Open in Zed button — `features/zed-button/`

| File | Installed to |
|---|---|
| `mods/dev-tabs/` (incl. `zed.svg`) | `mods/dev-tabs/` |
| `bin/herdr-zed-open` | `~/.local/bin/herdr-zed-open` |
| `upstream.diff` | — (changes compared to shipped `tabs`) |

A "Zed" button in the tab bar that opens the space's folder in Zed on the host.
If a Zed window already has that folder open, it is focused; otherwise a new
window opens (`zed -n`). For a devcontainer space it maps the container's
workspace folder back to the host folder it mounts.

`herdr-zed-open` finds open folders from Zed's window titles (X11, `xprop`),
matching by folder name. Plain `zed <folder>` can't do this alone because
`cli_default_open_behavior: existing_window` would add an unopened folder to
the active window. Without X11/`xprop` it always opens a new window.

Needs: `zed` on `PATH`, `xprop` for the focus part.

## When Herdr updates its shipped mods

`dev-tabs` and `dev-status` are clones of `tabs` and `workspace-status`
(v0.1.9) and won't pick up upstream changes. To rebase them, clone the new
shipped mod again and re-apply the `upstream.diff` of the matching feature.

## Dependencies between features

- `dev-tabs` (zed-button) also holds the tab bar's **Devcontainers** and
  **Ports** panel toggles, next to Files and Changes. Without it, those panels
  can only be reached through the layout.
- The Ports panel names containers after the machines in the
  `devcontainers.machines` shared state (else their folder).
- The Zed button reads the `devcontainers.machines` shared state the
  devcontainers mod writes, to map a devcontainer space back to its host
  folder. It shows only for local spaces and devcontainer spaces.
- The status bar's tasks look up `.vscode/` on the space's machine through
  `hc`, so they work in any space, devcontainer or not.
- `layout/dev.json` references all three mods. Installing only some features
  leaves an empty block where a missing mod would be.
