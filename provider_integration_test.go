package vcd

import (
	"os"
	"strconv"
	"testing"
	"time"

	"gitlab.com/gitlab-org/fleeting/fleeting/integration"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

func mustAtoi(s string) int {
	i, err := strconv.Atoi(s)
	if err != nil {
		panic(err)
	}
	return i
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

	// t.Run("static credentials via ssh keys", func(t *testing.T) {
	// 	t.Parallel()
	// 	var err error
	// 	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	// 	require.NoError(t, err)

	// 	pemBlock, err := ssh.MarshalPrivateKey(crypto.PrivateKey(privateKey), "")
	// 	require.NoError(t, err)

	// 	privateKeyPem := pem.EncodeToMemory(pemBlock)

	// 	integration.TestProvisioning(t,
	// 		pluginBinary,
	// 		integration.Config{
	// 			PluginConfig: InstanceGroup{
	// 				Name:              "vcd",
	// 				StrURL:            os.Getenv("VCD_URL"),
	// 				Org:               os.Getenv("VCD_ORG"),
	// 				VirtualDatacenter: os.Getenv("VCD_VDC"),
	// 				Network:           os.Getenv("VCD_NETWORK"),
	// 				IPAllocationMode:  os.Getenv("VCD_NETWORK_ALLOCATION_MODE"),
	// 				Token:             os.Getenv("VCD_TOKEN"),
	// 				InstanceGroupName:  "fleeting-test-pub-key",
	// 				VAppNamePrefix:    os.Getenv("VCD_VAPP_NAME_PREFIX"),
	// 				Catalog:           os.Getenv("VCD_CATALOG"),
	// 				Template:          os.Getenv("VCD_TEMPLATE"),
	// 				StorageProfile:    os.Getenv("VCD_STORAGE_PROFILE"),
	// 				CPUCount:          mustAtoi(os.Getenv("VCD_CPU_COUNT")),
	// 				CoresPerSocket:    mustAtoi(os.Getenv("VCD_CORES_PER_SOCKET")),
	// 				MemoryMB:          int64(mustAtoi(os.Getenv("VCD_MEMORY_MB"))),
	// 				DiskSizeGB:        mustAtoi(os.Getenv("VCD_DISK_SIZE_GB")),
	// 			},
	// 			// We need write something the Username field here. In reality, the username will be provided by ConnectInfo(),
	// 			// and as we use VCD+VMware Tools it is always either root or Administrator.
	// 			ConnectorConfig: provider.ConnectorConfig{
	// 				Timeout:              30 * time.Minute,
	// 				UseStaticCredentials: true,
	// 				Username:             "foobar",
	// 				Key:                  privateKeyPem,
	// 			},
	// 			MaxInstances:    3,
	// 			UseExternalAddr: true,
	// 		},
	// 	)
	// })

	t.Run("static credentials via user/password", func(t *testing.T) {
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
