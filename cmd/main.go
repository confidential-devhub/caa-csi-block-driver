// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"log"
	"os"

	"github.com/confidential-containers/cloud-api-adaptor/src/caa-csi-block-driver/pkg/driver"

	// Register providers — each init() calls provider.RegisterProvider()
	_ "github.com/confidential-containers/cloud-api-adaptor/src/caa-csi-block-driver/pkg/provider/aws"
	_ "github.com/confidential-containers/cloud-api-adaptor/src/caa-csi-block-driver/pkg/provider/azure"
	_ "github.com/confidential-containers/cloud-api-adaptor/src/caa-csi-block-driver/pkg/provider/libvirt"
)

var version = "0.1.0"

func main() {
	var cfg driver.Config

	flag.StringVar(&cfg.Endpoint, "endpoint", "unix:///var/run/csi.sock", "CSI endpoint")
	flag.StringVar(&cfg.DriverName, "drivername", "caa-csi-block.csi.confidentialcontainers.io", "CSI driver name")
	flag.StringVar(&cfg.NodeID, "nodeid", "", "Node ID")
	showVersion := flag.Bool("version", false, "Show version")

	flag.Parse()

	if *showVersion {
		log.Printf("cloud-csi-adaptor %s", version)
		os.Exit(0)
	}

	cfg.VendorVersion = version

	if cfg.NodeID == "" {
		hostname, _ := os.Hostname()
		cfg.NodeID = hostname
	}

	drv, err := driver.NewDriver(cfg)
	if err != nil {
		log.Fatalf("Failed to create driver: %v", err)
	}

	log.Printf("Starting cloud-csi-adaptor %s", version)
	if err := drv.Run(); err != nil {
		log.Fatalf("Driver failed: %v", err)
	}
}
