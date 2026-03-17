package vcd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"text/template"
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

// isEntityNotFoundError returns true if the error indicates the VCD entity no longer exists.
func isEntityNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "could not be found")
}

const (
	deleteInstanceTimeout        = 60 * time.Minute
	refreshBackoffMaxInterval    = 1 * time.Minute
	refreshBackoffMaxElapsedTime = 20 * time.Minute

	sshReadinessTimeout       = 5 * time.Minute
	sshReadinessRetryInterval = 5 * time.Second
	sshHandshakeTimeout       = 10 * time.Second

	vcdAPIVersion = "38.1"
)

type trustedPlatformModuleEdit struct {
	XMLName    xml.Name `xml:"root:TrustedPlatformModule"`
	Xmlns      string   `xml:"xmlns:root,attr"`
	TpmPresent bool     `xml:"root:TpmPresent"`
}

// waitForSSHReadiness attempts a full SSH handshake to verify the VM is reachable
// and sshd is functioning. It uses the same credentials that the runner/taskscaler
// will use (from ConnectorConfig), so if this succeeds, the runner will be able to
// connect too.
func (g *InstanceGroup) waitForSSHReadiness(ipAddress string) error {
	port := g.settings.ProtocolPort
	if port == 0 {
		port = 22
	}
	addr := fmt.Sprintf("%s:%d", ipAddress, port)

	// Build SSH auth methods from ConnectorConfig (same creds the runner uses)
	var authMethods []ssh.AuthMethod
	if g.settings.Password != "" {
		authMethods = append(authMethods, ssh.Password(g.settings.Password))
	}
	if g.settings.Key != nil {
		signer, err := ssh.ParsePrivateKey(g.settings.Key)
		if err != nil {
			return fmt.Errorf("failed to parse SSH key for readiness check: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	if len(authMethods) == 0 {
		// No static credentials configured — skip the SSH check.
		// The runner will handle authentication itself.
		g.log.Warn("skipping SSH readiness check: no static credentials configured")
		return nil
	}

	// Determine username (same logic as ConnectInfo)
	username := "root"
	if strings.Contains(strings.ToLower(g.settings.OS), "windows") {
		username = "Administrator"
	}

	config := &ssh.ClientConfig{
		User:            username,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         sshHandshakeTimeout,
	}

	g.log.Info("waiting for SSH readiness", "addr", addr, "username", username)

	deadline := time.Now().Add(sshReadinessTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, sshHandshakeTimeout)
		if err != nil {
			lastErr = err
			g.log.Debug("SSH not reachable yet", "addr", addr, "error", err)
			time.Sleep(sshReadinessRetryInterval)
			continue
		}

		// TCP connected — attempt SSH handshake
		sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
		if err != nil {
			conn.Close()
			lastErr = err
			g.log.Debug("SSH handshake failed", "addr", addr, "error", err)
			time.Sleep(sshReadinessRetryInterval)
			continue
		}

		// Success — clean up and return
		client := ssh.NewClient(sshConn, chans, reqs)
		client.Close()
		g.log.Info("SSH readiness check passed", "addr", addr)
		return nil
	}

	return fmt.Errorf("SSH readiness check timed out after %s for %s: %w", sshReadinessTimeout, addr, lastErr)
}

// createResult holds the result of a successful instance creation.
type createResult struct {
	VAppHREF  string
	VAppName  string
	VMName    string
	IPAddress string
	OSType    string
}

func (g *InstanceGroup) createInstance() (result *createResult, err error) {
	var vAppName string
	var completed bool

	// Panic recovery + cleanup on failure
	defer func() {
		if r := recover(); r != nil {
			g.log.Error("Panic recovered in createInstance", "panic", r, "vapp_name", vAppName)
			err = fmt.Errorf("panic in createInstance: %v", r)
		}

		if !completed && vAppName != "" {
			g.log.Warn("createInstance did not complete successfully, cleaning up",
				"vapp_name", vAppName,
				"error", err,
			)
			go func() {
				defer func() {
					if rec := recover(); rec != nil {
						g.log.Error("panic in createInstance cleanup", "vapp_name", vAppName, "panic", rec)
					}
				}()
				if cleanupErr := g.cleanUpInstanceByName(vAppName); cleanupErr != nil {
					g.log.Error("createInstance cleanup failed", "vapp_name", vAppName, "error", cleanupErr)
				}
			}()
		}
	}()

	client, err := g.getVCDClient()
	if err != nil {
		return nil, err
	}

	org, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetOrgByName", func() (*govcd.Org, error) {
		return client.GetOrgByName(g.Org)
	})
	if err != nil {
		return nil, err
	}

	vdc, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetVDCByName", func() (*govcd.Vdc, error) {
		return org.GetVDCByName(g.VirtualDatacenter, true)
	})
	if err != nil {
		g.log.Error("vdc not found", "vdc", g.VirtualDatacenter)
		return nil, err
	}

	tmpl, err := g.getVAppTemplate()
	if err != nil {
		g.log.Error("error getting vApp template", "error", err)
		return nil, err
	}

	g.log.Info("vApp template found", "vapp_template", tmpl.VAppTemplate.Name)

	network, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetOrgVdcNetworkByName", func() (*govcd.OrgVDCNetwork, error) {
		return vdc.GetOrgVdcNetworkByName(g.Network, true)
	})
	if err != nil {
		return nil, err
	}

	storageProfile, err := g.getStorageProfile()
	if err != nil {
		g.log.Error("error getting storage profile", "error", err)
		return nil, err
	}

	vAppName, err = generateVMName(g.VAppNamePrefix)
	if err != nil {
		return nil, err
	}

	g.log.Info("Creating a new vApp", "vapp", vAppName, "template", tmpl.VAppTemplate.Name)
	networks := []*types.OrgVDCNetwork{}
	networks = append(networks, network.OrgVDCNetwork)

	description := fmt.Sprintf("VM created by the %s GitLab Fleeting runner", g.Name)

	// Step 1: Create empty vApp (fast — returns immediately with HREF)
	vapp, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "CreateRawVApp", func() (*govcd.VApp, error) {
		return vdc.CreateRawVApp(vAppName, description)
	})
	if err != nil {
		g.log.Error("error creating empty vApp", "error", err)
		return nil, err
	}

	g.log.Info("empty vApp created", "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name)

	// Step 2: Add metadata tag IMMEDIATELY (fast — makes vApp visible to reconciler)
	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "AddMetadataEntryWithVisibility", func() error {
		return vapp.AddMetadataEntryWithVisibility(
			instanceGroupMetadataKey,
			g.InstanceGroupName,
			types.MetadataStringValue,
			types.MetadataReadWriteVisibility,
			false, // isSystem
		)
	}); err != nil {
		g.log.Error("error adding metadata to vApp",
			"error", err,
			"vapp_href", vapp.VApp.HREF,
			"vapp", vapp.VApp.Name,
		)
		return nil, err
	}

	// Step 3: Add org network to vApp (fast — required before adding VM with network)
	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "AddRAWNetworkConfig", func() error {
		g.log.Info("adding network config to vApp", "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name, "networks", networks)
		networkTask, netErr := vapp.AddRAWNetworkConfig(networks)
		if netErr != nil {
			return netErr
		}
		return networkTask.WaitTaskCompletion()
	}); err != nil {
		g.log.Error("error adding network config to vApp",
			"error", err,
			"vapp_href", vapp.VApp.HREF,
			"vapp", vapp.VApp.Name,
		)
		return nil, err
	}

	// Step 4: Add VM from template (SLOW — this is the expensive operation)
	netSection, err := g.getVMNetworkConnectionSection()
	if err != nil {
		return nil, err
	}

	// TODO(juan): If we have configured a compute policy, we should use it instead of the default one.
	computePolicy, err := g.getDefaultComputePolicy()
	if err != nil {
		g.log.Warn("error getting default compute policy, using default one", "error", err)
		computePolicy = nil
	} else {
		g.log.Info("using compute policy", "compute_policy", computePolicy.Name)
	}

	addVMTask, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "AddNewVMWithStorageProfile", func() (govcd.Task, error) {
		g.log.Info("adding VM to vApp", "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name, "netSection", netSection, "storageProfile", storageProfile)
		return vapp.AddNewVMWithComputePolicy(
			vAppName,
			*tmpl,
			netSection,
			storageProfile,
			computePolicy,
			true,
		)
	})
	if err != nil {
		g.log.Error("error adding VM to vApp", "error", err,
			"vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name)
		return nil, err
	}

	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "WaitTaskCompletion(AddVM)", func() error {
		return addVMTask.WaitTaskCompletion()
	}); err != nil {
		g.log.Error("error waiting for AddNewVMWithStorageProfile", "error", err)
		return nil, err
	}

	// Step 5: Refresh vApp to get Children.VM[0]
	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh vApp after AddVM", func() error {
		return vapp.Refresh()
	}); err != nil {
		g.log.Error("error refreshing vApp after adding VM", "error", err,
			"vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name)
		return nil, err
	}

	if vapp.VApp.Children == nil || len(vapp.VApp.Children.VM) != 1 {
		vmCount := 0
		if vapp.VApp.Children != nil {
			vmCount = len(vapp.VApp.Children.VM)
		}
		g.log.Error("vapp has unexpected number of VMs after AddNewVMWithStorageProfile",
			"vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
			"expected", 1, "got", vmCount)
		return nil, errUnexpectedNumberOfVMs
	}

	g.log.Info("waiting for VM to be fully created",
		"vapp_href", vapp.VApp.HREF,
		"vapp", vapp.VApp.Name,
		"vm_href", vapp.VApp.Children.VM[0].HREF,
		"vm", vapp.VApp.Children.VM[0].Name,
	)

	vm, err := g.waitForVMCreation(client, vapp)
	if err != nil {
		return nil, err
	}

	g.log.Info("VM is created",
		"vm", vm.VM.Name, "vm_href", vm.VM.HREF,
		"vapp_href", vapp.VApp.HREF,
		"vapp", vapp.VApp.Name,
	)

	g.log.Info("injecting credentials", "vm", vm.VM.Name)

	err = g.injectCredentials(vm)
	if err != nil {
		return nil, err
	}

	if vmRequiresTPM(vm) {
		tpmTask, err := g.changeVMTpm(client, vm, true)
		if err != nil {
			return nil, err
		}
		if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "WaitTaskCompletion(TPM)", func() error {
			return tpmTask.WaitTaskCompletion()
		}); err != nil {
			return nil, err
		}
	}

	g.log.Debug("refreshing VM", "vm", vm.VM.Name)

	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh VM", func() error {
		return vm.Refresh()
	}); err != nil {
		return nil, err
	}

	g.log.Info(
		"changing CPU and core count", "vm", vm.VM.Name,
		"vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
		"cpu_count", g.CPUCount, "cores_per_socket", g.CoresPerSocket,
	)

	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "ChangeCPUAndCoreCount", func() error {
		return vm.ChangeCPUAndCoreCount(&g.CPUCount, &g.CoresPerSocket)
	}); err != nil {
		return nil, err
	}

	g.log.Info(
		"changing memory", "vm", vm.VM.Name, "vapp_href",
		vapp.VApp.HREF, "vapp", vapp.VApp.Name,
		"memory_mb", g.MemoryMB,
	)

	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "ChangeMemory", func() error {
		return vm.ChangeMemory(g.MemoryMB)
	}); err != nil {
		return nil, err
	}

	g.log.Info(
		"changing disk size", "vm", vm.VM.Name, "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
		"disk_size_gb", g.DiskSizeGB,
	)

	vm, err = g.changeDiskSize(vm)
	if err != nil {
		return nil, err
	}

	g.log.Info("disk size changed, refreshing VM",
		"vm", vm.VM.Name, "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
	)

	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh VM after disk", func() error {
		return vm.Refresh()
	}); err != nil {
		return nil, err
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
		return nil, err
	}

	g.log.Info("powering on VM",
		"vm", vm.VM.Name, "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
		"ip_address", ipAddress,
	)

	powerTask, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "PowerOn", func() (*govcd.Task, error) {
		t, err := vm.PowerOn()
		if err != nil {
			return nil, err
		}
		return &t, nil
	})
	if err != nil {
		g.log.Error("error powering on VM", "error", err,
			"vm", vm.VM.Name, "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
		)
		return nil, err
	}

	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "WaitTaskCompletion(PowerOn)", func() error {
		return powerTask.WaitTaskCompletion()
	}); err != nil {
		return nil, err
	}

	g.log.Debug("VM reported as powered on", "vm", vm.VM.Name)

	vmStatus, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetStatus", func() (string, error) {
		return vm.GetStatus()
	})
	if err != nil || vmStatus != "POWERED_ON" {
		return nil, fmt.Errorf("vm %s is not powered on: status=%s err=%v", vm.VM.Name, vmStatus, err)
	}

	// Verify the VM is actually reachable via SSH before declaring success.
	// This catches VMs that boot but have broken networking, sshd not starting, etc.
	if err := g.waitForSSHReadiness(ipAddress); err != nil {
		g.log.Error("SSH readiness check failed", "error", err,
			"vm", vm.VM.Name, "vapp_href", vapp.VApp.HREF, "ip_address", ipAddress,
		)
		return nil, fmt.Errorf("VM powered on but not reachable via SSH: %w", err)
	}

	g.log.Info("instance created successfully",
		"vapp_name", vapp.VApp.Name,
		"vapp_href", vapp.VApp.HREF,
		"vm_name", vm.VM.Name,
	)

	var osType string
	if vm.VM.VmSpecSection != nil {
		osType = vm.VM.VmSpecSection.OsType
	}

	// Mark creation as complete - this prevents the cleanup defer from running
	completed = true
	return &createResult{
		VAppHREF:  vapp.VApp.HREF,
		VAppName:  vapp.VApp.Name,
		VMName:    vm.VM.Name,
		IPAddress: ipAddress,
		OSType:    osType,
	}, nil
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

	org, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetOrgByName(poll)", func() (*govcd.Org, error) {
		return client.GetOrgByName(g.Org)
	})
	if err != nil {
		g.log.Error("error getting org (returning empty list)", "error", err)
		return []*govcd.VApp{}, nil
	}

	vdc, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetVDCByName(poll)", func() (*govcd.Vdc, error) {
		return org.GetVDCByName(g.VirtualDatacenter, true)
	})
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

	type searchResult struct {
		results     []govcd.QueryItem
		explanation string
	}
	sr, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "SearchByFilter", func() (searchResult, error) {
		results, explanation, err := client.Client.SearchByFilter(types.QtVapp, criteria)
		return searchResult{results, explanation}, err
	})
	if err != nil {
		g.log.Error("error searching for vapps (returning empty list to avoid taskscaler death)", "error", err)
		return []*govcd.VApp{}, nil
	}

	vApps = []*govcd.VApp{}
	for _, result := range sr.results {
		vApp, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetVAppByHref(poll)", func() (*govcd.VApp, error) {
			return vdc.GetVAppByHref(result.GetHref())
		})
		if err != nil {
			g.log.Warn("error getting vApp, skipping",
				"name", result.GetName(),
				"href", result.GetHref(), "error", err)
			continue
		}

		vApps = append(vApps, vApp)
	}

	return vApps, nil
}

func (g *InstanceGroup) cleanUpInstanceByName(name string) error {
	client, err := g.getVCDClient()
	if err != nil {
		return err
	}

	org, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetOrgByName(cleanup)", func() (*govcd.Org, error) {
		return client.GetOrgByName(g.Org)
	})
	if err != nil {
		return err
	}

	vdc, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetVDCByName(cleanup)", func() (*govcd.Vdc, error) {
		return org.GetVDCByName(g.VirtualDatacenter, true)
	})
	if err != nil {
		return err
	}

	vapp, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetVAppByName(cleanup)", func() (*govcd.VApp, error) {
		return vdc.GetVAppByName(name, true)
	})
	if err != nil {
		return err
	}

	return g.deleteInstance(vapp.VApp.HREF)
}

func (g *InstanceGroup) deleteInstance(href string) (err error) {
	g.log.Info("deleting instance", "href", href)

	client, err := g.getVCDClient()
	if err != nil {
		return err
	}

	var vapp *govcd.VApp
	refreshVappOp := func() error {
		vapp = govcd.NewVApp(&client.Client)
		vapp.VApp.HREF = href
		err := safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh vApp(delete)", func() error {
			return vapp.Refresh()
		})
		if err != nil {
			if isEntityNotFoundError(err) {
				g.log.Info("vApp no longer exists, treating as already deleted", "href", href)
				return nil
			}
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

	// If the vApp was not found (entity doesn't exist), it's already deleted
	if vapp.VApp.Name == "" {
		return nil
	}

	g.log.Debug("deleting vapp", "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name)

	// If no VMs in vApp (empty vApp from interrupted creation), skip VM handling
	if vapp.VApp.Children == nil || len(vapp.VApp.Children.VM) == 0 {
		g.log.Info("deleting empty vApp (no VMs)", "href", href, "vapp", vapp.VApp.Name)
		emptyDelTask, delErr := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "Delete(emptyVApp)", func() (govcd.Task, error) {
			return vapp.Delete()
		})
		if delErr != nil {
			g.log.Error("error deleting empty vApp", "href", href, "error", delErr)
			return delErr
		}
		return safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "WaitTaskCompletion(Delete emptyVApp)", func() error {
			return emptyDelTask.WaitTaskCompletion()
		})
	}

	if len(vapp.VApp.Children.VM) != 1 {
		g.log.Error("vapp has unexpected number of VMs on delete", "href", href, "vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name, "expected 1 VM, got", len(vapp.VApp.Children.VM))
		return errUnexpectedNumberOfVMs
	}

	vm := govcd.NewVM(&client.Client)
	vm.VM.HREF = vapp.VApp.Children.VM[0].HREF
	err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh VM for delete", func() error {
		return vm.Refresh()
	})
	if err != nil {
		if isEntityNotFoundError(err) {
			g.log.Info("VM no longer exists, skipping VM-level cleanup", "href", href, "vm_href", vm.VM.HREF)
		} else {
			g.log.Error("error refreshing vm for deletion, skipping VM-level cleanup",
				"vapp_href", vapp.VApp.HREF, "vapp", vapp.VApp.Name,
				"href", href, "vm_href", vm.VM.HREF, "error", err)
		}
		// Fall through to delete the vApp directly — skip PowerOff/Undeploy since we can't refresh the VM
		fallbackTask, delErr := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "Delete(fallback)", func() (govcd.Task, error) {
			return vapp.Delete()
		})
		if delErr != nil {
			return delErr
		}
		return safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "WaitTaskCompletion(Delete fallback)", func() error {
			return fallbackTask.WaitTaskCompletion()
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), deleteInstanceTimeout)
	defer cancel()

	err = g.waitForTasksCompletion(ctx, vapp, vm)
	if err != nil {
		return err
	}

	g.log.Debug("powering off vapp",
		"vapp_href", vapp.VApp.HREF,
		"vapp", vapp.VApp.Name,
		"statusStr", types.VAppStatuses[vapp.VApp.Status],
		"statusInt", vapp.VApp.Status,
	)

	powerOffTask, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "PowerOff", func() (govcd.Task, error) {
		return vapp.PowerOff()
	})
	if err != nil {
		g.log.Info("unable to power off as it's already powered off", "error", err, "vapp", vapp.VApp.Name)
	} else {
		if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "WaitTaskCompletion(PowerOff)", func() error {
			return powerOffTask.WaitTaskCompletion()
		}); err != nil {
			return err
		}
	}

	undeployTask, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "Undeploy", func() (govcd.Task, error) {
		return vapp.Undeploy()
	})
	if err != nil {
		g.log.Info("unable to undeploy VApp, probably because it is already off", "error", err, "vapp", vapp.VApp.Name)
	} else {
		if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "WaitTaskCompletion(Undeploy)", func() error {
			return undeployTask.WaitTaskCompletion()
		}); err != nil {
			return err
		}
	}

	deleteTask, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "Delete", func() (govcd.Task, error) {
		return vapp.Delete()
	})
	if err != nil {
		g.log.Error("error deleting vapp, will be retried by reconciler", "href", href, "error", err)
		return err
	}

	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "WaitTaskCompletion(Delete)", func() error {
		return deleteTask.WaitTaskCompletion()
	}); err != nil {
		return err
	}

	return nil
}

func (g *InstanceGroup) getStorageProfile() (*types.Reference, error) {
	client, err := g.getVCDClient()
	if err != nil {
		return nil, err
	}

	org, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetOrgByName(storage)", func() (*govcd.Org, error) {
		return client.GetOrgByName(g.Org)
	})
	if err != nil {
		return nil, err
	}

	vdc, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetVDCByName(storage)", func() (*govcd.Vdc, error) {
		return org.GetVDCByName(g.VirtualDatacenter, false)
	})
	if err != nil {
		return nil, err
	}

	storageProfile, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "FindStorageProfileReference", func() (types.Reference, error) {
		return vdc.FindStorageProfileReference(g.StorageProfile)
	})
	if err != nil {
		return nil, err
	}

	return &storageProfile, nil
}

func (g *InstanceGroup) renameVM(client *govcd.VCDClient, vm *govcd.VM, name string) (*govcd.VM, error) {
	vm.VM.GuestCustomizationSection.ComputerName = name
	if err := safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "SetGuestCustomizationSection(rename)", func() error {
		_, err := vm.SetGuestCustomizationSection(vm.VM.GuestCustomizationSection)
		return err
	}); err != nil {
		return nil, err
	}

	apiEndpoint, _ := url.ParseRequestURI(vm.VM.HREF + "/action/reconfigureVm")
	renameTask, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "ExecuteTaskRequest(rename)", func() (govcd.Task, error) {
		return client.Client.ExecuteTaskRequest(apiEndpoint.String(), http.MethodPost,
			types.MimeVM, "error modifying VM: %s", &types.Vm{
				Xmlns: types.XMLNamespaceVCloud,
				Ovf:   types.XMLNamespaceOVF,
				Name:  name,
			})
	})
	if err != nil {
		return nil, err
	}
	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "WaitTaskCompletion(rename)", func() error {
		return renameTask.WaitTaskCompletion()
	}); err != nil {
		return nil, err
	}

	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh VM(rename)", func() error {
		return vm.Refresh()
	}); err != nil {
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
	vm, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "UpdateVmSpecSection(disk)", func() (*govcd.VM, error) {
		return vm.UpdateVmSpecSection(vm.VM.VmSpecSection, "")
	})
	if err != nil {
		return nil, err
	}

	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh VM(disk)", func() error {
		return vm.Refresh()
	}); err != nil {
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
	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh vApp(getVM)", func() error {
		return vapp.Refresh()
	}); err != nil {
		return nil, err
	}

	if vapp.VApp.Children == nil || len(vapp.VApp.Children.VM) == 0 {
		return nil, fmt.Errorf("vApp %s has no VMs (creation may still be in progress)", vAppHREF)
	}
	if len(vapp.VApp.Children.VM) != 1 {
		return nil, errUnexpectedNumberOfVMs
	}

	vm := govcd.NewVM(&client.Client)
	vm.VM.HREF = vapp.VApp.Children.VM[0].HREF
	if err = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh VM(getVM)", func() error {
		return vm.Refresh()
	}); err != nil {
		return nil, err
	}

	return vm, nil
}

func (g *InstanceGroup) getVAppTemplate() (*govcd.VAppTemplate, error) {
	client, err := g.getVCDClient()
	if err != nil {
		return nil, err
	}

	org, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetOrgByName(template)", func() (*govcd.Org, error) {
		return client.GetOrgByName(g.Org)
	})
	if err != nil {
		return nil, err
	}

	catalog, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetCatalogByName", func() (*govcd.Catalog, error) {
		return org.GetCatalogByName(g.Catalog, true)
	})
	if err != nil {
		return nil, err
	}

	catalogItem, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetCatalogItemByName", func() (*govcd.CatalogItem, error) {
		return catalog.GetCatalogItemByName(g.Template, true)
	})
	if err != nil {
		return nil, err
	}

	tmpl, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetVAppTemplate", func() (govcd.VAppTemplate, error) {
		return catalogItem.GetVAppTemplate()
	})
	if err != nil {
		return nil, err
	}

	if tmpl.VAppTemplate.Status != 8 {
		return nil, errTemplateNotReady
	}

	return &tmpl, nil
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

func (g *InstanceGroup) getDefaultComputePolicy() (*types.VdcComputePolicy, error) {
	client, err := g.getVCDClient()
	if err != nil {
		return nil, err
	}

	org, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetOrgByName(computePolicy)", func() (*govcd.Org, error) {
		return client.GetOrgByName(g.Org)
	})
	if err != nil {
		return nil, err
	}

	vdc, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetVDCByName(computePolicy)", func() (*govcd.Vdc, error) {
		return org.GetVDCByName(g.VirtualDatacenter, true)
	})
	if err != nil {
		return nil, err
	}

	defaultComputePolicy := vdc.Vdc.DefaultComputePolicy
	if defaultComputePolicy == nil {
		return nil, fmt.Errorf("default compute policy not found")
	}

	computePolicy, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetVdcComputePolicyById(computePolicy)", func() (*govcd.VdcComputePolicy, error) {
		return org.GetVdcComputePolicyById(defaultComputePolicy.ID)
	})
	if err != nil {
		return nil, fmt.Errorf("error getting compute policy: %w", err)
	}

	return computePolicy.VdcComputePolicy, nil
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
	}

	if g.settings.Key != nil {
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
	return safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "SetGuestCustomizationSection", func() error {
		_, err := vm.SetGuestCustomizationSection(vm.VM.GuestCustomizationSection)
		return err
	})
}

func (g *InstanceGroup) waitForVMCreation(client *govcd.VCDClient, vapp *govcd.VApp) (*govcd.VM, error) {
	vm := govcd.NewVM(&client.Client)
	vm.VM.HREF = vapp.VApp.Children.VM[0].HREF
	if err := safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh VM(waitCreate)", func() error {
		return vm.Refresh()
	}); err != nil {
		return nil, err
	}

	cWait := make(chan string, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				g.log.Error("panic in waitForVMCreation goroutine", "panic", r)
				cWait <- "err"
			}
		}()

		for {
			status, _ := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetStatus(waitCreate)", func() (string, error) {
				return vm.GetStatus()
			})
			if status == "POWERED_OFF" {
				break
			}
			time.Sleep(5 * time.Second)
		}

		for {
			err := safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh vApp(waitCreate)", func() error {
				return vapp.Refresh()
			})
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

	if err := safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh VM(waitCreate final)", func() error {
		return vm.Refresh()
	}); err != nil {
		return nil, err
	}

	return vm, nil
}

func (g *InstanceGroup) waitForTasksCompletion(ctx context.Context, vapp *govcd.VApp, vm *govcd.VM) error {
	// Helper function to check if there are any running tasks
	hasRunningTasks := func(tasks *types.TasksInProgress) bool {
		if tasks == nil || len(tasks.Task) == 0 {
			return false
		}

		for _, task := range tasks.Task {
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
			_ = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh vApp(waitTasks)", func() error {
				return vapp.Refresh()
			})
			_ = safeVCDCallVoid(context.Background(), g.log, g.InstanceGroupName, "Refresh VM(waitTasks)", func() error {
				return vm.Refresh()
			})
		}
	}

	return nil
}

func (g *InstanceGroup) getVCDClient() (*govcd.VCDClient, error) {
	if g.vcdClient != nil {
		g.log.Debug("using cached VCD client")
		_, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "GetOrgByName(cache check)", func() (*govcd.Org, error) {
			return g.vcdClient.GetOrgByName(g.Org)
		})
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

	if err := func() (retErr error) {
		defer func() {
			if r := recover(); r != nil {
				retErr = fmt.Errorf("panic in SetToken: %v", r)
			}
		}()
		return client.SetToken(org, govcd.ApiTokenHeader, token)
	}(); err != nil {
		return nil, fmt.Errorf("unable to authenticate to Org \"%s\": %s", org, err)
	}
	return client, nil
}

func vmRequiresTPM(vm *govcd.VM) bool {
	if strings.Contains(strings.ToLower(vm.VM.VmSpecSection.OsType), "windows11") {
		return true
	}

	return false
}

func (g *InstanceGroup) changeVMTpm(client *govcd.VCDClient, vm *govcd.VM, tpmPresent bool) (*govcd.Task, error) {
	trustedPlatformModuleEdit := &trustedPlatformModuleEdit{
		Xmlns:      types.XMLNamespaceVCloud,
		TpmPresent: tpmPresent,
	}

	task, err := safeVCDCall(context.Background(), g.log, g.InstanceGroupName, "ExecuteTaskRequest(TPM)", func() (govcd.Task, error) {
		return client.Client.ExecuteTaskRequest(
			vm.VM.HREF+"/action/editTrustedPlatformModule",
			http.MethodPost,
			"application/vnd.vmware.vcloud.TpmSection+xml",
			"error changing TPM for VM: %s",
			trustedPlatformModuleEdit,
		)
	})
	if err != nil {
		return nil, errCouldNotExecuteTaskRequest
	}

	return &task, nil
}

// getPrimaryIPAddress gets the primary IP address of the VM.
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
