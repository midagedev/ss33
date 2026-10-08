FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ss33 ./cmd/ss33

# alpine (not distroless): compose healthchecks call `curl`, and bootstrap jobs run `/bin/sh -c "mc ..."`.
FROM alpine:3.22
RUN apk add --no-cache curl ca-certificates
COPY --from=build /out/ss33 /usr/local/bin/ss33
RUN ln -s /usr/local/bin/ss33 /usr/local/bin/mc
EXPOSE 9000
VOLUME /data
ENTRYPOINT ["ss33"]
CMD ["server", "/data"]
