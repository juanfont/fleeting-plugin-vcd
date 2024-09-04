package vcd

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/hashicorp/go-hclog"
	"github.com/vmware/go-vcloud-director/v2/types/v56"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

type InstanceGroup struct {
	Name string `json:"name"`

	// Cloud Director connection config
	StrURL            string `json:"url"`
	Org               string `json:"org"`
	VirtualDatacenter string `json:"virtual_datacenter"`
	Network           string `json:"network"`
	IPAllocationMode  string `json:"ip_allocation_mode"`
	Token             string `json:"token"` // API token (vcd > 10.4 required)
	Catalog           string `json:"catalog"`
	Template          string `json:"template"`
	VApp              string `json:"vapp"` // vApp to deploy workers on
	VMNamePrefix      string `json:"vm_name_prefix"`
	StorageProfile    string `json:"storage_profile"`

	size int

	parsedURL *url.URL
	vAppHREF  string

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

	vapp, err := g.getOrCreateVApp()
	if err != nil {
		return provider.ProviderInfo{}, fmt.Errorf("getting or creating vApp: %w", err)
	}

	g.vAppHREF = vapp.VApp.HREF // this speeds-up subsequent calls

	return provider.ProviderInfo{
		ID:        path.Join("vcd", g.Org, g.VirtualDatacenter, g.Network, g.VApp),
		MaxSize:   128, // max number of VMs in a vApp
		Version:   Version.Version,
		BuildInfo: Version.BuildInfo(),
	}, nil
}

// TODO(juanfont): Right now, the Increase operation is extremely blocking, as it has to add a VM to the vApp one at a time.
// This is because vcd does not support performing multiple operations in parallel inside the same vApp.
// One possible solution is to create a new vApp for each VM, but this would require somehow keeping track of the created vApps.
func (g *InstanceGroup) Increase(ctx context.Context, delta int) (int, error) {
	added := 0
	for i := 1; i <= delta; i++ {
		vm, err := g.addVMToVApp()
		if err != nil {
			continue
		}
		added++
		g.log.Debug("added VM to vApp", "id", vm.VM.HREF, "name", vm.VM.Name)
	}

	return added, nil
}

// Decrease implements provider.InstanceGroup
func (g *InstanceGroup) Decrease(ctx context.Context, instances []string) ([]string, error) {
	if len(instances) == 0 {
		return nil, nil
	}

	deletedVMs := []string{}

	for _, id := range instances {
		if err := g.deleteVM(id); err != nil {
			g.log.Error("deleting VM", "id", id, "error", err)
		} else {
			deletedVMs = append(deletedVMs, id)
		}
	}

	return deletedVMs, nil
}

// Update implements provider.InstanceGroup
func (g *InstanceGroup) Update(ctx context.Context, update func(instance string, state provider.State)) error {
	vapp, err := g.getVApp()
	if err != nil {
		return fmt.Errorf("getting vApp: %w", err)
	}

	if vapp.VApp.Children == nil {
		g.size = 0
		return nil
	}

	g.size = len(vapp.VApp.Children.VM)

	for _, vm := range vapp.VApp.Children.VM {
		var state provider.State
		switch types.VAppStatuses[vm.Status] {
		// The lifecycle in VCD is:
		// - Deploying UNRESOLVED -> POWERED_OFF -> PARTIALLY_POWERED_OFF -> POWERED_ON
		// - Deleting POWERED_ON -> PARTIALLY_POWERED_OFF -> POWERED_OFF -> DELETING -> UNKNOWN
		case "UNRESOLVED":
			state = provider.StateCreating
		case "POWERED_ON":
			state = provider.StateRunning
		case "UNKNOWN":
			state = provider.StateDeleting
		case "POWERED_OFF", "PARTIALLY_POWERED_OFF":
			// as these two states are not final, we just ignore them
			g.log.Debug("unhandled instance status", "id", vm.HREF, "name", vm.Name, "status", vm.Status)
		default:
			g.log.Error("unexpected instance status", "id", vm.HREF, "name", vm.Name, "status", vm.Status)

		}

		update(vm.HREF, state)
	}

	return nil
}

// ConnectInfo implements provider.InstanceGroup
func (g *InstanceGroup) ConnectInfo(ctx context.Context, id string) (provider.ConnectInfo, error) {
	info := provider.ConnectInfo{
		ConnectorConfig: g.settings.ConnectorConfig,
	}

	vm, err := g.getVM(id)
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
	g.log.Info("Shutting down. Deleting vApp", "vApp", g.vAppHREF)
	return g.deleteVApp(g.vAppHREF)
}
