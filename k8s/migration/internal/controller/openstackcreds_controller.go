/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"
	vjailbreakv1alpha1 "github.com/platform9/vjailbreak/k8s/migration/api/v1alpha1"
	constants "github.com/platform9/vjailbreak/k8s/migration/pkg/constants"
	scope "github.com/platform9/vjailbreak/k8s/migration/pkg/scope"
	utils "github.com/platform9/vjailbreak/k8s/migration/pkg/utils"
	openstackpkg "github.com/platform9/vjailbreak/pkg/openstack"
	openstackvalidation "github.com/platform9/vjailbreak/pkg/validation/openstack"
	"github.com/platform9/vjailbreak/v2v-helper/pkg/k8sutils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// OpenstackCredsReconciler reconciles a OpenstackCreds object
type OpenstackCredsReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	Local                   bool
	MaxConcurrentReconciles int
}

// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=openstackcreds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=openstackcreds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=openstackcreds/finalizers,verbs=update
// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=pcdhosts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=pcdhosts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=pcdhosts/finalizers,verbs=update
// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=pcdclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=pcdclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=pcdclusters/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the OpenstackCreds object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.4/pkg/reconcile
func (r *OpenstackCredsReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	ctxlog := log.FromContext(ctx).WithName(constants.OpenstackCredsControllerName)
	// Get the OpenstackCreds object
	openstackcreds := &vjailbreakv1alpha1.OpenstackCreds{}
	if err := r.Get(ctx, req.NamespacedName, openstackcreds); err != nil {
		if apierrors.IsNotFound(err) {
			ctxlog.Info("Resource not found, likely deleted", "openstackcreds", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		ctxlog.Error(err, "Failed to get OpenstackCreds resource", "openstackcreds", req.NamespacedName)
		return ctrl.Result{}, err
	}
	ctxlog.V(1).Info("Retrieved OpenstackCreds resource", "openstackcreds", req.NamespacedName, "resourceVersion", openstackcreds.ResourceVersion)
	scope, err := scope.NewOpenstackCredsScope(scope.OpenstackCredsScopeParams{
		Logger:         ctxlog,
		Client:         r.Client,
		OpenstackCreds: openstackcreds,
	})
	if err != nil {
		ctxlog.Error(err, "Failed to create OpenstackCredsScope")
		return ctrl.Result{}, err
	}

	// Always close the scope when exiting this function such that we can persist any OpenstackCreds changes.
	defer func() {
		if openstackcreds.DeletionTimestamp.IsZero() {
			if err := scope.Close(); err != nil && reterr == nil {
				ctxlog.Error(err, "Failed to close OpenstackCredsScope")
				reterr = err
			}
		}
	}()

	if !openstackcreds.DeletionTimestamp.IsZero() {
		ctxlog.Info("Resource is being deleted, reconciling deletion", "openstackcreds", req.NamespacedName)
		err := r.reconcileDelete(ctx, scope)
		return ctrl.Result{}, err
	}
	ctxlog.Info("Reconciling normal state", "openstackcreds", req.NamespacedName)
	return r.reconcileNormal(ctx, scope)
}

func (r *OpenstackCredsReconciler) reconcileNormal(ctx context.Context,
	scope *scope.OpenstackCredsScope) (ctrl.Result, error) { //nolint:unparam //future use
	ctxlog := scope.Logger
	ctxlog.Info("Reconciling OpenstackCreds")
	openstackcreds := scope.OpenstackCreds
	if !controllerutil.ContainsFinalizer(openstackcreds, constants.OpenstackCredsFinalizer) {
		controllerutil.AddFinalizer(openstackcreds, constants.OpenstackCredsFinalizer)
		if err := r.Update(ctx, openstackcreds); err != nil {
			ctxlog.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Check if spec matches with kubectl.kubernetes.io/last-applied-configuration
	result := openstackvalidation.Validate(ctx, r.Client, scope.OpenstackCreds)

	if !result.Valid {
		// Update the status of the OpenstackCreds object
		errMsg := result.Message
		if strings.Contains(errMsg, "Creds are valid but for a different OpenStack environment") {
			if r.Local {
				// At this point creds are valid but controller is not able to fetch metadata
				// that is why we are getting this above error
				err := handleValidatedCreds(ctx, r, scope)
				if err != nil {
					return ctrl.Result{}, err
				}
			}
			errMsg = "Creds are valid but for a different OpenStack environment. Enter creds of same OpenStack environment"
		}
		ctxlog.Error(result.Error, "Error validating OpenstackCreds", "openstackcreds", scope.OpenstackCreds.Name)
		scope.OpenstackCreds.Status.OpenStackValidationStatus = constants.ValidationStatusFailed
		scope.OpenstackCreds.Status.OpenStackValidationMessage = errMsg
		ctxlog.Info("Updating status to failed", "openstackcreds", scope.OpenstackCreds.Name, "message", errMsg)
		if err := r.Status().Update(ctx, scope.OpenstackCreds); err != nil {
			ctxlog.Error(err, "Error updating status of OpenstackCreds", "openstackcreds", scope.OpenstackCreds.Name)
			return ctrl.Result{}, err
		}
		ctxlog.Info("Successfully updated status to failed")
	} else {
		// Update the status of the OpenstackCreds object
		scope.OpenstackCreds.Status.OpenStackValidationStatus = string(corev1.PodSucceeded)
		scope.OpenstackCreds.Status.OpenStackValidationMessage = "Successfully authenticated to Openstack"
		ctxlog.Info("Updating status to success", "openstackcreds", scope.OpenstackCreds.Name)
		if err := r.Status().Update(ctx, scope.OpenstackCreds); err != nil {
			ctxlog.Error(err, "Error updating status of OpenstackCreds", "openstackcreds", scope.OpenstackCreds.Name)
			return ctrl.Result{}, err
		}
		ctxlog.Info("Successfully updated status to success")
		err := handleValidatedCreds(ctx, r, scope)
		if err != nil {
			return ctrl.Result{}, err
		}

		// Discover storage arrays from Cinder configuration
		if err := r.discoverStorageArrays(ctx, scope); err != nil {
			ctxlog.Error(err, "Failed to discover storage arrays")
			// Don't fail reconciliation, just log error
		}
	}
	// Get vjailbreak settings to get requeue after time
	vjailbreakSettings, err := k8sutils.GetVjailbreakSettings(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to get vjailbreak settings")
	}

	// Requeue to update the status of the OpenstackCreds object more specifically it will update flavors
	return ctrl.Result{Requeue: true, RequeueAfter: time.Duration(vjailbreakSettings.OpenstackCredsRequeueAfterMinutes) * time.Minute}, nil
}

func (r *OpenstackCredsReconciler) reconcileDelete(ctx context.Context, scope *scope.OpenstackCredsScope) error {
	ctxlog := scope.Logger
	ctxlog.Info("Reconciling deletion", "openstackcreds", scope.OpenstackCreds.Name, "namespace", scope.OpenstackCreds.Namespace)
	openstackcreds := scope.OpenstackCreds

	ctxlog.Info("Deleting PCD cluster entry", "openstackcreds", openstackcreds.Name)
	if err := utils.DeleteEntryForNoPCDCluster(ctx, r.Client, openstackcreds); err != nil {
		ctxlog.Error(err, "Failed to delete PCD cluster entry")
		return errors.Wrap(err, "failed to delete PCD cluster")
	}

	ctxlog.Info("Cleaning up associated PCDCluster resources")
	pcdClusterList := &vjailbreakv1alpha1.PCDClusterList{}
	labelSelector := client.MatchingLabels{constants.OpenstackCredsLabel: openstackcreds.Name}
	if err := r.List(ctx, pcdClusterList, client.InNamespace(openstackcreds.Namespace), labelSelector); err != nil {
		return errors.Wrap(err, "failed to list PCDClusters for cleanup")
	}
	for i := range pcdClusterList.Items {
		pcdCluster := pcdClusterList.Items[i]
		ctxlog.Info("Deleting dependent PCDCluster", "name", pcdCluster.Name)
		if err := r.Delete(ctx, &pcdCluster); err != nil && !apierrors.IsNotFound(err) {
			return errors.Wrap(err, "failed to delete dependent PCDCluster")
		}
	}

	if secretName := openstackcreds.Spec.SecretRef.Name; secretName != "" {
		ctxlog.Info("Deleting associated secret", "secretName", secretName)
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: constants.NamespaceMigrationSystem}}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			ctxlog.Error(err, "Failed to delete associated secret", "secretName", secretName)
			return errors.Wrap(err, "failed to delete associated secret")
		}
		ctxlog.Info("Successfully deleted associated secret or it was already gone", "secretName", secretName)
	}

	ctxlog.Info("All cleanup successful. Removing finalizer.")
	if controllerutil.RemoveFinalizer(openstackcreds, constants.OpenstackCredsFinalizer) {
		if err := r.Update(ctx, openstackcreds); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			ctxlog.Error(err, "failed to update resource to remove finalizer")
			return err
		}
	}

	return nil
}

// discoverStorageArrays discovers storage arrays from OpenStack Cinder configuration
func (r *OpenstackCredsReconciler) discoverStorageArrays(ctx context.Context, scope *scope.OpenstackCredsScope) error {
	ctxlog := scope.Logger
	ctxlog.Info("Discovering storage arrays from OpenStack Cinder")

	backendMap, err := utils.GetBackendPools(ctx, r.Client, scope.OpenstackCreds)
	if err != nil {
		return errors.Wrap(err, "failed to get backend pools")
	}

	ctxlog.Info("Discovered backend pools", "count", len(backendMap))

	// Process each volume type
	for backendName, backendInfo := range backendMap {
		ctxlog.Info("Processing backend pool", "backendName", backendName, "backendInfo", backendInfo)
		r.createArrayCreds(ctx, scope, backendName, backendInfo)
	}

	return nil
}

func (r *OpenstackCredsReconciler) createArrayCreds(ctx context.Context, scope *scope.OpenstackCredsScope, backendName string, backendInfo map[string]string) {
	ctxlog := scope.Logger
	// Create ArrayCreds name: <volumeType>-<backendName>
	arrayCredsName := generateArrayCredsName(backendInfo["volumeType"], backendName)

	// Create a unique identifier label for this array configuration
	// This allows users to rename the ArrayCreds while preventing duplicates
	arrayIdentifier := fmt.Sprintf("%s-%s", backendInfo["volumeType"], backendName)
	arrayIdentifierLabel := fmt.Sprintf("vjailbreak.k8s.pf9.io/array-id-%s", arrayIdentifier)

	// Check if ArrayCreds with this identifier already exists (by label)
	existingArrayCredsList := &vjailbreakv1alpha1.ArrayCredsList{}
	labelSelector := client.MatchingLabels{arrayIdentifierLabel: "true"}
	err := r.List(ctx, existingArrayCredsList, client.InNamespace(constants.NamespaceMigrationSystem), labelSelector)

	if err != nil {
		ctxlog.Error(err, "Failed to list ArrayCreds", "label", arrayIdentifierLabel)
	}

	if len(existingArrayCredsList.Items) > 0 {
		ctxlog.Info("ArrayCreds with this array configuration already exists (possibly renamed by user), skipping",
			"existingName", existingArrayCredsList.Items[0].Name,
			"volumeType", backendInfo["volumeType"],
			"backend", backendName)
		return
	}

	ctxlog.Info("Creating ArrayCreds", "name", arrayCredsName, "volumeType", backendInfo["volumeType"], "backend", backendName)

	vendor := utils.GetArrayVendor(backendInfo["vendor"])
	if vendor == "unsupported" {
		ctxlog.Error(errors.New("unsupported array vendor"), "Failed to create ArrayCreds", "name", arrayCredsName)
	}

	// Create new ArrayCreds
	arrayCreds := &vjailbreakv1alpha1.ArrayCreds{
		ObjectMeta: metav1.ObjectMeta{
			Name:      arrayCredsName,
			Namespace: constants.NamespaceMigrationSystem,
			Labels: map[string]string{
				constants.OpenstackCredsLabel:           scope.OpenstackCreds.Name,
				"vjailbreak.k8s.pf9.io/auto-discovered": "true",
				arrayIdentifierLabel:                    "true", // Unique identifier for this array config
			},
		},
		Spec: vjailbreakv1alpha1.ArrayCredsSpec{
			VendorType:     vendor,
			AutoDiscovered: true,
			OpenStackMapping: vjailbreakv1alpha1.OpenstackMapping{
				VolumeType:        backendInfo["volumeType"],
				CinderBackendName: backendName,
				CinderHost:        backendInfo["cinderHost"],
			},
			SecretRef: corev1.ObjectReference{
				Name:      fmt.Sprintf("%s-openstack-secret", scope.OpenstackCreds.Name), // Empty - awaiting user input
				Namespace: constants.NamespaceMigrationSystem,
			},
		},
	}

	// Set owner reference
	if err := controllerutil.SetControllerReference(scope.OpenstackCreds, arrayCreds, r.Scheme); err != nil {
		ctxlog.Error(err, "Failed to set owner reference", "arrayCredsName", arrayCredsName)
	}

	// Create ArrayCreds
	if err := r.Create(ctx, arrayCreds); err != nil {
		ctxlog.Error(err, "Failed to create ArrayCreds", "name", arrayCredsName)
	}

	ctxlog.Info("Successfully created ArrayCreds", "name", arrayCredsName, "volumeType", backendInfo["volumeType"], "backend", backendName)
}

// generateArrayCredsName generates a name for ArrayCreds
func generateArrayCredsName(volumeType, backendName string) string {
	// Sanitize names to be valid Kubernetes resource names
	sanitize := func(s string) string {
		s = strings.ToLower(s)
		s = strings.ReplaceAll(s, "_", "-")
		s = strings.ReplaceAll(s, " ", "-")
		// Remove any characters that aren't alphanumeric or hyphen
		var result strings.Builder
		for _, r := range s {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
				result.WriteRune(r)
			}
		}
		return result.String()
	}

	name := fmt.Sprintf("%s-%s", sanitize(volumeType), sanitize(backendName))

	// Ensure name doesn't exceed 63 characters
	if len(name) > 63 {
		name = name[:63]
	}

	// Remove trailing hyphens
	name = strings.TrimRight(name, "-")

	return name
}

// SetupWithManager sets up the controller with the Manager.
func (r *OpenstackCredsReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Get max concurrent reconciles from vjailbreak settings configmap
	return ctrl.NewControllerManagedBy(mgr).
		For(&vjailbreakv1alpha1.OpenstackCreds{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r)
}

func handleValidatedCreds(ctx context.Context, r *OpenstackCredsReconciler, scope *scope.OpenstackCredsScope) error {
	ctxlog := scope.Logger
	err := utils.UpdateMasterNodeImageID(ctx, r.Client, r.Local)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			ctxlog.Error(err, "Failed to update master node image ID and flavor list, skipping reconciliation")
		} else {
			return errors.Wrap(err, "failed to update master node image id")
		}
		ctxlog.Error(err, "Failed to update master node image ID and flavor list")
	}

	ctxlog.Info("Creating dummy PCD cluster", "openstackcreds", scope.OpenstackCreds.Name)
	err = utils.CreateDummyPCDClusterForStandAlonePCDHosts(ctx, r.Client, scope.OpenstackCreds)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(err, "failed to create dummy PCD cluster")
	}

	flavors, err := utils.ListAllFlavors(ctx, r.Client, scope.OpenstackCreds)
	if err != nil {
		ctxlog.Error(err, "Failed to get flavors", "openstackcreds", scope.OpenstackCreds.Name)
		return errors.Wrap(err, "failed to get flavors")
	}
	scope.OpenstackCreds.Spec.Flavors = flavors

	openstackCredential, err := utils.GetOpenstackCredentialsFromSecret(ctx, r.Client, scope.OpenstackCreds.Spec.SecretRef.Name)
	if err != nil {
		ctxlog.Error(err, "Failed to get OpenStack credentials from secret", "secretName", scope.OpenstackCreds.Spec.SecretRef.Name)
		return errors.Wrap(err, "failed to get Openstack credentials from secret")
	}

	if scope.OpenstackCreds.Spec.ProjectName != openstackCredential.TenantName && openstackCredential.TenantName != "" {
		ctxlog.Info("Updating spec.projectName from secret", "oldName", scope.OpenstackCreds.Spec.ProjectName, "newName", openstackCredential.TenantName)
		scope.OpenstackCreds.Spec.ProjectName = openstackCredential.TenantName
	}

	if err = r.Update(ctx, scope.OpenstackCreds); err != nil {
		ctxlog.Error(err, "Error updating spec of OpenstackCreds", "openstackcreds", scope.OpenstackCreds.Name)
		return errors.Wrap(err, "failed to update spec of OpenstackCreds")
	}

	// update the status field openstackInfo
	ctxlog.Info("Getting OpenStack info", "openstackcreds", scope.OpenstackCreds.Name)
	openstackinfo, err := utils.GetOpenstackInfo(ctx, r.Client, scope.OpenstackCreds)
	if err != nil {
		ctxlog.Error(err, "Failed to get OpenStack info", "openstackcreds", scope.OpenstackCreds.Name)
		return errors.Wrap(err, "failed to get Openstack info")
	}
	scope.OpenstackCreds.Status.Openstack = *openstackinfo
	ctxlog.Info("Updating OpenstackCreds status with info", "openstackcreds", scope.OpenstackCreds.Name)
	if err := r.Status().Update(ctx, scope.OpenstackCreds); err != nil {
		ctxlog.Error(err, "Error updating status of OpenstackCreds", "openstackcreds", scope.OpenstackCreds.Name)
		return errors.Wrap(err, "failed to update OpenstackCreds status")
	}

	// Get vjailbreak settings to check if we should populate VMwareMachine flavors
	vjailbreakSettings, err := k8sutils.GetVjailbreakSettings(ctx, r.Client)
	if err != nil {
		ctxlog.Error(err, "Failed to get vjailbreak settings")
		return errors.Wrap(err, "failed to get vjailbreak settings")
	}

	// Only populate flavors if the setting is enabled
	if vjailbreakSettings.PopulateVMwareMachineFlavors {
		ctxlog.Info("Populating VMwareMachine objects with OpenStack flavors", "openstackcreds", scope.OpenstackCreds.Name)
		// Now with these creds we should populate the flavors as labels in vmwaremachine object.
		// This will help us to create the vmwaremachine object with the correct flavor.
		vmwaremachineList := &vjailbreakv1alpha1.VMwareMachineList{}
		if err := r.List(ctx, vmwaremachineList); err != nil {
			return errors.Wrap(err, "failed to list vmwaremachine objects")
		}

		for i := range vmwaremachineList.Items {
			vmwaremachine := &vmwaremachineList.Items[i]
			// Get the cpu and memory of the vmwaremachine object
			cpu := vmwaremachine.Spec.VMInfo.CPU
			memory := vmwaremachine.Spec.VMInfo.Memory

			// Get GPU requirements from VM
			passthroughGPUCount := vmwaremachine.Spec.VMInfo.GPU.PassthroughCount
			vgpuCount := vmwaremachine.Spec.VMInfo.GPU.VGPUCount

			// Now get the closest flavor based on the cpu, memory, and GPU requirements
			flavor, err := openstackpkg.GetClosestFlavour(cpu, memory, passthroughGPUCount, vgpuCount, flavors, false)
			if err != nil && !strings.Contains(err.Error(), "no suitable flavor found") {
				ctxlog.Info(fmt.Sprintf("Error message '%s'", vmwaremachine.Name))
				return errors.Wrap(err, "failed to get closest flavor")
			}
			// Now label the vmwaremachine object with the flavor name
			if flavor == nil {
				if err := utils.CreateOrUpdateLabel(ctx, r.Client, vmwaremachine, scope.OpenstackCreds.Name, "NOT_FOUND"); err != nil {
					return errors.Wrap(err, "failed to update vmwaremachine object")
				}
			} else {
				if err := utils.CreateOrUpdateLabel(ctx, r.Client, vmwaremachine, scope.OpenstackCreds.Name, flavor.ID); err != nil {
					return errors.Wrap(err, "failed to update vmwaremachine object")
				}
			}
		}
	} else {
		ctxlog.Info("Skipping VMwareMachine flavor population as it is disabled", "openstackcreds", scope.OpenstackCreds.Name)
	}

	if utils.IsOpenstackPCD(*scope.OpenstackCreds) {
		// Check if a sync is already in progress
		if scope.OpenstackCreds.Annotations == nil {
			scope.OpenstackCreds.Annotations = make(map[string]string)
		}

		syncInProgress := scope.OpenstackCreds.Annotations["pcd-sync-in-progress"]
		if syncInProgress == "true" {
			ctxlog.Info("PCD sync already in progress, skipping", "openstackcreds", scope.OpenstackCreds.Name)
			return nil
		}

		// Mark sync as in progress
		scope.OpenstackCreds.Annotations["pcd-sync-in-progress"] = "true"
		if err := r.Update(ctx, scope.OpenstackCreds); err != nil {
			ctxlog.Error(err, "Failed to mark PCD sync as in progress")
			return nil
		}

		ctxlog.Info("Starting asynchronous PCD sync", "openstackcreds", scope.OpenstackCreds.Name)

		// Run sync asynchronously to avoid blocking the controller
		go func() {
			// Create a new context for the background operation (not tied to reconciliation)
			syncCtx := context.Background()

			err := utils.SyncPCDInfo(syncCtx, r.Client, *scope.OpenstackCreds)

			// Get the latest version of the resource to update annotations
			latestCreds := &vjailbreakv1alpha1.OpenstackCreds{}
			if getErr := r.Get(syncCtx, client.ObjectKey{
				Name:      scope.OpenstackCreds.Name,
				Namespace: scope.OpenstackCreds.Namespace,
			}, latestCreds); getErr != nil {
				ctxlog.Error(getErr, "Failed to get OpenstackCreds for annotation update")
				return
			}

			if latestCreds.Annotations == nil {
				latestCreds.Annotations = make(map[string]string)
			}

			// Clear the in-progress flag
			delete(latestCreds.Annotations, "pcd-sync-in-progress")

			if err != nil {
				ctxlog.Error(err, "PCD sync failed")
				latestCreds.Annotations["pcd-sync-last-error"] = err.Error()
			} else {
				ctxlog.Info("PCD sync completed successfully", "openstackcreds", scope.OpenstackCreds.Name)
				delete(latestCreds.Annotations, "pcd-sync-last-error")
			}

			if updateErr := r.Update(syncCtx, latestCreds); updateErr != nil {
				ctxlog.Error(updateErr, "Failed to update OpenstackCreds annotations after sync")
			}
		}()
	}
	return nil
}
