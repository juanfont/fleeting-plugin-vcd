# fleeting-plugin-vcd

A GitLab [Fleeting](https://docs.gitlab.com/runner/fleet_scaling/fleeting.html) plugin for [VMware Cloud Director](https://www.vmware.com/products/cloud-infrastructure/cloud-director).

## Overview

This plugin enables GitLab Runner to dynamically provision and manage virtual machines in VMware Cloud Director for CI/CD job execution. It's part of GitLab's Fleeting ecosystem, which replaces Docker Machine for autoscaling runners.

Fleeting is an abstraction layer for cloud providers' instance groups, allowing for the provisioning of multiple identical instances with a minimal API focused on creation, connection, and deletion.

## Features

- Dynamic provisioning of VMs in VMware Cloud Director
- Support for both Linux and Windows VMs
- SSH key and password-based authentication
- Customizable VM resources (CPU, memory, disk size)
- Automatic garbage collection of stuck/failed instances
- Debug HTTP server for monitoring instance states

## Requirements

- VMware Cloud Director 10.4+ (for API token authentication)
- GitLab Runner with Fleeting support
- VM template with VMware Tools installed

## Installation

Download the latest release from [GitHub Releases](https://github.com/juanfont/fleeting-plugin-vcd/releases) and place the binary in a location accessible by GitLab Runner.

## Assumptions

- The vApp template must have a single VM
- The OS template must have VMware Tools (or open-vm-tools for Linux) installed
- If using the Docker Autoscaler executor, the Docker daemon must be installed and running on the VM
- For Windows machines, the OpenSSH service must be enabled (WinRM is not supported)

## Configuration

The plugin is configured via the GitLab Runner's `config.toml` file under `[runners.autoscaler.plugin_config]`.

### Required Configuration

| Parameter | Description |
|-----------|-------------|
| `name` | Unique name for this runner instance |
| `url` | VMware Cloud Director API URL |
| `org` | VCD Organization name |
| `token` | API token for authentication (VCD 10.4+) |
| `virtual_datacenter` | Virtual Datacenter name |
| `network` | Network to attach VMs to |
| `ip_allocation_mode` | IP allocation mode: `POOL` or `DHCP` |
| `instance_group_name` | Metadata tag to identify VMs belonging to this group |
| `vapp_name_prefix` | Prefix for vApp names |
| `catalog` | VCD Catalog containing the VM template |
| `template` | Template name within the catalog |
| `storage_profile` | Storage profile for VM disks |
| `cpu_count` | Number of vCPUs |
| `cores_per_socket` | CPU cores per socket |
| `memory_mb` | Memory in MB |

### Optional Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `disk_size_gb` | Disk size in GB (0 = use template default) | `0` |
| `debug_server_addr` | Debug HTTP server address (e.g., `127.0.0.1:27060`) | disabled |

### Example Configuration

See [`config.example.toml`](config.example.toml) for a complete example configuration.

```toml
[[runners]]
executor = "docker-autoscaler"

[runners.autoscaler]
plugin = "fleeting-plugin-vcd"
capacity_per_instance = 1
max_instances = 5

[runners.autoscaler.plugin_config]
name = "my-runner"
url = "https://vcd.example.com/api"
org = "MyOrg"
token = "your-api-token"
virtual_datacenter = "MyVDC"
network = "MyNetwork"
ip_allocation_mode = "POOL"
instance_group_name = "gitlab-runners"
vapp_name_prefix = "gitlab-runner"
catalog = "MyCatalog"
template = "Ubuntu-22.04"
storage_profile = "MyStorageProfile"
cpu_count = 4
cores_per_socket = 2
memory_mb = 8192
disk_size_gb = 100

# Optional: Enable debug server
debug_server_addr = "127.0.0.1:27060"

[runners.autoscaler.connector_config]
use_static_credentials = true
username = "root"
password = "your-password"
```

## Debug Server

When `debug_server_addr` is configured, the plugin exposes an HTTP server showing the current state of all instances. Access it at `http://[address]/` to see a table with instance states, creation times, and lifecycle events.

## Building from Source

```bash
# Using goreleaser
goreleaser build --single-target --snapshot --clean

# Or using make
make build
```

## Running Integration Tests

Integration tests require environment variables (not used by the plugin itself, only for tests):

```bash
export VCD_URL="https://vcd.example.com/api"
export VCD_ORG="MyOrg"
export VCD_TOKEN="your-api-token"
export VCD_VDC="MyVDC"
export VCD_NETWORK="MyNetwork"
export VCD_NETWORK_ALLOCATION_MODE="POOL"
export VCD_CATALOG="MyCatalog"
export VCD_TEMPLATE="Ubuntu-22.04"
export VCD_VAPP_NAME_PREFIX="test-runner"
export VCD_STORAGE_PROFILE="MyStorageProfile"
export VCD_CPU_COUNT="2"
export VCD_CORES_PER_SOCKET="2"
export VCD_MEMORY_MB="4096"
export VCD_DISK_SIZE_GB="50"

make test
```

## History

This plugin is based on:

- My previous [Docker Machine driver for VCD](https://github.com/juanfont/docker-machine-driver-vcd)
- The [Hetzner](https://gitlab.com/hetznercloud/fleeting-plugin-hetzner) plugin for Fleeting
- Joe Burnett's [Fleeting explanation on YouTube](https://www.youtube.com/watch?v=niZ508K4dts)

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

See [LICENSE](LICENSE) file.
