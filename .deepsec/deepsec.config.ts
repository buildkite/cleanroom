import { existsSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { defineConfig } from "deepsec/config";

const configDir = dirname(fileURLToPath(import.meta.url));
const legacyV010Root = "../../../.codex/worktrees/deepsec-v0.10.0-20260531/cleanroom";

const projects = [{ id: "cleanroom", root: ".." }];

if (existsSync(resolve(configDir, legacyV010Root))) {
  projects.push({ id: "cleanroom-v0.10.0", root: legacyV010Root });
}

export default defineConfig({ projects });
