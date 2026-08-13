# ── Stage 1: Build Go headless binaries ────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS go-build
ARG TARGETOS TARGETARCH
ENV CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH
WORKDIR /src
COPY relay /src/relay
COPY headless /src/headless
RUN go -C /src/headless/vk       build -trimpath -ldflags="-s -w" -o /out/headless-vk-creator . \
 && go -C /src/headless/telemost build -trimpath -ldflags="-s -w" -o /out/headless-telemost-creator . \
 && go -C /src/headless/wbstream build -trimpath -ldflags="-s -w" -o /out/headless-wbstream-creator . \
 && go -C /src/headless/dion     build -trimpath -ldflags="-s -w" -o /out/headless-dion-creator .

# ── Stage 2: Build TypeScript ──────────────────────────────────
FROM node:22-slim AS ts-build
WORKDIR /app
COPY creator-web/package.json creator-web/package-lock.json* ./
RUN npm ci --quiet
COPY creator-web/tsconfig.json ./
COPY creator-web/src ./src
RUN npm run build

# ── Stage 3: Runtime ───────────────────────────────────────────
FROM node:22-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tini \
 && rm -rf /var/lib/apt/lists/*

# Headless binaries
COPY --from=go-build /out/ /opt/wlb/bin/

# Node app
WORKDIR /app
COPY creator-web/package.json creator-web/package-lock.json* ./
RUN npm ci --quiet --omit=dev
COPY --from=ts-build /app/dist ./dist
COPY creator-web/public ./public

RUN mkdir -p /data /data/cookies /data/sessions \
 && chown -R node:node /data

USER node
ENV DATA_DIR=/data
ENV BINS_DIR=/opt/wlb/bin
ENV WEB_PORT=3000
VOLUME ["/data"]
EXPOSE ${WEB_PORT}

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["node", "dist/server.js"]
