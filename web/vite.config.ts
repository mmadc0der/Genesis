import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";
import { resolve } from "node:path";

export default defineConfig({
  plugins: [react()],
  build: {
    rollupOptions: {
      input: {
        main: resolve(__dirname, "index.html"),
        settings: resolve(__dirname, "settings.html"),
      },
    },
  },
  server: {
    host: "127.0.0.1",
    port: 4179,
    strictPort: true,
    proxy: {
      "/api": {
        target: "http://127.0.0.1:8790",
        ws: true,
      },
    },
  },
  test: {
    environment: "node",
  },
});
