FROM alpine:3.19
RUN apk add --no-cache e2fsprogs e2fsprogs-extra blkid
COPY bin/cloud-csi-adaptor /cloud-csi-adaptor
ENTRYPOINT ["/cloud-csi-adaptor"]
