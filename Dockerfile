# Multi-arch (amd64 for Proxmox, arm64 for the QNAP): the build stage always runs
# natively and cross-compiles, so no emulation is needed.
#
# CI compiles the binaries on the runner instead, where the Go build cache
# survives between runs, and hands them over as a build context named "build"
# (see .github/workflows/image.yml). A named context replaces the stage of the
# same name, so this stage is then skipped and the same files are copied from
# that directory: out/<arch>/readroom and an empty out/data.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/$TARGETARCH/readroom ./cmd/readroom \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
ARG TARGETARCH
COPY --from=build /out/${TARGETARCH}/readroom /readroom
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV READROOM_DB=/data/readroom.db READROOM_ADDR=:8090
VOLUME /data
EXPOSE 8090
ENTRYPOINT ["/readroom"]
