package vcd

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gitlab.com/gitlab-org/fleeting/fleeting/integration"
	"gitlab.com/gitlab-org/fleeting/fleeting/provider"
)

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

	if os.Getenv("VCD_VAPP") == "" {
		t.Error("mandatory environment variable VCD_VAPP not set")
	}

	if os.Getenv("VCD_VM_NAME_PREFIX") == "" {
		t.Error("mandatory environment variable VCD_VM_NAME_PREFIX not set")
	}

	pluginBinary := integration.BuildPluginBinary(t, "cmd/fleeting-plugin-vcd", "fleeting-plugin-vcd")

	t.Run("static credentials", func(t *testing.T) {
		var key PrivPub
		var err error
		key, err = rsa.GenerateKey(rand.Reader, 4096)
		require.NoError(t, err)

		keyBytes := pem.EncodeToMemory(
			&pem.Block{
				Type:  "RSA PRIVATE KEY",
				Bytes: x509.MarshalPKCS1PrivateKey(key.(*rsa.PrivateKey)),
			},
		)

		parsedURL, err := url.Parse(os.Getenv("VCD_URL"))
		require.NoError(t, err)

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
					Catalog:           os.Getenv("VCD_CATALOG"),
					Template:          os.Getenv("VCD_TEMPLATE"),
					VApp:              os.Getenv("VCD_VAPP"),
					VMNamePrefix:      os.Getenv("VCD_VM_NAME_PREFIX"),
					parsedURL:         parsedURL,
				},
				ConnectorConfig: provider.ConnectorConfig{
					Timeout:              10 * time.Minute,
					UseStaticCredentials: true,
					Username:             "root",
					Key:                  keyBytes,
				},
				MaxInstances:    3,
				UseExternalAddr: true,
			},
		)
	})
}
