import { defineConfig, passthroughImageService } from "astro/config";
import sitemap from "@astrojs/sitemap";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
    site: "https://petal.aly.town",
    integrations: [sitemap()],
    // Screenshots are GIFs and PNGs that ship as they are.
    image: { service: passthroughImageService() },
    markdown: { syntaxHighlight: false },
    vite: {
        plugins: [tailwindcss()],
        // The guides and screenshots live in the repository's docs directory.
        server: { fs: { allow: [".."] } },
    },
});
