# Petal website

Run `npm ci`, then `npm run dev` for a local server or `npm run build` to write the site to `dist/`.

Run `npm run format` to format the code with Prettier. Run `npm run lint` to check it with oxlint and Prettier.

The homepage is `src/pages/index.astro`. The guides under `/docs/` render the Markdown files in the repository's `docs/` directory, and the start page renders the setup section of the top-level README. To add a guide, add its file to the list in `src/pages/docs/[slug].astro`.

## Container image

The `Website` workflow builds an image that serves the site with nginx on port 8080. Pushes to `main` publish it as `ghcr.io/alyraffauf/petal-website`. To build and run it locally, start from the repository root, because the site renders `README.md` and `docs/`.

```sh
docker build -f website/Dockerfile -t petal-website .
docker run --rm -p 8080:8080 petal-website
```
