package vcd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/hashicorp/go-hclog"
	"github.com/vmware/go-vcloud-director/v3/govcd"
	"github.com/vmware/go-vcloud-director/v3/types/v56"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"golang.org/x/sync/semaphore"
)

var _ provider.InstanceGroup = (*InstanceGroup)(nil)

var (
	errInstanceNotFound    = errors.New("instance not found")
	errInstancePreexisting = errors.New("instance is preexisting")
)

const (
	// vcd does not implement any kind of instance group
	// so we use a metadata tag to identify the VMs of this fleeting instance group
	instanceGroupMetadataKey = "vcd-instance-group"
	maxSize                  = 128
	debugServerAddr          = "0.0.0.0:27060"
	maxConcurrentDeletions   = 5 // Limit concurrent deletion operations
)

type InstanceGroup struct {
	Name      string           `json:"name"`
	vcdClient *govcd.VCDClient `json:"-"`

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

	settings     provider.Settings
	stateManager *instanceStateManager
	debugServer  *DebugServer
	httpServer   *http.Server
	deletionSem  *semaphore.Weighted // Limits concurrent deletions
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

	g.stateManager = newInstanceStateManager(g.log)

	// Initialize deletion semaphore to limit concurrent API calls
	g.deletionSem = semaphore.NewWeighted(maxConcurrentDeletions)

	// Initialize debug server
	g.debugServer = NewDebugServer(g.log, g.stateManager, g.InstanceGroupName)
	g.httpServer = &http.Server{
		Addr:    debugServerAddr,
		Handler: g.debugServer,
	}

	// Start HTTP server in a goroutine
	go func() {
		g.log.Info("Starting debug HTTP server", "addr", debugServerAddr)
		if err := g.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			g.log.Error("Debug HTTP server failed", "error", err)
		}
	}()

	return provider.ProviderInfo{
		ID:        path.Join("vcd", g.Org, g.VirtualDatacenter, g.Network, g.VAppNamePrefix, g.InstanceGroupName),
		MaxSize:   maxSize,
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
func (g *InstanceGroup) Decrease(ctx context.Context, instancesToDelete []string) ([]string, error) {
	g.log.Info("Decreasing the number of instances", "instancesToDelete", instancesToDelete)
	if len(instancesToDelete) == 0 {
		return nil, nil
	}

	// Launch deletions in goroutines with semaphore to limit concurrency
	for _, instanceID := range instancesToDelete {
		go func() {
			// Acquire semaphore (blocks if max concurrent deletions reached)
			if err := g.deletionSem.Acquire(context.Background(), 1); err != nil {
				g.log.Error("failed to acquire deletion semaphore", "id", instanceID, "error", err)
				return
			}
			defer g.deletionSem.Release(1)

			// Perform deletion
			if err := g.deleteInstance(instanceID); err != nil {
				g.log.Error("deleting VM", "id", instanceID, "error", err)
			} else {
				g.log.Info("successfully deleted VM", "id", instanceID)
			}
		}()
	}

	// Return immediately - deletions happen in background
	return instancesToDelete, nil
}

// Update implements provider.InstanceGroup
func (g *InstanceGroup) Update(ctx context.Context, update func(instance string, state provider.State)) error {
	g.log.Info("Updating instance group state")
	client, err := g.getVCDClient()
	if err != nil {
		g.log.Error("error creating VCD client", "error", err)
		return err
	}

	vapps, err := g.getInstancesInInstanceGroup()
	if err != nil {
		g.log.Error("error getting vapps in instance group", "error", err)
		return fmt.Errorf("getting vapps in instance group: %w", err)
	}

	size := 0
	for _, vapp := range vapps {
		g.log.Debug("Checking status of vapp", "vApp", vapp.VApp.Name)
		if vapp.VApp.Children == nil || len(vapp.VApp.Children.VM) == 0 {
			g.log.Warn("vapp has no VMs", "vApp", vapp.VApp.HREF)
			continue
		}

		g.log.Debug("refreshing vapp", "vApp", vapp.VApp.Name)
		err := vapp.Refresh()
		if err != nil {
			g.log.Error("error refreshing vapp", "vApp", vapp.VApp.Name, "error", err)
			continue
		}

		// Check again after refresh - state might have changed
		if vapp.VApp.Children == nil || len(vapp.VApp.Children.VM) == 0 {
			g.log.Warn("vapp has no VMs after refresh", "vApp", vapp.VApp.HREF)
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

		ipAddress, err := getPrimaryIPAddress(vm)
		if err != nil {
			g.log.Error("error getting primary IP address", "vApp", vapp.VApp.HREF, "error", err)
			continue
		}

		// We update our state manager first, just in case this is a pre-existing instance.
		g.stateManager.Update(vapp.VApp.HREF, func(data instanceData) instanceData {
			data.VAppName = vapp.VApp.Name
			data.VMName = vapp.VApp.Children.VM[0].Name
			data.VAppStatus = types.VAppStatuses[vapp.VApp.Status]
			data.VMStatus = types.VAppStatuses[vapp.VApp.Children.VM[0].Status]
			data.IPAddress = ipAddress
			return data
		})

		_, state := g.stateManager.GetFleetingState(vapp.VApp.HREF)

		// we update the state of the VM, but we use the vApp href as id
		// so it is easier to find the vApp in the API response
		update(vapp.VApp.HREF, state)
	}

	g.size = size
	return nil
}

// ConnectInfo implements provider.InstanceGroup
func (g *InstanceGroup) ConnectInfo(ctx context.Context, id string) (provider.ConnectInfo, error) {
	found, data := g.stateManager.Get(id)
	if !found {
		return provider.ConnectInfo{}, errInstanceNotFound
	}

	if data.CreatedAt == nil {
		// Instruct Fleeting to kill the instance, as it is preexisting
		return provider.ConnectInfo{}, errInstancePreexisting
	}

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

	ipAddress, err := getPrimaryIPAddress(vm)
	if err != nil {
		return info, err
	}
	info.InternalAddr = ipAddress
	info.ExternalAddr = ipAddress

	if info.ExternalAddr == "" {
		return info, fmt.Errorf("no external address found for VM %s", id)
	}

	return info, nil
}

// Heartbeat is typical called by the taskscaler before the taskscaler does connect to the instance.
func (g *InstanceGroup) Heartbeat(ctx context.Context, id string) error {
	found, data := g.stateManager.Get(id)
	if !found {
		g.log.Warn("instance not found. this can happen when the instance is not fetched yet on start-up.", "id", id)
		return nil
	}

	if data.CreatedAt == nil {
		// Instruct Fleeting to kill the instance, as it is preexisting
		return errInstancePreexisting
	}

	return nil
}

func (g *InstanceGroup) Shutdown(ctx context.Context) error {
	g.log.Info("Shutting down the instance group")

	// Shutdown HTTP server first
	if g.httpServer != nil {
		g.log.Info("Shutting down debug HTTP server")
		if err := g.httpServer.Shutdown(ctx); err != nil {
			g.log.Error("Error shutting down debug HTTP server", "error", err)
		}
	}

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
