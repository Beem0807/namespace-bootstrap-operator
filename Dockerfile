# Build the manager binary
# Override BASE_IMAGE to build from another registry, e.g. docker.io/library/golang:1.26
ARG BASE_IMAGE=golang:1.26

# --platform=$BUILDPLATFORM pins the builder to the machine doing the building,
# NOT to the image being produced. Without it, buildx runs this whole stage under
# QEMU emulation for every non-native target - so a linux/arm64 build compiles Go
# under emulation and takes tens of minutes instead of about a minute.
#
# The binary is still built for the target: CGO_ENABLED=0 makes it pure static Go,
# so GOOS/GOARCH below cross-compile it with no sysroot and no cross-toolchain.
# That is what TARGETOS/TARGETARCH are for, and they only mean anything while the
# builder itself runs natively.
FROM --platform=$BUILDPLATFORM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH

# Stamped into the binary at link time. Defaulted, so a plain `docker build`
# still works and the result honestly identifies itself as a dev build.
ARG VERSION=dev
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# -trimpath strips local filesystem paths, so the binary is reproducible and does
# not leak the build directory. -s -w drop the symbol and DWARF tables.
#
# Deliberately no -a: it forces a rebuild of the entire standard library on every
# build, which buys nothing for a CGO_ENABLED=0 static binary and costs minutes.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w \
        -X github.com/Beem0807/namespace-bootstrap-operator/internal/version.Version=${VERSION} \
        -X github.com/Beem0807/namespace-bootstrap-operator/internal/version.Commit=${VCS_REF} \
        -X github.com/Beem0807/namespace-bootstrap-operator/internal/version.BuildDate=${BUILD_DATE}" \
      -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
#
# No --platform here on purpose: this stage IS the shipped image, so it must be
# the target platform. It only copies an already-cross-compiled binary.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
