package vcd

import (
	"errors"
	"fmt"
	"net/url"

	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

func (g *InstanceGroup) validate() error {
	errs := []error{}

	// Defaults
	if g.settings.Protocol == "" {
		g.settings.Protocol = provider.ProtocolSSH
	}

	// Checks
	if g.Name == "" {
		errs = append(errs, fmt.Errorf("missing required plugin config: name"))
	}

	if g.StrURL == "" {
		errs = append(errs, fmt.Errorf("missing required plugin config: url"))
	}

	_, err := url.Parse(g.StrURL)
	if err != nil {
		errs = append(errs, fmt.Errorf("invalid url: %s", err))
	}

	if g.Org == "" {
		errs = append(errs, fmt.Errorf("missing required plugin config: org"))
	}

	if g.Token == "" {
		errs = append(errs, fmt.Errorf("missing required plugin config: token"))
	}

	if g.VirtualDatacenter == "" {
		errs = append(errs, fmt.Errorf("missing required plugin config: virtual_datacenter"))
	}

	if g.Network == "" {
		errs = append(errs, fmt.Errorf("missing required plugin config: network"))
	}

	if g.IPAllocationMode == "" {
		errs = append(errs, fmt.Errorf("missing required plugin config: ip_allocation_mode"))
	}

	if g.IPAllocationMode != "DHCP" && g.IPAllocationMode != "POOL" {
		errs = append(errs, fmt.Errorf("invalid ip_allocation_mode: %s", g.IPAllocationMode))
	}

	if g.InstanceGroupName == "" {
		errs = append(errs, fmt.Errorf("missing required plugin config: instance_group_name"))
	}

	if g.VAppNamePrefix == "" {
		errs = append(errs, fmt.Errorf("missing required plugin config: vapp_name_prefix"))
	}

	if g.Catalog == "" {
		errs = append(errs, fmt.Errorf("missing required plugin config: catalog"))
	}

	if g.Template == "" {
		errs = append(errs, fmt.Errorf("missing required plugin config: template"))
	}

	if g.StorageProfile == "" {
		errs = append(errs, fmt.Errorf("missing required plugin config: storage_profile"))
	}

	if g.CPUCount == 0 {
		errs = append(errs, fmt.Errorf("missing required plugin config: cpu_count"))
	}

	if g.CoresPerSocket == 0 {
		errs = append(errs, fmt.Errorf("missing required plugin config: cores_per_socket"))
	}

	if g.MemoryMB == 0 {
		errs = append(errs, fmt.Errorf("missing required plugin config: memory_mb"))
	}

	// Disk size is optional
	// if g.DiskSizeGB == 0 {
	// 	errs = append(errs, fmt.Errorf("missing required plugin config: disk_size_gb"))
	// }

	if g.settings.UseStaticCredentials {
		if g.settings.Password == "" && g.settings.Key == nil {
			// we don't check Username because with vcd/vmware-tools we have to use either root or Administrator
			return fmt.Errorf("either root/password password or ssh key are required when using static credentials")
		}
	}

	return errors.Join(errs...)
}

func (g *InstanceGroup) populate() error {
	parsedURL, err := url.Parse(g.StrURL)
	if err != nil {
		return fmt.Errorf("invalid url: %s", err)
	}

	g.parsedURL = parsedURL

	if g.MaxConcurrentCreates <= 0 {
		g.MaxConcurrentCreates = defaultMaxConcurrentCreates
	}
	if g.MaxConcurrentDeletes <= 0 {
		g.MaxConcurrentDeletes = defaultMaxConcurrentDeletes
	}

	return nil
}
