// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/confidential-devhub/caa-csi-block-driver/pkg/driver"

	_ "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider/aws"
	_ "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider/azure"
	_ "github.com/confidential-devhub/caa-csi-block-driver/pkg/provider/libvirt"
)

var (
	driverName = flag.String("drivername", "caa-csi-block.csi.confidentialcontainers.io", "CSI driver name")
	endpoint   = flag.String("endpoint", "unix:///csi/csi.sock", "CSI endpoint")
	nodeID     = flag.String("nodeid", "", "Node ID")
	version    = "0.2.0"
)

func main() {
	flag.Parse()

	if *nodeID == "" {
		*nodeID = os.Getenv("KUBE_NODE_NAME")
	}
	if *nodeID == "" {
		log.Fatal("Node ID is required (--nodeid or KUBE_NODE_NAME env)")
	}

	d, err := driver.NewDriver(driver.Config{
		Endpoint:      *endpoint,
		DriverName:    *driverName,
		VendorVersion: version,
		NodeID:        *nodeID,
	})
	if err != nil {
		log.Fatalf("Failed to create driver: %v", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("Shutting down CSI driver...")
		d.Stop()
	}()

	if err := d.Run(); err != nil {
		log.Fatalf("Driver failed: %v", err)
	}
}
