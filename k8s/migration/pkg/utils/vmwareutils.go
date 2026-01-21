// Package utils provides utility functions for handling migration-related operations.
// It includes functions for working with VMware environments, ESXi hosts, VM management,
// and integration with Platform9 components for migration workflows.
package utils

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/pkg/errors"
	vjailbreakv1alpha1 "github.com/platform9/vjailbreak/k8s/migration/api/v1alpha1"
	constants "github.com/platform9/vjailbreak/k8s/migration/pkg/constants"
	scope "github.com/platform9/vjailbreak/k8s/migration/pkg/scope"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GetClusterK8sID returns a unified identifier for a cluster
func GetClusterK8sID(clusterName, datacenter string) string {
	baseName := clusterName
	if baseName == "" {
		baseName = constants.VMwareClusterNameStandAloneESX
	}

	if datacenter != "" {
		return fmt.Sprintf("%s-%s", baseName, datacenter)
	}

	return baseName
}

// GetVMwareClustersAndHosts retrieves a list of all available VMware clusters and their hosts
func GetVMwareClustersAndHosts(ctx context.Context, scope *scope.VMwareCredsScope) ([]VMwareClusterInfo, error) {
	// Pre-allocate clusters slice with initial capacity
	clusters := make([]VMwareClusterInfo, 0, 4)
	vmwarecreds, err := GetVMwareCredentialsFromSecret(ctx, scope.Client, scope.VMwareCreds.Spec.SecretRef.Name)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get vCenter credentials")
	}

	_, finder, err := GetFinderForVMwareCreds(ctx, scope.Client, scope.VMwareCreds, "")
	if err != nil {
		return nil, errors.Wrap(err, "failed to get finder for vCenter credentials")
	}

	var targetDatacenters []*object.Datacenter
	if vmwarecreds.Datacenter != "" {
		dc, err := finder.Datacenter(ctx, vmwarecreds.Datacenter)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to find specified datacenter %s", vmwarecreds.Datacenter)
		}
		targetDatacenters = []*object.Datacenter{dc}
	} else {
		// Fetch all datacenters
		targetDatacenters, err = finder.DatacenterList(ctx, "*")
		if err != nil {
			return nil, errors.Wrap(err, "failed to list all datacenters")
		}
	}

	// Iterate over each datacenter to find clusters
	for _, dc := range targetDatacenters {
		finder.SetDatacenter(dc)
		clusterList, err := finder.ClusterComputeResourceList(ctx, "*")
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				continue
			}
			return nil, errors.Wrapf(err, "failed to get cluster list for datacenter %s", dc.Name())
		}

		for _, cluster := range clusterList {
			var clusterProperties mo.ClusterComputeResource
			err := cluster.Properties(ctx, cluster.Reference(), []string{"name"}, &clusterProperties)
			if err != nil {
				return nil, errors.Wrap(err, "failed to get cluster properties")
			}

			hosts, err := cluster.Hosts(ctx)
			if err != nil {
				return nil, errors.Wrap(err, "failed to get hosts")
			}
			var vmHosts []VMwareHostInfo
			for _, host := range hosts {
				hostSummary, err := GetESXiSummary(ctx, scope.Client, host.Name(), scope.VMwareCreds, dc.Name())
				if err != nil {
					return nil, errors.Wrap(err, "failed to get ESXi summary")
				}
				vmHosts = append(vmHosts, VMwareHostInfo{Name: host.Name(), HardwareUUID: hostSummary.Summary.Hardware.Uuid})
			}
			clusters = append(clusters, VMwareClusterInfo{
				Name:       clusterProperties.Name,
				Hosts:      vmHosts,
				Datacenter: dc.Name(),
			})
		}
	}
	return clusters, nil
}

// createVMwareHost creates a VMware host resource in Kubernetes
func createVMwareHost(ctx context.Context, scope *scope.VMwareCredsScope, host VMwareHostInfo, credName, clusterName, namespace, datacenter string) (string, error) {
	hostk8sName, err := GetK8sCompatibleVMWareObjectName(host.Name, credName)
	if err != nil {
		return "", errors.Wrap(err, "failed to convert host name to k8s name")
	}

	clusterK8sID := clusterName
	if datacenter != "" {
		clusterK8sID = fmt.Sprintf("%s-%s", clusterName, datacenter)
	}
	clusterk8sName, err := GetK8sCompatibleVMWareObjectName(clusterK8sID, credName)
	if err != nil {
		return "", errors.Wrap(err, "failed to convert cluster name to k8s name")
	}

	labels := map[string]string{
		constants.VMwareClusterLabel: clusterk8sName,
		constants.VMwareCredsLabel:   credName,
	}
	annotations := map[string]string{}
	if datacenter != "" {
		annotations[constants.VMwareDatacenterLabel] = datacenter
	}

	vmwareHost := vjailbreakv1alpha1.VMwareHost{
		ObjectMeta: metav1.ObjectMeta{
			Name:        hostk8sName,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: vjailbreakv1alpha1.VMwareHostSpec{
			Name:         host.Name,
			HardwareUUID: host.HardwareUUID,
			ClusterName:  clusterName,
		},
	}
	existingHost := vjailbreakv1alpha1.VMwareHost{}
	if err := scope.Client.Get(ctx, client.ObjectKey{Name: hostk8sName, Namespace: namespace}, &existingHost); err == nil {
		if existingHost.Spec.Name != host.Name || existingHost.Spec.HardwareUUID != host.HardwareUUID || existingHost.Spec.ClusterName != clusterName || !reflect.DeepEqual(existingHost.Labels, vmwareHost.Labels) || !reflect.DeepEqual(existingHost.Annotations, vmwareHost.Annotations) {
			existingHost.Spec = vmwareHost.Spec
			existingHost.Labels = vmwareHost.Labels
			existingHost.Annotations = vmwareHost.Annotations
			updateErr := scope.Client.Update(ctx, &existingHost)
			if updateErr != nil {
				return "", errors.Wrap(updateErr, "failed to update vmware host")
			}
		}
	} else {
		err = scope.Client.Create(ctx, &vmwareHost)
		if err != nil && !apierrors.IsAlreadyExists(err) {
			return "", errors.Wrap(err, "failed to create vmware host")
		}
	}

	return hostk8sName, nil
}

// createVMwareCluster creates a VMware cluster resource in Kubernetes
func createVMwareCluster(ctx context.Context, scope *scope.VMwareCredsScope, cluster VMwareClusterInfo) error {
	log := scope.Logger

	clusterK8sID := GetClusterK8sID(cluster.Name, cluster.Datacenter)

	clusterk8sName, err := GetK8sCompatibleVMWareObjectName(clusterK8sID, scope.Name())
	if err != nil {
		return errors.Wrap(err, "failed to convert cluster name to k8s name")
	}

	labels := map[string]string{
		constants.VMwareCredsLabel: scope.Name(),
	}
	annotations := map[string]string{}
	if cluster.Datacenter != "" {
		annotations[constants.VMwareDatacenterLabel] = cluster.Datacenter
	}

	vmwareCluster := vjailbreakv1alpha1.VMwareCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:        clusterk8sName,
			Namespace:   scope.Namespace(),
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: vjailbreakv1alpha1.VMwareClusterSpec{
			Name: cluster.Name,
		},
	}

	// Create hosts and collect their k8s names
	for _, host := range cluster.Hosts {
		log.Info("Processing VMware host", "host", host.Name)
		hostk8sName, err := createVMwareHost(ctx, scope, host, scope.Name(), cluster.Name, scope.Namespace(), cluster.Datacenter)
		if err != nil {
			return err
		}
		vmwareCluster.Spec.Hosts = append(vmwareCluster.Spec.Hosts, hostk8sName)
	}

	// Create the cluster
	existingCluster := vjailbreakv1alpha1.VMwareCluster{}
	if err := scope.Client.Get(ctx, client.ObjectKey{Name: clusterk8sName, Namespace: scope.Namespace()}, &existingCluster); err == nil {
		if existingCluster.Annotations == nil {
			existingCluster.Annotations = make(map[string]string)
		}
		needsUpdate := existingCluster.Spec.Name != cluster.Name ||
			!reflect.DeepEqual(existingCluster.Labels, vmwareCluster.Labels) ||
			!reflect.DeepEqual(existingCluster.Annotations, vmwareCluster.Annotations) ||
			!reflect.DeepEqual(existingCluster.Spec.Hosts, vmwareCluster.Spec.Hosts)

		if needsUpdate {
			log.Info("Updating VMware cluster", "cluster", cluster.Name, "datacenter", cluster.Datacenter, "hasAnnotations", len(vmwareCluster.Annotations) > 0)
			existingCluster.Spec = vmwareCluster.Spec
			existingCluster.Labels = vmwareCluster.Labels
			existingCluster.Annotations = vmwareCluster.Annotations
			updateErr := scope.Client.Update(ctx, &existingCluster)
			if updateErr != nil {
				return errors.Wrap(updateErr, "failed to update vmware cluster")
			}
		}
	} else {
		log.Info("Creating VMware cluster", "cluster", cluster.Name, "datacenter", cluster.Datacenter, "hasAnnotations", len(vmwareCluster.Annotations) > 0)
		createErr := scope.Client.Create(ctx, &vmwareCluster)
		if createErr != nil && !apierrors.IsAlreadyExists(createErr) {
			return errors.Wrap(createErr, "failed to create vmware cluster")
		}
	}

	return nil
}

// CreateVMwareClustersAndHosts creates VMware clusters and hosts
func CreateVMwareClustersAndHosts(ctx context.Context, scope *scope.VMwareCredsScope) error {
	log := scope.Logger

	clusters, err := GetVMwareClustersAndHosts(ctx, scope)
	if err != nil {
		return errors.Wrap(err, "failed to get clusters and hosts")
	}

	// Create a dummy cluster for standalone ESX
	if err := CreateDummyClusterForStandAloneESX(ctx, scope, clusters); err != nil {
		return errors.Wrap(err, "failed to create dummy cluster for standalone ESX")
	}

	for _, cluster := range clusters {
		log.Info("Processing VMware cluster", "cluster", cluster.Name, "datacenter", cluster.Datacenter)
		if err := createVMwareCluster(ctx, scope, cluster); err != nil {
			return err
		}
	}
	return nil
}

// DeleteStaleVMwareClustersAndHosts removes VMware cluster and host resources that no longer exist in vCenter.
// This helps maintain synchronization between Kubernetes resources and the actual infrastructure state.
func DeleteStaleVMwareClustersAndHosts(ctx context.Context, scope *scope.VMwareCredsScope) error {
	clusters, err := GetVMwareClustersAndHosts(ctx, scope)
	if err != nil {
		return errors.Wrap(err, "failed to get clusters and hosts")
	}

	// add entry for dummy cluster so save it from cleanup
	vmwarecreds, err := GetVMwareCredentialsFromSecret(ctx, scope.Client, scope.VMwareCreds.Spec.SecretRef.Name)
	if err != nil {
		return errors.Wrap(err, "failed to get vCenter credentials")
	}

	_, finder, err := GetFinderForVMwareCreds(ctx, scope.Client, scope.VMwareCreds, "")
	if err != nil {
		return errors.Wrap(err, "failed to get finder for vCenter credentials")
	}

	// Get list of all target datacenters
	var targetDatacenters []*object.Datacenter
	if vmwarecreds.Datacenter != "" {
		dc, err := finder.Datacenter(ctx, vmwarecreds.Datacenter)
		if err != nil {
			return errors.Wrapf(err, "failed to find specified datacenter %s", vmwarecreds.Datacenter)
		}
		targetDatacenters = []*object.Datacenter{dc}
	} else {
		targetDatacenters, err = finder.DatacenterList(ctx, "*")
		if err != nil {
			return errors.Wrap(err, "failed to list all datacenters")
		}
	}

	standAloneHosts, err := FetchStandAloneESXHostsFromVcenter(ctx, scope, clusters)
	if err != nil {
		return errors.Wrap(err, "failed to fetch standalone ESX hosts")
	}

	for _, dc := range targetDatacenters {
		dcName := dc.Name()
		hosts, ok := standAloneHosts[dcName]
		if !ok {
			hosts = []*object.HostSystem{}
		}
		vmHosts := make([]VMwareHostInfo, 0, len(hosts))
		for _, host := range hosts {
			vmHosts = append(vmHosts, VMwareHostInfo{
				Name: host.Name(),
			})
		}
		clusters = append(clusters, VMwareClusterInfo{
			Name:       constants.VMwareClusterNameStandAloneESX,
			Hosts:      vmHosts,
			Datacenter: dcName,
		})
	}

	existingClusters := vjailbreakv1alpha1.VMwareClusterList{}
	if err := scope.Client.List(ctx, &existingClusters, client.MatchingLabels{constants.VMwareCredsLabel: scope.Name()}); err != nil {
		return errors.Wrap(err, "failed to list vmware clusters")
	}

	existingHosts := vjailbreakv1alpha1.VMwareHostList{}
	if err := scope.Client.List(ctx, &existingHosts, client.MatchingLabels{constants.VMwareCredsLabel: scope.Name()}); err != nil {
		return errors.Wrap(err, "failed to list vmware hosts")
	}

	// Create a map of valid cluster names for O(1) lookups
	clusterNames := make(map[string]bool)
	for _, cluster := range clusters {
		clusterK8sID := GetClusterK8sID(cluster.Name, cluster.Datacenter)
		cname, err := GetK8sCompatibleVMWareObjectName(clusterK8sID, scope.Name())
		if err != nil {
			return errors.Wrap(err, "failed to convert cluster name to k8s name")
		}
		clusterNames[cname] = true
	}

	// Delete only clusters that don't exist in vSphere anymore
	for _, existingCluster := range existingClusters.Items {
		if !clusterNames[existingCluster.Name] {
			if err := scope.Client.Delete(ctx, &existingCluster); err != nil {
				return errors.Wrap(err, "failed to delete stale vmware cluster")
			}
		}
	}

	// Create a map of valid host names for O(1) lookups
	hostNames := make(map[string]bool)
	for _, cluster := range clusters {
		for _, host := range cluster.Hosts {
			hname, err := GetK8sCompatibleVMWareObjectName(host.Name, scope.Name())
			if err != nil {
				return errors.Wrap(err, "failed to convert host name to k8s name")
			}
			hostNames[hname] = true
		}
	}

	// Delete only hosts that don't exist in vSphere anymore
	for _, existingHost := range existingHosts.Items {
		if !hostNames[existingHost.Name] {
			if err := scope.Client.Delete(ctx, &existingHost); err != nil {
				return errors.Wrap(err, "failed to delete stale vmware host")
			}
		}
	}
	return nil
}

// FilterVMwareHostsForCluster returns a list of VMwareHost resources associated with the specified cluster
// It filters the hosts by the VMwareClusterLabel matching the provided cluster name
func FilterVMwareHostsForCluster(ctx context.Context, k8sClient client.Client, clusterName string) ([]vjailbreakv1alpha1.VMwareHost, error) {
	// List all VMwareHost resources
	vmwareHosts := &vjailbreakv1alpha1.VMwareHostList{}

	// Filter VMwareHost resources by cluster name
	if err := k8sClient.List(ctx, vmwareHosts, client.MatchingLabels{constants.VMwareClusterLabel: clusterName}); err != nil {
		return nil, errors.Wrap(err, "failed to list VMwareHost resources")
	}

	return vmwareHosts.Items, nil
}

// FetchStandAloneESXHostsFromVcenter fetches standalone ESX hosts from vCenter
func FetchStandAloneESXHostsFromVcenter(ctx context.Context, scope *scope.VMwareCredsScope, clusters []VMwareClusterInfo) (map[string][]*object.HostSystem, error) {
	// Create a map of all hosts that are part of clusters for O(1) lookups
	clusteredHosts := make(map[string]bool)
	for _, cluster := range clusters {
		for _, host := range cluster.Hosts {
			clusteredHosts[host.Name] = true
		}
	}

	// Get VMware credentials to connect to vCenter
	vmwareCredsInfo, err := GetVMwareCredentialsFromSecret(ctx, scope.Client, scope.VMwareCreds.Spec.SecretRef.Name)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get vCenter credentials")
	}

	// Get finder for vCenter
	_, finder, err := GetFinderForVMwareCreds(ctx, scope.Client, scope.VMwareCreds, "")
	if err != nil {
		return nil, errors.Wrap(err, "failed to get finder for vCenter credentials")
	}

	var targetDatacenters []*object.Datacenter
	if vmwareCredsInfo.Datacenter != "" {
		dc, err := finder.Datacenter(ctx, vmwareCredsInfo.Datacenter)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to find specified datacenter %s", vmwareCredsInfo.Datacenter)
		}
		targetDatacenters = []*object.Datacenter{dc}
	} else {
		targetDatacenters, err = finder.DatacenterList(ctx, "*")
		if err != nil {
			return nil, errors.Wrap(err, "failed to list all datacenters")
		}
	}

	// Result map
	result := make(map[string][]*object.HostSystem)

	for _, dc := range targetDatacenters {
		finder.SetDatacenter(dc)
		hostList, err := finder.HostSystemList(ctx, "*")
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				continue
			}
			return nil, errors.Wrapf(err, "failed to get host list for datacenter %s", dc.Name())
		}

		for _, host := range hostList {
			// if part of a cluster, skip
			if clusteredHosts[host.Name()] {
				continue
			}
			result[dc.Name()] = append(result[dc.Name()], host)
		}
	}

	return result, nil
}

// CreateDummyClusterForStandAloneESX creates a VMware cluster for standalone ESX
func CreateDummyClusterForStandAloneESX(ctx context.Context, scope *scope.VMwareCredsScope, existingClusters []VMwareClusterInfo) error {
	log := scope.Logger

	vmwarecreds, err := GetVMwareCredentialsFromSecret(ctx, scope.Client, scope.VMwareCreds.Spec.SecretRef.Name)
	if err != nil {
		return errors.Wrap(err, "failed to get vCenter credentials")
	}

	_, finder, err := GetFinderForVMwareCreds(ctx, scope.Client, scope.VMwareCreds, "")
	if err != nil {
		return errors.Wrap(err, "failed to get finder for vCenter credentials")
	}

	// Get list of all target datacenters
	var targetDatacenters []*object.Datacenter
	if vmwarecreds.Datacenter != "" {
		dc, err := finder.Datacenter(ctx, vmwarecreds.Datacenter)
		if err != nil {
			return errors.Wrapf(err, "failed to find specified datacenter %s", vmwarecreds.Datacenter)
		}
		targetDatacenters = []*object.Datacenter{dc}
	} else {
		targetDatacenters, err = finder.DatacenterList(ctx, "*")
		if err != nil {
			return errors.Wrap(err, "failed to list all datacenters")
		}
	}
	standAloneHosts, err := FetchStandAloneESXHostsFromVcenter(ctx, scope, existingClusters)
	if err != nil {
		return errors.Wrap(err, "failed to fetch standalone ESX hosts")
	}
	for _, dc := range targetDatacenters {
		dcName := dc.Name()
		hosts := standAloneHosts[dcName]

		dummyClusterInfo := VMwareClusterInfo{
			Name:       constants.VMwareClusterNameStandAloneESX,
			Datacenter: dcName,
		}

		// Convert objects to HostInfo
		for _, host := range hosts {
			hostSummary, err := GetESXiSummary(ctx, scope.Client, host.Name(), scope.VMwareCreds, dcName)
			if err != nil {
				return errors.Wrap(err, "failed to get ESXi summary")
			}
			dummyClusterInfo.Hosts = append(dummyClusterInfo.Hosts, VMwareHostInfo{
				Name:         host.Name(),
				HardwareUUID: hostSummary.Summary.Hardware.Uuid,
			})
		}

		if err := createVMwareCluster(ctx, scope, dummyClusterInfo); err != nil {
			return err
		}
	}
	return nil
}
