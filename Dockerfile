# ge-agent: static Go binary + DIRECTIVE.md on distroless. Runnable standalone
# (one research cycle per invocation) and a binary-carrier for the
# ge-orchestrator image, which COPY --from's /ge-agent and /etc/ge-agent/.

# ---- build ----
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w" -o /out/ge-agent .

# ---- runtime ----
# distroless/static ships ca-certificates — needed for the MiniMax HTTPS calls.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ge-agent /ge-agent
COPY DIRECTIVE.md /etc/ge-agent/DIRECTIVE.md
USER nonroot:nonroot
ENV GE_AGENT_DIRECTIVE=/etc/ge-agent/DIRECTIVE.md
ENTRYPOINT ["/ge-agent"]
