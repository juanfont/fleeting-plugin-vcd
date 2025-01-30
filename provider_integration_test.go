package vcd

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"
	"gitlab.com/gitlab-org/fleeting/fleeting/integration"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
	"golang.org/x/crypto/ssh"
)

func TestBasicCloudDirector(t *testing.T) {
	if os.Getenv("VCD_URL") == "" {
		t.Error("mandatory environment variable VCD_URL not set")
	}

	if os.Getenv("VCD_ORG") == "" {
		t.Error("mandatory environment variable VCD_ORG not set")
	}

	if os.Getenv("VCD_VDC") == "" {
		t.Error("mandatory environment variable VCD_VDC not set")
	}

	if os.Getenv("VCD_NETWORK") == "" {
		t.Error("mandatory environment variable VCD_NETWORK not set")
	}

	if os.Getenv("VCD_NETWORK_ALLOCATION_MODE") == "" {
		t.Error("mandatory environment variable VCD_NETWORK_ALLOCATION_MODE not set")
	}

	if os.Getenv("VCD_TOKEN") == "" {
		t.Error("mandatory environment variable VCD_TOKEN not set")
	}

	if os.Getenv("VCD_CATALOG") == "" {
		t.Error("mandatory environment variable VCD_CATALOG not set")
	}

	if os.Getenv("VCD_TEMPLATE") == "" {
		t.Error("mandatory environment variable VCD_TEMPLATE not set")
	}

	if os.Getenv("VCD_VAPP_NAME_PREFIX") == "" {
		t.Error("mandatory environment variable VCD_VAPP not set")
	}

	t.Run("create_instance_group", func(t *testing.T) {
		t.Parallel()

		instanceGroupName := "test-instance-group-" + strconv.FormatInt(time.Now().Unix(), 10)

		ig := &InstanceGroup{
			Name:              instanceGroupName,
			StrURL:            os.Getenv("VCD_URL"),
			Org:               os.Getenv("VCD_ORG"),
			Token:             os.Getenv("VCD_TOKEN"),
			VirtualDatacenter: os.Getenv("VCD_VDC"),
			Network:           os.Getenv("VCD_NETWORK"),
			IPAllocationMode:  os.Getenv("VCD_NETWORK_ALLOCATION_MODE"),
			InstanceGroupName: instanceGroupName,
			VAppNamePrefix:    os.Getenv("VCD_VAPP_NAME_PREFIX"),
			Catalog:           os.Getenv("VCD_CATALOG"),
			Template:          os.Getenv("VCD_TEMPLATE"),
			StorageProfile:    os.Getenv("VCD_STORAGE_PROFILE"),
			CPUCount:          mustAtoi(os.Getenv("VCD_CPU_COUNT")),
			CoresPerSocket:    mustAtoi(os.Getenv("VCD_CORES_PER_SOCKET")),
			MemoryMB:          int64(mustAtoi(os.Getenv("VCD_MEMORY_MB"))),
			DiskSizeGB:        mustAtoi(os.Getenv("VCD_DISK_SIZE_GB")),
			log: hclog.New(&hclog.LoggerOptions{
				Name:   "test",
				Level:  hclog.Debug,
				Output: os.Stdout,
			}),

			settings: provider.Settings{
				ConnectorConfig: provider.ConnectorConfig{
					UseStaticCredentials: true,
					Password:             "ExcellentPassword123!",
				},
			},
		}

		err := ig.populate()
		require.NoError(t, err)

		err = ig.validate()
		require.NoError(t, err)

		// Create a new VM
		vm, err := ig.createVM()
		require.NoError(t, err)
		require.NotNil(t, vm)

		// Delete the VM
		err = vm.Refresh()
		require.NoError(t, err)

		vapp, err := vm.GetParentVApp()
		require.NoError(t, err)

		err = ig.deleteVApp(vapp.VApp.HREF)
		require.NoError(t, err)
	})
}

func TestProvisioning(t *testing.T) {
	if os.Getenv("VCD_URL") == "" {
		t.Error("mandatory environment variable VCD_URL not set")
	}

	if os.Getenv("VCD_ORG") == "" {
		t.Error("mandatory environment variable VCD_ORG not set")
	}

	if os.Getenv("VCD_VDC") == "" {
		t.Error("mandatory environment variable VCD_VDC not set")
	}

	if os.Getenv("VCD_NETWORK") == "" {
		t.Error("mandatory environment variable VCD_NETWORK not set")
	}

	if os.Getenv("VCD_NETWORK_ALLOCATION_MODE") == "" {
		t.Error("mandatory environment variable VCD_NETWORK_ALLOCATION_MODE not set")
	}

	if os.Getenv("VCD_TOKEN") == "" {
		t.Error("mandatory environment variable VCD_TOKEN not set")
	}

	if os.Getenv("VCD_CATALOG") == "" {
		t.Error("mandatory environment variable VCD_CATALOG not set")
	}

	if os.Getenv("VCD_TEMPLATE") == "" {
		t.Error("mandatory environment variable VCD_TEMPLATE not set")
	}

	if os.Getenv("VCD_VAPP_NAME_PREFIX") == "" {
		t.Error("mandatory environment variable VCD_VAPP not set")
	}

	pluginBinary := integration.BuildPluginBinary(t, "cmd/fleeting-plugin-vcd", "fleeting-plugin-vcd")

	t.Run("static_credentials_ssh_key", func(t *testing.T) {
		t.Parallel()
		var err error
		_, privateKey, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)

		pemBlock, err := ssh.MarshalPrivateKey(crypto.PrivateKey(privateKey), "")
		require.NoError(t, err)

		privateKeyPem := pem.EncodeToMemory(pemBlock)

		integration.TestProvisioning(t,
			pluginBinary,
			integration.Config{
				PluginConfig: InstanceGroup{
					Name:              "vcd",
					StrURL:            os.Getenv("VCD_URL"),
					Org:               os.Getenv("VCD_ORG"),
					VirtualDatacenter: os.Getenv("VCD_VDC"),
					Network:           os.Getenv("VCD_NETWORK"),
					IPAllocationMode:  os.Getenv("VCD_NETWORK_ALLOCATION_MODE"),
					Token:             os.Getenv("VCD_TOKEN"),
					InstanceGroupName: "fleeting-test-pub-key",
					VAppNamePrefix:    os.Getenv("VCD_VAPP_NAME_PREFIX"),
					Catalog:           os.Getenv("VCD_CATALOG"),
					Template:          os.Getenv("VCD_TEMPLATE"),
					StorageProfile:    os.Getenv("VCD_STORAGE_PROFILE"),
					CPUCount:          mustAtoi(os.Getenv("VCD_CPU_COUNT")),
					CoresPerSocket:    mustAtoi(os.Getenv("VCD_CORES_PER_SOCKET")),
					MemoryMB:          int64(mustAtoi(os.Getenv("VCD_MEMORY_MB"))),
					DiskSizeGB:        mustAtoi(os.Getenv("VCD_DISK_SIZE_GB")),
				},
				// We need write something the Username field here. In reality, the username will be provided by ConnectInfo(),
				// and as we use VCD+VMware Tools it is always either root or Administrator.
				ConnectorConfig: provider.ConnectorConfig{
					Timeout:              30 * time.Minute,
					UseStaticCredentials: true,
					Username:             "foobar",
					Key:                  privateKeyPem,
				},
				MaxInstances:    3,
				UseExternalAddr: true,
			},
		)
	})

	t.Run("static_credentials_user_password", func(t *testing.T) {
		t.Parallel()
		integration.TestProvisioning(t,
			pluginBinary,
			integration.Config{
				PluginConfig: InstanceGroup{
					Name:              "vcd",
					StrURL:            os.Getenv("VCD_URL"),
					Org:               os.Getenv("VCD_ORG"),
					VirtualDatacenter: os.Getenv("VCD_VDC"),
					Network:           os.Getenv("VCD_NETWORK"),
					IPAllocationMode:  os.Getenv("VCD_NETWORK_ALLOCATION_MODE"),
					Token:             os.Getenv("VCD_TOKEN"),
					InstanceGroupName: "fleeting-test-user-password",
					VAppNamePrefix:    os.Getenv("VCD_VAPP_NAME_PREFIX"),
					Catalog:           os.Getenv("VCD_CATALOG"),
					Template:          os.Getenv("VCD_TEMPLATE"),
					StorageProfile:    os.Getenv("VCD_STORAGE_PROFILE"),
					CPUCount:          mustAtoi(os.Getenv("VCD_CPU_COUNT")),
					CoresPerSocket:    mustAtoi(os.Getenv("VCD_CORES_PER_SOCKET")),
					MemoryMB:          int64(mustAtoi(os.Getenv("VCD_MEMORY_MB"))),
					DiskSizeGB:        mustAtoi(os.Getenv("VCD_DISK_SIZE_GB")),
				},
				// We need write some thing the Username field here. In reality, the username will be provided by ConnectInfo(),
				// and as we use VCD+VMware Tools it is always either root or Administrator.
				ConnectorConfig: provider.ConnectorConfig{
					Timeout:              30 * time.Minute,
					UseStaticCredentials: true,
					Username:             "foobar",
					Password:             "ExcellentPassword123!",
				},
				MaxInstances:    3,
				UseExternalAddr: true,
			},
		)
	})
}

func mustAtoi(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return i
}
