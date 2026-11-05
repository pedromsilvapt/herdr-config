// Ports of the running devcontainers, forwarded to this computer on request.
// Scans only when the panel opens and on Refresh; the panel forwards nothing
// until clicked (forwardPorts are forwarded by herdr-dc attach). A forward takes the same port here when it's free, a random one
// otherwise.
//
// The work is done by `herdr-dc` (a static binary from herdr-dc/ in this
// feature): `ports scan <id>` copies it into the container and lists listening
// ports; `ports forward <id> <port>` serves localhost and passes connections on,
// through a relay inside the container when the server listens on loopback
// only. `herdr-dc attach` forwards devcontainer.json's forwardPorts the same
// way (labelled dc.label from portsAttributes), so they show here too. Each forward runs in its own hc terminal, named pf-<id>-<port>, with
// the port it got as the label dc.host_port, so forwards survive the app
// restarting and the panel rebuilds its list from `hc ls`.

import { View, div, svg } from "gpui-kit";
import { v_flex, h_flex } from "gpui-base";
import { exec, hc, home, theme, notify, shared, spaces, focused, layout } from "herdr";

const I = (name) => `mods/workspace-chrome/${name}.svg`;
const OWNED = "devcontainers.machines";
const HINT = "herdr-dc isn't installed. Run ./install.sh devcontainers from the herdr customizations repo.";

const basename = (p) => p.replace(/\/+$/, "").split("/").pop() ?? p;
const termName = (id, port) => `pf-${id}-${port}`;

function find(node, id) {
  if (node?.id === id) return node;
  for (const child of node?.children ?? []) {
    const found = find(child, id);
    if (found) return found;
  }
  return null;
}

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

function lastLine(s) {
  return (s ?? "").split("\n").map((l) => l.trim()).filter(Boolean).pop() ?? "";
}

export default class Ports extends View {
  init(_props, cx) {
    this.containers = null; // [{ id, name, folder, ports: [{ port, addr, loopback, process }] | null, error }]
    this.forwards = {}; // "<id>-<port>" -> { address, hostPort, label }
    this.busy = {}; // "<id>-<port>" -> "forwarding" | "stopping"
    this.errors = {}; // "<id>-<port>" -> what the last action said
    this.problem = null;
    this.scanning = false;
    this.lastScan = null;
    this.shown = false;
    // A hidden block isn't rendered, so render can't see the panel close and
    // reopen; this only reads the layout (no docker) and scans on reopening.
    this.watchShown(cx);
    cx.timer.every(500, (cx) => this.watchShown(cx));
  }

  // Shown: the dev layout's panel is open on this mod. Outside that stack
  // (another layout), showing at all counts.
  isShown() {
    const root = layout();
    const body = find(root, "panel-body");
    if (!body?.children?.some((c) => c.id === "dc-ports")) return true;
    return !find(root, "panel")?.hidden && body.active === "dc-ports";
  }

  watchShown(cx) {
    const now = this.isShown();
    if (now && !this.shown) this.scan(cx);
    this.shown = now;
  }

  async bin() {
    if (this.binPath) return this.binPath;
    const r = await exec("sh", ["-c", "command -v herdr-dc"]).catch(() => null);
    if (r?.code !== 0) throw new Error(HINT);
    this.binPath = r.stdout.trim();
    return this.binPath;
  }

  scan(cx) {
    if (this.scanning) return;
    this.scanning = true;
    cx.notify();
    cx.spawn(async (cx) => {
      try {
        await this.load();
        this.problem = null;
      } catch (e) {
        this.problem = String(e?.message ?? e);
      }
      this.scanning = false;
      this.lastScan = new Date();
      cx.notify();
    });
  }

  async load() {
    const bin = await this.bin();
    const ps = await exec(await docker(), ["ps", "--filter", "label=devcontainer.local_folder", "--format", '{{.ID}}\t{{.Label "devcontainer.local_folder"}}']);
    if (ps.code !== 0) throw new Error(ps.stderr.trim() || "docker ps failed");
    const owned = Object.entries(shared(OWNED) ?? {});
    const containers = ps.stdout
      .split("\n")
      .filter(Boolean)
      .map((line) => {
        const [id, folder] = line.split("\t");
        const name = owned.find(([, o]) => o.id === id || o.folder === folder)?.[0] ?? basename(folder);
        return { id, name, folder, ports: null, error: null };
      });
    await Promise.all(
      containers.map(async (c) => {
        const r = await exec(bin, ["ports", "scan", c.id]).catch((e) => ({ code: 1, stderr: String(e?.message ?? e) }));
        if (r.code !== 0) c.error = lastLine(r.stderr) || "scan failed";
        else c.ports = JSON.parse(r.stdout || "[]");
      }),
    );
    containers.sort((a, b) => a.name.localeCompare(b.name));
    this.containers = containers;
    await this.loadForwards();
  }

  // Forwards from hc's terminals. Finished ones are cleared; ones whose
  // container no longer runs are stopped.
  async loadForwards() {
    const r = await hc(["ls", "--json"]);
    if (r.code !== 0) throw new Error(lastLine(r.stderr) || "hc ls failed");
    const running = new Set((this.containers ?? []).map((c) => c.id));
    const forwards = {};
    for (const t of JSON.parse(r.stdout).terminals ?? []) {
      const m = /^pf-([0-9a-f]{12})-(\d+)$/.exec(t.name ?? "");
      if (!m) continue;
      if (t.state !== "live" || !running.has(m[1])) {
        await this.stopTerminal(t.address);
        continue;
      }
      const g = await hc(["get", t.address, "--json"]).catch(() => null);
      let hostPort = null;
      let label = null;
      try {
        const labels = JSON.parse(g.stdout).labels ?? {};
        hostPort = Number(labels["dc.host_port"]) || null;
        label = labels["dc.label"] || null;
      } catch {}
      forwards[`${m[1]}-${m[2]}`] = { address: t.address, hostPort, label };
    }
    this.forwards = forwards;
  }

  async stopTerminal(address) {
    await hc(["kill", address, "--json"]).catch(() => null);
    await hc(["rm", address, "--json"]).catch(() => null);
  }

  act(c, port, what, cx) {
    const key = `${c.id}-${port}`;
    if (this.busy[key]) return;
    this.busy[key] = what;
    delete this.errors[key];
    cx.notify();
    cx.spawn(async (cx) => {
      try {
        if (what === "forwarding") await this.forward(c, port);
        else await this.stopTerminal(this.forwards[key].address);
      } catch (e) {
        this.errors[key] = String(e?.message ?? e);
      }
      delete this.busy[key];
      await this.loadForwards().catch(() => null);
      cx.notify();
    });
  }

  async forward(c, port) {
    const bin = await this.bin();
    const name = termName(c.id, port);
    const r = await hc(["run", "--bg", "--json", "--no-parent", "--name", name, "--label", `dc.container=${c.id}`, "--label", `dc.port=${port}`, "--", bin, "ports", "forward", c.id, String(port)]);
    if (r.code !== 0) throw new Error(lastLine(r.stderr || r.stdout) || "hc run failed");
    // Ready once it labels the port it got; up to ~15 s (a first copy into the container).
    let t = null;
    let hostPort = null;
    for (let i = 0; i < 50; i++) {
      const g = await hc(["get", name, "--json"]);
      t = JSON.parse(g.stdout);
      hostPort = Number(t.labels?.["dc.host_port"]) || null;
      if (hostPort || t.state !== "live") break;
      await exec("sleep", ["0.3"]).catch(() => null);
    }
    if (t.state !== "live" || !hostPort) {
      const out = await hc(["read", name]).catch(() => null);
      await this.stopTerminal(name);
      throw new Error(lastLine(out?.stdout).replace(/^herdr-dc(-ports)?: /, "") || "The forward didn't start.");
    }
    notify({
      title: `Forwarded ${c.name}:${port}`,
      body: hostPort === port ? `localhost:${port}` : `localhost:${hostPort} (port ${port} is taken here)`,
      level: "success",
      timeout: 4000,
    });
  }

  open(hostPort) {
    exec("xdg-open", [`http://localhost:${hostPort}`]).catch(() => null);
  }

  // The devcontainer of the focused space, if it's one.
  current() {
    return spaces().find((s) => s.id === focused()?.space)?.machine ?? null;
  }

  // The focused space's devcontainer first.
  ordered() {
    const current = this.current();
    const list = [...(this.containers ?? [])];
    const i = list.findIndex((c) => c.name === current);
    if (i > 0) list.unshift(...list.splice(i, 1));
    return list;
  }

  button(t, id, label, onClick) {
    return div()
      .id(id)
      .h(22)
      .px(8)
      .flex()
      .flex_none()
      .items_center()
      .rounded(6)
      .border_1()
      .border_color(t.border)
      .text_size(11.5)
      .text_color(t.muted)
      .cursor_pointer()
      .hover((s) => s.bg(t.hover).text_color(t.foreground))
      .on_click((_e, cx) => onClick(cx))
      .child(label);
  }

  portRow(t, c, p) {
    const key = `${c.id}-${p.port}`;
    const f = this.forwards[key];
    const busy = this.busy[key];
    const error = this.errors[key];
    const gone = !p.listening;
    const right = busy
      ? svg(I("spinner")).size(12).text_color(t.working).spin(900)
      : f
        ? h_flex()
            .gap(4)
            .items_center()
            .child(
              div()
                .id(`pf-open-${key}`)
                .text_size(12)
                .text_color(gone ? t.dim : t.accent)
                .cursor_pointer()
                .hover((s) => s.underline())
                .on_click(() => f.hostPort && this.open(f.hostPort))
                .child(`localhost:${f.hostPort ?? "?"}`),
            )
            .child(
              div()
                .id(`pf-stop-${key}`)
                .size(20)
                .flex()
                .items_center()
                .justify_center()
                .rounded(5)
                .cursor_pointer()
                .hover((s) => s.bg(t.hover))
                .on_click((_e, cx) => this.act(c, p.port, "stopping", cx))
                .child(svg(I("x")).size(11).text_color(t.muted)),
            )
        : this.button(t, `pf-fwd-${key}`, "Forward", (cx) => this.act(c, p.port, "forwarding", cx));
    const sub = [f?.label, gone ? "not listening" : p.process, gone || !p.loopback ? null : p.addr].filter(Boolean).join(" · ");
    return v_flex()
      .id(`pf-row-${key}`)
      .px(10)
      .py(5)
      .gap(3)
      .rounded(7)
      .hover((s) => s.bg(t.hover))
      .child(
        h_flex()
          .gap(8)
          .items_center()
          .child(div().w(46).flex_none().text_size(13).text_color(gone ? t.dim : t.foreground).child(String(p.port)))
          .child(div().flex_1().min_w(0).truncate().text_size(11.5).text_color(t.dim).child(sub))
          .child(right),
      )
      .when(Boolean(error), (el) => el.child(div().text_size(11.5).line_height("16px").text_color(t.ansi1).child(error)));
  }

  section(t, c) {
    // Forwards whose port stopped listening stay, so they can be stopped.
    const ports = (c.ports ?? []).map((p) => ({ ...p, listening: true }));
    for (const key of Object.keys(this.forwards)) {
      const [id, port] = key.split("-");
      if (id === c.id && !ports.some((p) => p.port === Number(port))) ports.push({ port: Number(port), listening: false });
    }
    ports.sort((a, b) => a.port - b.port);
    return v_flex()
      .gap(1)
      .pb(6)
      .child(
        h_flex()
          .px(10)
          .pt(8)
          .pb(4)
          .gap(8)
          .items_center()
          .child(div().size(8).flex_none().rounded(4).bg(t.ansi2))
          .child(div().flex_1().min_w(0).truncate().text_size(13).font_semibold().child(c.name))
          .when(c.name === this.current(), (el) =>
            el.child(div().flex_none().px(6).py(1).rounded(5).bg(t.selected).text_size(10.5).text_color(t.muted).child("current")),
          ),
      )
      .when(Boolean(c.error), (el) => el.child(div().px(10).text_size(11.5).line_height("16px").text_color(t.ansi1).child(c.error)))
      .when(!c.error && !ports.length, (el) => el.child(div().px(10).text_size(11.5).text_color(t.dim).child("Nothing listening")))
      .children(ports.map((p) => this.portRow(t, c, p)));
  }

  render() {
    const t = theme();
    const list = this.ordered();
    const n = Object.keys(this.forwards).length;
    const when = this.lastScan ? this.lastScan.toTimeString().slice(0, 5) : "";
    return v_flex()
      .id("dc-ports")
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
              .child(div().text_size(13).font_semibold().child("Ports"))
              .child(div().truncate().text_size(11).text_color(t.dim).child(this.containers ? `${n} forwarded · scanned ${when}` : "")),
          )
          .child(
            div()
              .id("pf-refresh")
              .size(24)
              .flex()
              .items_center()
              .justify_center()
              .rounded(6)
              .cursor_pointer()
              .hover((s) => s.bg(t.hover))
              .on_click((_e, cx) => this.scan(cx))
              .child(this.scanning ? svg(I("spinner")).size(13).text_color(t.working).spin(900) : div().text_size(14).text_color(t.muted).child("⟳")),
          ),
      )
      .child(
        v_flex()
          .id("pf-list")
          .flex_1()
          .p(6)
          .overflow_y_scrollbar()
          .when(Boolean(this.problem), (el) => el.child(div().p(10).text_size(12).line_height("17px").text_color(t.blocked).child(this.problem)))
          .when(this.containers === null && !this.problem, (el) => el.child(div().p(10).text_size(12).text_color(t.dim).child("Scanning…")))
          .when(this.containers !== null && !list.length && !this.problem, (el) =>
            el.child(div().p(10).text_size(12).line_height("17px").text_color(t.dim).child("No running devcontainers.")),
          )
          .children(list.map((c) => this.section(t, c))),
      );
  }
}
