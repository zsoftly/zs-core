# Builds the zs-core service image. Used by deploy/compose.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
# go build fetches only the modules the service needs, not the test-only ones.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/zs-core ./cmd/zs-core

FROM alpine:3.22
RUN addgroup -S -g 10001 zs && adduser -S -u 10001 -G zs -H -h /app zs
WORKDIR /app
COPY --from=build /out/zs-core /usr/local/bin/zs-core
COPY components.yaml /app/components.yaml
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/zs-core"]
CMD ["serve"]
