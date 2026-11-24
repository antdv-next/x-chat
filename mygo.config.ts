import { defineConfig } from "mygo-cli";
import pkg from "./package.json" with { type: "json" };

export default defineConfig({
  name: "x-chat",
  slug: "x-chat",
  identifier: "com.antdvnext.xchat",
  version: pkg.version,
  copyright: "© 2026 Antdv Next Team",
  license: "MIT",
  icon: "resources/icon.png",

  devUrl: "http://127.0.0.1:5173",
  devCommand: "pnpm dev:web",
  buildCommand: "pnpm build:web",
  // The frontend is embedded by internal/webdist (go:embed) so that the
  // `serve` mode can reach it too; mygo.SetFrontend hands it to the window.
  bindings: "web/src/gen/mygo.ts",
  out: "build",
  linux: { maintainer: "Antdv Next Team <aibayanyu@gmail.com>" },
});
