# Cross-compile on the build host; only the small runtime stage runs under emulation.
# BuildKit fills these in; the legacy builder needs --build-arg BUILDPLATFORM=linux/<arch>.
ARG BUILDPLATFORM
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/ss33 ./cmd/ss33

# alpine (not distroless): compose healthchecks call `curl`, and bootstrap jobs run `/bin/sh -c "mc ..."`.
FROM alpine:3.22
LABEL org.opencontainers.image.source="https://github.com/midagedev/ss33" \
      org.opencontainers.image.description="Small S3-compatible server and mc-compatible CLI for local development and CI" \
      org.opencontainers.image.licenses="Apache-2.0"
RUN apk add --no-cache curl ca-certificates
COPY --from=build /out/ss33 /usr/local/bin/ss33
# minio/mc scripts call both `mc` and `/usr/bin/mc`.
RUN ln -s /usr/local/bin/ss33 /usr/local/bin/mc && ln -s /usr/local/bin/ss33 /usr/bin/mc
EXPOSE 9000
VOLUME /data
ENTRYPOINT ["ss33"]
CMD ["server", "/data"]
