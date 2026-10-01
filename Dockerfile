# Hammurapi core image: one static binary with modes api | worker | runner | cleaner | migrate.
# The instance image adds an ACP agent on top of it (see Dockerfile.instance in
# the hammurapi repository).

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/hammurapi ./cmd/hammurapi \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/hammurapi-fakeagent ./cmd/fakeagent \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/fakegitlab ./cmd/fakegitlab  && mkdir -p /out/runs

# Release image (docker build --target release): Hammurapi + the ACP agent in one
# image — the instance image used by api, worker, runner, cleaner and migrate
# (PLT.INFRA-0002 R4). The agent version comes from deploy/versions.env
# (AGENT_VERSION) and changes only by PR. npm, corepack and yarn are removed after
# the install: nothing uses them at runtime, and their bundled dependencies
# (tar, undici, brace-expansion, ip-address…) are what the image scan flags.
FROM node:24-trixie-slim AS release
ARG VERSION=dev
ARG AGENT_VERSION
ARG AGENT_PACKAGE=@agentclientprotocol/claude-agent-acp
RUN test -n "$AGENT_VERSION" \
 && apt-get update && apt-get -y upgrade && apt-get install -y --no-install-recommends ca-certificates \
 && npm install -g --no-audit --no-fund "${AGENT_PACKAGE}@${AGENT_VERSION}" \
 && npm cache clean --force && rm -rf /var/lib/apt/lists/* /root/.npm \
 && rm -rf /usr/local/lib/node_modules/npm /usr/local/lib/node_modules/corepack /opt/yarn-* \
           /usr/local/bin/npm /usr/local/bin/npx /usr/local/bin/corepack /usr/local/bin/yarn /usr/local/bin/yarnpkg \
 && test -x /usr/local/bin/claude-agent-acp && test ! -e /usr/local/bin/npm \
 && mkdir -p /var/lib/hammurapi/runs && chown 1000:1000 /var/lib/hammurapi/runs
COPY --from=build /out/hammurapi /usr/local/bin/hammurapi
LABEL org.opencontainers.image.title="hammurapi-core" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/GreenOnGrey/hammurapi-core" \
      io.hammurapi.agent.package="${AGENT_PACKAGE}" \
      io.hammurapi.agent.version="${AGENT_VERSION}"
ENV ACP_AGENT_COMMAND=claude-agent-acp HOME=/home/node
# Numeric user (node): Kubernetes checks runAsNonRoot only for numeric users.
USER 1000:1000
WORKDIR /home/node
EXPOSE 8080 8081 9100
ENTRYPOINT ["/usr/local/bin/hammurapi"]
CMD ["api"]

# Development/test only: in-memory imitation of the GitLab API (docker build --target fakegitlab).
FROM gcr.io/distroless/static-debian12:nonroot AS fakegitlab
COPY --from=build /out/fakegitlab /usr/local/bin/fakegitlab
EXPOSE 8929
ENTRYPOINT ["/usr/local/bin/fakegitlab"]

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hammurapi /usr/local/bin/hammurapi
# Test-only scripted ACP agent, handy for smoke runs without an LLM agent.
COPY --from=build /out/hammurapi-fakeagent /usr/local/bin/hammurapi-fakeagent
# Working directories of runner tasks with RUNNER_EXECUTOR=local (a volume in docker compose).
COPY --from=build --chown=65532:65532 /out/runs /var/lib/hammurapi/runs
# Numeric user (distroless nonroot): Kubernetes checks runAsNonRoot only for numeric users.
USER 65532:65532
EXPOSE 8080 9100
ENTRYPOINT ["/usr/local/bin/hammurapi"]
CMD ["api"]
