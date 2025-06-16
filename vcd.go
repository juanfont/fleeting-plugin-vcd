package vcd

import (
	"bytes"
	"context"
	"crypto/tls"
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
	errTemplateNotReady      = errors.New("template not ready (status != 8)")
	errUnexpectedNumberOfVMs = errors.New("unexpected number of VMs in template")
	errDiskSectionNotFound   = errors.New("disk section not found")
)

const (
	deleteInstanceTimeout        = 20 * time.Minute
	refreshBackoffMaxInterval    = 1 * time.Minute
	refreshBackoffMaxElapsedTime = 10 * time.Minute

	vcdAPIVersion = "38.1"
)

func (g *InstanceGroup) createInstance() (*govcd.VApp, *govcd.VM, error) {
	client, err := newClient(*g.parsedURL, g.Org, g.Token, false)
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

	vAppName, err := generateVMName(g.VAppNamePrefix)
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

	vapp, err := vdc.GetVAppByName(vAppName, true)
	if err != nil {
		g.log.Error("error getting vapp", "error", err)
		return nil, nil, err
	}

	if len(vapp.VApp.Children.VM) != 1 {
		g.log.Error("expected 1 VM, got %d", len(vapp.VApp.Children.VM))
		return nil, nil, errUnexpectedNumberOfVMs
	}

	g.log.Debug("waiting for VM to be fully created",
		"vapp_href", vapp.VApp.HREF,
		"vapp", vapp.VApp.Name,
		"vm_href", vapp.VApp.Children.VM[0].HREF,
		"vm", vapp.VApp.Children.VM[0].Name,
	)

	vm, err := waitForVMCreation(client, vapp)
	if err != nil {
		return nil, nil, err
	}

	g.log.Debug("VM is ready", "vm", vm.VM.Name)

	err = vapp.AddMetadataEntry(types.MetadataStringValue, instanceGroupMetadataKey, g.InstanceGroupName)
	if err != nil {
		return nil, nil, err
	}

	g.log.Debug("injecting credentials", "vm", vm.VM.Name)

	err = g.injectCredentials(vm)
	if err != nil {
		return nil, nil, err
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

	err = vm.ChangeCPUAndCoreCount(&g.CPUCount, &g.CoresPerSocket)
	if err != nil {
		return nil, nil, err
	}

	err = vm.ChangeMemory(g.MemoryMB)
	if err != nil {
		return nil, nil, err
	}

	vm, err = g.changeDiskSize(vm)
	if err != nil {
		return nil, nil, err
	}

	g.log.Debug("powering on vm", "vm", vm.VM.Name)

	task, err = vm.PowerOn()
	if err != nil {
		return nil, nil, err
	}
	if err = task.WaitTaskCompletion(); err != nil {
		return nil, nil, err
	}

	g.log.Debug("VM reported as powered on", "vm", vm.VM.Name)

	if status, err := vm.GetStatus(); err != nil || status != "POWERED_ON" {
		panic(fmt.Sprintf("vm %s is not powered on: %s", vm.VM.Name, err))
	}

	g.log.Debug("VM is powered on", "vm", vm.VM.Name)

	return vapp, vm, nil
}

func (g *InstanceGroup) getInstancesInInstanceGroup() ([]*govcd.VApp, error) {
	client, err := newClient(*g.parsedURL, g.Org, g.Token, false)
	if err != nil {
		return nil, fmt.Errorf("error creating client: %w", err)
	}

	org, err := client.GetOrgByName(g.Org)
	if err != nil {
		return nil, fmt.Errorf("error getting org: %w", err)
	}

	vdc, err := org.GetVDCByName(g.VirtualDatacenter, true)
	if err != nil {
		return nil, fmt.Errorf("error getting VDC: %w", err)
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
		g.log.Error("error searching for vapps", "error", err)
		return nil, err
	}

	vApps := []*govcd.VApp{}
	for _, result := range results {
		vApp, err := vdc.GetVAppByHref(result.GetHref())
		if err != nil {
			g.log.Warn("error getting vApp",
				"name", result.GetName(),
				"href", result.GetHref(), "error", err)
			continue
		}

		vApps = append(vApps, vApp)
	}

	return vApps, nil
}

// deleteInstance deletes a vApp and its VM. Because it can
func (g *InstanceGroup) deleteInstance(href string) error {
	client, err := newClient(*g.parsedURL, g.Org, g.Token, false)
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
		return err
	}

	g.log.Debug("deleting vapp", "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name)

	if vapp.VApp.Children == nil || len(vapp.VApp.Children.VM) != 1 {
		return errUnexpectedNumberOfVMs
	}

	vm := govcd.NewVM(&client.Client)
	vm.VM.HREF = vapp.VApp.Children.VM[0].HREF
	err = vm.Refresh()
	if err != nil {
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
		return err
	}

	return task.WaitTaskCompletion()
}

func (g *InstanceGroup) getStorageProfile() (*types.Reference, error) {
	client, err := newClient(*g.parsedURL, g.Org, g.Token, false)
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
	client, err := newClient(*g.parsedURL, g.Org, g.Token, false)
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
	client, err := newClient(*g.parsedURL, g.Org, g.Token, false)
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

func newClient(apiURL url.URL, org string, token string, insecure bool) (*govcd.VCDClient, error) {
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
