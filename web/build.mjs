import { build, context } from "esbuild";
import { mkdir, copyFile } from "node:fs/promises";

await mkdir(new URL("./dist/", import.meta.url), { recursive: true });
await copyFile(new URL("./index.html", import.meta.url), new URL("./dist/index.html", import.meta.url));
const options = {
  absWorkingDir: new URL(".", import.meta.url).pathname,
  entryPoints: ["src/main.tsx"],
  outdir: "dist",
  entryNames: "app",
  bundle: true,
  minify: true,
  target: "es2022",
  define: { "process.env.NODE_ENV": '"production"' },
  legalComments: "external",
};
if (process.argv.includes("--watch")) {
  const watcher = await context(options);
  await watcher.watch();
  console.log("Watching Iota web assets; restart Go only when its code changes.");
} else {
  await build(options);
}
