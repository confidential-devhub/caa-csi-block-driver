// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package driver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

const (
	saTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	saCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// lookupNodeLabels returns region/zone from the Kubernetes node object.
// Overridden in tests. Returns empty strings when not running in-cluster.
var lookupNodeLabels = lookupNodeLabelsFromAPI

func resolveNodeTopology(ctx context.Context, nodeID string) *csi.Topology {
	region := os.Getenv("CSI_TOPOLOGY_REGION")
	zone := os.Getenv("CSI_TOPOLOGY_ZONE")
	if region == "" && zone == "" {
		r, z, err := lookupNodeLabels(ctx, nodeID)
		if err != nil {
			nsLogger.Printf("NodeGetInfo: failed to read node topology labels: %v", err)
			return nil
		}
		region, zone = r, z
	}
	if zone == "" {
		return nil
	}
	segments := map[string]string{topologyZoneKey: zone}
	if region != "" {
		segments[topologyRegionKey] = region
	}
	return &csi.Topology{Segments: segments}
}

func lookupNodeLabelsFromAPI(ctx context.Context, nodeID string) (region, zone string, err error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" || nodeID == "" {
		return "", "", nil
	}

	token, err := os.ReadFile(saTokenPath)
	if err != nil {
		return "", "", fmt.Errorf("reading serviceaccount token: %w", err)
	}
	caPEM, err := os.ReadFile(saCAPath)
	if err != nil {
		return "", "", fmt.Errorf("reading serviceaccount CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return "", "", fmt.Errorf("parsing serviceaccount CA")
	}

	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
	}

	u := &url.URL{
		Scheme: "https",
		Host:   net.JoinHostPort(host, port),
		Path:   "/api/v1/nodes/" + url.PathEscape(nodeID),
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+string(token))

	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("getting node %s: %w", nodeID, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", fmt.Errorf("reading node %s response: %w", nodeID, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("getting node %s: HTTP %d", nodeID, resp.StatusCode)
	}

	var node struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &node); err != nil {
		return "", "", fmt.Errorf("decoding node %s: %w", nodeID, err)
	}
	labels := node.Metadata.Labels
	zone = firstNonEmptyLabel(labels, k8sZoneLabel, k8sZoneLabelBeta)
	region = firstNonEmptyLabel(labels, k8sRegionLabel, k8sRegionLabelBeta)
	return region, zone, nil
}

func firstNonEmptyLabel(labels map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := labels[k]; v != "" {
			return v
		}
	}
	return ""
}
