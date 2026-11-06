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
    │   │                         README.md: how it works, its config
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

Lists host folders with a devcontainer config, and lets you create (`devcontainer up`), start and open
them. The right-click menu's **Open in Zed** opens a row's host folder in Zed
(focusing the window that already has it), and **Open in host machine** opens a
local space on it (or selects the one already open), whatever the container's state.
Syncs at start, when the panel opens, on Refresh and on the key; nothing
watches containers in the background (a 500 ms timer reads the layout to notice
the panel opening; it never runs docker). After start, stop, delete or create it
syncs every 2 s until the container gets there, the action fails or a minute
passes (for create, a minute from the end of the build). Containers changed
outside Herdr show as they were until the next sync, except that selecting a
space whose machine went away syncs once.

Each devcontainer becomes a herdr-core machine, reached with `herdr-dc-via`
(docker exec) instead of ssh, so agents installed in the container show in the
new-tab menu. The machine exists only while its container runs: a sync that
finds the container stopped removes the machine, so the Spaces sidebar doesn't
list it as unreachable, and starting it adds the machine again with its spaces
(the container's core keeps them). Every running container also gets a background `herdr-dc attach`
that does what VS Code's in-container server does: the user's environment and
`remoteEnv`, the host's SSH and GPG agents, git config and credentials, docker
registry credentials, `BROWSER` opening the host's browser, and forwarding
`forwardPorts`. How all of that works, and its configuration
(`~/.config/herdr-dc/config.json`), is in
[`herdr-dc/README.md`](features/devcontainers/herdr-dc/README.md).

Needs: `docker`, `@devcontainers/cli`
(`npm i -g --prefix ~/.local @devcontainers/cli`), and `go` to build
`herdr-dc` (`install.sh` skips it with a warning otherwise).

#### Ports panel — `mods/dc-ports/`

Lists the listening TCP ports of every running devcontainer (process name, and
the bind address when it's loopback only), and forwards one to this computer
when you click **Forward**. The panel forwards nothing on its own (the
container's `forwardPorts` are forwarded by `herdr-dc attach` and show
here with their `portsAttributes` label), and ports are only
scanned when the panel opens or on Refresh (a 500 ms timer reads the layout to
notice the panel reopening; it never runs docker). A forward takes the same
port on `127.0.0.1` when it's free, a random one otherwise; click
`localhost:N` to open it in the browser, `×` to stop it. Forwards outlive the
app, and stop when their container does. See
[`herdr-dc/README.md`](features/devcontainers/herdr-dc/README.md#ports) for
how they're made.

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
- The devcontainers menu's **Open in Zed** runs `herdr-zed-open` from
  zed-button; without it the entry shows an error toast.
- The Zed button reads the `devcontainers.machines` shared state the
  devcontainers mod writes, to map a devcontainer space back to its host
  folder. It shows only for local spaces and devcontainer spaces.
- The status bar's tasks look up `.vscode/` on the space's machine through
  `hc`, so they work in any space, devcontainer or not.
- `layout/dev.json` references all three mods. Installing only some features
  leaves an empty block where a missing mod would be.
