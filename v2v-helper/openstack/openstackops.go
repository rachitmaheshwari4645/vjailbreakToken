// Copyright © 2024 The vjailbreak authors

package openstack

import (
	context "context"
	"fmt"
	"os"
	"time"

	vjailbreakv1alpha1 "github.com/platform9/vjailbreak/k8s/migration/api/v1alpha1"
	"github.com/platform9/vjailbreak/v2v-helper/pkg/constants"
	"github.com/platform9/vjailbreak/v2v-helper/pkg/k8sutils"
	"github.com/platform9/vjailbreak/v2v-helper/pkg/utils"
	"github.com/platform9/vjailbreak/v2v-helper/vm"

	gophercloud "github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/subnets"
	netutils "github.com/platform9/vjailbreak/pkg/common/utils"
)

//go:generate mockgen -source=../openstack/openstackops.go -destination=../openstack/openstackops_mock.go -package=openstack

type OpenstackOperations interface {
	CreateVolume(ctx context.Context, name string, size int64, ostype string, uefi bool, volumetype string, setRDMLabel bool) (*volumes.Volume, error)
	WaitForVolume(ctx context.Context, volumeID string) error
	AttachVolumeToVM(ctx context.Context, volumeID string) error
	WaitForVolumeAttachment(ctx context.Context, volumeID string) error
	DetachVolumeFromVM(ctx context.Context, volumeID string) error
	SetVolumeUEFI(ctx context.Context, volume *volumes.Volume) error
	EnableQGA(ctx context.Context, volume *volumes.Volume) error
	SetVolumeImageMetadata(ctx context.Context, volume *volumes.Volume, setRDMLabel bool) error
	SetVolumeBootable(ctx context.Context, volume *volumes.Volume) error
	GetClosestFlavour(ctx context.Context, cpu int32, memory int32) (*flavors.Flavor, error)
	GetFlavor(ctx context.Context, flavorId string) (*flavors.Flavor, error)
	GetNetwork(ctx context.Context, networkname string) (*networks.Network, error)
	GetPort(ctx context.Context, portID string) (*ports.Port, error)
	ValidateAndCreatePort(ctx context.Context, networkid *networks.Network, mac string, ipPerMac map[string][]vm.IpEntry, vmname string, securityGroups []string, fallbackToDHCP bool, gatewayIP map[string]string) (*ports.Port, error)
	DeletePort(ctx context.Context, portID string) error
	GetSubnet(ctx context.Context, network []string, ip string) (*subnets.Subnet, error)
	CreatePort(ctx context.Context, networkid *networks.Network, mac string, ip []string, vmname string, securityGroups []string, fallbackToDHCP bool, gatewayIP map[string]string) (*ports.Port, error)
	CreateVM(ctx context.Context, flavor *flavors.Flavor, networkIDs, portIDs []string, vminfo vm.VMInfo, availabilityZone string, securityGroups []string, serverGroupID string, vjailbreakSettings k8sutils.VjailbreakSettings, useFlavorless bool) (*servers.Server, error)
	GetServerGroups(ctx context.Context, projectName string) ([]vjailbreakv1alpha1.ServerGroupInfo, error)
	GetSecurityGroupIDs(ctx context.Context, groupNames []string, projectName string) ([]string, error)
	DeleteVolume(ctx context.Context, volumeID string) error
	FindDevice(volumeID string) (string, error)
	ManageExistingVolume(name string, ref map[string]interface{}, host string, volumeType string) (*volumes.Volume, error)
	WaitUntilVMActive(ctx context.Context, vmID string) (bool, error)
}

func validateOpenStack(ctx context.Context, insecure bool) (*utils.OpenStackClients, error) {
	var opts gophercloud.AuthOptions
	token := os.Getenv("OS_TOKEN")
	utils.PrintLog(fmt.Sprintf("Using OpenStack token from environment variables: %s", token))
	if token != "" {
		var err error
		opts, err = AuthOptionsUsingTokenFromEnv()
		if err != nil {
			return nil, fmt.Errorf("failed to get OpenStack auth options using token: %s", err)
		}
	} else {
		var err error
		opts, err = openstack.AuthOptionsFromEnv()
		if err != nil {
			return nil, fmt.Errorf("failed to get OpenStack auth options: %s", err)
		}
		opts.AllowReauth = true
	}

	providerClient, err := openstack.NewClient(opts.IdentityEndpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider client: %s", err)
	}

	vjbNet := netutils.NewVjbNet()
	if insecure {
		vjbNet.Insecure = true
	}
	err = vjbNet.CreateSecureHTTPClient()
	if err != nil {
		return nil, fmt.Errorf("failed to create secure HTTP client %v", err)
	}
	providerClient.HTTPClient = *vjbNet.GetClient()
	// Connection Retry Block
	for i := 0; i < constants.MaxIntervalCount; i++ {
		err = openstack.Authenticate(ctx, providerClient, opts)

		if err == nil {
			break
		}
		time.Sleep(5 * time.Second) // Wait for 5 seconds before checking again
	}
	if err != nil {
		return nil, fmt.Errorf("failed to authenticate OpenStack client: %s", err)
	}

	endpoint := gophercloud.EndpointOpts{
		Region: os.Getenv("OS_REGION_NAME"),
	}

	blockStorageClient, err := openstack.NewBlockStorageV3(providerClient, endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to create block storage client: %s", err)
	}

	computeClient, err := openstack.NewComputeV2(providerClient, endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to create compute client: %s", err)
	}

	networkingClient, err := openstack.NewNetworkV2(providerClient, endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to create networking client: %s", err)
	}

	return &utils.OpenStackClients{
		BlockStorageClient: blockStorageClient,
		ComputeClient:      computeClient,
		NetworkingClient:   networkingClient,
		K8sClient:          nil,
		AuthURL:            opts.IdentityEndpoint,
		Tenant:             opts.TenantName,
	}, nil
}

func NewOpenStackClients(ctx context.Context, insecure bool) (*utils.OpenStackClients, error) {
	ostackclients, err := validateOpenStack(ctx, insecure)
	if err != nil {
		return nil, fmt.Errorf("failed to validate OpenStack connection: %s", err)
	}
	return ostackclients, nil
}

func AuthOptionsUsingTokenFromEnv() (gophercloud.AuthOptions, error) {
	authURL := os.Getenv("OS_AUTH_URL")
	tenantID := os.Getenv("OS_TENANT_ID")
	tenantName := os.Getenv("OS_TENANT_NAME")
	domainID := os.Getenv("OS_DOMAIN_ID")
	domainName := os.Getenv("OS_DOMAIN_NAME")
	applicationCredentialID := os.Getenv("OS_APPLICATION_CREDENTIAL_ID")
	applicationCredentialName := os.Getenv("OS_APPLICATION_CREDENTIAL_NAME")
	applicationCredentialSecret := os.Getenv("OS_APPLICATION_CREDENTIAL_SECRET")
	systemScope := os.Getenv("OS_SYSTEM_SCOPE")
	var nilOptions = gophercloud.AuthOptions{}
	// If OS_PROJECT_ID is set, overwrite tenantID with the value.
	if v := os.Getenv("OS_PROJECT_ID"); v != "" {
		tenantID = v
	}

	// If OS_PROJECT_NAME is set, overwrite tenantName with the value.
	if v := os.Getenv("OS_PROJECT_NAME"); v != "" {
		tenantName = v
	}

	if authURL == "" {
		err := gophercloud.ErrMissingEnvironmentVariable{
			EnvironmentVariable: "OS_AUTH_URL",
		}
		return nilOptions, err
	}

	if (applicationCredentialID != "" || applicationCredentialName != "") && applicationCredentialSecret == "" {
		err := gophercloud.ErrMissingEnvironmentVariable{
			EnvironmentVariable: "OS_APPLICATION_CREDENTIAL_SECRET",
		}
		return nilOptions, err
	}

	if domainID == "" && domainName == "" && tenantID == "" && tenantName != "" {
		err := gophercloud.ErrMissingEnvironmentVariable{
			EnvironmentVariable: "OS_PROJECT_ID",
		}
		return nilOptions, err
	}

	var scope *gophercloud.AuthScope
	if systemScope == "all" {
		scope = &gophercloud.AuthScope{
			System: true,
		}
	}

	ao := gophercloud.AuthOptions{
		IdentityEndpoint:            authURL,
		TenantID:                    tenantID,
		TenantName:                  tenantName,
		DomainID:                    domainID,
		DomainName:                  domainName,
		ApplicationCredentialID:     applicationCredentialID,
		ApplicationCredentialName:   applicationCredentialName,
		ApplicationCredentialSecret: applicationCredentialSecret,
		Scope:                       scope,
		TokenID:                     os.Getenv("OS_TOKEN"),
		AllowReauth:                 false,
	}

	return ao, nil
}
