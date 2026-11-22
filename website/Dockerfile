# Build from the repository root, because the site renders README.md and docs/:
#   docker build -f website/Dockerfile -t petal-website .
FROM node:24-slim AS build
WORKDIR /src/website
COPY website/package.json website/package-lock.json ./
RUN npm ci
COPY README.md /src/README.md
COPY docs /src/docs
COPY website ./
RUN npm run lint && npm run build

# Runs as a non-root user and listens on port 8080.
FROM nginxinc/nginx-unprivileged:stable-alpine
COPY website/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /src/website/dist /usr/share/nginx/html
