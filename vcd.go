package vcd

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vmware/go-vcloud-director/v2/govcd"
	"github.com/vmware/go-vcloud-director/v2/types/v56"
	"golang.org/x/crypto/ssh"
)

func (g *InstanceGroup) getOrCreateVApp() (*govcd.VApp, error) {
	vapp, err := g.getVApp()
	if err != nil {
		vapp, err = g.createVApp()
		if err != nil {
			return nil, err
		}
	}
	return vapp, nil
}

func (g *InstanceGroup) getVApp() (*govcd.VApp, error) {
	client, err := newClient(*g.parsedURL, g.Org, g.Token, false)
	if err != nil {
		return nil, err
	}

	if g.vAppHREF != "" { // this is way quicker
		vapp := govcd.NewVApp(&client.Client)
		vapp.VApp.HREF = g.vAppHREF
		err = vapp.Refresh()
		if err != nil {
			return nil, err
		}
		return vapp, nil
	}

	org, err := client.GetOrgByName(g.Org)
	if err != nil {
		return nil, err
	}
	vdc, err := org.GetVDCByName(g.VirtualDatacenter, false)
	if err != nil {
		return nil, err
	}
	vapp, err := vdc.GetVAppByName(g.VApp, true)
	if err != nil {
		return nil, err
	}

	return vapp, nil
}

func (g *InstanceGroup) createVApp() (*govcd.VApp, error) {
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

	vapp, err := vdc.CreateRawVApp(g.VApp, "vApp createed for GitLab fleeting")
	if err != nil {
		return nil, err
	}

	network, err := vdc.GetOrgVdcNetworkByName(g.Network, true)
	if err != nil {
		return nil, err
	}

	_, err = vapp.AddOrgNetwork(&govcd.VappNetworkSettings{}, network.OrgVDCNetwork, false)
	if err != nil {
		return nil, err
	}

	return vapp, nil
}

func (g *InstanceGroup) getVM(href string) (*govcd.VM, error) {
	client, err := newClient(*g.parsedURL, g.Org, g.Token, false)
	if err != nil {
		return nil, err
	}

	vm := govcd.NewVM(&client.Client)
	vm.VM.HREF = href
	err = vm.Refresh()
	if err != nil {
		return nil, err
	}

	return vm, nil
}

func (g *InstanceGroup) deleteVM(href string) error {
	client, err := newClient(*g.parsedURL, g.Org, g.Token, false)
	if err != nil {
		return err
	}

	vm := govcd.NewVM(&client.Client)
	vm.VM.HREF = href
	task, err := vm.Undeploy() // we don't care about a hard power-off
	if err != nil {
		return err
	}

	err = task.WaitTaskCompletion()
	if err != nil {
		return err
	}

	err = vm.Delete()
	if err != nil {
		return err
	}

	return nil
}

func (g *InstanceGroup) addVMToVApp() (*govcd.VM, error) {
	// client, err := newClient(*g.parsedURL, g.Org, g.Token, false)
	// if err != nil {
	// 	return nil, err
	// }

	vapp, err := g.getVApp()
	if err != nil {
		return nil, err
	}

	vmName, err := generateVMName(g.VMNamePrefix)
	if err != nil {
		return nil, err
	}

	template, err := g.getVAppTemplate()
	if err != nil {
		return nil, err
	}

	netSection, err := g.getVMNetworkConnectionSection()
	if err != nil {
		return nil, err
	}

	// TODO(juanfont): fully use vapp.AddNewVMWithComputePolicy with storage and compute policies
	task, err := vapp.AddNewVMWithComputePolicy(
		vmName,
		template,
		netSection, // network
		nil,        // storage
		nil,        // compute policy
		true,
	)
	if err != nil {
		return nil, err
	}

	if err = task.WaitTaskCompletion(); err != nil {
		return nil, err
	}

	vm, err := vapp.GetVMByName(vmName, true)
	if err != nil {
		return nil, err
	}

	err = g.injectCredentials(vm)
	if err != nil {
		return nil, err
	}

	task, err = vapp.PowerOn()
	if err != nil {
		return nil, err
	}
	if err = task.WaitTaskCompletion(); err != nil {
		return nil, err
	}

	return vm, err
}

func (g *InstanceGroup) getVAppTemplate() (govcd.VAppTemplate, error) {
	client, err := newClient(*g.parsedURL, g.Org, g.Token, false)
	if err != nil {
		return govcd.VAppTemplate{}, err
	}

	org, err := client.GetOrgByName(g.Org)
	if err != nil {
		return govcd.VAppTemplate{}, err
	}

	catalog, err := org.GetCatalogByName(g.Catalog, true)
	if err != nil {
		return govcd.VAppTemplate{}, err
	}

	catalogItem, err := catalog.GetCatalogItemByName(g.Template, true)
	if err != nil {
		return govcd.VAppTemplate{}, err
	}

	return catalogItem.GetVAppTemplate()
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
			"PublicKey": string(ssh.MarshalAuthorizedKey(sshPubKey)),
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

func newClient(apiURL url.URL, org string, token string, insecure bool) (*govcd.VCDClient, error) {
	client := &govcd.VCDClient{
		Client: govcd.Client{
			VCDHREF:    apiURL,
			APIVersion: "36.3",
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
