# Multi-arch (amd64 for Proxmox, arm64 for the QNAP): the build stage always runs
# natively and cross-compiles, so no emulation is needed.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/readroom ./cmd/readroom \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/readroom /readroom
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV READROOM_DB=/data/readroom.db READROOM_ADDR=:8090
VOLUME /data
EXPOSE 8090
ENTRYPOINT ["/readroom"]
