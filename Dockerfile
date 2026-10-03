# Hammurapi core image: one static binary with modes
# api | worker | agent | runner | cleaner | migrate.

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/hammurapi ./cmd/hammurapi \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/fakellm ./cmd/fakellm \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/fakegitlab ./cmd/fakegitlab && mkdir -p /out/runs /out/work

# Release image (docker build --target release): Hammurapi and the Pi agent in
# one image (PLT.INFRA-0002 R4, PLT.HMR-0004 R2) — api, worker, the agent
# operator (mode agent), runner, cleaner and migrate. The Pi version comes from
# deploy/versions.env (PI_VERSION) and changes only by PR. The
# hammurapi-workspace extension routes Pi's file and shell tools to runner
# tasks. npm, corepack and yarn are removed after the install: nothing uses
# them at runtime, and their bundled dependencies are what the image scan flags.
FROM node:24-trixie-slim AS release
ARG VERSION=dev
ARG PI_VERSION
ARG PI_PACKAGE=@earendil-works/pi-coding-agent
RUN test -n "$PI_VERSION" \
 && apt-get update && apt-get -y upgrade && apt-get install -y --no-install-recommends ca-certificates git \
 && npm install -g --no-audit --no-fund "${PI_PACKAGE}@${PI_VERSION}" \
 && npm cache clean --force && rm -rf /var/lib/apt/lists/* /root/.npm \
 && rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack /opt/yarn-* \
           /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack /usr/local/bin/yarn /usr/local/bin/yarnpkg \
 && test -x /usr/local/bin/pi && test ! -e /usr/local/bin/npm \
 && mkdir -p /var/lib/hammurapi/runs /work && chown 1000:1000 /var/lib/hammurapi/runs /work
COPY pi-extensions/ /opt/hammurapi/pi-extensions/
COPY --from=build /out/hammurapi /usr/local/bin/hammurapi
LABEL org.opencontainers.image.title="hammurapi-core" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/GreenOnGrey/hammurapi-core" \
      io.hammurapi.pi.package="${PI_PACKAGE}" \
      io.hammurapi.pi.version="${PI_VERSION}"
ENV HOME=/home/node PI_BINARY=/usr/local/bin/pi PI_EXTENSION_DIR=/opt/hammurapi/pi-extensions/hammurapi-workspace \
    AGENT_WORKDIR=/work PI_VERSION=${PI_VERSION}
# Numeric user (node): Kubernetes checks runAsNonRoot only for numeric users.
USER 1000:1000
WORKDIR /home/node
EXPOSE 8080 8081 8083 8090 8095 9100
ENTRYPOINT ["/usr/local/bin/hammurapi"]
CMD ["api"]

# Development/test only: in-memory imitation of the GitLab API (docker build --target fakegitlab).
FROM gcr.io/distroless/static-debian12:nonroot AS fakegitlab
COPY --from=build /out/fakegitlab /usr/local/bin/fakegitlab
EXPOSE 8929
ENTRYPOINT ["/usr/local/bin/fakegitlab"]

# Development/test only: a scripted OpenAI-compatible LLM for the demo stack
# (docker build --target fakellm); the real agent (Pi) talks to it.
FROM gcr.io/distroless/static-debian12:nonroot AS fakellm
COPY --from=build /out/fakellm /usr/local/bin/fakellm
EXPOSE 8099
ENTRYPOINT ["/usr/local/bin/fakellm"]

# Hammurapi without the agent (api, worker, runner, cleaner, migrate): the
# agent operator needs the release image.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hammurapi /usr/local/bin/hammurapi
# Working directories of runner tasks with RUNNER_EXECUTOR=local (a volume in docker compose).
COPY --from=build --chown=65532:65532 /out/runs /var/lib/hammurapi/runs
# Numeric user (distroless nonroot): Kubernetes checks runAsNonRoot only for numeric users.
USER 65532:65532
EXPOSE 8080 9100
ENTRYPOINT ["/usr/local/bin/hammurapi"]
CMD ["api"]
