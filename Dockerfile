# Tests and builds one plugin, with the framework it uses, in a pinned Go
# toolchain. Build deployments with the same image. The cache mounts keep Go's
# module and build caches in the Docker daemon between builds, so a warm
# daemon only recompiles what changed.
FROM golang:1.26.10-bookworm@sha256:e0c50aecba3a2edfab18924d493cd9210b73c4746fc5fd5b3bb46e0ec7f0465a
ARG PLUGIN
WORKDIR /src
ENV GOMAXPROCS=2
COPY go.mod go.sum ./
RUN --mount=type=cache,id=cliproxyapi-plugins-gomod,target=/go/pkg/mod \
    go mod download
COPY . ./
RUN --mount=type=cache,id=cliproxyapi-plugins-gomod,target=/go/pkg/mod \
    --mount=type=cache,id=cliproxyapi-plugins-gobuild,target=/root/.cache/go-build \
    make -C "${PLUGIN}" test build
