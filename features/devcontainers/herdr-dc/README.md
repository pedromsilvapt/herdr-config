# herdr-dc

A static Go helper used by the `devcontainers` and `dc-ports` mods.
`install.sh` builds it to `~/.local/lib/herdr/herdr-dc-linux-{amd64,arm64}` and
links the host's build as `~/.local/bin/herdr-dc`. It needs `go` to build
(`install.sh` skips it with a warning otherwise; it rebuilds only when the
source changes).

Like VS Code shipping its server, `herdr-dc` copies itself into the
container (piped through `docker exec`, again whenever its version changes),
since containers rarely have socat or nc. It goes in `/tmp/.herdr-dc/`, or
`/var/tmp/.herdr-dc/` or the remote user's `~/.cache/herdr-dc/` when `/tmp` is
`noexec`; `/tmp/.herdr-dc/dir` says which.

## `herdr-dc-via`

Each devcontainer is a herdr-core machine reached with `herdr-dc-via` (docker
exec) instead of ssh. docker exec skips shell profiles, so the bridge sources
the env file `herdr-dc attach` writes (below), or, before there is one, takes
`PATH` from the container user's login shell (`$SHELL -lc env`, falling back
to adding `~/.local/bin`). That way agents installed in their own folders,
like `claude` and `opencode`, show as installed in the new-tab menu.

## `herdr-dc attach <id>`

Every running container the devcontainers mod added gets a `herdr-dc attach
<id>`, an hc background terminal named `dc-attach-<name>` (labelled
`dc.container=<id>`), started by the syncs and replaced when it ended or the
container was rebuilt. It does what VS Code's in-container server does, for as
long as the container runs:
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
- gives the container's docker CLI the host's **registry credentials**: when
  the host has some (`credsStore`, `credHelpers`, or `auths` with a token),
  `credsStore` becomes `herdr-dc` in the remote user's docker config (unless
  it has a store of its own, like VS Code's), and
  `/usr/local/bin/docker-credential-herdr-dc` asks the host over the channel,
  which answers from `credHelpers`, then `auths`, then `credsStore`. `docker
  login` inside isn't stored on the host (it fails; log in on the host).
  Skipped without a docker CLI in the container.
- sets **`BROWSER`** to `<dir>/browser`, which opens http(s) URLs in the host's
  browser (`xdg-open`, or `browser.command`), detached and never through a
  shell. A URL on the container's `localhost` is forwarded first, as an hc
  `pf-<id>-<port>` terminal like the ports panel's (reused when there is one),
  and rewritten to the host port when it differs, so OAuth callbacks such as
  `gh auth login --web` work. Without a channel it prints the URL. Skipped when
  the container sets `BROWSER` itself.
- forwards the container's **`forwardPorts`** (see [Ports](#ports)).

`herdr-dc status <id>` shows what's set up and which host agents are used;
`hc read dc-attach-<name>` shows the attach's log.

## Ports

Used by the Ports panel (`dc-ports`) and by `attach`.
- `ports scan <id>` lists ports from `/proc/net/tcp{,6}` (skips Docker's DNS on
  `127.0.0.11` and herdr-core's own sockets).
- `ports forward <id> <port>` serves localhost and connects to the container's IP.
  A server on loopback only can't be reached that way, so it starts
  `in relay` inside the container (`docker exec -i`), listening on the
  container's IP; the relay ends when the forward does (stdin closes). When
  the host can't reach the container's IP (Docker Desktop, Colima, a remote
  engine), or a connection there is refused (the server wasn't listening yet,
  or only on loopback), the connection goes through `docker exec … in
  connect` instead (~40 ms more per connect). A forward ends when its
  container stops (`docker wait`), so a restart doesn't leave it on an old IP.
  The port doesn't have to be listening yet.
- `ports auto <id>` prints `forwardPorts` (from the `devcontainer.metadata`
  label; `"service:port"` entries for compose services are skipped) with
  their `portsAttributes` label, `onAutoForward` and `protocol`. Port keys
  and `a-b` ranges are matched, process patterns aren't.

Each forward is an hc background terminal named `pf-<container id>-<port>`
that labels itself `dc.host_port=<port>`, so forwards outlive the app and the
panel rebuilds its list from `hc ls`. A scan stops forwards whose container is
no longer running.

`herdr-dc attach` forwards the `ports auto` ports (except `onAutoForward:
"ignore"`) as the same `pf-…` terminals, labelled `dc.label`, reusing ones
already running. For the ones it started, it watches for up to 2 minutes
(every 2 s) for the port to listen, then sends a desktop notification
(`notify-send`; the default `notify`) or opens the browser (`openBrowser`,
`openBrowserOnce`, `openPreview`); `silent` does neither. So an app restart
(forwards survive) doesn't notify again; a container restart does.

## Configuration

`~/.config/herdr-dc/config.json`. Optional; a missing file or key means the default.
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
- `docker.credentialHelper` (`true`): the registry credentials above.
- `browser.enabled` (`true`), `browser.command` (`""`): `BROWSER` above, and
  what opens the URL on the host (words split on spaces, URL appended;
  `xdg-open` when empty).
- `ssh.agentSocket` (`""`): the host agent's socket, when it's somewhere the
  search above doesn't look (1Password, Secretive, KeePassXC…).
- `ports.autoForward` (`true`): forward `forwardPorts` on attach.
