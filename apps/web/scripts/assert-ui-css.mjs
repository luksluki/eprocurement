// Anti-silent gate for the Tailwind v4 @source registration (issue #51).
//
// Tailwind v4 scans the importing app's source tree (cwd of the build); the
// design system lives in packages/ui OUTSIDE that tree, so apps/web/src/app/
// globals.css registers @source "../../../../packages/ui/src". If that
// registration breaks, utilities used only by packages/ui components
// silently vanish from the built CSS — pages render unstyled and every
// jsdom/lint gate still passes. This script fails the `test` gate instead.
// It runs after `pnpm build` (turbo: test dependsOn build).
//
// Markers, each validated at runtime so the gate cannot rot silently:
//   - "focus-ring": the @utility from tokens.css — proves the token layer is
//     imported and compiles. (Weak on its own: its name appears inside the
//     imported tokens.css, so it can compile even without @source.)
//   - "bg-success-bg": a Badge class that exists ONLY in packages/ui React
//     sources — proves the @source scan actually reaches the components.
//     Verified empirically: without @source this utility is absent from the
//     built CSS while focus-ring still compiles (dev note 051).
// For each marker: (1) used by packages/ui sources, (2) NOT used by apps/web
// sources (uniqueness is what makes CSS presence prove the registration),
// (3) present in .next/static/css.

import { existsSync, readdirSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const webRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const repoRoot = resolve(webRoot, "..", "..");
const MARKERS = ["focus-ring", "bg-success-bg"];

function collectFiles(dir, exts) {
  const out = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === "node_modules" || entry.name.startsWith(".")) {
      continue;
    }
    const full = join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...collectFiles(full, exts));
    } else if (exts.some((ext) => entry.name.endsWith(ext))) {
      out.push(full);
    }
  }
  return out;
}

function filesUsing(files, marker) {
  return files.filter((file) => readFileSync(file, "utf8").includes(marker));
}

// 0. The built CSS must exist.
const cssDir = join(webRoot, ".next", "static", "css");
if (!existsSync(cssDir)) {
  console.error("assert-ui-css: no .next/static/css — run `pnpm build` before this gate.");
  process.exit(1);
}
const cssFiles = readdirSync(cssDir).filter((name) => name.endsWith(".css"));
const css = cssFiles.map((name) => readFileSync(join(cssDir, name), "utf8")).join("\n");

const uiSources = collectFiles(join(repoRoot, "packages", "ui", "src"), [".ts", ".tsx", ".css"]);
const webSources = collectFiles(join(webRoot, "src"), [".ts", ".tsx", ".css"]);
let failed = false;

for (const marker of MARKERS) {
  // 1. packages/ui must use the marker.
  if (filesUsing(uiSources, marker).length === 0) {
    console.error(`assert-ui-css: marker "${marker}" not found in packages/ui/src — pick a new marker.`);
    failed = true;
    continue;
  }
  // 2. apps/web must not (uniqueness = the proof).
  if (filesUsing(webSources, marker).length > 0) {
    console.error(
      `assert-ui-css: marker "${marker}" is now used by apps/web sources; ` +
        "its presence in the CSS would no longer prove the registration. Choose a packages/ui-only marker.",
    );
    failed = true;
    continue;
  }
  // 3. The compiled utility must exist.
  if (!css.includes(`.${marker}`)) {
    console.error(
      `assert-ui-css: utility ".${marker}" missing from .next/static/css — ` +
        'the @source "../../../../packages/ui/src" registration in src/app/globals.css is broken ' +
        "(packages/ui component styles compile silently empty).",
    );
    failed = true;
    continue;
  }
  console.log(`assert-ui-css: ok — ".${marker}" compiled; packages/ui styles are live.`);
}

process.exit(failed ? 1 : 0);
