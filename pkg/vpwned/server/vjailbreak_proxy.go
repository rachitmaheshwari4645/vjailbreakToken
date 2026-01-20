package server

import (
	"context"
	"fmt"
	"strings"
	"time"

	gophercloud "github.com/gophercloud/gophercloud/v2"
	openstack "github.com/gophercloud/gophercloud/v2/openstack"
	ports "github.com/gophercloud/gophercloud/v2/openstack/networking/v2/ports"
	errors "github.com/pkg/errors"
	netutils "github.com/platform9/vjailbreak/common/utils"
	vjailbreakv1alpha1 "github.com/platform9/vjailbreak/k8s/migration/api/v1alpha1"
	openstackvalidation "github.com/platform9/vjailbreak/pkg/validation/openstack"
	vmwarevalidation "github.com/platform9/vjailbreak/pkg/validation/vmware"
	api "github.com/platform9/vjailbreak/pkg/vpwned/api/proto/v1/service"
	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlLog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

type vjailbreakProxy struct {
	api.UnimplementedVailbreakProxyServer
	K8sClient client.Client
}

type OpenstackCredsinfo struct {
	AuthUrl    string `json:"auth_url"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	DomainName string `json:"domain_name"`
	TenantName string `json:"tenant_name"`
	RegionName string `json:"region_name"`
	Insecure   bool   `json:"insecure"`
	Token      string `json:"token"`
}

func (p *vjailbreakProxy) ValidateOpenstackIp(ctx context.Context, in *api.ValidateOpenstackIpRequest) (*api.ValidateOpenstackIpResponse, error) {
	const fn = "ValidateOpenstackIp"
	logrus.WithField("func", fn).Info("Starting ValidateOpenstackIp request")
	defer logrus.WithField("func", fn).Info("Completed ValidateOpenstackIp request")

	retVal := &api.ValidateOpenstackIpResponse{}

	ips := in.GetIp()
	openstackAccessInfo := in.AccessInfo

	logrus.WithFields(logrus.Fields{
		"ip_count": len(ips),
		"ips":      ips,
	}).Info("Validating OpenStack IPs")

	// Add a check if there are same ips present in the list
	ipMap := make(map[string]bool)
	for _, ip := range ips {
		if _, ok := ipMap[ip]; ok {
			ipMap[ip] = false
			logrus.WithField("ip", ip).Warn("Duplicate IP detected")
		} else {
			ipMap[ip] = true
		}
	}

	logrus.Info("Creating OpenStack clients")
	openstackClients, err := GetOpenStackClients(ctx, openstackAccessInfo)
	if err != nil {
		logrus.WithError(err).Error("Failed to create OpenStack clients")
		return nil, err
	}
	logrus.Info("Successfully created OpenStack clients")

	for idx, ip := range ips {
		logrus.WithFields(logrus.Fields{
			"ip":    ip,
			"index": idx,
		}).Debug("Validating IP")

		if ipMap[ip] == false {
			logrus.WithField("ip", ip).Warn("IP marked as duplicate, skipping validation")
			retVal.IsValid = append(retVal.IsValid, false)
			retVal.Reason = append(retVal.Reason, "Duplicate IP")
			continue
		}

		isInUse, reason, err := isIPInUse(openstackClients.NetworkingClient, ip)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"ip":    ip,
				"error": err,
			}).Error("Failed to check if IP is in use")
			return nil, err
		}

		logrus.WithFields(logrus.Fields{
			"ip":        ip,
			"is_in_use": isInUse,
			"reason":    reason,
		}).Info("IP validation result")

		retVal.IsValid = append(retVal.IsValid, !isInUse)
		retVal.Reason = append(retVal.Reason, reason)
	}

	return retVal, nil
}

type OpenStackClients struct {
	BlockStorageClient *gophercloud.ServiceClient
	ComputeClient      *gophercloud.ServiceClient
	NetworkingClient   *gophercloud.ServiceClient
}

// Helper: checks if IP is used by any port or floating IP in OpenStack
func isIPInUse(client *gophercloud.ServiceClient, ip string) (bool, string, error) {
	const fn = "isIPInUse"
	logrus.WithFields(logrus.Fields{"func": fn, "ip": ip}).Debug("Entering isIPInUse")
	defer logrus.WithFields(logrus.Fields{"func": fn, "ip": ip}).Debug("Exiting isIPInUse")
	// Check fixed IPs on ports
	portList, err := ports.List(client, ports.ListOpts{
		FixedIPs: []ports.FixedIPOpts{{IPAddress: ip}},
	}).AllPages(context.TODO())
	if err != nil {
		logrus.WithFields(logrus.Fields{"func": fn, "ip": ip}).WithError(err).Error("Failed to list ports for IP")
		return false, "", err
	}
	allPorts, _ := ports.ExtractPorts(portList)
	if len(allPorts) > 0 {
		return true, "Already in use (port)", nil
	}

	return false, "Available", nil
}

// GetOpenstackCredentialsFromSecret retrieves and checks the secret
func GetOpenstackCredentialsFromSecret(ctx context.Context, k3sclient client.Client, secretName string, secretNamespace string) (vjailbreakv1alpha1.OpenStackCredsInfo, error) {
	const fn = "GetOpenstackCredentialsFromSecret"
	logrus.WithFields(logrus.Fields{"func": fn, "secret": secretName, "namespace": secretNamespace}).Debug("Entering GetOpenstackCredentialsFromSecret")
	defer logrus.WithFields(logrus.Fields{"func": fn, "secret": secretName, "namespace": secretNamespace}).Debug("Exiting GetOpenstackCredentialsFromSecret")
	secret := &corev1.Secret{}
	if err := k3sclient.Get(ctx, k8stypes.NamespacedName{Namespace: secretNamespace, Name: secretName}, secret); err != nil {
		logrus.WithFields(logrus.Fields{"func": fn, "secret": secretName, "namespace": secretNamespace}).WithError(err).Error("Failed to get secret")
		return vjailbreakv1alpha1.OpenStackCredsInfo{}, errors.Wrap(err, "failed to get secret")
	}

	// Extract and validate each field
	fields := map[string]string{
		"AuthURL":    string(secret.Data["OS_AUTH_URL"]),
		"DomainName": string(secret.Data["OS_DOMAIN_NAME"]),
		"Username":   string(secret.Data["OS_USERNAME"]),
		"Password":   string(secret.Data["OS_PASSWORD"]),
		"TenantName": string(secret.Data["OS_TENANT_NAME"]),
		"RegionName": string(secret.Data["OS_REGION_NAME"]),
		"Token" : string(secret.Data["OS_TOKEN"]),
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

	insecureStr := string(secret.Data["OS_INSECURE"])
	insecure := strings.EqualFold(strings.TrimSpace(insecureStr), "true")

	return vjailbreakv1alpha1.OpenStackCredsInfo{
		AuthURL:    fields["AuthURL"],
		DomainName: fields["DomainName"],
		Username:   fields["Username"],
		Password:   fields["Password"],
		RegionName: fields["RegionName"],
		TenantName: fields["TenantName"],
		Token: 		fields["Token"],
		Insecure:   insecure,
	}, nil
}

// GetOpenStackClients is a function to create openstack clients
func GetOpenStackClients(ctx context.Context, openstackAccessInfo *api.OpenstackAccessInfo) (*OpenStackClients, error) {
	const fn = "GetOpenStackClients"
	logrus.WithField("func", fn).Debug("Starting client creation")

	if openstackAccessInfo == nil {
		logrus.WithField("func", fn).Error("openstackAccessInfo is nil")
		return nil, fmt.Errorf("openstackAccessInfo cannot be nil")
	}

	logrus.WithField("func", fn).Debug("Creating in-cluster k8s client")

	k8sclient, err := CreateInClusterClient()
	if err != nil {
		logrus.WithField("func", fn).WithError(err).Error("Failed to create in-cluster k8s client")
		return nil, err
	}
	logrus.WithField("func", fn).Info("Successfully created in-cluster k8s client")

	logrus.WithField("func", fn).Info("Retrieving OpenStack credentials from secret")

	openstackCreds, err := GetOpenstackCredentialsFromSecret(ctx, k8sclient, openstackAccessInfo.SecretName, openstackAccessInfo.SecretNamespace)
	if err != nil {
		logrus.WithField("func", fn).WithError(err).Error("Failed to get OpenStack credentials from secret")
		return nil, err
	}
	logrus.WithFields(logrus.Fields{
		"auth_url": openstackCreds.AuthURL,
		"region":   openstackCreds.RegionName,
		"insecure": openstackCreds.Insecure,
		"func":     fn,
	}).Info("Successfully retrieved OpenStack credentials")

	endpoint := gophercloud.EndpointOpts{
		Region: openstackCreds.RegionName,
	}

	logrus.WithFields(logrus.Fields{"region": openstackCreds.RegionName, "func": fn}).Info("Creating OpenStack provider client")
	providerClient, err := ValidateAndGetProviderClient(&openstackCreds)
	if err != nil {
		logrus.WithFields(logrus.Fields{"region": openstackCreds.RegionName, "func": fn}).WithError(err).Error("Failed to create provider client")
		return nil, err
	}
	if providerClient == nil {
		logrus.WithFields(logrus.Fields{"region": openstackCreds.RegionName, "func": fn}).Error("Provider client is nil")
		return nil, fmt.Errorf("failed to get provider client for region '%s'", openstackCreds.RegionName)
	}
	logrus.WithFields(logrus.Fields{"region": openstackCreds.RegionName, "func": fn}).Info("Successfully created and authenticated provider client")

	logrus.WithFields(logrus.Fields{"region": openstackCreds.RegionName, "func": fn}).Debug("Creating compute client")
	computeClient, err := openstack.NewComputeV2(providerClient, endpoint)
	if err != nil {
		logrus.WithFields(logrus.Fields{"region": openstackCreds.RegionName, "func": fn}).WithError(err).Error("Failed to create compute client")
		return nil, fmt.Errorf("failed to create openstack compute client for region '%s'", openstackCreds.RegionName)
	}

	logrus.WithFields(logrus.Fields{"region": openstackCreds.RegionName, "func": fn}).Debug("Creating block storage client")
	blockStorageClient, err := openstack.NewBlockStorageV3(providerClient, endpoint)
	if err != nil {
		logrus.WithFields(logrus.Fields{"region": openstackCreds.RegionName, "func": fn}).WithError(err).Error("Failed to create block storage client")
		return nil, fmt.Errorf("failed to create openstack block storage client for region '%s'",
			openstackCreds.RegionName)
	}

	logrus.WithFields(logrus.Fields{"region": openstackCreds.RegionName, "func": fn}).Debug("Creating networking client")
	networkingClient, err := openstack.NewNetworkV2(providerClient, endpoint)
	if err != nil {
		logrus.WithFields(logrus.Fields{"region": openstackCreds.RegionName, "func": fn}).WithError(err).Error("Failed to create networking client")
		return nil, fmt.Errorf("failed to create openstack networking client for region '%s'",
			openstackCreds.RegionName)
	}

	logrus.WithFields(logrus.Fields{"region": openstackCreds.RegionName, "func": fn}).Info("Successfully created all OpenStack clients")
	return &OpenStackClients{
		BlockStorageClient: blockStorageClient,
		ComputeClient:      computeClient,
		NetworkingClient:   networkingClient,
	}, nil
}

func ValidateAndGetProviderClient(openstackAccessInfo *vjailbreakv1alpha1.OpenStackCredsInfo) (*gophercloud.ProviderClient, error) {
	const fn = "ValidateAndGetProviderClient"
	logrus.WithFields(logrus.Fields{"func": fn, "auth_url": openstackAccessInfo.AuthURL, "region": openstackAccessInfo.RegionName, "insecure": openstackAccessInfo.Insecure}).Debug("Entering ValidateAndGetProviderClient")
	defer logrus.WithFields(logrus.Fields{"func": fn, "auth_url": openstackAccessInfo.AuthURL, "region": openstackAccessInfo.RegionName, "insecure": openstackAccessInfo.Insecure}).Debug("Exiting ValidateAndGetProviderClient")

	providerClient, err := openstack.NewClient(openstackAccessInfo.AuthURL)
	if err != nil {
		logrus.WithField("func", fn).WithError(err).Error("Failed to create OpenStack provider client")
		return nil, err
	}
	vjbNet := netutils.NewVjbNet()
	if openstackAccessInfo.Insecure {
		vjbNet.Insecure = true
	} else {
		fmt.Printf("Warning: TLS verification is enforced by default. If you encounter certificate errors, set OS_INSECURE=true to skip verification.\n")
	}

	vjbNet.SetTimeout(60 * time.Second)
	if vjbNet.CreateSecureHTTPClient() == nil {
		providerClient.HTTPClient = *vjbNet.GetClient()
	} else {
		return nil, fmt.Errorf("failed to create secure HTTP client")
	}

	err = openstack.Authenticate(context.TODO(), providerClient, gophercloud.AuthOptions{
		IdentityEndpoint: openstackAccessInfo.AuthURL,
		Username:         openstackAccessInfo.Username,
		Password:         openstackAccessInfo.Password,
		DomainName:       openstackAccessInfo.DomainName,
		TenantName:       openstackAccessInfo.TenantName,
	})
	if err != nil {
		logrus.WithField("func", fn).WithError(err).Error("Failed to authenticate OpenStack provider client")
		return nil, err
	}

	return providerClient, nil
}

// countValid is a helper function to count valid IPs
func countValid(validFlags []bool) int {
	const fn = "countValid"
	logrus.WithField("func", fn).Debug("Entering countValid")
	count := 0
	for _, valid := range validFlags {
		if valid {
			count++
		}
	}
	logrus.WithFields(logrus.Fields{"func": fn, "count": count}).Debug("Exiting countValid")
	return count
}

// Create in cluster k8s client
func CreateInClusterClient() (client.Client, error) {
	const fn = "CreateInClusterClient"
	logrus.WithField("func", fn).Debug("Creating in-cluster k8s client")
	config, err := rest.InClusterConfig()
	if err != nil {
		logrus.WithField("func", fn).WithError(err).Error("Failed to get in-cluster config")
		return nil, err
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(vjailbreakv1alpha1.AddToScheme(scheme))

	clientset, err := client.New(config, client.Options{
		Scheme: scheme,
	})
	if err != nil {
		logrus.WithField("func", fn).WithError(err).Error("Failed to create k8s client")
		return nil, err
	}
	logrus.WithField("func", fn).Info("Successfully created in-cluster k8s client")
	return clientset, nil
}

func (p *vjailbreakProxy) RevalidateCredentials(ctx context.Context, in *api.RevalidateCredentialsRequest) (*api.RevalidateCredentialsResponse, error) {
	const fn = "RevalidateCredentials"
	logrus.WithFields(logrus.Fields{"func": fn, "kind": in.GetKind(), "name": in.GetName(), "namespace": in.GetNamespace()}).Info("Entering RevalidateCredentials")
	defer logrus.WithFields(logrus.Fields{"func": fn, "kind": in.GetKind(), "name": in.GetName(), "namespace": in.GetNamespace()}).Info("Exiting RevalidateCredentials")

	zapLogger := zap.New(zap.UseDevMode(true))
	ctx = ctrlLog.IntoContext(ctx, zapLogger)
	reqLogger := ctrlLog.FromContext(ctx)

	kind := in.GetKind()
	name := in.GetName()
	namespace := in.GetNamespace()
	logrus.WithFields(logrus.Fields{"func": fn, "kind": kind, "name": name, "namespace": namespace}).Info("Revalidating credentials")

	switch kind {

	case "VmwareCreds":
		vmwcreds := &vjailbreakv1alpha1.VMwareCreds{}
		if err := p.K8sClient.Get(ctx, k8stypes.NamespacedName{Name: name, Namespace: namespace}, vmwcreds); err != nil {
			logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).WithError(err).Error("Failed to get VMwareCreds")
			return nil, fmt.Errorf("failed to get VMwareCreds %s: %w", name, err)
		}

		logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).Info("Starting VMware validation")
		result := vmwarevalidation.Validate(ctx, p.K8sClient, vmwcreds)

		// Update status immediately
		if result.Valid {
			vmwcreds.Status.VMwareValidationStatus = "Succeeded"
			vmwcreds.Status.VMwareValidationMessage = result.Message
			logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).Info("VMware validation succeeded")

			if err := p.K8sClient.Status().Update(ctx, vmwcreds); err != nil {
				logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).WithError(err).Error("Failed to update VMwareCreds status after success")
				return nil, fmt.Errorf("failed to update status: %w", err)
			}

			// Fetch resources
			logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).Info("Triggering VMware resource fetch")

			// Capture name and namespace
			credName := vmwcreds.Name
			credNamespace := vmwcreds.Namespace

			go func() {
				bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
				defer cancel()

				bgCtx = ctrlLog.IntoContext(bgCtx, reqLogger)

				freshVMCreds := &vjailbreakv1alpha1.VMwareCreds{}
				if err := p.K8sClient.Get(bgCtx, k8stypes.NamespacedName{
					Name:      credName,
					Namespace: credNamespace,
				}, freshVMCreds); err != nil {
					logrus.WithFields(logrus.Fields{"func": fn, "name": credName, "namespace": credNamespace}).WithError(err).Error("Failed to fetch VMwareCreds in background")
					return
				}

				logrus.WithFields(logrus.Fields{"func": fn, "name": credName, "namespace": credNamespace}).Info("Fetching VMware resources")
				resources, err := vmwarevalidation.FetchResourcesPostValidation(bgCtx, p.K8sClient, freshVMCreds)
				if err != nil {
					logrus.WithFields(logrus.Fields{"func": fn, "name": credName, "namespace": credNamespace}).WithError(err).Warn("Failed to fetch VMware resources")
				} else {
					logrus.WithFields(logrus.Fields{"func": fn, "name": credName, "namespace": credNamespace, "vm_count": len(resources.VMInfo)}).Info("Successfully fetched VMware resources")
				}
			}()

		} else {
			vmwcreds.Status.VMwareValidationStatus = "Failed"
			vmwcreds.Status.VMwareValidationMessage = result.Message
			logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace, "message": result.Message}).Warn("VMware validation failed")

			if err := p.K8sClient.Status().Update(ctx, vmwcreds); err != nil {
				logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).WithError(err).Error("Failed to update VMwareCreds status")
				return nil, fmt.Errorf("failed to update VMwareCreds status: %w", err)
			}
		}

		responseMsg := fmt.Sprintf("Validation completed for %s: %s", name, result.Message)
		return &api.RevalidateCredentialsResponse{Message: responseMsg}, nil

	case "OpenstackCreds":
		oscreds := &vjailbreakv1alpha1.OpenstackCreds{}
		if err := p.K8sClient.Get(ctx, k8stypes.NamespacedName{Name: name, Namespace: namespace}, oscreds); err != nil {
			logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).WithError(err).Error("Failed to get OpenstackCreds")
			return nil, fmt.Errorf("failed to get OpenstackCreds %s: %w", name, err)
		}

		logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).Info("Starting OpenStack validation")
		result := openstackvalidation.Validate(ctx, p.K8sClient, oscreds)

		if result.Valid {
			oscreds.Status.OpenStackValidationStatus = "Succeeded"
			oscreds.Status.OpenStackValidationMessage = result.Message
			logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).Info("OpenStack validation succeeded")

			// Update status immediately after validation succeeds
			if err := p.K8sClient.Status().Update(ctx, oscreds); err != nil {
				logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).WithError(err).Error("Failed to update OpenstackCreds status after success")
				return nil, fmt.Errorf("failed to update status: %w", err)
			}

			// Fetch resources post-validation
			logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).Info("Fetching OpenStack resources")
			resources, err := openstackvalidation.FetchResourcesPostValidation(ctx, p.K8sClient, oscreds)
			if err != nil {
				logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).WithError(err).Warn("Failed to fetch OpenStack resources")
			} else {
				// Refetch the latest version before updating spec
				freshOSCreds := &vjailbreakv1alpha1.OpenstackCreds{}
				if err := p.K8sClient.Get(ctx, k8stypes.NamespacedName{Name: name, Namespace: namespace}, freshOSCreds); err != nil {
					logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).WithError(err).Warn("Failed to refetch OpenstackCreds for spec update")
				} else {
					freshOSCreds.Spec.Flavors = resources.Flavors

					// Update the spec
					if err := p.K8sClient.Update(ctx, freshOSCreds); err != nil {
						logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).WithError(err).Warn("Failed to update OpenstackCreds spec")
					} else {
						logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace, "flavor_count": len(resources.Flavors)}).Info("Updated OpenstackCreds spec with flavors")
					}

					// Refetch again before updating status with OpenStack info
					if err := p.K8sClient.Get(ctx, k8stypes.NamespacedName{Name: name, Namespace: namespace}, freshOSCreds); err != nil {
						logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).WithError(err).Warn("Failed to refetch OpenstackCreds for status update")
					} else {
						// Update status with OpenStack info
						if resources.OpenstackInfo != nil {
							freshOSCreds.Status.Openstack = *resources.OpenstackInfo
						}

						if err := p.K8sClient.Status().Update(ctx, freshOSCreds); err != nil {
							logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).WithError(err).Warn("Failed to update OpenstackCreds status with OpenStack info")
						} else {
							logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace, "flavor_count": len(resources.Flavors)}).Info("Successfully fetched and updated OpenStack resources")
						}
					}
				}
			}
		} else {
			oscreds.Status.OpenStackValidationStatus = "Failed"
			oscreds.Status.OpenStackValidationMessage = result.Message
			logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace, "message": result.Message}).Warn("OpenStack validation failed")

			if err := p.K8sClient.Status().Update(ctx, oscreds); err != nil {
				logrus.WithFields(logrus.Fields{"func": fn, "name": name, "namespace": namespace}).WithError(err).Error("Failed to update OpenstackCreds status")
				return nil, fmt.Errorf("failed to update OpenstackCreds status: %w", err)
			}
		}

		responseMsg := fmt.Sprintf("Validation completed for %s: %s", name, result.Message)
		return &api.RevalidateCredentialsResponse{Message: responseMsg}, nil

	default:
		logrus.WithFields(logrus.Fields{"func": fn, "kind": kind, "name": name, "namespace": namespace}).Error("Unknown credentials kind")
		return nil, fmt.Errorf("unknown credentials kind: %s", kind)
	}
}

func (p *vjailbreakProxy) InjectEnvVariables(ctx context.Context, in *api.InjectEnvVariablesRequest) (*api.InjectEnvVariablesResponse, error) {
	const fn = "InjectEnvVariables"
	logrus.WithFields(logrus.Fields{
		"func":        fn,
		"http_proxy":  in.GetHttpProxy(),
		"https_proxy": in.GetHttpsProxy(),
		"no_proxy":    in.GetNoProxy(),
	}).Info("Starting InjectEnvVariables request")
	defer logrus.WithField("func", fn).Info("Completed InjectEnvVariables request")

	const (
		configMapName      = "pf9-env"
		configMapNamespace = "migration-system"
		deploymentName     = "migration-controller-manager"
		deploymentNs       = "migration-system"
	)

	httpProxy := in.GetHttpProxy()
	httpsProxy := in.GetHttpsProxy()
	noProxy := in.GetNoProxy()

	if httpProxy == "" && httpsProxy == "" && noProxy == "" {
		logrus.WithField("func", fn).Error("All environment variables are empty")
		return &api.InjectEnvVariablesResponse{
			Success: false,
			Message: "At least one environment variable must be provided",
		}, nil
	}

	// If at least one of http_proxy or https_proxy is present, ensure .svc,.cluster.local,localhost,127.0.0.1,169.254.169.254,10.43.0.0/16 is in no_proxy
	if (httpProxy != "" || httpsProxy != "") {
		requiredNoProxyValues := []string{".svc", ".cluster.local", "localhost", "127.0.0.1", "169.254.169.254", "10.43.0.0/16"}
		noProxyList := []string{}
		
		if noProxy != "" {
			noProxyList = strings.Split(noProxy, ",")
			// Trim spaces from each entry
			for i := range noProxyList {
				noProxyList[i] = strings.TrimSpace(noProxyList[i])
			}
		}
		
		// Check and add missing values
		for _, required := range requiredNoProxyValues {
			found := false
			for _, existing := range noProxyList {
				if existing == required {
					found = true
					break
				}
			}
			if !found {
				noProxyList = append(noProxyList, required)
				logrus.WithFields(logrus.Fields{
					"func":  fn,
					"value": required,
				}).Info("Auto-appending value to no_proxy")
			}
		}
		
		// Reconstruct no_proxy
		noProxy = strings.Join(noProxyList, ",")
		logrus.WithFields(logrus.Fields{
			"func":     fn,
			"no_proxy": noProxy,
		}).Info("Updated no_proxy value")
	}

	k8sClient, err := CreateInClusterClient()
	if err != nil {
		logrus.WithField("func", fn).WithError(err).Error("Failed to create in-cluster k8s client")
		return &api.InjectEnvVariablesResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to create Kubernetes client: %v", err),
		}, err
	}

	logrus.WithField("func", fn).Info("Step 1: Preparing environment variables data")
	envData := make(map[string]string)
	if httpProxy != "" {
		envData["http_proxy"] = httpProxy
	}
	if httpsProxy != "" {
		envData["https_proxy"] = httpsProxy
	}
	if noProxy != "" {
		envData["no_proxy"] = noProxy
	}

	logrus.WithField("func", fn).Info("Step 2: Updating or creating ConfigMap")
	existingCM := &corev1.ConfigMap{}
	err = k8sClient.Get(ctx, k8stypes.NamespacedName{
		Name:      configMapName,
		Namespace: configMapNamespace,
	}, existingCM)

	if err == nil {
		logrus.WithField("func", fn).Info("ConfigMap exists, updating environment variables")
		if existingCM.Data == nil {
			existingCM.Data = make(map[string]string)
		}
		
		// Update or add non-empty values
		for key, value := range envData {
			existingCM.Data[key] = value
		}
		
		// Delete keys when empty values are sent
		if in.GetHttpProxy() == "" {
			delete(existingCM.Data, "http_proxy")
			logrus.WithField("func", fn).Info("Deleting http_proxy from ConfigMap")
		}
		if in.GetHttpsProxy() == "" {
			delete(existingCM.Data, "https_proxy")
			logrus.WithField("func", fn).Info("Deleting https_proxy from ConfigMap")
		}
		// If both http_proxy and https_proxy are empty, also delete no_proxy
		if in.GetHttpProxy() == "" && in.GetHttpsProxy() == "" {
			delete(existingCM.Data, "no_proxy")
			logrus.WithField("func", fn).Info("Deleting no_proxy from ConfigMap (both proxies are empty)")
		} else if in.GetNoProxy() == "" && noProxy == "" {
			// Only delete no_proxy if it's still empty after auto-append logic
			delete(existingCM.Data, "no_proxy")
			logrus.WithField("func", fn).Info("Deleting no_proxy from ConfigMap")
		}
		
		if err := k8sClient.Update(ctx, existingCM); err != nil {
			logrus.WithFields(logrus.Fields{"func": fn, "configmap": configMapName}).WithError(err).Error("Failed to update existing ConfigMap")
			return &api.InjectEnvVariablesResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to update existing ConfigMap: %v", err),
			}, err
		}
		logrus.WithFields(logrus.Fields{"func": fn, "total_env_count": len(existingCM.Data)}).Info("Successfully updated existing ConfigMap")
	} else {
		logrus.WithField("func", fn).Info("ConfigMap does not exist, creating new one")
		newCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configMapName,
				Namespace: configMapNamespace,
			},
			Data: envData,
		}
		if err := k8sClient.Create(ctx, newCM); err != nil {
			logrus.WithFields(logrus.Fields{"func": fn, "configmap": configMapName}).WithError(err).Error("Failed to create new ConfigMap")
			return &api.InjectEnvVariablesResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to create new ConfigMap: %v", err),
			}, err
		}
		logrus.WithFields(logrus.Fields{"func": fn, "env_count": len(envData)}).Info("Successfully created new ConfigMap")
	}

	logrus.WithField("func", fn).Info("Step 3: Triggering rollout restart of controller deployment")
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if getErr := k8sClient.Get(ctx, k8stypes.NamespacedName{
			Name:      deploymentName,
			Namespace: deploymentNs,
		}, deployment); getErr != nil {
			return getErr
		}
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = make(map[string]string)
		}
		deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)
		return k8sClient.Update(ctx, deployment)
	}); err != nil {
		logrus.WithFields(logrus.Fields{"func": fn, "deployment": deploymentName}).WithError(err).Error("Failed to trigger rollout restart of controller")
		return &api.InjectEnvVariablesResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to trigger rollout restart of controller: %v", err),
		}, err
	}
	logrus.WithField("func", fn).Info("Successfully triggered rollout restart of controller deployment")

	logrus.WithField("func", fn).Info("Step 4: Triggering rollout restart of vpwned-sdk deployment")
	vpwnedDeploymentName := "migration-vpwned-sdk"
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment := &appsv1.Deployment{}
		if getErr := k8sClient.Get(ctx, k8stypes.NamespacedName{
			Name:      vpwnedDeploymentName,
			Namespace: deploymentNs,
		}, deployment); getErr != nil {
			return getErr
		}
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = make(map[string]string)
		}
		deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)
		return k8sClient.Update(ctx, deployment)
	}); err != nil {
		logrus.WithFields(logrus.Fields{"func": fn, "deployment": vpwnedDeploymentName}).WithError(err).Error("Failed to trigger rollout restart of vpwned-sdk")
		return &api.InjectEnvVariablesResponse{
			Success: false,
			Message: fmt.Sprintf("Failed to trigger rollout restart of vpwned-sdk: %v", err),
		}, err
	}
	logrus.WithField("func", fn).Info("Successfully triggered rollout restart of vpwned-sdk deployment")

	successMsg := fmt.Sprintf("Successfully injected environment variables and restarted %s and %s deployments", deploymentName, vpwnedDeploymentName)
	logrus.WithField("func", fn).Info(successMsg)

	return &api.InjectEnvVariablesResponse{
		Success: true,
		Message: successMsg,
	}, nil
}
