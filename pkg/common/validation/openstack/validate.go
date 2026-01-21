package openstack

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/pkg/errors"
	vjailbreakv1alpha1 "github.com/platform9/vjailbreak/k8s/migration/api/v1alpha1"
	"github.com/platform9/vjailbreak/k8s/migration/pkg/utils"
	netutils "github.com/platform9/vjailbreak/pkg/common/utils"
	corev1 "k8s.io/api/core/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	namespaceMigrationSystem = "migration-system"
)

// ValidationResult holds the outcome of credential validation
type ValidationResult struct {
	Valid   bool
	Message string
	Error   error
}

// Validate performs complete OpenStack credential validation
func Validate(ctx context.Context, k8sClient client.Client, openstackcreds *vjailbreakv1alpha1.OpenstackCreds) ValidationResult {
	// Ensure logger exists
	ctx = ensureLogger(ctx)
	// Get credentials from secret
	openstackCredential, err := getCredentialsFromSecret(ctx, k8sClient, openstackcreds.Spec.SecretRef.Name)
	if err != nil {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("Failed to get credentials from secret: %s", err.Error()),
			Error:   err,
		}
	}

	// Create provider client
	providerClient, err := openstack.NewClient(openstackCredential.AuthURL)
	if err != nil {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("Failed to create OpenStack client: %s", err.Error()),
			Error:   err,
		}
	}

	vjbNet := netutils.NewVjbNet()
	if openstackCredential.Insecure {
		vjbNet.Insecure = true
	} else {
		fmt.Printf("Warning: TLS verification is enforced by default. If you encounter certificate errors, set OS_INSECURE=true to skip verification.\n")
	}
	vjbNet.SetTimeout(60 * time.Second)
	if err := vjbNet.CreateSecureHTTPClient(); err != nil {
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("failed to create secure HTTP client"),
			Error:   fmt.Errorf("failed to create secure HTTP client %v", err),
		}
	}
	providerClient.HTTPClient = *vjbNet.GetClient()

	// Authenticate
	var authOpts gophercloud.AuthOptions
	if openstackCredential.Token != "" {
		authOpts = gophercloud.AuthOptions{
			IdentityEndpoint: openstackCredential.AuthURL,
			TokenID:          openstackCredential.Token,
			DomainName:       openstackCredential.DomainName,
		}
	} else {
		authOpts = gophercloud.AuthOptions{
			IdentityEndpoint: openstackCredential.AuthURL,
			Username:         openstackCredential.Username,
			Password:         openstackCredential.Password,
			DomainName:       openstackCredential.DomainName,
			TenantName:       openstackCredential.TenantName,
		}
	}
	if err := openstack.Authenticate(ctx, providerClient, authOpts); err != nil {
		var message string
		switch {
		case strings.Contains(err.Error(), "401"):
			message = "Authentication failed: invalid username, password, or project/domain. Please verify your credentials"
		case strings.Contains(err.Error(), "404"):
			message = "Authentication failed: the authentication URL or tenant/project name is incorrect"
		case strings.Contains(err.Error(), "timeout"):
			message = "Connection timeout: unable to reach the OpenStack authentication service. Please check your network connection and Auth URL"
		default:
			message = fmt.Sprintf("Authentication failed: %s. Please verify your OpenStack credentials", err.Error())
		}
		return ValidationResult{
			Valid:   false,
			Message: message,
			Error:   err,
		}
	}

	// Verify credentials match current environment
	_, err = verifyCredentialsMatchCurrentEnvironment(providerClient, openstackCredential.RegionName)
	if err != nil {
		if strings.Contains(err.Error(), "Creds are valid but for a different OpenStack environment") {
			return ValidationResult{
				Valid:   false,
				Message: "Creds are valid but for a different OpenStack environment",
				Error:   err,
			}
		}
		return ValidationResult{
			Valid:   false,
			Message: fmt.Sprintf("Failed to verify credentials against current environment: %s", err.Error()),
			Error:   err,
		}
	}

	return ValidationResult{
		Valid:   true,
		Message: "Successfully authenticated to Openstack",
		Error:   nil,
	}
}

// getCredentialsFromSecret retrieves OpenStack credentials from a Kubernetes secret
func getCredentialsFromSecret(ctx context.Context, k8sClient client.Client, secretName string) (vjailbreakv1alpha1.OpenStackCredsInfo, error) {
	var openstackCredsInfo vjailbreakv1alpha1.OpenStackCredsInfo
	secret := &corev1.Secret{}
	err := k8sClient.Get(ctx, k8stypes.NamespacedName{Name: secretName, Namespace: namespaceMigrationSystem}, secret)
	if err != nil {
		return openstackCredsInfo, errors.Wrap(err, "failed to get secret")
	}

	fields := map[string]string{
		"AuthURL":    string(secret.Data["OS_AUTH_URL"]),
		"DomainName": string(secret.Data["OS_DOMAIN_NAME"]),
		"Username":   string(secret.Data["OS_USERNAME"]),
		"Password":   string(secret.Data["OS_PASSWORD"]),
		"TenantName": string(secret.Data["OS_TENANT_NAME"]),
		"RegionName": string(secret.Data["OS_REGION_NAME"]),
		"Token":      string(secret.Data["OS_TOKEN"]),
	}

	if fields["Token"] != "" {
		required := []string{"AuthURL", "DomainName", "TenantName", "RegionName"}
		for _, key := range required {
			if fields[key] == "" {
				return vjailbreakv1alpha1.OpenStackCredsInfo{},
					fmt.Errorf("field %s is empty or missing in secret", key)
			}
		}
	} else {
		for key, value := range fields {
			if value == "" && key != "Token" {
				return vjailbreakv1alpha1.OpenStackCredsInfo{},
					fmt.Errorf("field %s is empty or missing in secret", key)
			}
		}
	}

	openstackCredsInfo.AuthURL = fields["AuthURL"]
	openstackCredsInfo.Username = fields["Username"]
	openstackCredsInfo.Password = fields["Password"]
	openstackCredsInfo.DomainName = fields["DomainName"]
	openstackCredsInfo.TenantName = fields["TenantName"]
	openstackCredsInfo.RegionName = fields["RegionName"]
	openstackCredsInfo.Token = fields["Token"]

	insecureStr := string(secret.Data["OS_INSECURE"])
	openstackCredsInfo.Insecure = strings.EqualFold(strings.TrimSpace(insecureStr), "true")

	return openstackCredsInfo, nil
}

// verifyCredentialsMatchCurrentEnvironment checks if the provided credentials can access the current instance
func verifyCredentialsMatchCurrentEnvironment(providerClient *gophercloud.ProviderClient, regionName string) (bool, error) {
	// Get current instance metadata
	metadata, err := utils.GetCurrentInstanceMetadata()
	if err != nil {
		return false, fmt.Errorf("unable to get current instance metadata: %w. "+
			"Please ensure this is running on an OpenStack instance with metadata service enabled", err)
	}

	computeClient, err := openstack.NewComputeV2(providerClient, gophercloud.EndpointOpts{
		Region: regionName,
	})

	if err != nil {
		return false, fmt.Errorf("failed to create OpenStack compute client: %w", err)
	}
	_, err = servers.Get(context.TODO(), computeClient, metadata.UUID).Extract()
	if err != nil {
		if strings.Contains(err.Error(), "Resource not found") ||
			strings.Contains(err.Error(), "No server with a name or ID") {
			return false, errors.New("Creds are valid but for a different OpenStack environment")
		}
		return false, fmt.Errorf("failed to verify instance access: %w. "+
			"Please check if the provided credentials have compute:get_server permission", err)
	}
	return true, nil
}

// PostValidationResources holds resources fetched after successful validation
type PostValidationResources struct {
	Flavors       []flavors.Flavor
	OpenstackInfo *vjailbreakv1alpha1.OpenstackInfo
	ProjectName   string
}

// FetchResourcesPostValidation fetches OpenStack resources after successful credential validation
func FetchResourcesPostValidation(ctx context.Context, k8sClient client.Client, openstackcreds *vjailbreakv1alpha1.OpenstackCreds) (*PostValidationResources, error) {
	if openstackcreds == nil {
		return nil, fmt.Errorf("openstackcreds cannot be nil")
	}

	ctx = ensureLogger(ctx)

	log.Printf("Updating Master Node Image ID")
	err := utils.UpdateMasterNodeImageID(ctx, k8sClient, false)
	if err != nil {
		log.Printf("Warning: Failed to update master node image ID: %v", err)
	}

	log.Printf("Creating Dummy PCD Cluster if needed")
	err = utils.CreateDummyPCDClusterForStandAlonePCDHosts(ctx, k8sClient, openstackcreds)
	if err != nil {
		if !strings.Contains(err.Error(), "already exists") {
			return nil, errors.Wrap(err, "failed to create dummy PCD cluster")
		}
		log.Printf("Dummy PCD Cluster already exists, continuing")
	}

	log.Printf("Listing Flavors")
	flavorsList, err := utils.ListAllFlavors(ctx, k8sClient, openstackcreds)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list flavors")
	}

	log.Printf("Getting OpenStack Info")
	openstackInfo, err := utils.GetOpenstackInfo(ctx, k8sClient, openstackcreds)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get OpenStack info")
	}

	openstackCredential, err := getCredentialsFromSecret(ctx, k8sClient, openstackcreds.Spec.SecretRef.Name)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get credentials for project name")
	}

	return &PostValidationResources{
		Flavors:       flavorsList,
		OpenstackInfo: openstackInfo,
		ProjectName:   openstackCredential.TenantName,
	}, nil
}

func ensureLogger(ctx context.Context) context.Context {
	l := ctrllog.FromContext(ctx)
	if l.GetSink() == nil {
		return ctrllog.IntoContext(ctx, zap.New(zap.UseDevMode(true)))
	}
	return ctx
}
