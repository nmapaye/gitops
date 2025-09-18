# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.21 AS builder
WORKDIR /src
COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o /out/manager ./

FROM gcr.io/distroless/static:nonroot
USER 65532:65532
COPY --from=builder /out/manager /manager
ENTRYPOINT ["/manager"]
