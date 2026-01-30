package vcd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/vmware/go-vcloud-director/v3/govcd"
	"github.com/vmware/go-vcloud-director/v3/types/v56"
	"golang.org/x/crypto/ssh"
)

var (
	errTemplateNotReady           = errors.New("template not ready (status != 8)")
	errUnexpectedNumberOfVMs      = errors.New("unexpected number of VMs in template")
	errDiskSectionNotFound        = errors.New("disk section not found")
	errCouldNotExecuteTaskRequest = errors.New("could not execute task request")
)

const (
	deleteInstanceTimeout        = 60 * time.Minute
	refreshBackoffMaxInterval    = 1 * time.Minute
	refreshBackoffMaxElapsedTime = 20 * time.Minute

	// vcd does not implement any kind of instance group, and it tends
	// to have hiccups with the API, so we need to garbage collect the instances
	// periodically that failed to be deployed, or failed to be deleted.
	instanceGarbageCollectionInterval = 1 * time.Hour
	maxVAppAgeBeforeGC                = 24 * time.Hour

	vcdAPIVersion = "38.1"
)

type trustedPlatformModuleEdit struct {
	XMLName    xml.Name `xml:"root:TrustedPlatformModule"`
	Xmlns      string   `xml:"xmlns:root,attr"`
	TpmPresent bool     `xml:"root:TpmPresent"`
}

func (g *InstanceGroup) createInstance() (vapp *govcd.VApp, vm *govcd.VM, err error) {
	var vAppName string
	var completed bool
	startTime := time.Now()

	// Combined defer: panic recovery + cleanup on failure + metrics
	// This ensures cleanup happens for ALL failure modes: errors, panics, early returns
	defer func() {
		// First, recover from any panics
		if r := recover(); r != nil {
			g.log.Error("Panic recovered in createInstance", "panic", r, "vapp_name", vAppName)
			err = fmt.Errorf("panic in createInstance: %v", r)
		}

		// Record metrics
		duration := time.Since(startTime).Seconds()
		InstanceCreationDuration.WithLabelValues(g.InstanceGroupName).Observe(duration)
		if completed {
			InstancesCreatedTotal.WithLabelValues(g.InstanceGroupName).Inc()
		} else {
			InstancesFailedTotal.WithLabelValues(g.InstanceGroupName, "create").Inc()
		}

		// Then, clean up if creation didn't complete successfully
		if !completed && vAppName != "" {
			g.log.Warn("createInstance did not complete successfully, cleaning up",
				"vapp_name", vAppName,
				"error", err,
			)
			go g.cleanUpInstanceByName(vAppName)
		}
	}()

	client, err := g.getVCDClient()
	if err != nil {
		return nil, nil, err
	}

	org, err := client.GetOrgByName(g.Org)
	if err != nil {
		return nil, nil, err
	}

	vdc, err := org.GetVDCByName(g.VirtualDatacenter, true)
	if err != nil {
		g.log.Error("vdc %s not found", g.VirtualDatacenter)
		return nil, nil, err
	}

	template, err := g.getVAppTemplate()
	if err != nil {
		return nil, nil, err
	}

	network, err := vdc.GetOrgVdcNetworkByName(g.Network, true)
	if err != nil {
		return nil, nil, err
	}

	storageProfile, err := g.getStorageProfile()
	if err != nil {
		g.log.Error("error getting storage profile", "error", err)
		return nil, nil, err
	}

	vAppName, err = generateVMName(g.VAppNamePrefix)
	if err != nil {
		return nil, nil, err
	}

	g.log.Info("Creating a new vApp", "vapp", vAppName, "template", template.VAppTemplate.Name)
	networks := []*types.OrgVDCNetwork{}
	networks = append(networks, network.OrgVDCNetwork)
	task, err := vdc.ComposeVApp(
		networks,
		*template,
		*storageProfile,
		vAppName,
		fmt.Sprintf("VM created by the %s GitLab Fleeting runner", g.Name),
		true)
	if err != nil {
		g.log.Error("error creating vapp", "error", err)
		return nil, nil, err
	}

	if err = task.WaitTaskCompletion(); err != nil {
		g.log.Error("error waiting for task completion", "error", err)
		return nil, nil, err
	}

	vapp, err = vdc.GetVAppByName(vAppName, true)
	if err != nil {
		g.log.Error("error getting vapp", "error", err, "vapp_name", vAppName)
		return nil, nil, err
	}

	if len(vapp.VApp.Children.VM) != 1 {
		g.log.Error(
			"vapp has unexpected number of VMs",
			"vapp_href", vapp.VApp.HREF,
			"vapp", vapp.VApp.Name,
			"expected", 1,
			"got", len(vapp.VApp.Children.VM))
		return nil, nil, errUnexpectedNumberOfVMs
	}

	g.log.Info("waiting for VM to be fully created",
		"vapp_href", vapp.VApp.HREF,
		"vapp", vapp.VApp.Name,
		"vm_href", vapp.VApp.Children.VM[0].HREF,
		"vm", vapp.VApp.Children.VM[0].Name,
	)

	vm, err = waitForVMCreation(client, vapp)
	if err != nil {
		return nil, nil, err
	}

	g.stateManager.Update(vapp.VApp.HREF, func(data instanceData) instanceData {
		return instanceData{
			InstanceID: vapp.VApp.HREF,
			VAppName:   vapp.VApp.Name,
			VMName:     vapp.VApp.Children.VM[0].Name,
			VAppStatus: types.VAppStatuses[vapp.VApp.Status],
			VMStatus:   types.VAppStatuses[vapp.VApp.Children.VM[0].Status],
			CreatedAt:  now(),
		}
	})

	g.log.Info("VM is created",
		"vm", vm.VM.Name, "vm_href", vm.VM.HREF,
		"vapp_href", vapp.VApp.HREF,
		"vapp", vapp.VApp.Name,
	)

	// Refresh vApp before adding metadata to ensure it's fully loaded
	err = vapp.Refresh()
	if err != nil {
		g.log.Error(
			"error refreshing vApp before adding metadata",
			"error", err,
			"vapp_href", vapp.VApp.HREF,
			"vapp", vapp.VApp.Name,
		)
		return nil, nil, err
	}

	err = vapp.AddMetadataEntryWithVisibility(
		instanceGroupMetadataKey,
		g.InstanceGroupName,
		types.MetadataStringValue,
		types.MetadataReadWriteVisibility,
		false, // isSystem
	)
	if err != nil {
		g.log.Error(
			"error adding metadata to vApp",
			"error", err,
			"vapp_href", vapp.VApp.HREF,
			"vapp", vapp.VApp.Name,
			"metadata_key", instanceGroupMetadataKey,
			"metadata_value", g.InstanceGroupName,
		)
		return nil, nil, err
	}

	g.log.Info("injecting credentials", "vm", vm.VM.Name)

	err = g.injectCredentials(vm)
	if err != nil {
		return nil, nil, err
	}

	if vmRequiresTPM(vm) {
		task, err := changeVMTpm(client, vm, true)
		if err != nil {
			return nil, nil, err
		}
		if err = task.WaitTaskCompletion(); err != nil {
			return nil, nil, err
		}
	}

	g.log.Debug("refreshing VM", "vm", vm.VM.Name)

	err = vm.Refresh()
	if err != nil {
		return nil, nil, err
	}

	// vm, err = g.renameVM(client, vm, vAppName)
	// if err != nil {
	// 	return nil, err
	// }

	g.log.Info(
		"changing CPU and core count", "vm", vm.VM.Name,
		"vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
		"cpu_count", g.CPUCount, "cores_per_socket", g.CoresPerSocket,
	)

	err = vm.ChangeCPUAndCoreCount(&g.CPUCount, &g.CoresPerSocket)
	if err != nil {
		return nil, nil, err
	}

	g.log.Info(
		"changing memory", "vm", vm.VM.Name, "vapp_href",
		vapp.VApp.HREF, "vapp", vapp.VApp.Name,
		"memory_mb", g.MemoryMB,
	)

	err = vm.ChangeMemory(g.MemoryMB)
	if err != nil {
		return nil, nil, err
	}

	g.log.Info(
		"changing disk size", "vm", vm.VM.Name, "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
		"disk_size_gb", g.DiskSizeGB,
	)

	vm, err = g.changeDiskSize(vm)
	if err != nil {
		return nil, nil, err
	}

	g.log.Info("disk size changed, refreshing VM",
		"vm", vm.VM.Name, "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
	)

	err = vm.Refresh()
	if err != nil {
		return nil, nil, err
	}

	g.log.Info("getting primary IP address",
		"vm", vm.VM.Name, "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
	)

	ipAddress, err := getPrimaryIPAddress(vm)
	if err != nil {
		g.log.Error("error getting primary IP address while creating instance",
			"vapp_href", vapp.VApp.HREF,
			"vapp_name", vapp.VApp.Name,
			"vm_name", vm.VM.Name,
			"error", err)
		return nil, nil, err
	}

	g.stateManager.Update(vapp.VApp.HREF, func(data instanceData) instanceData {
		data.InstanceID = vapp.VApp.HREF
		data.VAppStatus = types.VAppStatuses[vapp.VApp.Status]
		data.VMStatus = types.VAppStatuses[vapp.VApp.Children.VM[0].Status]
		data.IPAddress = ipAddress
		return data
	})

	g.log.Info("powering on VM",
		"vm", vm.VM.Name, "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
		"ip_address", ipAddress,
	)

	task, err = vm.PowerOn()
	if err != nil {
		g.log.Error("error powering on VM", "error", err,
			"vm", vm.VM.Name, "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
		)
		return nil, nil, err
	}

	g.stateManager.Update(vapp.VApp.HREF, func(data instanceData) instanceData {
		data.InstanceID = vapp.VApp.HREF
		data.VAppStatus = types.VAppStatuses[vapp.VApp.Status]
		data.VMStatus = types.VAppStatuses[vapp.VApp.Children.VM[0].Status]
		data.BootingAt = now()
		return data
	})

	if err = task.WaitTaskCompletion(); err != nil {
		return nil, nil, err
	}

	g.stateManager.Update(vapp.VApp.HREF, func(data instanceData) instanceData {
		data.InstanceID = vapp.VApp.HREF
		data.VAppStatus = types.VAppStatuses[vapp.VApp.Status]
		data.VMStatus = types.VAppStatuses[vapp.VApp.Children.VM[0].Status]
		data.BootedAt = now()
		return data
	})

	g.log.Debug("VM reported as powered on", "vm", vm.VM.Name)

	if status, err := vm.GetStatus(); err != nil || status != "POWERED_ON" {
		panic(fmt.Sprintf("vm %s is not powered on: %s", vm.VM.Name, err))
	}

	g.stateManager.Update(vapp.VApp.HREF, func(data instanceData) instanceData {
		data.InstanceID = vapp.VApp.HREF
		data.VAppStatus = types.VAppStatuses[vapp.VApp.Status]
		data.VMStatus = types.VAppStatuses[vapp.VApp.Children.VM[0].Status]
		return data
	})

	g.log.Info("instance created successfully",
		"vapp_name", vapp.VApp.Name,
		"vapp_href", vapp.VApp.HREF,
		"vm_name", vm.VM.Name,
	)

	// Mark creation as complete - this prevents the cleanup defer from running
	completed = true
	return vapp, vm, nil
}

func (g *InstanceGroup) getInstancesInInstanceGroup() (vApps []*govcd.VApp, err error) {
	// Recover from panics in VCD library (known issue with SearchByFilter).
	// Return empty list instead of error to avoid killing the taskscaler.
	defer func() {
		if r := recover(); r != nil {
			g.log.Error("Panic recovered in getInstancesInInstanceGroup (returning empty list)", "panic", r)
			vApps = []*govcd.VApp{}
			err = nil
		}
	}()

	client, err := g.getVCDClient()
	if err != nil {
		g.log.Error("error creating VCD client (returning empty list)", "error", err)
		return []*govcd.VApp{}, nil
	}

	org, err := client.GetOrgByName(g.Org)
	if err != nil {
		g.log.Error("error getting org (returning empty list)", "error", err)
		return []*govcd.VApp{}, nil
	}

	vdc, err := org.GetVDCByName(g.VirtualDatacenter, true)
	if err != nil {
		g.log.Error("error getting VDC (returning empty list)", "error", err)
		return []*govcd.VApp{}, nil
	}

	criteria := &govcd.FilterDef{
		Filters: map[string]string{
			"name_regex": fmt.Sprintf("^%s", g.VAppNamePrefix),
		},
		Metadata: []govcd.MetadataDef{
			{
				Key:      instanceGroupMetadataKey,
				Type:     "STRING",
				Value:    g.InstanceGroupName,
				IsSystem: false,
			},
		},
		UseMetadataApiFilter: true,
	}

	results, _, err := client.Client.SearchByFilter(types.QtVapp, criteria)
	if err != nil {
		// Don't propagate SearchByFilter errors to the taskscaler.
		// Returning an error here causes the taskscaler to stop reconciling
		// this runner group entirely, which is catastrophic.
		// Instead, return an empty list and let the next reconcile retry.
		g.log.Error("error searching for vapps (returning empty list to avoid taskscaler death)", "error", err)
		return []*govcd.VApp{}, nil
	}

	vApps = []*govcd.VApp{}
	for _, result := range results {
		vApp, err := vdc.GetVAppByHref(result.GetHref())
		if err != nil {
			g.log.Warn("error getting vApp. pruning from state manager",
				"name", result.GetName(),
				"href", result.GetHref(), "error", err)
			g.stateManager.Prune(result.GetHref())
			continue
		}

		vApps = append(vApps, vApp)
	}

	return vApps, nil
}

func (g *InstanceGroup) runGarbageCollection() error {
	g.log.Info("starting garbage collection")
	ticker := time.NewTicker(instanceGarbageCollectionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx := context.Background()
			deleted, err := g.garbageCollectInstances(ctx)
			if err != nil {
				g.log.Error("error garbage collecting instances", "error", err)
			}
			g.log.Info("garbage collected instances", "deleted", deleted)
		}
	}
}

func (g *InstanceGroup) garbageCollectInstances(ctx context.Context) (int, error) {
	startTime := time.Now()
	GCRunsTotal.WithLabelValues(g.InstanceGroupName).Inc()

	defer func() {
		duration := time.Since(startTime).Seconds()
		GCDuration.WithLabelValues(g.InstanceGroupName).Observe(duration)
	}()

	g.log.Info("garbage collecting instances")
	vapps, err := g.getInstancesInInstanceGroup()
	if err != nil {
		return 0, fmt.Errorf("error getting instances in instance group: %w", err)
	}

	deleted := 0
	for _, vapp := range vapps {
		if vapp.VApp.Children == nil || len(vapp.VApp.Children.VM) != 1 {
			g.log.Warn("vapp has no VMs", "vApp", vapp.VApp.HREF)
		}

		dateCreated, err := time.Parse("2006-01-02T15:04:05.000Z", vapp.VApp.DateCreated)
		if err != nil {
			g.log.Warn("could not parse DateCreated for vapp", "vApp", vapp.VApp.HREF, "dateCreated", vapp.VApp.DateCreated, "error", err)
			continue
		}

		if dateCreated.Add(maxVAppAgeBeforeGC).Before(time.Now()) {
			g.log.Info("garbage collecting vapp", "vApp", vapp.VApp.HREF)
			g.stateManager.Update(vapp.VApp.HREF, func(data instanceData) instanceData {
				data.GarbageCollectedAt = now()
				return data
			})
			err = g.deleteInstance(vapp.VApp.HREF)
			if err != nil {
				g.log.Error("error deleting vapp", "vApp", vapp.VApp.HREF, "error", err)
			}
			deleted++
			GCInstancesCollectedTotal.WithLabelValues(g.InstanceGroupName).Inc()
			continue
		}
	}

	return deleted, nil
}

func (g *InstanceGroup) cleanUpInstanceByName(name string) error {
	client, err := g.getVCDClient()
	if err != nil {
		return err
	}

	org, err := client.GetOrgByName(g.Org)
	if err != nil {
		return err
	}

	vdc, err := org.GetVDCByName(g.VirtualDatacenter, true)
	if err != nil {
		return err
	}

	vapp, err := vdc.GetVAppByName(name, true)
	if err != nil {
		return err
	}

	return g.deleteInstance(vapp.VApp.HREF)
}

// deleteInstance deletes a vApp and its VM. Because it can
func (g *InstanceGroup) deleteInstance(href string) (err error) {
	startTime := time.Now()

	defer func() {
		duration := time.Since(startTime).Seconds()
		InstanceDeletionDuration.WithLabelValues(g.InstanceGroupName).Observe(duration)
		if err == nil {
			InstancesDeletedTotal.WithLabelValues(g.InstanceGroupName).Inc()
		} else {
			InstancesFailedTotal.WithLabelValues(g.InstanceGroupName, "delete").Inc()
		}
	}()

	g.log.Info("deleting instance", "href", href)
	// Mark deletion as started
	g.stateManager.Update(href, func(data instanceData) instanceData {
		if data.DeletingAt == nil {
			data.DeletingAt = now()
		}
		return data
	})

	client, err := g.getVCDClient()
	if err != nil {
		return err
	}

	var vapp *govcd.VApp
	refreshVappOp := func() error {
		vapp = govcd.NewVApp(&client.Client)
		vapp.VApp.HREF = href
		err := vapp.Refresh()
		if err != nil {
			g.log.Warn("failed to refresh vApp, retrying...",
				"href", href,
				"error", err)
			return err
		}
		return nil
	}

	err = backoff.Retry(
		refreshVappOp,
		backoff.NewExponentialBackOff(
			backoff.WithMaxElapsedTime(refreshBackoffMaxElapsedTime),
			backoff.WithMaxInterval(refreshBackoffMaxInterval),
		),
	)
	if err != nil {
		g.log.Error("error refreshing vapp", "href", href, "error", err)
		return err
	}

	g.log.Debug("deleting vapp", "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name)

	if vapp.VApp.Children == nil || len(vapp.VApp.Children.VM) != 1 {
		g.log.Error("vapp has unexpected number of VMs on delete", "href", href, "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name, "expected 1 VM, got", len(vapp.VApp.Children.VM))
		return errUnexpectedNumberOfVMs
	}

	vm := govcd.NewVM(&client.Client)
	vm.VM.HREF = vapp.VApp.Children.VM[0].HREF
	err = vm.Refresh()
	if err != nil {
		g.log.Error("error refreshing vm for deletion",
			"vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
			"href", href, "vm_href", vm.VM.HREF, "vm", vm.VM.Name, "error", err)
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), deleteInstanceTimeout)
	defer cancel()

	err = waitForTasksCompletion(ctx, vapp, vm)
	if err != nil {
		return err
	}

	g.log.Debug("powering off vapp",
		"vapp_href", vapp.VApp.HREF,
		"vapp", vapp.VApp.Name,
		"statusStr", types.VAppStatuses[vapp.VApp.Status],
		"statusInt", vapp.VApp.Status,
	)

	task, err := vapp.PowerOff()
	if err != nil {
		g.log.Info("unable to power off as it's already powered off", "error", err, "vapp", vapp.VApp.Name)
	} else {
		err = task.WaitTaskCompletion()
		if err != nil {
			return err
		}
	}
	task, err = vapp.Undeploy()
	if err != nil {
		// it's fine if the VApp is already powered off
		g.log.Info("unable to undeploy VApp, probably because it is already off", "error", err, "vapp", vapp.VApp.Name)
	} else {
		err = task.WaitTaskCompletion()
		if err != nil {
			return err
		}
	}

	task, err = vapp.Delete()
	if err != nil {
		// Sometimes VCD is just not cooperating. So we need to retry.
		g.log.Error("error deleting vapp", "href", href, "error", err)

		// FUCK THIS SHIT. FUCK VCD. FUCK THE WHOLE THING.
		time.Sleep(60 * time.Second)

		task, err = vapp.Delete()
		if err != nil {
			g.log.Error("error deleting vapp", "href", href, "error", err)
			return err
		}
	}

	err = task.WaitTaskCompletion()
	if err != nil {
		return err
	}

	// Mark deletion as completed (soft delete)
	g.stateManager.Update(href, func(data instanceData) instanceData {
		data.DeletedAt = now()
		return data
	})

	return nil
}

func (g *InstanceGroup) getStorageProfile() (*types.Reference, error) {
	client, err := g.getVCDClient()
	if err != nil {
		return nil, err
	}

	org, err := client.GetOrgByName(g.Org)
	if err != nil {
		return nil, err
	}

	vdc, err := org.GetVDCByName(g.VirtualDatacenter, false)
	if err != nil {
		return nil, err
	}

	storageProfile, err := vdc.FindStorageProfileReference(g.StorageProfile)
	if err != nil {
		return nil, err
	}

	return &storageProfile, nil
}

func (g *InstanceGroup) renameVM(client *govcd.VCDClient, vm *govcd.VM, name string) (*govcd.VM, error) {
	vm.VM.GuestCustomizationSection.ComputerName = name
	_, err := vm.SetGuestCustomizationSection(vm.VM.GuestCustomizationSection)
	if err != nil {
		return nil, err
	}

	apiEndpoint, _ := url.ParseRequestURI(vm.VM.HREF + "/action/reconfigureVm")
	task, err := client.Client.ExecuteTaskRequest(apiEndpoint.String(), http.MethodPost,
		types.MimeVM, "error modifying VM: %s", &types.Vm{
			Xmlns: types.XMLNamespaceVCloud,
			Ovf:   types.XMLNamespaceOVF,
			Name:  name,
		})
	if err != nil {
		return nil, err
	}
	err = task.WaitTaskCompletion()
	if err != nil {
		return nil, err
	}

	err = vm.Refresh()
	if err != nil {
		return nil, err
	}

	return vm, nil
}

func (g *InstanceGroup) changeDiskSize(vm *govcd.VM) (*govcd.VM, error) {
	if vm.VM.VmSpecSection.DiskSection == nil || len(vm.VM.VmSpecSection.DiskSection.DiskSettings) == 0 {
		g.log.Error("disk section not found")
		return nil, errDiskSectionNotFound
	}

	if vm.VM.VmSpecSection.DiskSection.DiskSettings[0].SizeMb >= int64(g.DiskSizeGB)*1024 {
		g.log.Info("current disk size is >= of the desired size. doing nothing.", "size", vm.VM.VmSpecSection.DiskSection.DiskSettings[0].SizeMb)
		return vm, nil
	}

	g.log.Debug("changing disk size to", "size", g.DiskSizeGB)
	vm.VM.VmSpecSection.DiskSection.DiskSettings[0].SizeMb = int64(g.DiskSizeGB) * 1024
	vm, err := vm.UpdateVmSpecSection(vm.VM.VmSpecSection, "")
	if err != nil {
		return nil, err
	}

	err = vm.Refresh()
	if err != nil {
		return nil, err
	}

	return vm, nil
}

func (g *InstanceGroup) getVMFromVAppHREF(vAppHREF string) (*govcd.VM, error) {
	client, err := g.getVCDClient()
	if err != nil {
		return nil, err
	}

	vapp := govcd.NewVApp(&client.Client)
	vapp.VApp.HREF = vAppHREF
	err = vapp.Refresh()
	if err != nil {
		return nil, err
	}

	if len(vapp.VApp.Children.VM) != 1 {
		return nil, errUnexpectedNumberOfVMs
	}

	vm := govcd.NewVM(&client.Client)
	vm.VM.HREF = vapp.VApp.Children.VM[0].HREF
	err = vm.Refresh()
	if err != nil {
		return nil, err
	}

	return vm, nil
}

func (g *InstanceGroup) getVAppTemplate() (*govcd.VAppTemplate, error) {
	client, err := g.getVCDClient()
	if err != nil {
		return nil, err
	}

	org, err := client.GetOrgByName(g.Org)
	if err != nil {
		return nil, err
	}

	catalog, err := org.GetCatalogByName(g.Catalog, true)
	if err != nil {
		return nil, err
	}

	catalogItem, err := catalog.GetCatalogItemByName(g.Template, true)
	if err != nil {
		return nil, err
	}

	template, err := catalogItem.GetVAppTemplate()
	if err != nil {
		return nil, err
	}

	if template.VAppTemplate.Status != 8 {
		return nil, errTemplateNotReady
	}

	return &template, nil
}

func (g *InstanceGroup) getVMNetworkConnectionSection() (*types.NetworkConnectionSection, error) {
	netConn := &types.NetworkConnection{}
	netSection := &types.NetworkConnectionSection{}
	netSection.NetworkConnection = append(netSection.NetworkConnection, netConn)
	netConn = netSection.NetworkConnection[0]

	switch g.IPAllocationMode {
	case "DHCP":
		netConn.IPAddressAllocationMode = types.IPAllocationModeDHCP
	case "POOL":
		netConn.IPAddressAllocationMode = types.IPAllocationModePool
	default:
		return nil, fmt.Errorf("invalid IP allocation mode: %s", g.IPAllocationMode)
	}

	netConn.NetworkConnectionIndex = 0
	netConn.IsConnected = true
	netConn.NeedsCustomization = true
	netConn.Network = g.Network

	return netSection, nil
}

func (g *InstanceGroup) injectCredentials(vm *govcd.VM) error {
	if !g.settings.UseStaticCredentials {
		return fmt.Errorf("dynamic credentials are not supported yet")
	}

	if g.settings.Password != "" {
		vm.VM.GuestCustomizationSection.Enabled = boolPointer(true)
		vm.VM.GuestCustomizationSection.AdminPassword = g.settings.Password
		vm.VM.GuestCustomizationSection.AdminPasswordEnabled = boolPointer(true)
		vm.VM.GuestCustomizationSection.AdminPasswordAuto = boolPointer(false)
		vm.VM.GuestCustomizationSection.ResetPasswordRequired = boolPointer(false)
	} else if g.settings.Key != nil {
		priv, err := ssh.ParseRawPrivateKey(g.settings.Key)
		if err != nil {
			return fmt.Errorf("reading private key: %w", err)
		}
		var ok bool
		var key PrivPub
		key, ok = priv.(PrivPub)
		if !ok {
			return fmt.Errorf("key doesn't export PublicKey()")
		}

		sshPubKey, err := ssh.NewPublicKey(key.Public())
		if err != nil {
			return fmt.Errorf("generating ssh public key: %w", err)
		}

		var customizationScript string
		if strings.Contains(vm.VM.VmSpecSection.OsType, "windows") {
			customizationScript = windowsGuestCustomizationScript
		} else {
			customizationScript = linuxGuestCustomizationScript
		}

		templ := template.Must(template.New("script").Parse(customizationScript))
		var script bytes.Buffer
		err = templ.Execute(&script, map[string]string{
			"PublicKey": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPubKey))),
		})
		if err != nil {
			return err
		}

		vm.VM.GuestCustomizationSection.Enabled = boolPointer(true)
		vm.VM.GuestCustomizationSection.CustomizationScript = script.String()

	}
	_, err := vm.SetGuestCustomizationSection(vm.VM.GuestCustomizationSection)
	return err
}

func waitForVMCreation(client *govcd.VCDClient, vapp *govcd.VApp) (*govcd.VM, error) {
	vm := govcd.NewVM(&client.Client)
	vm.VM.HREF = vapp.VApp.Children.VM[0].HREF
	err := vm.Refresh()
	if err != nil {
		return nil, err
	}

	cWait := make(chan string, 1)
	go func() {
		for {
			status, _ := vm.GetStatus()
			if status == "POWERED_OFF" {
				break
			}
			time.Sleep(5 * time.Second)
		}

		for {
			vapp.Refresh()
			if err != nil {
				cWait <- "err"
				return
			}
			if vapp.VApp.Tasks == nil {
				time.Sleep(15 * time.Second) // let's give this old chap some time
				break

			}
			time.Sleep(5 * time.Second)
		}

		cWait <- "ok"
	}()

	select {
	case res := <-cWait:
		if res == "err" {
			return nil, fmt.Errorf("error while deploying VM")
		}
	case <-time.After(60 * time.Minute):
		return nil, fmt.Errorf("timeout while deploying VM")
	}

	if vm.VM.VmSpecSection == nil {
		return nil, fmt.Errorf("VM spec section not found")
	}

	err = vm.Refresh()
	if err != nil {
		return nil, err
	}

	return vm, nil
}

func waitForTasksCompletion(ctx context.Context, vapp *govcd.VApp, vm *govcd.VM) error {
	// Helper function to check if there are any running tasks
	hasRunningTasks := func(tasks *types.TasksInProgress) bool {
		if tasks == nil || len(tasks.Task) == 0 {
			return false
		}

		for _, task := range tasks.Task {
			// Check if task is not in a completed state
			// The execution status of the task. One of queued, preRunning, running, success, error, aborted
			if task.Status != "success" && task.Status != "error" && task.Status != "aborted" {
				return true
			}
		}
		return false
	}

	// Main loop
	for hasRunningTasks(vapp.VApp.Tasks) || hasRunningTasks(vm.VM.Tasks) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
			vapp.Refresh()
			vm.Refresh()
		}
	}

	return nil
}

func (g *InstanceGroup) getVCDClient() (*govcd.VCDClient, error) {
	if g.vcdClient != nil {
		g.log.Debug("using cached VCD client")
		_, err := g.vcdClient.GetOrgByName(g.Org)
		if err == nil {
			return g.vcdClient, nil
		}

		g.log.Debug("cached VCD client is invalid, creating new one")
	}

	client, err := newVCDClient(*g.parsedURL, g.Org, g.Token, false)
	if err != nil {
		return nil, err
	}

	g.vcdClient = client
	return client, nil
}

func newVCDClient(apiURL url.URL, org string, token string, insecure bool) (*govcd.VCDClient, error) {
	client := &govcd.VCDClient{
		Client: govcd.Client{
			VCDHREF:    apiURL,
			APIVersion: vcdAPIVersion,
			Http: http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{
						InsecureSkipVerify: insecure,
					},
					Proxy:               http.ProxyFromEnvironment,
					TLSHandshakeTimeout: 120 * time.Second,
				},
				Timeout: 600 * time.Second,
			},
			MaxRetryTimeout: 60,
		},
	}

	err := client.SetToken(org, govcd.ApiTokenHeader, token)
	if err != nil {
		return nil, fmt.Errorf("unable to authenticate to Org \"%s\": %s", org, err)
	}
	return client, nil
}

func vmRequiresTPM(vm *govcd.VM) bool {
	// FIXME(juan): We should have something more sophisticated here
	if strings.Contains(strings.ToLower(vm.VM.VmSpecSection.OsType), "windows11") {
		return true
	}

	return false
}

func changeVMTpm(client *govcd.VCDClient, vm *govcd.VM, tpmPresent bool) (*govcd.Task, error) {
	trustedPlatformModuleEdit := &trustedPlatformModuleEdit{
		Xmlns:      types.XMLNamespaceVCloud,
		TpmPresent: tpmPresent,
	}

	task, err := client.Client.ExecuteTaskRequest(
		vm.VM.HREF+"/action/editTrustedPlatformModule",
		http.MethodPost,
		"application/vnd.vmware.vcloud.TpmSection+xml",
		"error changing TPM for VM: %s",
		trustedPlatformModuleEdit,
	)
	if err != nil {
		return nil, errCouldNotExecuteTaskRequest
	}

	return &task, nil
}

// getPrimaryIPAddress gets the primary IP address of the VM. We have a hell of assumptions here, (one VM, one NIC, one IP address)
func getPrimaryIPAddress(vm *govcd.VM) (string, error) {
	if vm.VM.NetworkConnectionSection == nil {
		return "", fmt.Errorf("network connection section not found")
	}

	networks := vm.VM.NetworkConnectionSection.NetworkConnection
	for _, n := range networks {
		if n.IsConnected && n.IPAddress != "" {
			return n.IPAddress, nil
		}
	}

	return "", fmt.Errorf("no primary IP address found")
}
