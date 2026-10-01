FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

ARG TARGETARCH
ARG VERSION=dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY pkg/ pkg/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -ldflags "-X main.version=$VERSION" -o /caa-csi-block-driver ./cmd/

FROM alpine:3.19
RUN apk add --no-cache e2fsprogs e2fsprogs-extra blkid
COPY --from=builder /caa-csi-block-driver /caa-csi-block-driver
ENTRYPOINT ["/caa-csi-block-driver"]
