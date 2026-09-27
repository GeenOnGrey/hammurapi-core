# Hammurapi core image: one static binary with modes api | worker | cleaner | migrate.
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
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/fakegitlab ./cmd/fakegitlab

# Development/test only: in-memory imitation of the GitLab API (docker build --target fakegitlab).
FROM gcr.io/distroless/static-debian12:nonroot AS fakegitlab
COPY --from=build /out/fakegitlab /usr/local/bin/fakegitlab
EXPOSE 8929
ENTRYPOINT ["/usr/local/bin/fakegitlab"]

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/hammurapi /usr/local/bin/hammurapi
# Test-only scripted ACP agent, handy for smoke runs without an LLM agent.
COPY --from=build /out/hammurapi-fakeagent /usr/local/bin/hammurapi-fakeagent
USER nonroot:nonroot
EXPOSE 8080 9100
ENTRYPOINT ["/usr/local/bin/hammurapi"]
CMD ["api"]
