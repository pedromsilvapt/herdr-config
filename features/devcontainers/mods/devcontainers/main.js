// Devcontainers as machines. The panel keeps a list of host folders
// (shared "devcontainers.folders"), each uncreated, stopped or running:
// folders join it from containers the devcontainer tools made here (label
// devcontainer.local_folder), from local spaces whose folder has a
// devcontainer config, and from Add folder. A container becomes a herdr-core
// machine reached with `herdr-dc-via` (docker exec) instead of ssh. Synced at
// start, when the panel opens, on Refresh and on the `devcontainers.refresh`
// key; nothing watches containers in the background. After an action (start,
// stop, delete, create) it syncs every 2 s until the container is in the state
// asked for, the action fails or a minute passes (for create, a minute from
// the end of the build). A container changed outside Herdr shows as it was
// until then, except that selecting a space whose machine went away looks
// again.
//
// hc only saves a machine it can reach, and the bridge never starts a
// container, so a machine exists only while its container runs: a sync that
// finds the container stopped removes the machine (so the Spaces sidebar
// doesn't list it as unreachable), and it's added again when the container
// runs, its spaces with it (they're kept by the container's core). Opening
// or starting one starts it, and so does selecting a space on one stopped
// outside Herdr since the last sync. Create builds one with
// `devcontainer up` (@devcontainers/cli) in a terminal window, with the
// flags `herdr-dc dotfiles args` prints (dotfiles, the Wayland mount, and
// --skip-post-attach, since attach runs postAttachCommand). Rebuild (and
// Rebuild without cache, which adds --build-no-cache) deletes the container
// and its machine, then builds again the same way; it opens a space on the new
// container only when the old one had one.
//
// Clicking a row opens its container (or creates it); right-clicking opens
// a menu of the rest, a popup copy of this mod (props.menu) that hands its
// choice to the panel through shared "devcontainers.request".
//
// Only machines this mod added (shared "devcontainers.machines") are ever
// changed or removed; "added" means the machine is there now. A folder can leave the list only while it has no
// container; otherwise the next sync would bring it back.
//
// Each running container this mod added gets a `herdr-dc attach` (SSH and
// GPG agents, X11, the env file herdr-dc-via sources) in a background hc terminal named
// dc-attach-<name>, labelled dc.container=<id>. Started before the machine is
// added (herdr-dc-via waits for its env file), and by the syncs above, not
// watched: one that ended (its container stopped) is replaced on the next
// sync while the container runs, and one for an old id (rebuilt) is killed.
//
// Two copies in the `dev` layout: the panel, and a headless floating one
// (props.headless) that draws nothing but is always shown, so the start sync
// runs from launch and a space on a stopped container wakes it even while the
// panel is closed. A shared lock keeps them from
// writing at once. The build window's title bar is this mod too (props.part
// "bar").

import { View, div, svg } from "gpui-kit";
import { v_flex, h_flex } from "gpui-base";
import { exec, hc, home, theme, run, notify, props, shared, setShared, spaces, focused, machines, terminals, layout, nextCommand, pickFolder, openOverlay, closeOverlay } from "herdr";

const I = (name) => `mods/workspace-chrome/${name}.svg`;
const OWNED = "devcontainers.machines";
const FOLDERS = "devcontainers.folders";
const LOCK = "devcontainers.lock";
const REQUEST = "devcontainers.request";
const BRIDGE = "herdr-dc-via";
const DC_HINT = "herdr-dc isn't installed, so containers get no SSH agent or env file. Run ./install.sh devcontainers from the herdr customizations repo.";
const ATTACH = "dc-attach-";
const attachName = (name) => `${ATTACH}${name}`.slice(0, 32);
const lastLine = (s) => (s ?? "").trim().split("\n").filter(Boolean).pop() ?? "";
const CLI_HINT = "Install @devcontainers/cli: npm i -g --prefix ~/.local @devcontainers/cli";

const basename = (p) => p.replace(/\/+$/, "").split("/").pop() ?? p;
const slug = (s) => s.toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, "") || "devcontainer";
const tilde = (p) => (p.startsWith(home()) ? "~" + p.slice(home().length) : p);
const ORDER = { running: 0, stopped: 1, uncreated: 2 };
// The state an action leaves its container in.
const EXPECT = { starting: "running", opening: "running", stopping: "stopped", deleting: "uncreated" };
// Busy states that run a `devcontainer up`.
const BUILDING = (b) => b === "creating" || b === "rebuilding" || b === "rebuilding-clean";

function find(node, id) {
  if (node?.id === id) return node;
  for (const child of node?.children ?? []) {
    const found = find(child, id);
    if (found) return found;
  }
  return null;
}

// ok | missing | noconfig for each folder, in one call.
const HEALTH = 'for f; do if [ ! -d "$f" ]; then echo missing; elif [ -f "$f/.devcontainer/devcontainer.json" ] || [ -f "$f/.devcontainer.json" ]; then echo ok; else echo noconfig; fi; done';

async function health(folders) {
  if (!folders.length) return {};
  const r = await exec("sh", ["-c", HEALTH, "_", ...folders]).catch(() => null);
  const out = (r?.stdout ?? "").split("\n");
  return Object.fromEntries(folders.map((f, i) => [f, out[i] || "ok"]));
}

// The user VS Code / the devcontainer CLI open terminals as.
function remoteUser(c) {
  let meta = [];
  try {
    meta = JSON.parse(c.Config?.Labels?.["devcontainer.metadata"] ?? "[]");
  } catch {}
  const pick = (k) => [...(Array.isArray(meta) ? meta : [meta])].reverse().find((m) => m?.[k])?.[k];
  return pick("remoteUser") ?? pick("containerUser") ?? (c.Config?.User || "root");
}

function describe(c) {
  const folder = c.Config.Labels["devcontainer.local_folder"];
  const mount = (c.Mounts ?? []).find((m) => m.Source === folder);
  return {
    id: c.Id.slice(0, 12),
    folder,
    workdir: mount?.Destination ?? `/workspaces/${basename(folder)}`,
    user: remoteUser(c),
    running: Boolean(c.State?.Running),
  };
}

const targetOf = (d) => `${BRIDGE} ${d.id} ${d.user} ${d.workdir}`;

// dockerPath from herdr-dc's config (~/.config/herdr-dc/config.json), read
// once; a change takes effect when the app restarts.
let dockerBin = null;
async function docker() {
  if (dockerBin) return dockerBin;
  const r = await exec("sh", ["-c", 'cat "${XDG_CONFIG_HOME:-$HOME/.config}/herdr-dc/config.json" 2>/dev/null']).catch(() => null);
  let p = "";
  try {
    p = JSON.parse(r?.stdout || "{}").dockerPath ?? "";
  } catch {}
  if (p.startsWith("~/")) p = home() + p.slice(1);
  dockerBin = p || "docker";
  return dockerBin;
}

async function sleep(s) {
  await exec("sleep", [String(s)]).catch(() => null);
}

function hcError(r) {
  try {
    return JSON.parse(r.stdout || r.stderr).error?.message ?? (r.stderr || r.stdout).trim();
  } catch {
    return (r.stderr || r.stdout || "it didn't answer").trim();
  }
}

export default class Devcontainers extends View {
  init(_props, cx) {
    if (props()?.part === "bar" || props()?.menu) return;
    this.items = null; // [{ folder, name, health, state, c: describe() | null, added, error }]
    this.problem = null; // docker missing and the like
    this.hint = null; // herdr-dc missing
    this.busy = {}; // folder -> "creating" | "starting" | "stopping" | "deleting" | "opening"
    this.errors = {}; // folder -> what the last action said
    this.building = {}; // folder -> address of its `devcontainer up` terminal
    this.openAfter = new Set(); // folders to open a space on once their machine is ready
    this.checked = new Set(); // local space folders already looked at
    this.expect = {}; // folder -> { state, until }: an action's outcome, synced for until it shows
    this.waiting = false;
    this.syncing = false;
    this.lastSync = null;
    this.shown = false;
    if (props()?.headless) this.sync(cx);
    else {
      // A hidden block isn't rendered, so render can't see the panel close and
      // reopen; this only reads the layout (no docker) and syncs on opening.
      this.watchShown(cx);
      cx.timer.every(500, (cx) => this.watchShown(cx));
    }
    cx.spawn(async (cx) => {
      for (;;) {
        const c = await nextCommand();
        if (c === "refresh") this.sync(cx, true);
      }
    });
  }

  // Shown: the dev layout's panel is open on this mod. Outside that stack
  // (another layout), showing at all counts.
  isShown() {
    const root = layout();
    const body = find(root, "panel-body");
    if (!body?.children?.some((c) => c.id === "devcontainers")) return true;
    return !find(root, "panel")?.hidden && body.active === "devcontainers";
  }

  watchShown(cx) {
    const now = this.isShown();
    if (now && !this.shown) this.sync(cx);
    this.shown = now;
  }

  // A lock older than a minute is from a copy that went away mid-sync.
  lock() {
    const held = shared(LOCK);
    if (held && Date.now() - held < 60000) return false;
    setShared(LOCK, Date.now());
    return true;
  }

  async locked(fn) {
    for (let i = 0; !this.lock(); i++) {
      if (i >= 20) throw new Error("Another sync is stuck; try again in a minute.");
      await sleep(1);
    }
    try {
      return await fn();
    } finally {
      setShared(LOCK, null);
    }
  }

  sync(cx, loud = false) {
    if (this.syncing) return;
    if (!this.lock()) {
      // Another copy is writing: just show what's there.
      cx.spawn(async (cx) => {
        await this.reconcile(false).catch(() => null);
        this.settle();
        cx.notify();
      });
      return;
    }
    this.syncing = true;
    cx.notify();
    cx.spawn(async (cx) => {
      try {
        await this.reconcile().finally(() => setShared(LOCK, null));
        this.problem = null;
        if (loud) notify({ title: "Devcontainers refreshed", body: this.counts(), level: "success", timeout: 3000 });
      } catch (e) {
        this.problem = String(e?.message ?? e);
      }
      this.syncing = false;
      this.lastSync = new Date();
      this.settle();
      cx.notify();
    });
  }

  // Drops the actions whose container got where they were going, failed or
  // ran out of time.
  settle() {
    const now = Date.now();
    for (const [folder, e] of Object.entries(this.expect)) {
      const item = this.items?.find((i) => i.folder === folder);
      const done =
        e.state === "uncreated"
          ? !item || item.state === "uncreated"
          : e.state === "stopped"
            ? item?.state === "stopped"
            : item?.state === "running" && item.added && machines().find((m) => m.name === item.name)?.layout === "ready";
      if (done || item?.error || this.errors[folder]) delete this.expect[folder];
      else if (e.until !== null && now > e.until) {
        delete this.expect[folder];
        if (BUILDING(this.busy[folder])) delete this.busy[folder];
        this.openAfter.delete(folder);
        this.errors[folder] = `Still not ${e.state} after a minute.`;
      }
    }
  }

  // A quiet sync: writes when it gets the lock, else only reads.
  async look() {
    if (this.lock()) await this.reconcile().catch(() => null).finally(() => setShared(LOCK, null));
    else await this.reconcile(false).catch(() => null);
    this.settle();
  }

  // Syncs every 2 s while an action waits for its container. A create's
  // minute starts when its build terminal ends; until then nothing runs
  // (terminals() is Herdr's own list, no hc call).
  waitFor(cx) {
    if (this.waiting || !Object.keys(this.expect).length) return;
    this.waiting = true;
    cx.spawn(async (cx) => {
      while (Object.keys(this.expect).length) {
        await cx.sleep(2000);
        let due = false;
        for (const [folder, e] of Object.entries(this.expect)) {
          const address = this.building[folder];
          const building = address && terminals().some((t) => t.address === address && t.state === "live");
          if (building) continue;
          if (e.until === null) e.until = Date.now() + 60000;
          due = true;
        }
        if (!due) continue;
        await this.look();
        cx.notify();
      }
      this.waiting = false;
    });
  }

  async discover() {
    const ps = await exec(await docker(), ["ps", "-a", "-q", "--no-trunc", "--filter", "label=devcontainer.local_folder"]);
    if (ps.code !== 0) throw new Error(ps.stderr.trim() || "docker ps failed");
    const ids = ps.stdout.split("\n").map((s) => s.trim()).filter(Boolean);
    if (!ids.length) return [];
    const ins = await exec(await docker(), ["inspect", ...ids]);
    if (ins.code !== 0) throw new Error(ins.stderr.trim() || "docker inspect failed");
    return JSON.parse(ins.stdout).map(describe);
  }

  async machineList() {
    const r = await hc(["machine", "ls", "--json"]);
    return r.code === 0 ? JSON.parse(r.stdout).machines ?? [] : [];
  }

  // A machine name for a folder joining the list: its folder's, made unique
  // against machines this mod didn't add and the list's other names.
  nameFor(folder, folders, known, owned) {
    const used = new Set([...known.map((m) => m.name).filter((n) => !owned[n]), ...Object.values(folders).map((f) => f.name)]);
    const base = slug(basename(folder));
    let name = base;
    for (let i = 1; used.has(name); i++) name = i === 1 ? `${base}-dc` : `${base}-dc-${i}`;
    return name;
  }

  // write: false only reads (the list, machines and containers as they are now).
  async reconcile(write = true) {
    const found = await this.discover();
    const known = await this.machineList();
    const owned = { ...(shared(OWNED) ?? {}) };
    const folders = { ...(shared(FOLDERS) ?? {}) };
    const before = JSON.stringify([owned, folders]);
    const join = (folder) => {
      if (folders[folder]) return;
      const mine = Object.keys(owned).find((n) => owned[n].folder === folder);
      folders[folder] = { name: mine ?? this.nameFor(folder, folders, known, owned) };
    };

    if (write) {
      for (const d of found) join(d.folder);
      // Local spaces whose folder has a devcontainer config, looked at once each.
      const fresh = [...new Set(spaces().filter((s) => s.machine === null && s.folder).map((s) => s.folder))].filter((f) => !folders[f] && !this.checked.has(f));
      fresh.forEach((f) => this.checked.add(f));
      const h = await health(fresh);
      for (const f of fresh) if (h[f] === "ok") join(f);

      // Machines whose container is gone or stopped: the folder stays,
      // uncreated or stopped, and the machine is added again when it runs
      // (its spaces live in the container's core, so they come back with it).
      // That keeps stopped containers out of the Spaces sidebar.
      for (const [name, o] of Object.entries(owned)) {
        if (found.some((d) => d.folder === o.folder && d.running)) continue;
        const i = known.findIndex((m) => m.name === name);
        if (i !== -1) {
          // Kept as ours if hc couldn't remove it, so the next sync tries again.
          const r = await hc(["machine", "rm", name, "--json"]).catch(() => null);
          if (r?.code !== 0) continue;
          known.splice(i, 1);
        }
        delete owned[name];
      }
    }

    const h = await health(Object.keys(folders));
    const items = [];
    for (const [folder, { name }] of Object.entries(folders)) {
      const d = found.find((x) => x.folder === folder) ?? null;
      const machine = known.find((m) => m.name === name);
      let error = null;
      if (d && write) {
        if (/\s/.test(d.workdir) || /\s/.test(d.user)) {
          error = "Its workspace folder has a space in it; the bridge can't pass that yet.";
        } else if (machine && owned[name] && machine.target !== targetOf(d)) {
          // Rebuilt: same project, new container.
          await hc(["machine", "rm", name, "--json"]).catch(() => null);
          delete owned[name];
          if (d.running) error = await this.add({ ...d, name }, owned);
        } else if (machine && !owned[name]) {
          error = `A machine called ${name} already exists.`;
        } else if (!machine && d.running) {
          error = await this.add({ ...d, name }, owned);
        }
      }
      if (d?.running && BUILDING(this.busy[folder])) delete this.busy[folder];
      items.push({ folder, name, health: h[folder] ?? "ok", c: d, state: d ? (d.running ? "running" : "stopped") : "uncreated", added: Boolean(owned[name]), error });
    }
    items.sort((a, b) => ORDER[a.state] - ORDER[b.state] || a.name.localeCompare(b.name));
    if (write && JSON.stringify([owned, folders]) !== before) {
      setShared(OWNED, owned);
      setShared(FOLDERS, folders);
    }
    if (write) await this.attachAll(items).catch(() => null);
    this.items = items;
    await this.watchBuilds();
    this.openReady();
  }

  // Adds a running container; returns why it failed, or null. Its attach
  // starts first: herdr-dc-via waits for the env file attach writes while one
  // runs, so herdr-core's server (started by `machine add`) gets that
  // environment rather than just PATH.
  async add(item, owned) {
    await this.attachAll([{ name: item.name, state: "running", added: true, c: item }], { prune: false }).catch(() => null);
    const r = await hc(["machine", "add", "--name", item.name, "--via", targetOf(item), "--install", "--json"]).catch((e) => ({ code: 1, stdout: "", stderr: String(e) }));
    if (r.code !== 0) return hcError(r);
    owned[item.name] = { folder: item.folder, id: item.id };
    return null;
  }

  async herdrDc() {
    if (this.dcBin) return this.dcBin;
    const r = await exec("sh", ["-c", "command -v herdr-dc"]).catch(() => null);
    if (r?.code === 0) this.dcBin = r.stdout.trim();
    return this.dcBin ?? null;
  }

  // One live `herdr-dc attach` per running, added container: ended ones and
  // ones for another container id are cleared, missing ones started. An
  // attach that failed while its container still runs shows why on the row.
  // prune: false leaves attaches for containers not in items alone.
  async attachAll(items, { prune = true } = {}) {
    const bin = await this.herdrDc();
    this.hint = bin ? null : DC_HINT;
    if (!bin) return;
    const r = await hc(["ls", "--all", "--json"]);
    if (r.code !== 0) return;
    const want = new Map(items.filter((i) => i.state === "running" && i.added && !i.error).map((i) => [attachName(i.name), i]));
    const have = new Set();
    for (const t of JSON.parse(r.stdout).terminals ?? []) {
      if (!t.name?.startsWith(ATTACH)) continue;
      const item = want.get(t.name);
      if (!item && !prune) continue;
      if (item && t.state === "live") {
        const g = await hc(["get", t.address, "--json"]).catch(() => null);
        let id = null;
        try {
          id = JSON.parse(g.stdout).labels?.["dc.container"] ?? null;
        } catch {}
        if (id === item.c.id) {
          have.add(t.name);
          continue;
        }
      } else if (item) {
        const why = lastLine((await hc(["read", t.address]).catch(() => null))?.stdout).replace(/^herdr-dc: /, "");
        if (why && why !== "the container stopped") item.attachError = `Attach: ${why}`;
      }
      if (t.state === "live") await hc(["kill", t.address, "--json"]).catch(() => null);
      await hc(["rm", t.address, "--json"]).catch(() => null);
    }
    for (const [name, item] of want) {
      if (have.has(name)) continue;
      await hc(["run", "--bg", "--json", "--no-parent", "--name", name, "--label", `dc.container=${item.c.id}`, "--", bin, "attach", item.c.id]).catch(() => null);
    }
  }

  // A build whose terminal ended without a running container failed.
  async watchBuilds() {
    const folders = Object.keys(this.building);
    if (!folders.length) return;
    const r = await hc(["ls", "--json"]).catch(() => null);
    let live = [];
    try {
      live = JSON.parse(r.stdout).terminals.filter((t) => t.state === "live").map((t) => t.address);
    } catch {}
    for (const f of folders) {
      if (live.includes(this.building[f])) continue;
      delete this.building[f];
      if (BUILDING(this.busy[f])) {
        delete this.busy[f];
        this.openAfter.delete(f);
        this.errors[f] = "The build ended without a running container.";
      }
    }
  }

  // Spaces asked for with Create open once their machine is ready.
  openReady() {
    for (const f of [...this.openAfter]) {
      const item = this.items.find((i) => i.folder === f);
      if (!item?.added || machines().find((m) => m.name === item.name)?.layout !== "ready") continue;
      this.openAfter.delete(f);
      run("space.new", { machine: item.name, folder: item.c.workdir });
    }
  }

  // Starts the container, adds it if it wasn't, and waits for its core.
  async start(item) {
    const r = await exec(await docker(), ["start", item.c.id]);
    if (r.code !== 0) throw new Error(r.stderr.trim() || "docker start failed");
    if (!item.added) {
      let why = null;
      await this.locked(async () => {
        const owned = { ...(shared(OWNED) ?? {}) };
        for (let i = 0; i < 5; i++) {
          why = await this.add({ ...item.c, name: item.name }, owned);
          if (!why) break;
          await sleep(1);
        }
        if (!why) setShared(OWNED, owned);
      });
      if (why) throw new Error(why);
    }
    for (let i = 0; i < 30; i++) {
      if (machines().find((m) => m.name === item.name)?.layout === "ready") return;
      await sleep(1);
    }
  }

  // open: open a space on it once ready; noCache: --build-no-cache.
  async create(item, { open = true, noCache = false, verb = "Building" } = {}) {
    const v = await exec("devcontainer", ["--version"]).catch(() => null);
    if (v?.code !== 0) throw new Error(CLI_HINT);
    // Create-time flags from herdr-dc's config (dotfiles, the Wayland mount,
    // --skip-post-attach),
    // one per line. A failed build waits for Enter so its output can be read;
    // a good one closes.
    const bin = await this.herdrDc();
    const flags = bin ? await exec(bin, ["dotfiles", "args"]).catch(() => null) : null;
    const extra = flags?.code === 0 ? flags.stdout.split("\n").filter((l) => l !== "") : [];
    if (noCache) extra.push("--build-no-cache");
    const script = 'f=$1 d=$2; shift 2; devcontainer up --docker-path "$d" --workspace-folder "$f" "$@"; s=$?; [ $s -eq 0 ] && exit 0; echo; echo "devcontainer up failed ($s). Press Enter to close."; read _; exit $s';
    const r = await hc(["run", "--bg", "--json", "--no-parent", "--name", `dc-up-${item.name}`.slice(0, 32), "--cwd", item.folder, "--", "sh", "-c", script, "_", item.folder, await docker(), ...extra]);
    let address = null;
    try {
      address = r.code === 0 ? JSON.parse(r.stdout).address : null;
    } catch {}
    if (!address) throw new Error(`Couldn't open a terminal for the build: ${hcError(r)}`);
    this.building[item.folder] = address;
    if (open) this.openAfter.add(item.folder);
    openOverlay({
      kind: "window",
      key: `dc-up-${item.name}`,
      width: 760,
      height: 420,
      layout: {
        type: "column",
        children: [
          { type: "block", size: { px: 30 }, content: { mod: "devcontainers", props: { part: "bar", name: item.name, verb } } },
          { type: "block", content: { terminal: address } },
        ],
      },
    });
  }

  async remove(item) {
    await this.locked(async () => {
      const folders = { ...(shared(FOLDERS) ?? {}) };
      delete folders[item.folder];
      setShared(FOLDERS, folders);
    });
  }

  async deleteContainer(item) {
    const r = await exec(await docker(), ["rm", "-f", item.c.id]);
    if (r.code !== 0) throw new Error(r.stderr.trim() || "docker rm failed");
    await this.locked(async () => {
      const owned = { ...(shared(OWNED) ?? {}) };
      if (owned[item.name]) {
        await hc(["machine", "rm", item.name, "--json"]).catch(() => null);
        delete owned[item.name];
        setShared(OWNED, owned);
      }
    });
  }

  async addFolder() {
    const folder = await pickFolder({ prompt: "Add" });
    if (!folder) return;
    const h = (await health([folder]))[folder];
    if (h !== "ok") {
      notify({ title: "Not a devcontainer folder", body: `${tilde(folder)} has no .devcontainer/devcontainer.json or .devcontainer.json.`, level: "warning", timeout: 5000 });
      return;
    }
    await this.locked(async () => {
      const folders = { ...(shared(FOLDERS) ?? {}) };
      if (folders[folder]) return;
      folders[folder] = { name: this.nameFor(folder, folders, await this.machineList(), shared(OWNED) ?? {}) };
      setShared(FOLDERS, folders);
    });
  }

  act(item, what, cx) {
    const key = item?.folder ?? "+";
    if (this.busy[key]) return;
    this.busy[key] = what;
    delete this.errors[key];
    cx.notify();
    cx.spawn(async (cx) => {
      try {
        if (what === "adding") await this.addFolder();
        else if (what === "stopping") {
          const r = await exec(await docker(), ["stop", item.c.id]);
          if (r.code !== 0) throw new Error(r.stderr.trim() || "docker stop failed");
        } else if (what === "deleting") await this.deleteContainer(item);
        else if (what === "removing") await this.remove(item);
        else if (what === "creating" || what === "rebuilding" || what === "rebuilding-clean") {
          if (what !== "creating") {
            // Reopen only what was open: the old machine's spaces go with it.
            const open = spaces().some((s) => s.machine === item.name);
            await this.deleteContainer(item);
            await this.create(item, { open, noCache: what === "rebuilding-clean", verb: "Rebuilding" });
          } else await this.create(item);
          // Busy until the container runs or the build ends.
          this.expect[key] = { state: "running", until: null };
          this.waitFor(cx);
          return;
        } else {
          if (!item.c.running || !item.added) {
            notify({ title: `Starting ${item.name}…`, level: "info", timeout: 3000 });
            await this.start(item);
          }
          if (what === "opening") {
            const here = spaces().find((s) => s.machine === item.name);
            run(here ? "space.select" : "space.new", here ? { id: here.id } : { machine: item.name, folder: item.c.workdir });
          }
        }
        delete this.busy[key];
      } catch (e) {
        delete this.busy[key];
        this.errors[key] = String(e?.message ?? e);
      }
      if (item && EXPECT[what] && !this.errors[key]) this.expect[key] = { state: EXPECT[what], until: Date.now() + 60000 };
      await this.look();
      cx.notify();
      this.waitFor(cx);
    });
  }

  // A choice from the right-click menu. Called from render, like wake().
  takeRequest(cx) {
    const req = shared(REQUEST);
    if (!req || req.at === this.handled) return;
    this.handled = req.at;
    cx.timer.after(0, (cx) => {
      setShared(REQUEST, null);
      const item = this.items?.find((i) => i.folder === req.folder);
      if (item) this.act(item, req.what, cx);
    });
  }

  // The row's click: open it, or create it.
  primary(item, cx) {
    if (this.busy[item.folder] || item.health !== "ok") return;
    this.act(item, item.state === "uncreated" ? "creating" : "opening", cx);
  }

  menu(item) {
    const { folder, name, state, health } = item;
    openOverlay({ mod: "devcontainers", kind: "popup", width: 200, height: 174, props: { menu: { folder, name, state, health } } });
  }

  // The popup: its choice goes to the panel copy. Delete asks once more, for 4 s.
  menuView(t) {
    const m = props().menu;
    const ok = m.health === "ok";
    const send = (what) => {
      setShared(REQUEST, { folder: m.folder, what, at: Date.now() });
      closeOverlay(props().overlay);
    };
    const confirming = (id) => this.confirm?.id === id && Date.now() - this.confirm.at < 4000;
    // A destructive entry: the first click asks, the second sends.
    const sure = (id, icon, label, what, enabled) => [
      id,
      icon,
      confirming(id) ? `${label}?` : label,
      (cx) => {
        if (confirming(id)) return send(what);
        this.confirm = { id, at: Date.now() };
        cx.timer.after(4000, (cx) => cx.notify());
        cx.notify();
      },
      enabled,
      confirming(id),
    ];
    const entries =
      m.state === "uncreated"
        ? [
            ["create", "plus", "Create", () => send("creating"), ok],
            ["remove", "x", "Remove from list", () => send("removing"), true],
          ]
        : [
            ["open", "terminal", "Open", () => send("opening"), ok],
            m.state === "running" ? ["stop", "square", "Stop", () => send("stopping"), true] : ["start", "plus", "Start", () => send("starting"), ok],
            ["rebuild", "layers", "Rebuild", () => send("rebuilding"), ok],
            ["rebuild-clean", "layers", "Rebuild without cache", () => send("rebuilding-clean"), ok],
            sure("delete", "x", "Delete container", "deleting", true),
          ];
    return v_flex()
      .w_full()
      .p(5)
      .bg(t.popover)
      .text_size(12.8)
      .children(
        entries.map(([id, icon, label, onClick, enabled, danger]) =>
          h_flex()
            .id(`dc-menu-${id}`)
            .h(31)
            .px(8)
            .gap(9)
            .rounded(7)
            .items_center()
            .text_color(danger ? t.ansi1 : t.foreground)
            .when(!enabled, (el) => el.opacity(0.4))
            .when(enabled, (el) => el.cursor_pointer().hover((s) => s.bg(t.hover)).on_click((_e, cx) => onClick(cx)))
            .child(svg(I(icon)).size(14).flex_none().text_color(danger ? t.ansi1 : t.muted))
            .child(div().flex_1().child(label)),
        ),
      );
  }

  // Selecting a space on a stopped container starts it. Called from render,
  // where cx.notify() throws, so act() runs on the next tick. One that still
  // looks running while its machine is unavailable may have been stopped
  // outside Herdr since the last sync: that syncs once, to look again.
  wake(cx) {
    const sp = spaces().find((s) => s.id === focused()?.space);
    const item = sp?.machine && this.items?.find((i) => i.name === sp.machine && i.added);
    if (!item || this.busy[item.folder] || this.waking) return;
    const gone = machines().find((m) => m.name === item.name)?.layout === "unavailable";
    if (!gone && this.rechecked === item.name) this.rechecked = null;
    if (item.state === "running" && gone && this.rechecked !== item.name && !this.syncing) {
      this.rechecked = item.name;
      cx.timer.after(0, (cx) => this.sync(cx));
      return;
    }
    if (item.state !== "stopped") return;
    this.waking = true;
    cx.timer.after(0, (cx) => {
      this.waking = false;
      this.act(item, "starting", cx);
    });
  }

  counts() {
    const n = (s) => (this.items ?? []).filter((i) => i.state === s).length;
    return ["running", "stopped", "uncreated"].map((s) => `${n(s)} ${s}`).join(" · ");
  }

  row(t, item) {
    const busy = this.busy[item.folder];
    // Uncreated: a hollow dot.
    const color = { running: t.ansi2, stopped: t.dim, uncreated: t.chrome }[item.state];
    const warning = { missing: "Folder not found on disk", noconfig: "No devcontainer.json in this folder" }[item.health];
    const error = this.errors[item.folder] ?? item.error ?? item.attachError;
    const sub = item.state === "stopped" && !item.added ? `${tilde(item.folder)} · added when it starts` : tilde(item.folder);
    return v_flex()
      .id(`dc-${item.name}`)
      .px(10)
      .py(8)
      .gap(6)
      .rounded(8)
      .hover((s) => s.bg(t.hover))
      .when(!busy && item.health === "ok", (el) => el.cursor_pointer())
      .on_click((_e, cx) => this.primary(item, cx))
      .on_mouse_down("right", (_e, cx) => {
        cx.stop_propagation();
        this.menu(item);
      })
      .child(
        h_flex()
          .gap(8)
          .items_center()
          .child(
            busy
              ? svg(I("spinner")).size(10).text_color(t.working).spin(900)
              : div().size(8).flex_none().rounded(4).bg(color).when(item.state === "uncreated", (el) => el.border_1().border_color(t.dim)),
          )
          .child(div().flex_1().min_w(0).truncate().text_size(13).text_color(item.health === "missing" ? t.muted : t.foreground).child(item.name))
          .child(div().flex_none().text_size(11).text_color(t.dim).child(busy === "rebuilding-clean" ? "rebuilding" : (busy ?? item.state))),
      )
      .child(div().pl(16).truncate().text_size(11.5).text_color(t.dim).child(sub))
      .when(Boolean(warning), (el) => el.child(div().pl(16).text_size(11.5).text_color(t.blocked).child(`⚠ ${warning}`)))
      .when(Boolean(error), (el) => el.child(div().pl(16).text_size(11.5).line_height("16px").text_color(t.ansi1).child(error)));
  }

  // The build window's title bar.
  bar(t) {
    return h_flex()
      .size_full()
      .px(12)
      .gap(8)
      .items_center()
      .bg(t.chrome)
      .border_b(1)
      .border_color(t.border)
      .text_size(12.5)
      .text_color(t.muted)
      .child(svg(I("layers")).size(14))
      .child(div().flex_1().child(`${props().verb ?? "Building"} ${props().name}: devcontainer up`))
      .child(
        div()
          .id("dc-bar-close")
          .w(22)
          .h(22)
          .rounded(6)
          .flex()
          .items_center()
          .justify_center()
          .cursor_pointer()
          .hover((s) => s.bg(t.hover))
          .on_mouse_down("left", (_e, cx) => cx.stop_propagation())
          .on_click(() => closeOverlay(props().overlay))
          .child(svg(I("x")).size(12)),
      );
  }

  headerButton(t, id, onClick, icon) {
    return div()
      .id(id)
      .size(24)
      .flex()
      .items_center()
      .justify_center()
      .rounded(6)
      .cursor_pointer()
      .hover((s) => s.bg(t.hover))
      .on_click((_e, cx) => onClick(cx))
      .child(icon);
  }

  render(cx) {
    const t = theme();
    if (props()?.part === "bar") return this.bar(t);
    if (props()?.menu) return this.menuView(t);
    this.wake(cx);
    if (props()?.headless) return div();
    this.takeRequest(cx);
    const items = this.items ?? [];
    const when = this.lastSync ? this.lastSync.toTimeString().slice(0, 5) : "";
    return v_flex()
      .id("devcontainers")
      .size_full()
      .bg(t.chrome)
      .text_color(t.foreground)
      .child(
        h_flex()
          .h(48)
          .px(12)
          .gap(6)
          .items_center()
          .border_b(1)
          .border_color(t.border)
          .child(
            v_flex()
              .flex_1()
              .min_w(0)
              .child(div().text_size(13).font_semibold().child("Devcontainers"))
              .child(div().truncate().text_size(11).text_color(t.dim).child(this.items ? `${this.counts()} · ${when}` : "")),
          )
          .child(
            this.headerButton(t, "dc-add", (cx) => this.act(null, "adding", cx), this.busy["+"] ? svg(I("spinner")).size(13).text_color(t.working).spin(900) : svg(I("plus")).size(14).text_color(t.muted)),
          )
          .child(
            this.headerButton(t, "dc-refresh", (cx) => this.sync(cx, true), this.syncing ? svg(I("spinner")).size(13).text_color(t.working).spin(900) : div().text_size(14).text_color(t.muted).child("⟳")),
          ),
      )
      .child(
        v_flex()
          .id("dc-list")
          .flex_1()
          .p(6)
          .gap(2)
          .overflow_y_scrollbar()
          .when(Boolean(this.problem), (el) => el.child(div().p(10).text_size(12).line_height("17px").text_color(t.blocked).child(this.problem)))
          .when(Boolean(this.hint), (el) => el.child(div().p(10).text_size(12).line_height("17px").text_color(t.blocked).child(this.hint)))
          .when(Boolean(this.errors["+"]), (el) => el.child(div().p(10).text_size(12).text_color(t.ansi1).child(this.errors["+"])))
          .when(this.items === null && !this.problem, (el) => el.child(div().p(10).text_size(12).text_color(t.dim).child("Looking for devcontainers…")))
          .when(this.items !== null && !items.length && !this.problem, (el) =>
            el.child(div().p(10).text_size(12).line_height("17px").text_color(t.dim).child("No devcontainer folders yet. Add one with +, or open a space in a folder that has a .devcontainer.")),
          )
          .children(items.map((i) => this.row(t, i))),
      );
  }
}
