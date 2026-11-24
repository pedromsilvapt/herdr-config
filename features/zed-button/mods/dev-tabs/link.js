import { exec, home, notify, shared, spaces } from "herdr";
import { hostFolder, toHostPath } from "./host.js";

// Ctrl-clicked file paths open in the Zed window of the space's project, at
// the clicked line and column. Zed runs on the host, so a devcontainer path is
// mapped back to the host folder it mounts. The window comes from space.folder;
// the terminal's cwd only resolves relative paths.

// Files Zed can't usefully show: these go to the system's opener.
const NOT_FOR_EDITOR = /\.(pdf|zip|tar|gz|tgz|bz2|xz|7z|rar|mp4|mkv|webm|mov|avi|mp3|flac|wav|ogg|docx?|xlsx?|pptx?|odt|ods|odp)$/i;

// "a.rs:3:7", "a.rs(3,7)", "a.rs#L3C7" and a trailing ":" become {path, line, col}.
export function splitPosition(text) {
  const m = text.match(/^(.+?)(?::(\d+)(?::(\d+))?:?|\((\d+)(?:,\s*(\d+))?\):?|#L(\d+)(?:C(\d+))?|:)$/);
  if (!m) return { path: text, line: null, col: null };
  return { path: m[1], line: m[2] ?? m[4] ?? m[6] ?? null, col: m[3] ?? m[5] ?? m[7] ?? null };
}

// A file: URI's path, keeping a #L fragment as text; anything else unchanged.
export function fromUri(text) {
  const m = text.match(/^file:\/\/[^/]*(\/[^#]*)(#.*)?$/i);
  if (!m) return text;
  try {
    return decodeURIComponent(m[1]) + (m[2] ?? "");
  } catch {
    return m[1] + (m[2] ?? "");
  }
}

// Removes "." and ".." from an absolute path.
export function normalize(path) {
  const out = [];
  for (const part of path.split("/")) {
    if (part === "..") out.pop();
    else if (part && part !== ".") out.push(part);
  }
  return "/" + out.join("/");
}

const count = (s, ch) => s.split(ch).length - 1;

// Drops what a sentence puts after a path: "see a.rs:3." or "(in a.rs)".
// Closing brackets and quotes go only when unmatched in the path.
export function trimTrailing(text) {
  for (let t = text; ;) {
    const before = t;
    t = t.replace(/[.,;:!?]+$/, "");
    const last = t.slice(-1);
    const open = { ")": "(", "]": "[", "}": "{" }[last];
    if (open ? count(t, open) < count(t, last) : (last === "'" || last === '"') && count(t, last) % 2 === 1) t = t.slice(0, -1);
    if (t === before) return t;
  }
}

// The readings of a click, likeliest first. The exact text comes first, so a
// file really named "a:3" or "a." wins over "a" at line 3. Then the same with
// trailing punctuation removed. Herdr may already have removed the position
// from link.path, so it is taken from raw then.
export function candidates(link) {
  const raw = fromUri(link.raw);
  const exact = link.path ?? raw;
  const lost = link.path && !link.path.endsWith(raw) ? splitPosition(trimTrailing(raw)) : { line: null, col: null };
  const out = [];
  const add = (c) => {
    if (c.path && !out.some(o => o.path === c.path && o.line === c.line && o.col === c.col)) out.push(c);
  };
  for (const t of [exact, trimTrailing(exact)]) {
    add({ path: t, line: lost.line, col: lost.col });
    add(splitPosition(t));
  }
  return out;
}

async function machineHome(space) {
  if (space.machine === null) return home();
  const r = await exec("sh", ["-c", 'printf %s "$HOME"'], { on: space.machine }).catch(() => null);
  return r?.code === 0 ? r.stdout : null;
}

async function absolute(path, space, cwd) {
  if (path === "~" || path.startsWith("~/")) {
    const h = await machineHome(space);
    return h ? normalize(h + path.slice(1)) : null;
  }
  if (path.startsWith("/")) return normalize(path);
  return cwd ? normalize(`${cwd}/${path}`) : null;
}

async function kindOf(path) {
  const r = await exec("sh", ["-c", 'if [ -d "$1" ]; then echo dir; elif [ -f "$1" ]; then echo file; fi', "_", path]).catch(() => null);
  return r?.stdout.trim() || null;
}

// The first reading that exists on the host. `mapped` says whether any
// reading had a host path at all, so a miss can say why.
async function locate(link, space, cwd) {
  const seen = new Set();
  for (const c of candidates(link)) {
    const abs = await absolute(c.path, space, cwd);
    if (!abs) continue;
    const host = await toHostPath(space, abs);
    if (!host || seen.has(host)) continue;
    seen.add(host);
    const kind = await kindOf(host);
    if (kind) return { found: { ...c, host, kind }, mapped: true };
  }
  return { found: null, mapped: seen.size > 0 };
}

export async function handleLink({ link, source }) {
  if (link.kind !== "file") return "pass";
  const space = spaces().find(s => s.id === source.space);
  if (!space) return "pass";
  const container = space.machine !== null;
  if (container && !shared("devcontainers.machines")?.[space.machine]) return "pass";

  const { found, mapped } = await locate(link, space, source.cwd);
  if (!found) {
    if (!container) return "pass";
    if (mapped) notify({ title: "File not found", body: `${link.raw} isn't there, relative to ${source.cwd ?? "the terminal's folder"}.`, level: "warning", timeout: 5000 });
    else notify({ title: "Not on the host", body: `${link.raw} isn't in this devcontainer's workspace folder.`, level: "warning", timeout: 5000 });
    return "handled";
  }

  if (found.kind === "dir" || NOT_FOR_EDITOR.test(found.host)) {
    if (!container) return "pass";
    // Herdr has no opener for a remote terminal's files, but this one is on the host.
    await exec("xdg-open", [found.host]).catch(() => null);
    return "handled";
  }

  const project = await hostFolder(space);
  if (!project) return "pass";
  const target = found.host + (found.line ? `:${found.line}${found.col ? `:${found.col}` : ""}` : "");
  const r = await exec("herdr-zed-open", [project, target]).catch(e => ({ code: 1, stderr: String(e?.message ?? e) }));
  if (r.code !== 0) notify({ title: "Couldn't open Zed", body: (r.stderr || r.stdout || "").trim() || `zed ${target} failed`, level: "error", timeout: 5000 });
  return "handled";
}
