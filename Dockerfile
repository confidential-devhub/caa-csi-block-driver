FROM alpine:3.19
RUN apk add --no-cache e2fsprogs e2fsprogs-extra blkid
COPY bin/caa-csi-block-driver /caa-csi-block-driver
ENTRYPOINT ["/caa-csi-block-driver"]
