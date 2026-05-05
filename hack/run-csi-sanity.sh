#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BINARY="$ROOT_DIR/bin/caa-csi-block-driver"
TEST_DIR=$(mktemp -d /tmp/csi-sanity-XXXXXX)

cleanup() {
    if [ -n "${DRIVER_PID:-}" ] && kill -0 "$DRIVER_PID" 2>/dev/null; then
        kill "$DRIVER_PID" 2>/dev/null || true
        wait "$DRIVER_PID" 2>/dev/null || true
    fi
    rm -rf "$TEST_DIR" /tmp/csi-test-mnt-* /tmp/csi-sanity-mnt-*
}
trap cleanup EXIT

if [ ! -x "$BINARY" ]; then
    echo "Error: driver binary not found at $BINARY"
    echo "Run 'make build' first."
    exit 1
fi

if ! command -v csi-sanity &>/dev/null; then
    echo "Installing csi-sanity..."
    go install github.com/kubernetes-csi/csi-test/v5/cmd/csi-sanity@latest
fi
CSI_SANITY=$(command -v csi-sanity || echo "$(go env GOPATH)/bin/csi-sanity")

mkdir -p "$TEST_DIR"/{pool,volstore,kata-vols}

cat > "$TEST_DIR/params.yaml" <<EOF
cloudProvider: libvirt
cloudProviderVolumePath: $TEST_DIR/pool
EOF

cat > "$TEST_DIR/create-path.sh" <<'SCRIPT'
#!/bin/bash
dir=$(mktemp -d /tmp/csi-sanity-mnt-XXXXXX)
echo "$dir"
SCRIPT

cat > "$TEST_DIR/remove-path.sh" <<'SCRIPT'
#!/bin/bash
rm -rf "$1"
SCRIPT

cat > "$TEST_DIR/check-path.sh" <<'SCRIPT'
#!/bin/bash
if [ ! -e "$1" ]; then echo "not_found"
elif [ -f "$1" ]; then echo "file"
elif [ -d "$1" ]; then echo "directory"
else echo "other"; fi
SCRIPT

chmod +x "$TEST_DIR"/*.sh

KATA_DIRECT_VOLUME_ROOT_PATH="$TEST_DIR/kata-vols" \
CSI_VOLUME_STORE_DIR="$TEST_DIR/volstore" \
"$BINARY" \
    --endpoint="unix://$TEST_DIR/csi.sock" \
    --drivername=caa-csi-block.csi.confidentialcontainers.io \
    --nodeid=test-node &
DRIVER_PID=$!
sleep 2

if ! kill -0 "$DRIVER_PID" 2>/dev/null; then
    echo "Error: driver failed to start"
    exit 1
fi

echo "Driver started (PID $DRIVER_PID), running csi-sanity..."
echo ""

SKIP="Snapshot|ListVolumes|GetCapacity|ControllerPublishVolume|ControllerUnpublishVolume"
SKIP+="|ExpandVolume|NodeExpandVolume|NodeGetVolumeStats|ModifyVolume"
SKIP+="|GroupController|volume from an existing source"

"$CSI_SANITY" \
    --csi.endpoint="unix://$TEST_DIR/csi.sock" \
    --csi.testvolumeparameters="$TEST_DIR/params.yaml" \
    --csi.createstagingpathcmd="$TEST_DIR/create-path.sh" \
    --csi.removestagingpathcmd="$TEST_DIR/remove-path.sh" \
    --csi.createmountpathcmd="$TEST_DIR/create-path.sh" \
    --csi.removemountpathcmd="$TEST_DIR/remove-path.sh" \
    --csi.checkpathcmd="$TEST_DIR/check-path.sh" \
    --ginkgo.skip="$SKIP" \
    "$@"
