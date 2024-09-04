# fleeting-plugin-vcd

A GitLab [Fleeting](https://docs.gitlab.com/runner/fleet_scaling/fleeting.html) plugin for [VMware Cloud Director](https://www.vmware.com/products/cloud-infrastructure/cloud-director).

## Overview

This plugin enables GitLab Runner to dynamically provision and manage virtual machines in VMware Cloud Director for CI/CD job execution. It's part of GitLab's Fleeting ecosystem, which replaces Docker Machine for autoscaling runners.

Fleeting is an abstraction layer for cloud providers' instance groups, allowing for the provisioning of multiple identical instances with a minimal API focused on creation, connection, and deletion.

## Features

- Dynamic provisioning of VMs in VMware Cloud Director
- Support for both Linux and Windows VMs
- SSH key and password-based authentication
- Customizable VM templates and network settings

## Requirements

- Go 1.x (for building)
- VMware Cloud Director environment
- GitLab Runner with Fleeting support

## Assumptions

- The vApp template must have a single VM
- The OS template must have VMware Tools (or open-vm-tools for Linux) installed
- For Windows machines, the OpenSSH service must be enabled (WinRM is not supported)

## Building the plugin

To build the plugin:

```
make build
```

This uses `goreleaser` to create the binary.

## Configuration

The plugin requires several environment variables to be set:

- `VCD_URL`: VMware Cloud Director API URL
- `VCD_ORG`: Organization name
- `VCD_VDC`: Virtual Data Center name
- `VCD_NETWORK`: Network name
- `VCD_NETWORK_ALLOCATION_MODE`: IP allocation mode (DHCP or POOL)
- `VCD_TOKEN`: API token (VCD 10.4+ required)
- `VCD_CATALOG`: Catalog name containing the VM template
- `VCD_TEMPLATE`: VM template name
- `VCD_VAPP_NAME_PREFIX`: Prefix for created vApps
- `VCD_VM_NAME_PREFIX`: Prefix for created VMs
- `VCD_STORAGE_PROFILE`: (Optional) Storage profile name

## Running Integration Tests

To run the integration tests:

```
make test
```

Note: Ensure all required environment variables are set before running the tests.

## Usage

This plugin is designed to be used with GitLab Runner's Fleeting executor. Refer to the [GitLab Runner Fleeting documentation](https://docs.gitlab.com/runner/executors/fleeting.html) for setup and configuration instructions.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## History

This plugin is based on:
- My previous [Docker Machine driver for VCD](https://github.com/juanfont/docker-machine-driver-vcd)
- The [Hetzner](https://gitlab.com/hetznercloud/fleeting-plugin-hetzner) plugin for Fleeting
- Joe Burnett's [Fleeting explanation on YouTube](https://www.youtube.com/watch?v=niZ508K4dts)

