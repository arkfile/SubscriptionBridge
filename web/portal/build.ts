// Compiles portal.ts into the Go-embedded Drop-in init script.
const outdir = `${import.meta.dir}/../../internal/httpapi/static`;

const result = await Bun.build({
  entrypoints: [`${import.meta.dir}/portal.ts`],
  outdir,
  target: "browser",
  format: "iife",
  minify: false,
  banner: "/* generated from web/portal/portal.ts; do not edit */",
});

if (!result.success) {
  for (const log of result.logs) {
    console.error(log);
  }
  process.exit(1);
}
