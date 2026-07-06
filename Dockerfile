# ── Frontend source ──────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM alpine:3.22.0 AS frontend-source

ARG FRONTEND_REPOSITORY=https://github.com/kittors/codeProxy.git
ARG FRONTEND_REF=main
ARG FRONTEND_COMMIT=

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# Local `docker compose up -d` from the CliRelay repo should always build the
# current management panel instead of depending on a separately checked out
# `frontend/` directory or an outdated published image. FRONTEND_COMMIT is part
# of this layer on purpose: a moving branch name alone is invisible to Docker's
# cache, so the exact frontend SHA must bust the clone layer.
RUN git clone --depth=1 --branch "${FRONTEND_REF}" "${FRONTEND_REPOSITORY}" frontend \
  && if [ -n "${FRONTEND_COMMIT}" ]; then \
    cd frontend \
    && git fetch --depth=1 origin "${FRONTEND_COMMIT}" \
    && git checkout --detach "${FRONTEND_COMMIT}"; \
  fi

# ── Frontend build ───────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM oven/bun:1 AS frontend-builder

WORKDIR /frontend
COPY --from=frontend-source /src/frontend/ .
ARG UI_VERSION=dev
ARG FRONTEND_REPOSITORY=https://github.com/kittors/codeProxy.git
ARG FRONTEND_REF=main
ARG FRONTEND_COMMIT=none
ARG BUILD_DATE=unknown
ENV VITE_APP_VERSION=${UI_VERSION}
ENV VITE_PANEL_REPOSITORY=${FRONTEND_REPOSITORY}
ENV VITE_PANEL_REF=${FRONTEND_REF}
ENV VITE_PANEL_COMMIT=${FRONTEND_COMMIT}
ENV VITE_PANEL_BUILD_DATE=${BUILD_DATE}
RUN bun install --frozen-lockfile
RUN bun run build

# ── Backend build ────────────────────────────────────────────────────────────
FROM --platform=$BUILDPLATFORM golang:1.26.1-alpine AS backend-builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
ARG UI_VERSION=dev
ARG FRONTEND_REF=main
ARG FRONTEND_COMMIT=none

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
  -ldflags="-s -w \
    -X 'github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo.Version=${VERSION}' \
    -X 'github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo.Commit=${COMMIT}' \
    -X 'github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo.BuildDate=${BUILD_DATE}' \
    -X 'github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo.FrontendVersion=${UI_VERSION}' \
    -X 'github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo.FrontendCommit=${FRONTEND_COMMIT}' \
    -X 'github.com/router-for-me/CLIProxyAPI/v6/internal/buildinfo.FrontendRef=${FRONTEND_REF}'" \
  -o ./CLIProxyAPI ./cmd/server/

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
  -ldflags="-s -w" \
  -o ./clirelay-updater ./cmd/updater/

# ── Runtime ──────────────────────────────────────────────────────────────────
FROM alpine:3.22.0

RUN apk add --no-cache tzdata ca-certificates docker-cli docker-cli-compose su-exec

RUN addgroup -S -g 10001 clirelay \
  && adduser -S -D -H -u 10001 -h /CLIProxyAPI -s /sbin/nologin -G clirelay clirelay \
  && mkdir -p /CLIProxyAPI/panel /CLIProxyAPI/auths /CLIProxyAPI/logs /CLIProxyAPI/data \
  && chown -R clirelay:clirelay /CLIProxyAPI

COPY --from=backend-builder --chown=clirelay:clirelay /app/CLIProxyAPI /CLIProxyAPI/CLIProxyAPI
COPY --from=backend-builder --chown=clirelay:clirelay /app/clirelay-updater /CLIProxyAPI/clirelay-updater
COPY --from=frontend-builder --chown=clirelay:clirelay /frontend/dist/ /CLIProxyAPI/panel/

COPY --chown=clirelay:clirelay config.example.yaml /CLIProxyAPI/config.example.yaml
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
COPY scripts/migrate-sqlite-to-postgres.sh /usr/local/bin/migrate-sqlite-to-postgres.sh
COPY scripts/init-compose-env.sh /usr/local/bin/clirelay-init-env

WORKDIR /CLIProxyAPI

EXPOSE 8317

ENV TZ=Asia/Shanghai \
    MANAGEMENT_PANEL_DIR=/CLIProxyAPI/panel \
    AUTH_PATH=/CLIProxyAPI/auths \
    CLIRELAY_LOCALE=zh

RUN chmod +x /usr/local/bin/docker-entrypoint.sh /usr/local/bin/migrate-sqlite-to-postgres.sh /usr/local/bin/clirelay-init-env \
  && cp /usr/share/zoneinfo/${TZ} /etc/localtime \
  && echo "${TZ}" > /etc/timezone

USER root

ENTRYPOINT ["docker-entrypoint.sh"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q -T 2 -O /dev/null http://127.0.0.1:8317/healthz

CMD ["./CLIProxyAPI"]
