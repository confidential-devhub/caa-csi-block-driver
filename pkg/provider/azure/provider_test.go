// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

import (
	"testing"
)

func TestIsValidAzureResourceID(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{
			name:  "valid disk encryption set ID",
			input: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/my-rg/providers/Microsoft.Compute/diskEncryptionSets/my-des",
			want:  true,
		},
		{
			name:  "valid with different provider",
			input: "/subscriptions/abc123/resourceGroups/rg1/providers/Microsoft.Network/virtualNetworks/vnet1",
			want:  true,
		},
		{
			name:  "empty string",
			input: "",
			want:  false,
		},
		{
			name:  "missing leading slash",
			input: "subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Compute/diskEncryptionSets/des1",
			want:  false,
		},
		{
			name:  "missing subscriptions segment",
			input: "/resourceGroups/rg1/providers/Microsoft.Compute/diskEncryptionSets/des1",
			want:  false,
		},
		{
			name:  "random string",
			input: "not-a-resource-id",
			want:  false,
		},
		{
			name:  "partial resource ID",
			input: "/subscriptions/sub1/resourceGroups/rg1",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidAzureResourceID(tt.input)
			if got != tt.want {
				t.Errorf("isValidAzureResourceID(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNewAzureProvider_Validation(t *testing.T) {
	baseParams := map[string]string{
		"azureSubscriptionId": "sub-123",
		"azureResourceGroup":  "rg-test",
		"azureLocation":       "eastus",
	}

	copyWith := func(extra map[string]string) map[string]string {
		m := make(map[string]string, len(baseParams)+len(extra))
		for k, v := range baseParams {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	tests := []struct {
		name      string
		params    map[string]string
		wantErr   bool
		errSubstr string
	}{
		{
			name:      "missing subscription ID",
			params:    map[string]string{"azureResourceGroup": "rg", "azureLocation": "eastus"},
			wantErr:   true,
			errSubstr: "azureSubscriptionId is required",
		},
		{
			name:      "missing resource group",
			params:    map[string]string{"azureSubscriptionId": "sub", "azureLocation": "eastus"},
			wantErr:   true,
			errSubstr: "azureResourceGroup is required",
		},
		{
			name: "missing location allowed for recovery/list/delete",
			params: map[string]string{
				"azureSubscriptionId": "sub",
				"azureResourceGroup":  "rg",
			},
			wantErr: false,
		},
		{
			name:      "invalid IOPS - not a number",
			params:    copyWith(map[string]string{"azureDiskIops": "abc"}),
			wantErr:   true,
			errSubstr: "invalid azureDiskIops",
		},
		{
			name:      "invalid IOPS - zero",
			params:    copyWith(map[string]string{"azureDiskIops": "0"}),
			wantErr:   true,
			errSubstr: "invalid azureDiskIops",
		},
		{
			name:      "invalid IOPS - negative",
			params:    copyWith(map[string]string{"azureDiskIops": "-100"}),
			wantErr:   true,
			errSubstr: "invalid azureDiskIops",
		},
		{
			name:      "invalid MBps - not a number",
			params:    copyWith(map[string]string{"azureDiskMbps": "fast"}),
			wantErr:   true,
			errSubstr: "invalid azureDiskMbps",
		},
		{
			name:      "invalid MBps - zero",
			params:    copyWith(map[string]string{"azureDiskMbps": "0"}),
			wantErr:   true,
			errSubstr: "invalid azureDiskMbps",
		},
		{
			name:      "IOPS rejected for StandardSSD_LRS",
			params:    copyWith(map[string]string{"azureDiskIops": "5000", "azureDiskSKU": "StandardSSD_LRS"}),
			wantErr:   true,
			errSubstr: "only supported for UltraSSD_LRS and PremiumV2_LRS",
		},
		{
			name:      "MBps rejected for Premium_LRS",
			params:    copyWith(map[string]string{"azureDiskMbps": "200", "azureDiskSKU": "Premium_LRS"}),
			wantErr:   true,
			errSubstr: "only supported for UltraSSD_LRS and PremiumV2_LRS",
		},
		{
			name:      "IOPS rejected for default SKU",
			params:    copyWith(map[string]string{"azureDiskIops": "5000"}),
			wantErr:   true,
			errSubstr: "only supported for UltraSSD_LRS and PremiumV2_LRS",
		},
		{
			name:      "invalid encryption set ID",
			params:    copyWith(map[string]string{"azureDiskEncryptionSetId": "not-a-resource-id"}),
			wantErr:   true,
			errSubstr: "invalid azureDiskEncryptionSetId",
		},
		{
			name: "valid encryption set ID",
			params: copyWith(map[string]string{
				"azureDiskEncryptionSetId": "/subscriptions/sub-123/resourceGroups/rg-test/providers/Microsoft.Compute/diskEncryptionSets/my-des",
			}),
			wantErr: false,
		},
		{
			name:    "SKU fallback from azureDiskSKU",
			params:  copyWith(map[string]string{"azureDiskSKU": "UltraSSD_LRS", "azureDiskIops": "5000"}),
			wantErr: false,
		},
		{
			name:    "SKU fallback from azureDiskSku (lowercase)",
			params:  copyWith(map[string]string{"azureDiskSku": "PremiumV2_LRS", "azureDiskMbps": "200"}),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewAzureProvider(tt.params)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error but got nil")
				}
				if tt.errSubstr != "" && !contains(err.Error(), tt.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.errSubstr)
				}
			} else {
				// NewAzureProvider may fail on credential creation in test environments;
				// that's expected and not a validation error.
				if err != nil && contains(err.Error(), "invalid azure") {
					t.Errorf("unexpected validation error: %v", err)
				}
			}
		})
	}
}

func TestDiskName(t *testing.T) {
	p := &AzureProvider{}

	tests := []struct {
		name     string
		volumeID string
		wantPfx  string
		maxLen   int
	}{
		{
			name:     "simple volume ID",
			volumeID: "pvc-abc123",
			wantPfx:  "csi-vol-pvc-abc123",
			maxLen:   80,
		},
		{
			name:     "special characters replaced",
			volumeID: "pvc/test:vol@1",
			wantPfx:  "csi-vol-pvc-test-vol-1",
			maxLen:   80,
		},
		{
			name:     "very long volume ID is truncated",
			volumeID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			maxLen:   80,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.diskName(tt.volumeID)
			if len(got) > tt.maxLen {
				t.Errorf("diskName(%q) length %d exceeds max %d", tt.volumeID, len(got), tt.maxLen)
			}
			if tt.wantPfx != "" && got != tt.wantPfx {
				t.Errorf("diskName(%q) = %q, want %q", tt.volumeID, got, tt.wantPfx)
			}
		})
	}
}

func TestSnapName(t *testing.T) {
	p := &AzureProvider{}

	tests := []struct {
		name       string
		snapshotID string
		want       string
		maxLen     int
	}{
		{
			name:       "simple snapshot ID",
			snapshotID: "snap-abc123",
			want:       "csi-snap-snap-abc123",
			maxLen:     80,
		},
		{
			name:       "special characters replaced",
			snapshotID: "snap/test:id@1",
			want:       "csi-snap-snap-test-id-1",
			maxLen:     80,
		},
		{
			name:       "very long snapshot ID is truncated",
			snapshotID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			maxLen:     80,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.snapName(tt.snapshotID)
			if len(got) > tt.maxLen {
				t.Errorf("snapName(%q) length %d exceeds max %d", tt.snapshotID, len(got), tt.maxLen)
			}
			if tt.want != "" && got != tt.want {
				t.Errorf("snapName(%q) = %q, want %q", tt.snapshotID, got, tt.want)
			}
		})
	}
}

func TestBytesToGiB(t *testing.T) {
	tests := []struct {
		name      string
		sizeBytes int64
		want      int32
	}{
		{"zero bytes yields 1 GiB minimum", 0, 1},
		{"one byte rounds up to 1 GiB", 1, 1},
		{"exactly 1 GiB", 1024 * 1024 * 1024, 1},
		{"1 GiB + 1 byte rounds up to 2", 1024*1024*1024 + 1, 2},
		{"exactly 2 GiB", 2 * 1024 * 1024 * 1024, 2},
		{"half GiB rounds up to 1", 512 * 1024 * 1024, 1},
		{"10 GiB", 10 * 1024 * 1024 * 1024, 10},
		{"10 GiB minus 1 byte rounds up to 10", 10*1024*1024*1024 - 1, 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bytesToGiB(tt.sizeBytes)
			if got != tt.want {
				t.Errorf("bytesToGiB(%d) = %d, want %d", tt.sizeBytes, got, tt.want)
			}
		})
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
