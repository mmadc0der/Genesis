import { defineConfig } from "vite";
import preact from "@preact/preset-vite";

export default defineConfig({
  plugins: [preact()],
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
});
