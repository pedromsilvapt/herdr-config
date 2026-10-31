// The status bar, plus the selected space's VS Code task buttons: the
// `VsCodeTaskButtons.tasks` list from the project's .vscode/settings.json (the
// Task Buttons extension), each running its task from .vscode/tasks.json. The
// project is the nearest folder up from the space's folder that has a
// .vscode/settings.json, looked up on the space's machine (a devcontainer's
// through its bridge), so tasks run where the space lives. A task runs in a tab
// named after its button. Clicking again selects that tab while the task still
// runs, and runs it again there once it has finished, as VS Code does (its
// isBackground can't tell: most of these tasks set it, builds too). Buttons
// that don't fit go behind "+N" (props.part "more").

import { View, div, svg } from "gpui-kit";
import { h_flex, v_flex } from "gpui-base";
import { spaces, panes, agents, theme, run, window, exec, hc, readTextFile, props, openOverlay, closeOverlay } from "herdr";

// The nearest folder up from $1 with a .vscode/settings.json.
const FIND = 'd=$1; while [ -n "$d" ]; do [ -f "$d/.vscode/settings.json" ] && { echo "$d"; exit 0; }; [ "$d" = / ] && exit 1; d=$(dirname "$d"); done; exit 1';

// JSON with comments and trailing commas, as VS Code writes it.
function jsonc(text) {
  let out = "";
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (c === '"') {
      let j = i + 1;
      while (j < text.length && text[j] !== '"') j += text[j] === "\\" ? 2 : 1;
      out += text.slice(i, j + 1);
      i = j;
    } else if (c === "/" && text[i + 1] === "/") {
      while (i < text.length && text[i] !== "\n") i++;
      out += "\n";
    } else if (c === "/" && text[i + 1] === "*") {
      i = text.indexOf("*/", i + 2);
      if (i < 0) break;
      i++;
    } else out += c;
  }
  return JSON.parse(out.replace(/,(\s*[\]}])/g, "$1"));
}

const quote = (s) => (/^[\w@%+=:,./-]+$/.test(s) ? s : `'${s.replace(/'/g, "'\\''")}'`);
const argText = (a) => (typeof a === "string" ? a : a?.value ?? "");

function substitute(s, root) {
  return String(s)
    .replaceAll("${workspaceFolder}", root)
    .replaceAll("${workspaceRoot}", root)
    .replaceAll("${workspaceFolderBasename}", root.split("/").pop())
    .replace(/\$\{env:(\w+)\}/g, "$$$1");
}

// A button and how to run its task: { label, tooltip, left, argv, cwd, line, reveal, problem }.
// `line` is the same for typing into the tab's shell (sh, bash, zsh or fish) to run it again.
function resolve(btn, tasks, root) {
  const label = btn.label ?? btn.task ?? "?";
  const base = { label, tooltip: btn.tooltip ?? btn.description ?? null, left: btn.alignment === "left" };
  const task = tasks.find((t) => t.label === btn.task);
  if (!task) return { ...base, problem: `No task "${btn.task}" in .vscode/tasks.json` };
  const sub = (s) => substitute(s, root);
  const args = (task.args ?? []).map((a) => sub(argText(a)));
  let argv;
  let cwd = task.options?.cwd ? sub(task.options.cwd) : root;
  if (task.type === "npm") {
    argv = ["sh", "-c", `npm run ${quote(task.script ?? "")}`];
    if (task.path) cwd = `${root}/${task.path}`;
  } else if (task.type === "process") {
    argv = [sub(task.command), ...args];
  } else if (task.command) {
    argv = ["sh", "-c", [sub(argText(task.command)), ...args.map(quote)].join(" ")];
  } else return { ...base, problem: `Task "${btn.task}" has no command` };
  const env = Object.entries(task.options?.env ?? {}).map(([k, v]) => `${k}=${sub(v)}`);
  if (env.length) argv = ["env", ...env, ...argv];
  const line = `cd ${quote(cwd)} && ${argv.map(quote).join(" ")}`;
  return { ...base, argv, cwd, line, reveal: task.presentation?.reveal ?? "always", tooltip: base.tooltip ?? argv.join(" ") };
}

async function load(space, folder) {
  const on = space.machine;
  const r = await exec("sh", ["-c", FIND, "_", folder], { on });
  if (r.code !== 0) return [];
  const root = r.stdout.trim();
  const settings = jsonc((await readTextFile(`${root}/.vscode/settings.json`, { on })) ?? "{}");
  const list = settings["VsCodeTaskButtons.tasks"];
  if (!Array.isArray(list) || !list.length) return [];
  const text = await readTextFile(`${root}/.vscode/tasks.json`, { on }).catch(() => null);
  const tasks = text ? jsonc(text).tasks ?? [] : [];
  return list.filter((b) => b && typeof b === "object").map((b) => resolve(b, tasks, root));
}

const tabOf = (space, b) => space?.tabs.find((tab) => tab.named && tab.name === b.label);
const terminalOf = (tab) => (tab ? panes().find((p) => p.tab === tab.id && p.terminal)?.terminal ?? null : null);

// Whether a task tab's terminal still runs its task. tab.new runs argv in a
// wrapper shell (`sh -c '"$@"; exec "${SHELL:-sh}" -i'`, `fish -ic '...'`) that
// becomes the interactive shell when the task ends; a task run again with
// `hc send` is a child of that shell. So it's finished when the terminal's own
// process is in front without a -c. Null: unknown.
async function running(terminal) {
  const r = await hc(["get", terminal, "--json"]).catch(() => null);
  if (r?.code !== 0) return null;
  try {
    const d = JSON.parse(r.stdout);
    const fg = d.foreground;
    return !fg || fg.pid !== d.pid || (fg.argv ?? []).some((x) => /^-\w*c$/.test(x));
  } catch {
    return null;
  }
}

async function states(space, buttons) {
  const out = {};
  for (const b of buttons) {
    const terminal = terminalOf(tabOf(space, b));
    if (terminal) out[b.label] = await running(terminal);
  }
  return out;
}

// No tab: a new one. Still running: show it. Finished: run it again there.
async function launch(spaceId, b) {
  const space = spaces().find((s) => s.id === spaceId);
  if (!space || !b.argv) return;
  const tab = tabOf(space, b);
  const terminal = terminalOf(tab);
  const busy = terminal ? await running(terminal) : null;
  if (busy === false) {
    await hc(["send", terminal, b.line]);
    if (b.reveal !== "never" && b.reveal !== "silent") run("tab.select", { id: tab.id });
  } else if (tab && busy) run("tab.select", { id: tab.id });
  else run("tab.new", { space: space.id, name: b.label, folder: b.cwd, argv: b.argv });
}

// Where the space is now: its shown tab's focused (else first) pane's folder.
function folderOf(space) {
  const tab = space?.tabs.find((t) => t.active);
  const here = panes().filter((p) => p.tab === tab?.id && p.cwd);
  return (here.find((p) => p.focused) ?? here[0])?.cwd ?? space?.folder ?? "";
}

const keyOf = (space) => {
  const folder = folderOf(space);
  return folder ? `${space.machine ?? ""}|${folder}` : null;
};
const width = (b) => b.label.length * 6.6 + 34;

export default class WorkspaceStatus extends View {
  init(_props, cx) {
    if (props()?.part === "more") {
      this.states = props().states ?? {};
      return;
    }
    this.cache = {}; // "machine|folder" -> buttons
    this.key = null;
    this.size = null;
    this.states = {}; // label -> running (true), finished (false) or unknown (null)
    cx.timer.every(15000, (cx) => this.reload(cx));
    cx.timer.every(3000, (cx) => this.poll(cx));
  }

  // Only while the space has task tabs.
  poll(cx) {
    const space = spaces().find((s) => s.active);
    const buttons = (this.key && this.cache[this.key]) || [];
    if (!buttons.some((b) => tabOf(space, b))) {
      if (Object.keys(this.states).length) {
        this.states = {};
        cx.notify();
      }
      return;
    }
    cx.spawn(async (cx) => {
      const next = await states(space, buttons);
      if (JSON.stringify(next) === JSON.stringify(this.states)) return;
      this.states = next;
      cx.notify();
    });
  }

  click(spaceId, b, cx) {
    cx.spawn(async (cx) => {
      await launch(spaceId, b);
      this.poll(cx);
    });
  }

  reload(cx) {
    const space = spaces().find((s) => s.active);
    const key = keyOf(space);
    this.key = key;
    if (!key) return;
    cx.spawn(async (cx) => {
      try {
        this.cache[key] = await load(space, folderOf(space));
      } catch {
        // Unreachable machine or a broken file: keep what was there.
      }
      cx.notify();
    });
  }

  // Running: a dot. Finished, tab still open: a hollow dot. Otherwise ▶.
  dot(t, space, b, state) {
    if (!tabOf(space, b) || state === undefined) return div().text_size(8).child("▶");
    if (state === false) return div().size(6).rounded(3).border_1().border_color(t.muted);
    return div().size(6).rounded(3).bg(t.working);
  }

  button(t, id, b, space) {
    const ok = Boolean(b.argv);
    return h_flex()
      .id(id)
      .h(20)
      .px(7)
      .gap(5)
      .flex_none()
      .items_center()
      .rounded(5)
      .text_color(ok ? t.muted : t.dim)
      .when(!ok, (el) => el.opacity(0.5))
      .when(ok, (el) => el.cursor_pointer().hover((s) => s.bg(t.hover).text_color(t.foreground)).on_click((_e, cx) => this.click(space.id, b, cx)))
      .tooltip(b.problem ?? b.tooltip ?? b.label)
      .child(this.dot(t, space, b, this.states[b.label]))
      .child(b.label);
  }

  // The +N popup.
  more(t) {
    const { space: spaceId, buttons, overlay } = props();
    const space = spaces().find((s) => s.id === spaceId);
    return v_flex()
      .size_full()
      .p(4)
      .gap(1)
      .text_size(12.5)
      .children(
        buttons.map((b, i) =>
          h_flex()
            .id(`more-${i}`)
            .h(28)
            .px(8)
            .gap(8)
            .items_center()
            .rounded(6)
            .text_color(b.argv ? t.foreground : t.dim)
            .when(Boolean(b.argv), (el) =>
              el
                .cursor_pointer()
                .hover((s) => s.bg(t.hover))
                .on_click((_e, cx) =>
                  cx.spawn(async () => {
                    await launch(spaceId, b);
                    closeOverlay(overlay);
                  }),
                ),
            )
            .tooltip(b.problem ?? b.tooltip ?? b.label)
            .child(h_flex().w(10).justify_center().text_color(t.muted).child(this.dot(t, space, b, this.states[b.label])))
            .child(div().flex_1().truncate().child(b.label)),
        ),
      );
  }

  render(cx) {
    const t = theme();
    if (props()?.part === "more") return this.more(t);
    const space = spaces().find(s => s.active);
    const key = keyOf(space);
    if (key !== this.key) this.reload(cx);
    const list = agents();
    const needs = list.filter(a => a.status === "blocked").length;
    const label = (icon, text) => h_flex().gap(6).items_center().flex_none().child(svg(`mods/workspace-chrome/${icon}.svg`).size(13)).child(text);

    // Left buttons first, then right ones, as many as fit; the rest go behind +N.
    const buttons = key ? this.cache[key] ?? [] : [];
    const ordered = [...buttons.filter((b) => b.left), ...buttons.filter((b) => !b.left)];
    const machine = space?.machine ?? "This computer";
    const agentsText = `${list.length} agents${needs > 0 ? ` · ${needs} needs you` : ""}`;
    const fixed = 28 + 4 * 16 + 2 * 19 + (machine.length + (space?.branch?.length ?? 0) + agentsText.length) * 6.8;
    let room = (this.size?.width ?? 1200) - fixed;
    const shown = [];
    for (let i = 0; i < ordered.length; i++) {
      const rest = i < ordered.length - 1 ? 44 : 0;
      if (width(ordered[i]) + rest > room) break;
      room -= width(ordered[i]) + 2;
      shown.push(ordered[i]);
    }
    const hidden = ordered.slice(shown.length);
    const left = shown.filter((b) => b.left);
    const right = shown.filter((b) => !b.left);
    const btn = (offset) => (b, i) => this.button(t, `task-${offset + i}`, b, space);

    return h_flex().id("root").on_size_change((size, cx) => { this.size = size; cx.notify(); })
      .size_full().px(14).gap(16).items_center().bg(t.chrome).border_t(1).border_color(t.border).text_size(11.8).text_color(t.muted)
      .child(label(space?.machine ? "server" : "monitor", machine))
      .when(Boolean(space?.branch), el => el.child(label("branch", space.branch)))
      .child(h_flex().flex_none().gap(6).items_center().child(div().size(6).rounded(3).bg(t.dim)).child(`${list.length} agents`)
        .when(needs > 0, el => el.child(div().text_color(t.blocked).child(`· ${needs} needs you`))))
      .when(left.length > 0, el => el.child(h_flex().gap(2).items_center().children(left.map(btn(0)))))
      .child(div().flex_1())
      .when(right.length > 0, el => el.child(h_flex().gap(2).items_center().children(right.map(btn(left.length)))))
      .when(hidden.length > 0, el => el.child(
        div().id("task-more").h(20).px(7).flex().flex_none().items_center().rounded(5).cursor_pointer()
          .hover((s) => s.bg(t.hover).text_color(t.foreground))
          .tooltip(hidden.map((b) => b.label).join(", "))
          .on_click(() => openOverlay({ mod: "dev-status", kind: "popup", width: 240, height: Math.min(hidden.length * 29 + 10, 400), props: { part: "more", space: space.id, buttons: hidden, states: this.states } }))
          .child(`+${hidden.length}`)))
      .when(!buttons.length, el => el.child(div().id("palette").text_color(t.dim).on_click(() => run("app.palette")).child(`${window()?.platform === "macos" ? "⌘" : "Ctrl"} Shift P  commands`)));
  }
}
