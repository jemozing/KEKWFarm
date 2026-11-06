# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS source
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/

FROM source AS runner-build
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags="-s -w" -o /out/kekwfarm-runner ./cmd/duckrunnerweb
# Preserve the sticky control directory when Docker initializes its volume.
RUN mkdir -p /out/runner-root/control \
    && chmod 1777 /out/runner-root/control \
    && touch /out/runner-root/control/.keep

FROM scratch AS runner
COPY --from=runner-build /out/runner-root/ /
COPY --from=runner-build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=runner-build /out/kekwfarm-runner /usr/local/bin/kekwfarm-runner
USER 65532:65532
WORKDIR /data
EXPOSE 5138
ENTRYPOINT ["/usr/local/bin/kekwfarm-runner"]
CMD ["-listen", "0.0.0.0:5138", "-data", "/data", "-config", "/config/duckfarm.json", "-broker-socket", "/control/engine.sock"]

FROM source AS farm-build
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -trimpath -ldflags="-s -w" -o /out/kekwfarm ./cmd/duckfarm

FROM scratch AS farm
COPY --from=farm-build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=farm-build /out/kekwfarm /usr/local/bin/kekwfarm
USER 65532:65532
EXPOSE 5137
ENTRYPOINT ["/usr/local/bin/kekwfarm"]
CMD ["serve", "-config", "/config/duckfarm.json"]
