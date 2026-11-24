import { shared, hc, home } from "herdr";

// The folder a devcontainer machine mounts its host folder at.
async function workdirOf(machine) {
  const r = await hc(["machine", "ls", "--json"]).catch(() => null);
  try {
    const target = JSON.parse(r.stdout).machines.find(m => m.name === machine)?.target ?? "";
    return target.split(" ")[3] ?? null;
  } catch {
    return null;
  }
}

const inside = (path, dir) => path === dir || path.startsWith(dir + "/");

// The space's folder as this computer sees it: a local space's as is, a
// devcontainer's (a machine the devcontainers mod added) mapped from its
// workspace folder back to the host folder it mounts. Null for other machines.
export async function hostFolder(space) {
  const folder = space.folder;
  if (space.machine === null) return folder || home();
  const owned = shared("devcontainers.machines")?.[space.machine];
  if (!owned) return null;
  const workdir = await workdirOf(space.machine);
  if (workdir && folder && inside(folder, workdir)) return owned.folder + folder.slice(workdir.length);
  return owned.folder;
}

// An absolute path in the space's machine as this computer sees it. Null when
// it has no host equivalent (a devcontainer path outside the workspace mount).
export async function toHostPath(space, path) {
  if (space.machine === null) return path;
  const owned = shared("devcontainers.machines")?.[space.machine];
  if (!owned) return null;
  const workdir = await workdirOf(space.machine);
  return workdir && inside(path, workdir) ? owned.folder + path.slice(workdir.length) : null;
}
