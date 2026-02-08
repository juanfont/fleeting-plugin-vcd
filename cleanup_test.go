package vcd

import (
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"
	"github.com/vmware/go-vcloud-director/v3/govcd"
	"github.com/vmware/go-vcloud-director/v3/types/v56"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

// TestCleanupOrphanedVMs finds and deletes all VMs with the test prefix.
// Run manually: go test -v -run TestCleanupOrphanedVMs -timeout 30m
func TestCleanupOrphanedVMs(t *testing.T) {
	if os.Getenv("VCD_URL") == "" {
		t.Skip("VCD_URL not set, skipping")
	}

	ig := &InstanceGroup{
		Name:              "cleanup",
		StrURL:            os.Getenv("VCD_URL"),
		Org:               os.Getenv("VCD_ORG"),
		Token:             os.Getenv("VCD_TOKEN"),
		VirtualDatacenter: os.Getenv("VCD_VDC"),
		Network:           os.Getenv("VCD_NETWORK"),
		IPAllocationMode:  os.Getenv("VCD_NETWORK_ALLOCATION_MODE"),
		InstanceGroupName: "cleanup",
		VAppNamePrefix:    os.Getenv("VCD_VAPP_NAME_PREFIX"),
		Catalog:           os.Getenv("VCD_CATALOG"),
		Template:          os.Getenv("VCD_TEMPLATE"),
		StorageProfile:    os.Getenv("VCD_STORAGE_PROFILE"),
		CPUCount:          1,
		CoresPerSocket:    1,
		MemoryMB:          512,
		log: hclog.New(&hclog.LoggerOptions{
			Name:   "cleanup",
			Level:  hclog.Debug,
			Output: os.Stdout,
		}),
		settings: provider.Settings{
			ConnectorConfig: provider.ConnectorConfig{
				UseStaticCredentials: true,
				Password:             "unused",
			},
		},
	}

	err := ig.populate()
	require.NoError(t, err)

	client, err := ig.getVCDClient()
	require.NoError(t, err)

	org, err := client.GetOrgByName(ig.Org)
	require.NoError(t, err)

	vdc, err := org.GetVDCByName(ig.VirtualDatacenter, true)
	require.NoError(t, err)

	prefix := os.Getenv("VCD_VAPP_NAME_PREFIX")
	criteria := &govcd.FilterDef{
		Filters: map[string]string{
			"name_regex": "^" + prefix,
		},
	}

	results, _, err := client.Client.SearchByFilter(types.QtVapp, criteria)
	require.NoError(t, err)

	t.Logf("found %d vApps with prefix %q", len(results), prefix)

	deleted := 0
	for _, r := range results {
		name := r.GetName()
		href := r.GetHref()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		vapp, err := vdc.GetVAppByHref(href)
		if err != nil {
			t.Logf("  error getting vApp %s: %v", name, err)
			continue
		}
		t.Logf("deleting: %s (href=%s)", name, vapp.VApp.HREF)
		err = ig.deleteInstance(vapp.VApp.HREF)
		if err != nil {
			t.Logf("  error: %v", err)
		} else {
			deleted++
		}
	}
	t.Logf("deleted %d orphaned vApps", deleted)
}
