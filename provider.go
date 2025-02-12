package vcd

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/hashicorp/go-hclog"
	"github.com/vmware/go-vcloud-director/v2/govcd"
	"github.com/vmware/go-vcloud-director/v2/types/v56"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

const (
	// vcd does not implement any kind of instance group
	// so we use a metadata tag to identify the VMs of this fleeting instance group
	instanceGroupMetadataKey = "vcd-instance-group"
)

type InstanceGroup struct {
	Name string `json:"name"`

	// Cloud Director connection config
	StrURL            string `json:"url"`
	Org               string `json:"org"`
	Token             string `json:"token"`
	VirtualDatacenter string `json:"virtual_datacenter"`
	Network           string `json:"network"`
	IPAllocationMode  string `json:"ip_allocation_mode"`  // API token (vcd > 10.4 required)
	InstanceGroupName string `json:"instance_group_name"` // Metadata tag to use for the VMs of this fleeting instance group
	VAppNamePrefix    string `json:"vapp_name_prefix"`
	Catalog           string `json:"catalog"`
	Template          string `json:"template"`
	StorageProfile    string `json:"storage_profile"`
	CPUCount          int    `json:"cpu_count"`
	CoresPerSocket    int    `json:"cores_per_socket"`
	MemoryMB          int64  `json:"memory_mb"`
	DiskSizeGB        int    `json:"disk_size_gb"`

	size int

	parsedURL *url.URL

	log hclog.Logger

	settings provider.Settings
}

// Init implements provider.InstanceGroup
func (g *InstanceGroup) Init(ctx context.Context, logger hclog.Logger, settings provider.Settings) (provider.ProviderInfo, error) {
	g.settings = settings
	g.log = logger.With("org", g.Org, "vdc", g.VirtualDatacenter, "network", g.Network)

	if err := g.validate(); err != nil {
		return provider.ProviderInfo{}, err
	}

	if err := g.populate(); err != nil {
		return provider.ProviderInfo{}, err
	}

	if !g.settings.UseStaticCredentials {
		return provider.ProviderInfo{}, fmt.Errorf("dynamic credentials are not supported yet")
	}

	return provider.ProviderInfo{
		ID:        path.Join("vcd", g.Org, g.VirtualDatacenter, g.Network, g.VAppNamePrefix, g.InstanceGroupName),
		MaxSize:   128,
		Version:   Version.Version,
		BuildInfo: Version.BuildInfo(),
	}, nil
}

func (g *InstanceGroup) Increase(ctx context.Context, delta int) (int, error) {
	fmt.Println("Increasing")
	added := 0
	for i := 1; i <= delta; i++ {
		go g.createInstance()
		added++
		g.log.Debug("added VM to vApp")
	}

	return added, nil
}

// Decrease implements provider.InstanceGroup
func (g *InstanceGroup) Decrease(ctx context.Context, instances []string) ([]string, error) {
	if len(instances) == 0 {
		return nil, nil
	}

	deletedVMs := []string{}
	for _, instanceID := range instances {
		if err := g.deleteInstance(instanceID); err != nil {
			g.log.Error("deleting VM", "id", instanceID, "error", err)
		} else {
			deletedVMs = append(deletedVMs, instanceID)
		}
	}

	return deletedVMs, nil
}

// Update implements provider.InstanceGroup
func (g *InstanceGroup) Update(ctx context.Context, update func(instance string, state provider.State)) error {
	g.log.Debug("Updating instance group")
	client, err := newClient(*g.parsedURL, g.Org, g.Token, false)
	if err != nil {
		return err
	}

	vapps, err := g.getInstancesInInstanceGroup()
	if err != nil {
		g.log.Error("error getting vapps in instance group", "error", err)
		return fmt.Errorf("getting vapps in instance group: %w", err)
	}

	g.log.Debug("found vapps", "number", len(vapps))

	size := 0
	for _, vapp := range vapps {
		g.log.Debug("Checking status of vapp", "vApp", vapp.VApp.Name)
		if len(vapp.VApp.Children.VM) == 0 {
			g.log.Debug("vapp has no VMs", "vApp", vapp.VApp.HREF)
			continue
		}

		g.log.Debug("refreshing vapp", "vApp", vapp.VApp.Name)
		err := vapp.Refresh()
		if err != nil {
			g.log.Error("error refreshing vapp", "vApp", vapp.VApp.Name, "error", err)
			continue
		}

		g.log.Debug("refreshing VM", "vApp", vapp.VApp.HREF)
		vmHREF := vapp.VApp.Children.VM[0].HREF
		vm := govcd.NewVM(&client.Client)
		vm.VM.HREF = vmHREF
		err = vm.Refresh()
		if err != nil {
			g.log.Error("error refreshing VM", "VM", vm.VM.Name, "error", err)
			continue
		}

		size++

		var state provider.State
		switch types.VAppStatuses[vm.VM.Status] {
		// The lifecycle in VCD is:
		// - Deploying UNRESOLVED -> POWERED_OFF -> PARTIALLY_POWERED_OFF -> POWERED_ON
		// - Deleting POWERED_ON -> PARTIALLY_POWERED_OFF -> POWERED_OFF -> DELETING -> UNKNOWN
		case "UNRESOLVED", "PARTIALLY_POWERED_OFF":
			state = provider.StateCreating
		case "POWERED_ON":
			state = provider.StateRunning
		case "UNKNOWN":
			state = provider.StateDeleting

		default:
			g.log.Info("unexpected instance status", "id", vm.VM.HREF, "name", vm.VM.Name, "status", vm.VM.Status, "statusName", types.VAppStatuses[vm.VM.Status])
			if vm.VM.Tasks != nil {
				for _, t := range vm.VM.Tasks.Task {
					g.log.Debug("task", "id", t.ID, "status", t.Status, "name", t.Name, "operation", t.Operation, "vm", vm.VM.Name, "vApp", vapp.VApp.Name)
				}
			}

			// TODO(juanfont): Check for failed signs and handle then. E.g., there is a task in the VM task list that shows failed
		}

		// we update the state of the VM, but we use the vApp href as id
		// so it is easier to find the vApp in the API response
		update(vapp.VApp.HREF, state)
	}

	g.size = size
	return nil
}

// ConnectInfo implements provider.InstanceGroup
func (g *InstanceGroup) ConnectInfo(ctx context.Context, id string) (provider.ConnectInfo, error) {
	info := provider.ConnectInfo{
		ConnectorConfig: g.settings.ConnectorConfig,
	}

	vm, err := g.getVMFromVAppHREF(id)
	if err != nil {
		return info, err
	}

	info.Arch = "amd64" // vcd does not support anything else

	if strings.Contains(vm.VM.VmSpecSection.OsType, "windows") {
		info.OS = "windows"
		info.Username = "Administrator" // we rely on VMware Guest Customization
	} else {
		info.OS = "linux"
		info.Username = "root" // we rely on VMware Guest Customization
	}

	info.Protocol = provider.ProtocolSSH

	// We assume that the vApp has only one VM with only one NIC
	if vm.VM.NetworkConnectionSection != nil {
		networks := vm.VM.NetworkConnectionSection.NetworkConnection
		for _, n := range networks {
			if n.IPAddress != "" {
				info.InternalAddr = n.IPAddress
				info.ExternalAddr = n.IPAddress
			}
		}
	}

	if info.ExternalAddr == "" {
		return info, fmt.Errorf("no external address found for VM %s", id)
	}

	return info, nil
}

func (g *InstanceGroup) Shutdown(ctx context.Context) error {
	vapps, err := g.getInstancesInInstanceGroup()
	if err != nil {
		return fmt.Errorf("getting vapps in instance group: %w", err)
	}

	for _, vapp := range vapps {
		if !strings.HasPrefix(vapp.VApp.Name, g.VAppNamePrefix) {
			g.log.Warn("skipping vApp deletion", "vApp", vapp.VApp.HREF, "name", vapp.VApp.Name)
			continue
		}
		g.log.Info("Shutting down. Deleting vApp", "vApp", vapp.VApp.HREF)
		err = g.deleteInstance(vapp.VApp.HREF)
		if err != nil {
			g.log.Error("error deleting vApp", "vApp", vapp.VApp.HREF, "error", err)
		}
	}

	return nil
}
