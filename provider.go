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
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
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
)

type InstanceGroup struct {
	Name      string           `json:"name"`
	vcdClient *govcd.VCDClient `json:"-"`

	// Cloud Director connection config
	StrURL               string `json:"url"`
	Org                  string `json:"org"`
	Token                string `json:"token"`
	VirtualDatacenter    string `json:"virtual_datacenter"`
	Network              string `json:"network"`
	IPAllocationMode     string `json:"ip_allocation_mode"`
	InstanceGroupName    string `json:"instance_group_name"`
	VAppNamePrefix       string `json:"vapp_name_prefix"`
	Catalog              string `json:"catalog"`
	Template             string `json:"template"`
	StorageProfile       string `json:"storage_profile"`
	CPUCount             int    `json:"cpu_count"`
	CoresPerSocket       int    `json:"cores_per_socket"`
	MemoryMB             int64  `json:"memory_mb"`
	DiskSizeGB           int    `json:"disk_size_gb"`
	DebugServerAddr      string `json:"debug_server_addr"`
	MaxConcurrentCreates int    `json:"max_concurrent_creates"`
	MaxConcurrentDeletes int    `json:"max_concurrent_deletes"`

	parsedURL *url.URL

	log hclog.Logger

	settings    provider.Settings
	store       *desiredStateStore
	ig          VCDInstanceGroup
	debugServer *DebugServer
	httpServer  *http.Server
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

	// Create store
	g.store = newDesiredStateStore(g.log, g.InstanceGroupName)

	// Create and start reconciler
	config := ReconcilerConfig{
		MaxConcurrentCreates: g.MaxConcurrentCreates,
		MaxConcurrentDeletes: g.MaxConcurrentDeletes,
	}
	reconciler := newVCDInstanceGroup(g.log, g.store, g, config)
	reconciler.Start()
	g.ig = reconciler

	// Initialize debug server if address is configured
	if g.DebugServerAddr != "" {
		g.debugServer = NewDebugServer(g.log, g.store, g.InstanceGroupName)
		g.httpServer = &http.Server{
			Addr:    g.DebugServerAddr,
			Handler: g.debugServer,
		}

		go func() {
			g.log.Info("Starting debug HTTP server", "addr", g.DebugServerAddr)
			if err := g.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				g.log.Error("Debug HTTP server failed", "error", err)
			}
		}()
	} else {
		g.log.Debug("Debug HTTP server disabled (no address configured)")
	}

	return provider.ProviderInfo{
		ID:        path.Join("vcd", g.Org, g.VirtualDatacenter, g.Network, g.VAppNamePrefix, g.InstanceGroupName),
		MaxSize:   maxSize,
		Version:   Version.Version,
		BuildInfo: Version.BuildInfo(),
	}, nil
}

func (g *InstanceGroup) Increase(ctx context.Context, delta int) (int, error) {
	g.log.Info("Increasing", "delta", delta)
	ids := g.ig.Increase(delta)
	return len(ids), nil
}

// Decrease implements provider.InstanceGroup
func (g *InstanceGroup) Decrease(ctx context.Context, instancesToDelete []string) ([]string, error) {
	g.log.Info("Decreasing the number of instances", "instancesToDelete", instancesToDelete)
	if len(instancesToDelete) == 0 {
		return nil, nil
	}

	g.ig.Decrease(instancesToDelete)

	return instancesToDelete, nil
}

// Update implements provider.InstanceGroup
func (g *InstanceGroup) Update(ctx context.Context, update func(instance string, state provider.State)) error {
	g.log.Info("Updating instance group state")

	instances := g.ig.Instances()

	size := 0
	for _, inst := range instances {
		if inst.Phase == PhaseDeleted {
			continue
		}

		// Use VApp HREF as instance ID for fleeting (same as before).
		// For instances still being created (no HREF yet), skip — they're not visible to fleeting yet.
		id := inst.ID
		if id == "" {
			// Instance is still in PendingCreate/Creating, hasn't gotten an HREF yet.
			// We still count it toward size but report as Creating.
			size++
			continue
		}

		size++

		var state provider.State
		switch inst.Phase {
		case PhasePendingCreate, PhaseCreating:
			state = provider.StateCreating
		case PhaseRunning:
			if inst.CreatedAt == nil {
				// Preexisting instance — report as Running so fleeting can manage it
				state = provider.StateRunning
			} else {
				state = provider.StateRunning
			}
		case PhasePendingDelete, PhaseDeleting:
			state = provider.StateDeleting
		default:
			state = provider.StateRunning
		}

		update(id, state)
	}

	PoolSize.WithLabelValues(g.InstanceGroupName).Set(float64(size))

	return nil
}

// ConnectInfo implements provider.InstanceGroup
func (g *InstanceGroup) ConnectInfo(ctx context.Context, id string) (provider.ConnectInfo, error) {
	inst, found := g.ig.Instance(id)
	if !found {
		return provider.ConnectInfo{}, errInstanceNotFound
	}

	if inst.CreatedAt == nil {
		return provider.ConnectInfo{}, errInstancePreexisting
	}

	info := provider.ConnectInfo{
		ConnectorConfig: g.settings.ConnectorConfig,
	}

	info.Arch = "amd64" // vcd does not support anything else

	// Try to determine OS from cached OSType first
	if inst.OSType != "" {
		if strings.Contains(inst.OSType, "windows") {
			info.OS = "windows"
			info.Username = "Administrator"
		} else {
			info.OS = "linux"
			info.Username = "root"
		}
	} else {
		// Fall back to querying VCD directly
		vm, err := g.getVMFromVAppHREF(id)
		if err != nil {
			return info, err
		}

		if strings.Contains(vm.VM.VmSpecSection.OsType, "windows") {
			info.OS = "windows"
			info.Username = "Administrator"
		} else {
			info.OS = "linux"
			info.Username = "root"
		}
	}

	info.Protocol = provider.ProtocolSSH

	ipAddress := inst.IPAddress
	if ipAddress == "" {
		return info, fmt.Errorf("no IP address found for instance %s", id)
	}

	info.InternalAddr = ipAddress
	info.ExternalAddr = ipAddress

	return info, nil
}

func (g *InstanceGroup) Heartbeat(ctx context.Context, id string) error {
	inst, found := g.ig.Instance(id)
	if !found {
		g.log.Warn("instance not found. this can happen when the instance is not fetched yet on start-up.", "id", id)
		return nil
	}

	if inst.CreatedAt == nil {
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

	return g.ig.Shutdown(ctx)
}
