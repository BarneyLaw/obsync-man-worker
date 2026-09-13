import { defineConfig } from "vitest/config";
import { fileURLToPath } from "node:url";

export default defineConfig({
  resolve: {
    alias: {
      // sync.ts needs Notice/normalizePath/Platform at runtime. The real
      // package is a type-only stub outside the app, so point it at ours.
      obsidian: fileURLToPath(new URL("./test/obsidian.ts", import.meta.url)),
    },
  },
  test: {
    include: ["src/**/*.test.ts"],
  },
});
