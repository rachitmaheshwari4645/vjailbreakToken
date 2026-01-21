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
	"os"
	"os/user"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/pkg/errors"
	vjailbreakv1alpha1 "github.com/platform9/vjailbreak/k8s/migration/api/v1alpha1"
	"github.com/platform9/vjailbreak/k8s/migration/pkg/constants"
	"github.com/platform9/vjailbreak/k8s/migration/pkg/scope"
	utils "github.com/platform9/vjailbreak/k8s/migration/pkg/utils"
	"github.com/platform9/vjailbreak/k8s/migration/pkg/verrors"
	openstackpkg "github.com/platform9/vjailbreak/pkg/common/openstack"
	"github.com/platform9/vjailbreak/v2v-helper/pkg/k8sutils"
	"github.com/platform9/vjailbreak/v2v-helper/vcenter"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/session"
	govmomitypes "github.com/vmware/govmomi/vim25/types"
)

// VDDKDirectory is the path to VMware VDDK installation directory used for VM disk conversion
const VDDKDirectory = "/home/ubuntu/vmware-vix-disklib-distrib"

// StorageCopyMethod is the storage copy method value for Storage Accelerated copy
const StorageCopyMethod = "StorageAcceleratedCopy"

// MigrationPlanReconciler reconciles a MigrationPlan object
type MigrationPlanReconciler struct {
	client.Client
	Scheme                  *runtime.Scheme
	ctxlog                  logr.Logger
	MaxConcurrentReconciles int
}

var migrationPlanFinalizer = "migrationplan.vjailbreak.pf9.io/finalizer"

// The default image. This is replaced by Go linker flags in the Dockerfile
var v2vimage = "platform9/v2v-helper:v0.1"

// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods/log,verbs=get;list
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete

// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=migrationplans,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=migrationplans/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=migrationplans/finalizers,verbs=update
// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=migrationtemplates,verbs=get;list;watch

// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=migrationtemplates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=migrationtemplates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=vjailbreak.k8s.pf9.io,resources=migrationtemplates/finalizers,verbs=update

// Reconcile reads that state of the cluster for a MigrationPlan object and makes necessary changes
func (r *MigrationPlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	r.ctxlog = log.FromContext(ctx)
	migrationplan := &vjailbreakv1alpha1.MigrationPlan{}

	if err := r.Get(ctx, req.NamespacedName, migrationplan); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		r.ctxlog.Error(err, fmt.Sprintf("failed to read MigrationPlan '%s'", migrationplan.Name))
		return ctrl.Result{}, errors.Wrapf(err, "failed to read MigrationPlan '%s'", migrationplan.Name)
	}

	err := utils.ValidateMigrationPlan(migrationplan)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to validate MigrationPlan")
	}

	// Set default migration type if not provided
	if migrationplan.Spec.MigrationStrategy.Type == "" {
		vjailbreakSettings, err := k8sutils.GetVjailbreakSettings(ctx, r.Client)
		if err != nil {
			r.ctxlog.Error(err, "Failed to get vjailbreak settings")
			return ctrl.Result{}, errors.Wrap(err, "failed to get vjailbreak settings")
		}
		migrationplan.Spec.MigrationStrategy.Type = vjailbreakSettings.DefaultMigrationMethod
		// Update the spec
		if err := r.Update(ctx, migrationplan); err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to update migration plan with default type")
		}
	}

	migrationPlanScope, err := scope.NewMigrationPlanScope(scope.MigrationPlanScopeParams{
		Logger:        r.ctxlog,
		Client:        r.Client,
		MigrationPlan: migrationplan,
	})
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to create scope")
	}

	// Always close the scope when exiting this function such that we can persist any MigrationPlan changes.
	defer func() {
		if err := migrationPlanScope.Close(); err != nil && reterr == nil {
			reterr = err
		}
	}()

	// examine DeletionTimestamp to determine if object is under deletion or not
	if !migrationplan.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, migrationPlanScope)
	}

	return r.reconcileNormal(ctx, migrationPlanScope)
}

func (r *MigrationPlanReconciler) reconcileNormal(ctx context.Context, scope *scope.MigrationPlanScope) (ctrl.Result, error) {
	migrationplan := scope.MigrationPlan
	log := scope.Logger
	log.Info(fmt.Sprintf("Reconciling MigrationPlan '%s'", migrationplan.Name))

	controllerutil.AddFinalizer(migrationplan, migrationPlanFinalizer)

	res, err := r.ReconcileMigrationPlanJob(ctx, migrationplan, scope)
	if err != nil {
		return res, errors.Wrap(err, "failed to reconcile migration plan job")
	}
	return res, nil
}

//nolint:unparam //future use
func (r *MigrationPlanReconciler) reconcileDelete(
	ctx context.Context,
	scope *scope.MigrationPlanScope,
) (ctrl.Result, error) {
	migrationplan := scope.MigrationPlan
	ctxlog := log.FromContext(ctx).WithName(constants.MigrationControllerName)

	// The object is being deleted
	ctxlog.Info(fmt.Sprintf("MigrationPlan '%s' CR is being deleted", migrationplan.Name))

	// Now that the finalizer has completed deletion tasks, we can remove it
	// to allow deletion of the Migration object
	controllerutil.RemoveFinalizer(migrationplan, migrationPlanFinalizer)
	if err := r.Update(ctx, migrationplan); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to remove finalizer")
	}

	return ctrl.Result{}, nil
}

func (r *MigrationPlanReconciler) getMigrationTemplateAndCreds(
	ctx context.Context,
	migrationplan *vjailbreakv1alpha1.MigrationPlan,
) (*vjailbreakv1alpha1.MigrationTemplate, *vjailbreakv1alpha1.VMwareCreds, *corev1.Secret, error) {
	ctxlog := log.FromContext(ctx)

	migrationtemplate := &vjailbreakv1alpha1.MigrationTemplate{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      migrationplan.Spec.MigrationTemplate,
		Namespace: migrationplan.Namespace,
	}, migrationtemplate); err != nil {
		ctxlog.Error(err, "Failed to get MigrationTemplate")
		return nil, nil, nil, errors.Wrap(err, "failed to get MigrationTemplate")
	}

	vmwcreds := &vjailbreakv1alpha1.VMwareCreds{}
	if ok, err := r.checkStatusSuccess(ctx, migrationtemplate.Namespace, migrationtemplate.Spec.Source.VMwareRef, true, vmwcreds); !ok {
		return nil, nil, nil, errors.Wrap(err, "VMwareCreds not validated")
	}

	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      vmwcreds.Spec.SecretRef.Name,
		Namespace: migrationplan.Namespace,
	}, secret); err != nil {
		return nil, nil, nil, errors.Wrap(err, "failed to get vCenter Secret")
	}

	return migrationtemplate, vmwcreds, secret, nil
}

func (r *MigrationPlanReconciler) reconcilePostMigration(ctx context.Context, scope *scope.MigrationPlanScope, vm string) error {
	migrationplan := scope.MigrationPlan
	ctxlog := log.FromContext(ctx).WithName(constants.MigrationControllerName)

	if migrationplan.Spec.PostMigrationAction == nil {
		ctxlog.Info("No post-migration actions configured")
		return nil
	}

	if migrationplan.Spec.PostMigrationAction.RenameVM == nil &&
		migrationplan.Spec.PostMigrationAction.MoveToFolder == nil {
		ctxlog.Info("No post-migration actions enabled")
		return nil
	}

	// Get required resources
	_, vmwcreds, secret, err := r.getMigrationTemplateAndCreds(ctx, migrationplan)
	if err != nil {
		return errors.Wrap(err, "failed to get migration resources")
	}

	// Extract and validate credentials
	username, password, host, err := extractVCenterCredentials(secret)
	if err != nil {
		return errors.Wrap(err, "invalid vCenter credentials")
	}

	// Create vCenter client and get datacenter
	vcClient, dc, err := createVCenterClientAndDC(ctx, host, username, password, vmwcreds.Spec.DataCenter)
	if err != nil {
		return errors.Wrap(err, "failed to create vCenter client")
	}
	defer func() {
		if vcClient.VCClient != nil {
			sessionManager := session.NewManager(vcClient.VCClient)
			err = sessionManager.Logout(ctx) // Best effort logout
			if err != nil {
				ctxlog.Error(err, "Failed to logout from vCenter")
			}
		}
	}()

	if migrationplan.Spec.PostMigrationAction.RenameVM != nil && *migrationplan.Spec.PostMigrationAction.RenameVM {
		if err := r.renameVM(ctx, vcClient, migrationplan, vm); err != nil {
			return errors.Wrap(err, "failed to rename VM")
		}
		vm += migrationplan.Spec.PostMigrationAction.Suffix
	}

	if migrationplan.Spec.PostMigrationAction.MoveToFolder != nil && *migrationplan.Spec.PostMigrationAction.MoveToFolder {
		if err := r.moveVMToFolder(ctx, vcClient, dc, migrationplan, vm); err != nil {
			return errors.Wrap(err, "failed to move VM to folder")
		}
	}

	return nil
}

func (*MigrationPlanReconciler) renameVM(
	ctx context.Context,
	vcClient *vcenter.VCenterClient,
	migrationplan *vjailbreakv1alpha1.MigrationPlan,
	vm string,
) error {
	ctxlog := log.FromContext(ctx)
	suffix := migrationplan.Spec.PostMigrationAction.Suffix
	if suffix == "" {
		suffix = "_migrated_to_pcd"
		ctxlog.Info("Using default suffix", "suffix", suffix)
	}
	newVMName := vm + suffix
	ctxlog.Info("Renaming VM", "oldName", vm, "newName", newVMName)
	return vcClient.RenameVM(ctx, vm, newVMName)
}

func (*MigrationPlanReconciler) moveVMToFolder(
	ctx context.Context,
	vcClient *vcenter.VCenterClient,
	dc *object.Datacenter,
	migrationplan *vjailbreakv1alpha1.MigrationPlan,
	vm string,
) error {
	ctxlog := log.FromContext(ctx)
	folderName := migrationplan.Spec.PostMigrationAction.FolderName
	if folderName == "" {
		folderName = "vjailbreakedVMs"
		ctxlog.Info("Using default folder name", "folderName", folderName)
	}

	ctxlog.Info("Ensuring folder exists...", "folder", folderName)
	if _, err := EnsureVMFolderExists(ctx, vcClient.VCFinder, dc, folderName); err != nil {
		ctxlog.Error(err, "Folder creation/verification failed")
		return errors.Wrapf(err, "failed to ensure folder '%s' exists", folderName)
	}

	ctxlog.Info("Moving VM to folder", "vm", vm, "folder", folderName)
	if err := vcClient.MoveVMFolder(ctx, vm, folderName); err != nil {
		ctxlog.Error(err, "VM move failed")
		return errors.Wrapf(err, "failed to move VM '%s' to folder '%s'", vm, folderName)
	}
	return nil
}

func createVCenterClientAndDC(
	ctx context.Context,
	host, username, password, datacenterName string,
) (*vcenter.VCenterClient, *object.Datacenter, error) {
	ctxlog := log.FromContext(ctx)
	ctxlog.Info("Creating vCenter client...", "host", host, "insecure", true)

	vcClient, err := vcenter.VCenterClientBuilder(ctx, username, password, host, true)
	if err != nil {
		ctxlog.Error(err, "Failed to create vCenter client")
		return nil, nil, errors.Wrapf(err, "failed to create vCenter client")
	}
	ctxlog.Info("vCenter client created successfully")

	if datacenterName == "" {
		ctxlog.Info("No datacenter specified, returning client without datacenter scope")
		return vcClient, nil, nil
	}

	ctxlog.Info("Using datacenter", "datacenter", datacenterName)
	dc, err := vcClient.VCFinder.Datacenter(ctx, datacenterName)
	if err != nil {
		ctxlog.Error(err, "Failed to find datacenter")
		return nil, nil, errors.Wrapf(err, "failed to find datacenter '%s'", datacenterName)
	}
	ctxlog.Info("Datacenter located", "datacenter", dc)

	return vcClient, dc, nil
}

func extractVCenterCredentials(secret *corev1.Secret) (username, password, host string, err error) {
	u, ok := secret.Data["VCENTER_USERNAME"]
	if !ok {
		err = errors.New("username not found in secret")
		return
	}
	p, ok := secret.Data["VCENTER_PASSWORD"]
	if !ok {
		err = errors.New("password not found in secret")
		return
	}
	h, ok := secret.Data["VCENTER_HOST"]
	if !ok {
		err = errors.New("host not found in secret")
		return
	}
	username = string(u)
	password = string(p)
	host = string(h)
	return
}

// GetVMwareMachineForVM fetches the VMwareMachine corresponding to a given VM name
func GetVMwareMachineForVM(ctx context.Context, r *MigrationPlanReconciler, vm string, migrationtemplate *vjailbreakv1alpha1.MigrationTemplate, vmwcreds *vjailbreakv1alpha1.VMwareCreds) (*vjailbreakv1alpha1.VMwareMachine, error) {
	// Generate the expected VMwareMachine name
	vmk8sname, err := utils.GetK8sCompatibleVMWareObjectName(vm, vmwcreds.Name)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get k8s compatible name for VM %s", vm)
	}

	// Fetch individual VMwareMachine
	vmMachine := &vjailbreakv1alpha1.VMwareMachine{}
	err = r.Get(ctx, types.NamespacedName{
		Name:      vmk8sname,
		Namespace: migrationtemplate.Namespace,
	}, vmMachine)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errors.Errorf("VMwareMachine %s not found for VM %s", vmk8sname, vm)
		}
		return nil, errors.Wrapf(err, "failed to get VMwareMachine %s for VM %s", vmk8sname, vm)
	}

	// Verify VMwareMachine has correct VMwareCreds label
	if vmMachine.Labels == nil {
		return nil, errors.Errorf("VMwareMachine %s has no labels", vmMachine.Name)
	}

	expectedLabel := vmwcreds.Name
	actualLabel, exists := vmMachine.Labels[constants.VMwareCredsLabel]
	if !exists {
		return nil, errors.Errorf("VMwareMachine %s missing required label %s", vmMachine.Name, constants.VMwareCredsLabel)
	}

	if actualLabel != expectedLabel {
		return nil, errors.Errorf("VMwareMachine %s has incorrect VMwareCreds label: expected %s, got %s", vmMachine.Name, expectedLabel, actualLabel)
	}

	// Verify VM name matches
	if vmMachine.Spec.VMInfo.Name != vm {
		return nil, errors.Errorf("VMwareMachine %s VM name mismatch: expected %s, got %s", vmMachine.Name, vm, vmMachine.Spec.VMInfo.Name)
	}
	return vmMachine, nil
}

// ReconcileMigrationPlanJob reconciles jobs created by the migration plan
//
//nolint:gocyclo
func (r *MigrationPlanReconciler) ReconcileMigrationPlanJob(ctx context.Context,
	migrationplan *vjailbreakv1alpha1.MigrationPlan,
	scope *scope.MigrationPlanScope) (ctrl.Result, error) {
	totalVMs := 0
	for _, group := range migrationplan.Spec.VirtualMachines {
		totalVMs += len(group)
	}
	allVMNames := make([]string, 0, totalVMs)
	for _, group := range migrationplan.Spec.VirtualMachines {
		allVMNames = append(allVMNames, group...)
	}

	if migrationplan.Status.MigrationStatus == corev1.PodSucceeded {
		r.ctxlog.Info("Migration already completed, skipping job reconciliation", "migrationplan", migrationplan.Name)
		return ctrl.Result{}, nil
	}

	if migrationplan.Status.MigrationStatus == corev1.PodFailed {
		// Check if any Migration objects exist for this MigrationPlan
		migrationList := &vjailbreakv1alpha1.MigrationList{}
		listOpts := []client.ListOption{
			client.InNamespace(migrationplan.Namespace),
			client.MatchingLabels{"migrationplan": migrationplan.Name},
		}
		if err := r.List(ctx, migrationList, listOpts...); err != nil {
			r.ctxlog.Error(err, "Failed to list migrations for retry check", "migrationplan", migrationplan.Name)
			return ctrl.Result{}, errors.Wrap(err, "failed to list migrations for retry check")
		}

		// Map existing migrations to detect deletions
		existingMigrationMap := make(map[string]bool)
		hasExistingFailures := false
		for _, m := range migrationList.Items {
			existingMigrationMap[m.Spec.VMName] = true
			if m.Status.Phase == vjailbreakv1alpha1.VMMigrationPhaseFailed {
				hasExistingFailures = true
			}
		}

		retryTriggeredByDeletion := false
		for _, name := range allVMNames {
			if !existingMigrationMap[name] {
				retryTriggeredByDeletion = true
				break
			}
		}

		// If the specific "Failed" objects are gone (user deleted them for retry),
		// but the plan still says "Failed", we reset the plan status.
		if !hasExistingFailures || retryTriggeredByDeletion {
			if strings.HasPrefix(migrationplan.Status.MigrationMessage, constants.MigrationPlanValidationFailedPrefix) {
				return ctrl.Result{}, nil
			}

			r.ctxlog.Info("Resetting Plan status for retry", "migrationplan", migrationplan.Name)

			err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				latest := &vjailbreakv1alpha1.MigrationPlan{}
				if getErr := r.Get(ctx, types.NamespacedName{Name: migrationplan.Name, Namespace: migrationplan.Namespace}, latest); getErr != nil {
					if apierrors.IsNotFound(getErr) {
						return nil
					}
					return getErr
				}
				latest.Status.MigrationStatus = ""
				latest.Status.MigrationMessage = ""
				return r.Status().Update(ctx, latest)
			})

			if err != nil {
				return ctrl.Result{}, errors.Wrap(err, "failed to reset status for retry")
			}
			return ctrl.Result{Requeue: true}, nil
		}

		r.ctxlog.Info("Migration failures still exist, skipping reconciliation", "migrationplan", migrationplan.Name)
		return ctrl.Result{}, nil
	}

	migrationtemplate, vmwcreds, _, err := r.getMigrationTemplateAndCreds(ctx, migrationplan)
	if err != nil {
		r.ctxlog.Error(err, "Failed to get migration template and credentials")
		return ctrl.Result{}, err
	}

	// Validate VM OS types before proceeding with migration
	validVMs, _, validationErr := r.validateMigrationPlanVMs(ctx, migrationplan, migrationtemplate, vmwcreds)

	if validationErr != nil {
		r.ctxlog.Error(validationErr, "Migration plan validation failed", "migrationplan", migrationplan.Name)

		for _, vmName := range allVMNames {
			vmMachine, err := GetVMwareMachineForVM(ctx, r, vmName, migrationtemplate, vmwcreds)
			if err != nil {
				r.ctxlog.Error(err, "Failed to get vmMachine for pre-creation", "vm", vmName)
				continue
			}

			migrationObj, createErr := r.CreateMigration(ctx, migrationplan, vmName, vmMachine)
			if createErr != nil {
				r.ctxlog.Error(createErr, "Failed to create migration object during validation failure documentation", "vm", vmName)
				continue
			}

			r.markMigrationValidationFailed(ctx, migrationObj, vmName, "VM failed migration plan validation")
		}

		if err := r.UpdateMigrationPlanStatus(ctx, migrationplan, corev1.PodFailed, fmt.Sprintf("Migration plan validation failed: %v", validationErr)); err != nil {
			r.ctxlog.Error(err, "Failed to update migration plan status after validation failure")
		}
		return ctrl.Result{}, validationErr
	}

	for _, vmName := range allVMNames {
		vmMachine, err := GetVMwareMachineForVM(ctx, r, vmName, migrationtemplate, vmwcreds)
		if err != nil {
			r.ctxlog.Error(err, "Failed to get vmMachine for migration creation", "vm", vmName)
			continue
		}

		migrationObj, createErr := r.CreateMigration(ctx, migrationplan, vmName, vmMachine)
		if createErr != nil {
			r.ctxlog.Error(createErr, "Failed to create migration object", "vm", vmName)
			continue
		}

		isValid := false
		for _, v := range validVMs {
			if v.Spec.VMInfo.Name == vmName {
				isValid = true
				break
			}
		}

		if !isValid {
			r.markMigrationValidationFailed(ctx, migrationObj, vmName, "VM failed migration plan validation")
		}
	}

	r.ctxlog.Info("Reconciling MigrationPlanJob", "migrationplan", migrationplan.Name)
	// Fetch VMwareCreds CR
	if ok, err := r.checkStatusSuccess(ctx, migrationtemplate.Namespace, migrationtemplate.Spec.Source.VMwareRef, true, vmwcreds); !ok {
		return ctrl.Result{}, errors.Wrapf(err, "failed to check vmwarecreds status '%s'", migrationtemplate.Spec.Source.VMwareRef)
	}
	// Fetch OpenStackCreds CR
	openstackcreds := &vjailbreakv1alpha1.OpenstackCreds{}
	if ok, err := r.checkStatusSuccess(ctx, migrationtemplate.Namespace, migrationtemplate.Spec.Destination.OpenstackRef,
		false, openstackcreds); !ok {
		return ctrl.Result{}, errors.Wrapf(err, "failed to check openstackcreds status '%s'", migrationtemplate.Spec.Destination.OpenstackRef)
	}

	var arraycreds *vjailbreakv1alpha1.ArrayCreds
	// Check if StorageCopyMethod is StorageAcceleratedCopy
	if migrationtemplate.Spec.StorageCopyMethod == StorageCopyMethod {
		// Fetch ArrayCredsMapping CR first
		arrayCredsMapping := &vjailbreakv1alpha1.ArrayCredsMapping{}
		if err := r.Get(ctx, types.NamespacedName{Name: migrationtemplate.Spec.ArrayCredsMapping, Namespace: migrationtemplate.Namespace}, arrayCredsMapping); err != nil {
			return ctrl.Result{}, errors.Wrapf(err, "failed to get ArrayCredsMapping '%s'", migrationtemplate.Spec.ArrayCredsMapping)
		}

		// Validate ArrayCredsMapping has mappings
		if len(arrayCredsMapping.Spec.Mappings) == 0 {
			return ctrl.Result{}, errors.Errorf("ArrayCredsMapping '%s' has no mappings defined", migrationtemplate.Spec.ArrayCredsMapping)
		}
		for _, mapping := range arrayCredsMapping.Spec.Mappings {
			arraycreds = &vjailbreakv1alpha1.ArrayCreds{}
			if err := r.Get(ctx, types.NamespacedName{Name: mapping.Target, Namespace: migrationtemplate.Namespace}, arraycreds); err != nil {
				return ctrl.Result{}, errors.Wrapf(err, "failed to get ArrayCreds '%s' from mapping", mapping.Target)
			}
			if arraycreds.Status.ArrayValidationStatus != string(corev1.PodSucceeded) {
				return ctrl.Result{}, errors.Errorf("ArrayCreds '%s' is not validated (status: %s)", mapping.Target, arraycreds.Status.ArrayValidationStatus)
			}
		}
	} else {
		arraycreds = nil
	}

	// Starting the Migrations
	if migrationplan.Status.MigrationStatus == "" {
		err := r.UpdateMigrationPlanStatus(ctx, migrationplan, corev1.PodRunning, "Migration(s) in progress")
		if err != nil {
			return ctrl.Result{}, errors.Wrap(err, "failed to update migration plan status")
		}
	}

	// vmMachinesArr is created to maintain order in which VM migration is triggered
	vmMachinesArr := validVMs
	vmMachinesMap := make(map[string]*vjailbreakv1alpha1.VMwareMachine, len(validVMs))
	for _, vmMachine := range validVMs {
		vmMachinesMap[vmMachine.Spec.VMInfo.Name] = vmMachine
	}

	// Migrate RDM disks if any
	err = r.migrateRDMdisks(ctx, migrationplan, vmMachinesMap, openstackcreds)
	if err != nil {
		return r.handleRDMDiskMigrationError(ctx, migrationplan, err)
	}

	if paused, err := r.checkAndHandlePausedPlan(ctx, migrationplan); paused {
		return ctrl.Result{}, err
	}

	for _, parallelvms := range migrationplan.Spec.VirtualMachines {
		migrationobjs := &vjailbreakv1alpha1.MigrationList{}
		err := r.TriggerMigration(ctx, migrationplan, migrationobjs, openstackcreds, vmwcreds, arraycreds, migrationtemplate, vmMachinesArr)
		if err != nil {
			if strings.Contains(err.Error(), "VDDK_MISSING") {
				r.ctxlog.Info("Requeuing due to missing VDDK files.")
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			return ctrl.Result{}, errors.Wrapf(err, "failed to trigger migration")
		}

		allFinished, err := r.processMigrationPhases(ctx, scope, migrationplan, migrationobjs, parallelvms)
		if err != nil {
			return ctrl.Result{}, err
		}

		if !allFinished {
			r.ctxlog.Info("Migration(s) still in progress, requeuing plan", "migrationplan", migrationplan.Name)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}

	r.ctxlog.Info(fmt.Sprintf("All VMs in MigrationPlan '%s' have been successfully migrated", migrationplan.Name))
	migrationplan.Status.MigrationStatus = corev1.PodSucceeded
	migrationplan.Status.MigrationMessage = "All migrations completed successfully"
	err = r.Status().Update(ctx, migrationplan)
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to update migration plan status")
	}

	return ctrl.Result{}, nil
}

// checkAndHandlePausedPlan checks if migration plan is paused and handles it
func (r *MigrationPlanReconciler) checkAndHandlePausedPlan(ctx context.Context, migrationplan *vjailbreakv1alpha1.MigrationPlan) (bool, error) {
	if !utils.IsMigrationPlanPaused(ctx, migrationplan.Name, r.Client) {
		return false, nil
	}
	migrationplan.Status.MigrationStatus = "Paused"
	migrationplan.Status.MigrationMessage = "Migration plan is paused"
	if err := r.Update(ctx, migrationplan); err != nil {
		return true, errors.Wrap(err, "failed to update migration plan status")
	}
	return true, nil
}

// processMigrationPhases processes migration phases for triggered migrations
func (r *MigrationPlanReconciler) processMigrationPhases(
	ctx context.Context,
	scope *scope.MigrationPlanScope,
	migrationplan *vjailbreakv1alpha1.MigrationPlan,
	migrationobjs *vjailbreakv1alpha1.MigrationList,
	parallelvms []string,
) (bool, error) {
	allFinished := true

	for i := 0; i < len(migrationobjs.Items); i++ {
		migration := migrationobjs.Items[i]

		switch migration.Status.Phase {
		case vjailbreakv1alpha1.VMMigrationPhaseFailed, vjailbreakv1alpha1.VMMigrationPhaseValidationFailed:
			r.ctxlog.Info("Migration failed for VM", "vm", migration.Spec.VMName)
			err := r.UpdateMigrationPlanStatus(ctx, migrationplan, corev1.PodFailed,
				fmt.Sprintf("Migration for VM '%s' failed: %s", migration.Spec.VMName, migration.Status.Conditions[0].Message))
			return false, err

		case vjailbreakv1alpha1.VMMigrationPhaseSucceeded:
			err := r.reconcilePostMigration(ctx, scope, migration.Spec.VMName)
			if err != nil {
				r.ctxlog.Error(err, "Post-migration actions failed for VM", "vm", migration.Spec.VMName)
				return false, errors.Wrap(err, "failed post-migration")
			}
			continue

		default:
			r.ctxlog.Info("VM migration still in progress",
				"vm", migration.Spec.VMName,
				"phase", migration.Status.Phase,
				"currentBatch", parallelvms)
			allFinished = false
		}
	}
	return allFinished, nil
}

// handleRDMDiskMigrationError handles errors that occur during RDM disk migration
func (r *MigrationPlanReconciler) handleRDMDiskMigrationError(ctx context.Context, migrationplan *vjailbreakv1alpha1.MigrationPlan, err error) (ctrl.Result, error) {
	if err == verrors.ErrRDMDiskNotMigrated {
		delay := 25 * time.Second
		r.ctxlog.Info("RDM disk not migrated yet, requeuing MigrationPlan for polling.", "requeueAfter", delay)

		// Refetch the migration plan to get the latest version before updating
		if err := r.Get(ctx, types.NamespacedName{Name: migrationplan.Name, Namespace: migrationplan.Namespace}, migrationplan); err != nil {
			r.ctxlog.Error(err, "Failed to refetch MigrationPlan before updating status")
			return ctrl.Result{RequeueAfter: delay}, nil
		}
		newMessage := "RDM disk not migrated yet, requeuing MigrationPlan."
		if migrationplan.Status.MigrationMessage != newMessage || migrationplan.Status.MigrationStatus != corev1.PodPending {
			if err := r.UpdateMigrationPlanStatus(ctx, migrationplan, corev1.PodPending, newMessage); err != nil {
				r.ctxlog.Error(err, "Failed to update MigrationPlan status")
			}
		}

		return ctrl.Result{RequeueAfter: delay}, nil
	}
	// Handle any other RDM disk migration errors
	r.ctxlog.Info("RDM disk migration failed, failing MigrationPlan.", "error", err.Error())

	// Refetch the migration plan to get the latest version before updating
	if refetchErr := r.Get(ctx, types.NamespacedName{Name: migrationplan.Name, Namespace: migrationplan.Namespace}, migrationplan); refetchErr != nil {
		r.ctxlog.Error(refetchErr, "Failed to refetch MigrationPlan before failing")
		return ctrl.Result{}, fmt.Errorf("failed to refetch MigrationPlan: %w", refetchErr)
	}

	migrationplan.Status.MigrationStatus = corev1.PodFailed
	migrationplan.Status.MigrationMessage = fmt.Sprintf("RDM disk migration failed. Reason : %s", err)
	if err := r.UpdateMigrationPlanStatus(ctx, migrationplan, corev1.PodFailed, fmt.Sprintf("RDM disk migration failed. Reason : %s", err)); err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to update MigrationPlan status: %w", err)
	}
	return ctrl.Result{}, err
}

// UpdateMigrationPlanStatus updates the status of a MigrationPlan
func (r *MigrationPlanReconciler) UpdateMigrationPlanStatus(ctx context.Context,
	migrationplan *vjailbreakv1alpha1.MigrationPlan, status corev1.PodPhase, message string,
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// Get the latest version of the MigrationPlan
		latest := &vjailbreakv1alpha1.MigrationPlan{}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      migrationplan.Name,
			Namespace: migrationplan.Namespace,
		}, latest); err != nil {
			return err
		}

		// Only update if the status is different to prevent unnecessary updates
		if latest.Status.MigrationStatus == status && latest.Status.MigrationMessage == message {
			return nil
		}

		// Update the status
		latest.Status.MigrationStatus = status
		latest.Status.MigrationMessage = message

		// Use Status().Update() for status subresource
		if err := r.Status().Update(ctx, latest); err != nil {
			return err
		}

		return nil
	})
}

// CreateMigration creates a new Migration resource
func (r *MigrationPlanReconciler) CreateMigration(ctx context.Context,
	migrationplan *vjailbreakv1alpha1.MigrationPlan,
	vm string, vmMachine *vjailbreakv1alpha1.VMwareMachine,
) (*vjailbreakv1alpha1.Migration, error) {
	ctxlog := r.ctxlog.WithValues("vm", vm)
	ctxlog.Info("Creating Migration for VM")

	vmwarecreds, err := utils.GetVMwareCredsNameFromMigrationPlan(ctx, r.Client, migrationplan)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get vmware credentials")
	}
	vmk8sname, err := utils.GetK8sCompatibleVMWareObjectName(vm, vmwarecreds)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get vm name")
	}
	vminfo := &vmMachine.Spec.VMInfo

	migrationobj := &vjailbreakv1alpha1.Migration{}
	err = r.Get(ctx, types.NamespacedName{Name: utils.MigrationNameFromVMName(vmk8sname), Namespace: migrationplan.Namespace}, migrationobj)
	if err != nil && apierrors.IsNotFound(err) {
		// Get assigned IPs for this VM from the migration plan
		assignedIP := ""
		if migrationplan.Spec.AssignedIPsPerVM != nil {
			if ips, ok := migrationplan.Spec.AssignedIPsPerVM[vm]; ok {
				assignedIP = ips
			}
		}

		migrationobj = &vjailbreakv1alpha1.Migration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      utils.MigrationNameFromVMName(vmk8sname),
				Namespace: migrationplan.Namespace,
				Labels: map[string]string{
					"migrationplan":              migrationplan.Name,
					constants.NumberOfDisksLabel: strconv.Itoa(len(vminfo.Disks)),
				},
			},
			Spec: vjailbreakv1alpha1.MigrationSpec{
				MigrationPlan: migrationplan.Name,
				VMName:        vm,
				// PodRef will be set in the migration controller
				InitiateCutover:         migrationplan.Spec.MigrationStrategy.AdminInitiatedCutOver,
				DisconnectSourceNetwork: migrationplan.Spec.MigrationStrategy.DisconnectSourceNetwork,
				AssignedIP:              assignedIP,
				MigrationType:           migrationplan.Spec.MigrationStrategy.Type,
			},
			Status: vjailbreakv1alpha1.MigrationStatus{
				Phase:      vjailbreakv1alpha1.VMMigrationPhasePending,
				TotalDisks: len(vminfo.Disks),
			},
		}
		migrationobj.Labels = MergeLabels(migrationobj.Labels, migrationplan.Labels)
		err = r.createResource(ctx, migrationplan, migrationobj)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create Migration for VM %s", vm)
		}

		// Set retryable status based on whether VM has RDM disks
		// VMs with RDM disks cannot be retried through UI because shared RDM disk state
		// prevents automatic retry (RDMDisk CR may be in Error or Managed state)
		hasRDMDisks := len(vmMachine.Spec.VMInfo.RDMDisks) > 0
		retryable := !hasRDMDisks

		migrationobj.Status.Retryable = &retryable
		if err := r.Status().Update(ctx, migrationobj); err != nil {
			ctxlog.Error(err, "Failed to set retryable status", "retryable", retryable, "hasRDMDisks", hasRDMDisks)
			// Don't fail migration creation if status update fails, just log the error
		} else {
			ctxlog.Info("Set migration retryable status", "retryable", retryable, "hasRDMDisks", hasRDMDisks)
		}
	}
	return migrationobj, nil
}

// CreateJob creates a job to run v2v-helper
func (r *MigrationPlanReconciler) CreateJob(ctx context.Context,
	migrationplan *vjailbreakv1alpha1.MigrationPlan,
	migrationtemplate *vjailbreakv1alpha1.MigrationTemplate,
	migrationobj *vjailbreakv1alpha1.Migration,
	vm string,
	firstbootconfigMapName string,
	vmwareSecretRef string,
	openstackSecretRef string,
	vmMachine *vjailbreakv1alpha1.VMwareMachine,
	arrayCredsSecretRef string,
) error {
	vmwarecreds, err := utils.GetVMwareCredsNameFromMigrationPlan(ctx, r.Client, migrationplan)
	if err != nil {
		return errors.Wrap(err, "failed to get vmware credentials")
	}
	vmk8sname, err := utils.GetK8sCompatibleVMWareObjectName(vm, vmwarecreds)
	if err != nil {
		return errors.Wrap(err, "failed to get vm name")
	}
	jobName, err := utils.GetJobNameForVMName(vm, vmwarecreds)
	if err != nil {
		return errors.Wrap(err, "failed to get job name")
	}
	pointtrue := true
	cutoverlabel := "yes"
	if migrationplan.Spec.MigrationStrategy.AdminInitiatedCutOver {
		cutoverlabel = "no"
	}
	envVars := []corev1.EnvVar{
		{
			Name: "POD_NAME",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		},
		{
			Name:  "VMWARE_MACHINE_OBJECT_NAME",
			Value: vmk8sname,
		},
		{
			Name:  "USE_FLAVORLESS",
			Value: strconv.FormatBool(migrationtemplate.Spec.UseFlavorless),
		},
	}

	if migrationtemplate.Spec.UseFlavorless {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "FLAVORLESS_FLAVOR_ID",
			Value: vmMachine.Spec.TargetFlavorID,
		})
	}

	job := &batchv1.Job{}
	err = r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: migrationplan.Namespace}, job)
	if err != nil && apierrors.IsNotFound(err) {
		r.ctxlog.Info(fmt.Sprintf("Creating new Job '%s' for VM '%s'", jobName, vm))
		job = &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      jobName,
				Namespace: migrationplan.Namespace,
			},
			Spec: batchv1.JobSpec{
				PodFailurePolicy: &batchv1.PodFailurePolicy{
					Rules: []batchv1.PodFailurePolicyRule{
						{
							Action: batchv1.PodFailurePolicyActionFailJob,
							OnExitCodes: &batchv1.PodFailurePolicyOnExitCodesRequirement{
								Values:   []int32{0},
								Operator: batchv1.PodFailurePolicyOnExitCodesOpNotIn,
							},
						},
					},
				},
				TTLSecondsAfterFinished: nil,
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							constants.VMNameLabel: vmk8sname,
							"startCutover":        cutoverlabel,
						},
					},
					Spec: corev1.PodSpec{
						RestartPolicy:                 corev1.RestartPolicyNever,
						ServiceAccountName:            "migration-controller-manager",
						TerminationGracePeriodSeconds: ptr.To(constants.TerminationPeriod),
						HostNetwork:                   true,
						DNSPolicy:                     corev1.DNSClusterFirstWithHostNet,
						Containers: []corev1.Container{
							{
								Name:            "fedora",
								Image:           v2vimage,
								ImagePullPolicy: corev1.PullIfNotPresent,
								Command:         []string{"/home/fedora/manager"},
								SecurityContext: &corev1.SecurityContext{
									Privileged: &pointtrue,
								},
								Env: envVars,
								EnvFrom: func() []corev1.EnvFromSource {
									envFrom := []corev1.EnvFromSource{
										{
											SecretRef: &corev1.SecretEnvSource{
												LocalObjectReference: corev1.LocalObjectReference{
													Name: vmwareSecretRef,
												},
											},
										},
										{
											SecretRef: &corev1.SecretEnvSource{
												LocalObjectReference: corev1.LocalObjectReference{
													Name: openstackSecretRef,
												},
											},
										},
									}
									if arrayCredsSecretRef != "" {
										envFrom = append(envFrom, corev1.EnvFromSource{
											SecretRef: &corev1.SecretEnvSource{
												LocalObjectReference: corev1.LocalObjectReference{
													Name: arrayCredsSecretRef,
												},
											},
										})
									}
									envFrom = append(envFrom, corev1.EnvFromSource{
										ConfigMapRef: &corev1.ConfigMapEnvSource{
											LocalObjectReference: corev1.LocalObjectReference{
												Name: "pf9-env",
											},
										},
									})
									return envFrom
								}(),
								VolumeMounts: []corev1.VolumeMount{
									{
										Name:      "vddk",
										MountPath: "/home/fedora/vmware-vix-disklib-distrib",
									},
									{
										Name:      "dev",
										MountPath: "/dev",
									},
									{
										Name:      "firstboot",
										MountPath: "/home/fedora/scripts",
									},
									{
										Name:      "logs",
										MountPath: "/var/log/pf9",
									},
									{
										Name:      "virtio-driver",
										MountPath: "/home/fedora/virtio-win",
									},
								},
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:              resource.MustParse("1000m"),
										corev1.ResourceMemory:           resource.MustParse("1Gi"),
										corev1.ResourceEphemeralStorage: resource.MustParse("3Gi"),
									},
									Limits: corev1.ResourceList{
										corev1.ResourceCPU:              resource.MustParse("2000m"),
										corev1.ResourceMemory:           resource.MustParse("3Gi"),
										corev1.ResourceEphemeralStorage: resource.MustParse("3Gi"),
									},
								},
							},
						},
						Volumes: []corev1.Volume{
							{
								Name: "vddk",
								VolumeSource: corev1.VolumeSource{
									HostPath: &corev1.HostPathVolumeSource{
										Path: "/home/ubuntu/vmware-vix-disklib-distrib",
										Type: utils.NewHostPathType("Directory"),
									},
								},
							},
							{
								Name: "dev",
								VolumeSource: corev1.VolumeSource{
									HostPath: &corev1.HostPathVolumeSource{
										Path: "/dev",
										Type: utils.NewHostPathType("Directory"),
									},
								},
							},
							{
								Name: "firstboot",
								VolumeSource: corev1.VolumeSource{
									ConfigMap: &corev1.ConfigMapVolumeSource{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: firstbootconfigMapName,
										},
									},
								},
							},
							{
								Name: "logs",
								VolumeSource: corev1.VolumeSource{
									HostPath: &corev1.HostPathVolumeSource{
										Path: "/var/log/pf9",
										Type: utils.NewHostPathType("DirectoryOrCreate"),
									},
								},
							},
							{
								Name: "virtio-driver",
								VolumeSource: corev1.VolumeSource{
									HostPath: &corev1.HostPathVolumeSource{
										Path: "/home/ubuntu/virtio-win",
										Type: utils.NewHostPathType("DirectoryOrCreate"),
									},
								},
							},
						},
					},
				},
			},
		}
		if err := r.createResource(ctx, migrationobj, job); err != nil {
			r.ctxlog.Error(err, fmt.Sprintf("Failed to create Job '%s'", jobName))
			return errors.Wrap(err, fmt.Sprintf("failed to create job '%s'", jobName))
		}
	}
	return nil
}

// CreateFirstbootConfigMap creates a firstboot config map for migration
func (r *MigrationPlanReconciler) CreateFirstbootConfigMap(ctx context.Context,
	migrationplan *vjailbreakv1alpha1.MigrationPlan, migrationobj *vjailbreakv1alpha1.Migration, vm string,
) (*corev1.ConfigMap, error) {
	vmwarecreds, err := utils.GetVMwareCredsNameFromMigrationPlan(ctx, r.Client, migrationplan)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get vmware credentials")
	}
	vmname, err := utils.GetK8sCompatibleVMWareObjectName(vm, vmwarecreds)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get vm name")
	}
	configMapName := utils.GetFirstbootConfigMapName(vmname)
	configMap := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: migrationplan.Namespace}, configMap)
	if err != nil && apierrors.IsNotFound(err) {
		r.ctxlog.Info(fmt.Sprintf("Creating new ConfigMap '%s' for VM '%s'", configMapName, vmname))
		configMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configMapName,
				Namespace: migrationplan.Namespace,
			},
			Data: map[string]string{
				"user_firstboot.sh": migrationplan.Spec.FirstBootScript,
			},
		}
		err = r.createResource(ctx, migrationobj, configMap)
		if err != nil {
			r.ctxlog.Error(err, fmt.Sprintf("Failed to create ConfigMap '%s'", configMapName))
			return nil, errors.Wrapf(err, "failed to create config map '%s'", configMapName)
		}
	}
	return configMap, nil
}

// CreateMigrationConfigMap creates a config map for migration
func (r *MigrationPlanReconciler) CreateMigrationConfigMap(ctx context.Context,
	migrationplan *vjailbreakv1alpha1.MigrationPlan,
	migrationtemplate *vjailbreakv1alpha1.MigrationTemplate,
	migrationobj *vjailbreakv1alpha1.Migration,
	openstackcreds *vjailbreakv1alpha1.OpenstackCreds,
	vmwcreds *vjailbreakv1alpha1.VMwareCreds, vm string, vmMachine *vjailbreakv1alpha1.VMwareMachine,
	arraycreds *vjailbreakv1alpha1.ArrayCreds,
) (*corev1.ConfigMap, error) {
	vmname, err := utils.GetK8sCompatibleVMWareObjectName(vm, vmwcreds.Name)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get vm name")
	}
	configMapName := utils.GetMigrationConfigMapName(vmname)
	virtiodrivers := ""
	if migrationtemplate.Spec.VirtioWinDriver == "" {
		virtiodrivers = "https://fedorapeople.org/groups/virt/virtio-win/direct-downloads/stable-virtio/virtio-win.iso"
	} else {
		virtiodrivers = migrationtemplate.Spec.VirtioWinDriver
	}
	openstacknws, openstackvolumetypes, err := r.reconcileMapping(ctx, migrationtemplate, openstackcreds, vmwcreds, vm)
	if err != nil {
		return nil, errors.Wrap(err, "failed to reconcile mapping")
	}

	openstackports := []string{}
	// If advanced options are set, replace the networks and/or volume types with the ones in the advanced options
	if !reflect.DeepEqual(migrationplan.Spec.AdvancedOptions, vjailbreakv1alpha1.AdvancedOptions{}) {
		if len(migrationplan.Spec.AdvancedOptions.GranularNetworks) > 0 {
			if err = utils.VerifyNetworks(ctx, r.Client, openstackcreds, migrationplan.Spec.AdvancedOptions.GranularNetworks); err != nil {
				return nil, errors.Wrap(err, "failed to verify networks in advanced mapping")
			}
			openstacknws = migrationplan.Spec.AdvancedOptions.GranularNetworks
		}
		if len(migrationplan.Spec.AdvancedOptions.GranularVolumeTypes) > 0 {
			if err = utils.VerifyStorage(ctx, r.Client, openstackcreds, migrationplan.Spec.AdvancedOptions.GranularVolumeTypes); err != nil {
				return nil, errors.Wrap(err, "failed to verify volume types in advanced mapping")
			}
			openstackvolumetypes = migrationplan.Spec.AdvancedOptions.GranularVolumeTypes
		}
		if len(migrationplan.Spec.AdvancedOptions.GranularPorts) > 0 {
			if err = utils.VerifyPorts(ctx, r.Client, openstackcreds, migrationplan.Spec.AdvancedOptions.GranularPorts); err != nil {
				return nil, errors.Wrap(err, "failed to verify ports in advanced mapping")
			}
			openstackports = migrationplan.Spec.AdvancedOptions.GranularPorts
		}
	}
	// Create MigrationConfigMap
	configMap := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: configMapName, Namespace: migrationplan.Namespace}, configMap)
	if err != nil && apierrors.IsNotFound(err) {
		r.ctxlog.Info(fmt.Sprintf("Creating new ConfigMap '%s' for VM '%s'", configMapName, vmname))
		configMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      configMapName,
				Namespace: migrationplan.Namespace,
			},
			Data: map[string]string{
				"SOURCE_VM_NAME":             vm,
				"CONVERT":                    "true", // Assume that the vm always has to be converted
				"TYPE":                       migrationplan.Spec.MigrationStrategy.Type,
				"DATACOPYSTART":              migrationplan.Spec.MigrationStrategy.DataCopyStart.Format(time.RFC3339),
				"CUTOVERSTART":               migrationplan.Spec.MigrationStrategy.VMCutoverStart.Format(time.RFC3339),
				"CUTOVEREND":                 migrationplan.Spec.MigrationStrategy.VMCutoverEnd.Format(time.RFC3339),
				"NEUTRON_NETWORK_NAMES":      strings.Join(openstacknws, ","),
				"NEUTRON_PORT_IDS":           strings.Join(openstackports, ","),
				"CINDER_VOLUME_TYPES":        strings.Join(openstackvolumetypes, ","),
				"VIRTIO_WIN_DRIVER":          virtiodrivers,
				"PERFORM_HEALTH_CHECKS":      strconv.FormatBool(migrationplan.Spec.MigrationStrategy.PerformHealthChecks),
				"HEALTH_CHECK_PORT":          migrationplan.Spec.MigrationStrategy.HealthCheckPort,
				"VMWARE_MACHINE_OBJECT_NAME": vmMachine.Name,
				"SECURITY_GROUPS":            strings.Join(migrationplan.Spec.SecurityGroups, ","),
				"SERVER_GROUP":               migrationplan.Spec.ServerGroup,
				"RDM_DISK_NAMES":             strings.Join(vmMachine.Spec.VMInfo.RDMDisks, ","),
				"FALLBACK_TO_DHCP":           strconv.FormatBool(migrationplan.Spec.FallbackToDHCP),
				"PERIODIC_SYNC_INTERVAL":     migrationplan.Spec.AdvancedOptions.PeriodicSyncInterval,
				"PERIODIC_SYNC_ENABLED":      strconv.FormatBool(migrationplan.Spec.AdvancedOptions.PeriodicSyncEnabled),
				"NETWORK_PERSISTENCE":        strconv.FormatBool(migrationplan.Spec.AdvancedOptions.NetworkPersistence),
			},
		}
		if utils.IsOpenstackPCD(*openstackcreds) {
			configMap.Data["TARGET_AVAILABILITY_ZONE"] = migrationtemplate.Spec.TargetPCDClusterName
		}

		// Check if assigned IP is set from Migration spec
		if migrationobj.Spec.AssignedIP != "" {
			configMap.Data["ASSIGNED_IP"] = migrationobj.Spec.AssignedIP
		} else {
			configMap.Data["ASSIGNED_IP"] = ""
		}

		// Check if target flavor is set
		if vmMachine.Spec.TargetFlavorID != "" {
			configMap.Data["TARGET_FLAVOR_ID"] = vmMachine.Spec.TargetFlavorID
		} else {
			// If target flavor is not set, use the closest matching flavor
			allFlavors, err := utils.ListAllFlavors(ctx, r.Client, openstackcreds)
			if err != nil {
				return nil, errors.Wrap(err, "failed to list all flavors")
			}

			// UseGPUFlavor is only applicable for PCD credentials
			useGPUFlavor := migrationtemplate.Spec.UseGPUFlavor && utils.IsOpenstackPCD(*openstackcreds)

			// Get GPU requirements from VM
			passthroughGPUCount := vmMachine.Spec.VMInfo.GPU.PassthroughCount
			vgpuCount := vmMachine.Spec.VMInfo.GPU.VGPUCount

			var flavor *flavors.Flavor
			flavor, err = openstackpkg.GetClosestFlavour(vmMachine.Spec.VMInfo.CPU, vmMachine.Spec.VMInfo.Memory, passthroughGPUCount, vgpuCount, allFlavors, useGPUFlavor)
			if err != nil {
				return nil, errors.Wrap(err, "failed to get closest flavor")
			}
			if flavor == nil {
				gpuInfo := ""
				if passthroughGPUCount > 0 || vgpuCount > 0 {
					gpuInfo = fmt.Sprintf(", %d passthrough GPU(s), and %d vGPU(s)", passthroughGPUCount, vgpuCount)
				} else {
					gpuInfo = " without GPU"
				}
				return nil, errors.Errorf("no suitable flavor found for %d vCPUs, %d MB RAM%s", vmMachine.Spec.VMInfo.CPU, vmMachine.Spec.VMInfo.Memory, gpuInfo)
			}
			configMap.Data["TARGET_FLAVOR_ID"] = flavor.ID
		}

		if vmMachine.Spec.VMInfo.OSFamily == "" {
			return nil, errors.Errorf(
				"OSFamily is not available for the VM '%s', "+
					"cannot perform the migration. Please set OSFamily explicitly in the VMwareMachine CR",
				vmMachine.Name)
		}

		configMap.Data["OS_FAMILY"] = vmMachine.Spec.VMInfo.OSFamily
		configMap.Data["DISCONNECT_SOURCE_NETWORK"] = strconv.FormatBool(migrationobj.Spec.DisconnectSourceNetwork)

		if migrationtemplate.Spec.OSFamily != "" {
			configMap.Data["OS_FAMILY"] = migrationtemplate.Spec.OSFamily
		}

		if migrationtemplate.Spec.StorageCopyMethod == StorageCopyMethod {
			configMap.Data["STORAGE_COPY_METHOD"] = StorageCopyMethod
			configMap.Data["VENDOR_TYPE"] = arraycreds.Spec.VendorType
			configMap.Data["ARRAY_CREDS_MAPPING"] = migrationtemplate.Spec.ArrayCredsMapping
		}

		err = r.createResource(ctx, migrationobj, configMap)
		if err != nil {
			r.ctxlog.Error(err, fmt.Sprintf("Failed to create ConfigMap '%s'", configMapName))
			return nil, errors.Wrapf(err, "failed to create config map '%s'", configMapName)
		}
	}
	return configMap, nil
}

func (r *MigrationPlanReconciler) createResource(ctx context.Context, owner metav1.Object, controlled client.Object) error {
	err := ctrl.SetControllerReference(owner, controlled, r.Scheme)
	if err != nil {
		return errors.Wrap(err, "failed to set controller reference")
	}
	err = r.Create(ctx, controlled)
	if err != nil {
		return errors.Wrap(err, "failed to create resource")
	}
	return nil
}

//nolint:dupl // Same logic to migrationtemplate reconciliation, excluding from linting to keep both reconcilers separate
func (r *MigrationPlanReconciler) checkStatusSuccess(ctx context.Context,
	namespace, credsname string,
	isvmware bool,
	credsobj client.Object,
) (bool, error) {
	client := r.Client
	err := client.Get(ctx, types.NamespacedName{Name: credsname, Namespace: namespace}, credsobj)
	if err != nil {
		return false, errors.Wrap(err, "failed to get Creds")
	}

	if isvmware {
		vmwareCreds, ok := credsobj.(*vjailbreakv1alpha1.VMwareCreds)
		if !ok {
			return false, errors.Wrap(err, "failed to convert credentials to VMwareCreds")
		}
		if vmwareCreds.Status.VMwareValidationStatus != string(corev1.PodSucceeded) {
			return false, errors.Errorf("vmwarecreds '%s' CR is not validated", vmwareCreds.Name)
		}
	} else {
		openstackCreds, ok := credsobj.(*vjailbreakv1alpha1.OpenstackCreds)
		if !ok {
			return false, errors.Wrap(err, "failed to convert credentials to OpenstackCreds")
		}
		if openstackCreds.Status.OpenStackValidationStatus != string(corev1.PodSucceeded) {
			return false, errors.Errorf("openstackcreds '%s' CR is not validated", openstackCreds.Name)
		}
	}
	return true, nil
}

func (r *MigrationPlanReconciler) reconcileMapping(ctx context.Context,
	migrationtemplate *vjailbreakv1alpha1.MigrationTemplate,
	openstackcreds *vjailbreakv1alpha1.OpenstackCreds,
	vmwcreds *vjailbreakv1alpha1.VMwareCreds,
	vm string,
) (openstacknws, openstackvolumetypes []string, err error) {
	ctxlog := r.ctxlog.WithValues("reconcileMapping", vm)
	ctxlog.Info("Reconciling mapping for VM")
	// Get datacenter from VM's cluster annotation
	datacenter, err := r.getDatacenterForVM(ctx, vm, vmwcreds, migrationtemplate)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to get datacenter for VM")
	}

	openstacknws, err = r.reconcileNetwork(ctx, migrationtemplate, openstackcreds, vmwcreds, vm, datacenter)
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to reconcile network")
	}
	ctxlog.Info("Reconciled network", "vm", vm, "openstacknws", openstacknws)
	ctxlog.Info("storage method", "vm", vm, "storage method", migrationtemplate.Spec.StorageCopyMethod)
	// Skip storage mapping reconciliation for StorageCopyMethod storage copy method
	// as it uses ArrayCredsMapping instead of StorageMapping
	if migrationtemplate.Spec.StorageCopyMethod != StorageCopyMethod {
		openstackvolumetypes, err = r.reconcileStorage(ctx, migrationtemplate, vmwcreds, openstackcreds, vm, datacenter)
		if err != nil {
			return nil, nil, errors.Wrap(err, "failed to reconcile storage")
		}
		return openstacknws, openstackvolumetypes, nil
	}
	return openstacknws, openstackvolumetypes, nil
}

//nolint:dupl // Similar logic to storages reconciliation, excluding from linting to keep it readable
func (r *MigrationPlanReconciler) reconcileNetwork(ctx context.Context,
	migrationtemplate *vjailbreakv1alpha1.MigrationTemplate,
	openstackcreds *vjailbreakv1alpha1.OpenstackCreds,
	vmwcreds *vjailbreakv1alpha1.VMwareCreds,
	vm string,
	datacenter string,
) ([]string, error) {
	vmnws, err := utils.GetVMwNetworks(ctx, r.Client, vmwcreds, datacenter, vm)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get network")
	}
	// Fetch the networkmap
	networkmap := &vjailbreakv1alpha1.NetworkMapping{}
	err = r.Get(ctx, types.NamespacedName{Name: migrationtemplate.Spec.NetworkMapping, Namespace: migrationtemplate.Namespace}, networkmap)
	if err != nil {
		return nil, errors.Wrap(err, "failed to retrieve NetworkMapping CR")
	}

	openstacknws := []string{}
	// Process each VM network (including duplicates for multiple NICs)
	// Map each NIC's network to the corresponding target network
	for _, vmnw := range vmnws {
		found := false
		for _, nwm := range networkmap.Spec.Networks {
			if vmnw == nwm.Source {
				openstacknws = append(openstacknws, nwm.Target)
				found = true
				break // Use the first matching mapping
			}
		}
		if !found {
			return nil, errors.Errorf("VMware network %q not found in NetworkMapping", vmnw)
		}
	}

	// Get unique networks for validation
	uniqueTargets := make(map[string]bool)
	for _, target := range openstacknws {
		uniqueTargets[target] = true
	}
	//nolint:prealloc // Preallocating the slice is not possible as the length is unknown
	var uniqueTargetList []string
	for target := range uniqueTargets {
		uniqueTargetList = append(uniqueTargetList, target)
	}

	if networkmap.Status.NetworkmappingValidationStatus != string(corev1.PodSucceeded) {
		err = utils.VerifyNetworks(ctx, r.Client, openstackcreds, uniqueTargetList)
		if err != nil {
			return nil, errors.Wrap(err, "failed to verify networks")
		}
		networkmap.Status.NetworkmappingValidationStatus = string(corev1.PodSucceeded)
		networkmap.Status.NetworkmappingValidationMessage = "NetworkMapping validated"
		err = r.Status().Update(ctx, networkmap)
		if err != nil {
			return nil, errors.Wrap(err, "failed to update networkmapping status")
		}
	}
	return openstacknws, nil
}

//nolint:dupl // Similar logic to networks reconciliation, excluding from linting to keep it readable
func (r *MigrationPlanReconciler) reconcileStorage(ctx context.Context,
	migrationtemplate *vjailbreakv1alpha1.MigrationTemplate,
	vmwcreds *vjailbreakv1alpha1.VMwareCreds,
	openstackcreds *vjailbreakv1alpha1.OpenstackCreds,
	vm string,
	datacenter string,
) ([]string, error) {
	vmds, err := utils.GetVMwDatastore(ctx, r.Client, vmwcreds, datacenter, vm)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get datastores")
	}
	// Fetch the StorageMap
	storagemap := &vjailbreakv1alpha1.StorageMapping{}
	err = r.Get(ctx, types.NamespacedName{Name: migrationtemplate.Spec.StorageMapping, Namespace: migrationtemplate.Namespace}, storagemap)
	if err != nil {
		return nil, errors.Wrap(err, "failed to retrieve StorageMapping CR")
	}

	openstackvolumetypes := []string{}
	for _, vmdatastore := range vmds {
		for _, storagemaptype := range storagemap.Spec.Storages {
			if vmdatastore == storagemaptype.Source {
				openstackvolumetypes = append(openstackvolumetypes, storagemaptype.Target)
			}
		}
	}
	if len(openstackvolumetypes) != len(vmds) {
		return nil, errors.Errorf("VMware Datastore(s) not found in StorageMapping vm(%d) openstack(%d)", len(vmds), len(openstackvolumetypes))
	}
	if storagemap.Status.StoragemappingValidationStatus != string(corev1.PodSucceeded) {
		err = utils.VerifyStorage(ctx, r.Client, openstackcreds, openstackvolumetypes)
		if err != nil {
			return nil, errors.Wrap(err, "failed to verify datastores")
		}
		storagemap.Status.StoragemappingValidationStatus = string(corev1.PodSucceeded)
		storagemap.Status.StoragemappingValidationMessage = "StorageMapping validated"
		err = r.Status().Update(ctx, storagemap)
		if err != nil {
			return nil, errors.Wrap(err, "failed to update storagemapping status")
		}
	}
	return openstackvolumetypes, nil
}

// TriggerMigration triggers a migration process
func (r *MigrationPlanReconciler) TriggerMigration(ctx context.Context,
	migrationplan *vjailbreakv1alpha1.MigrationPlan,
	migrationobjs *vjailbreakv1alpha1.MigrationList,
	openstackcreds *vjailbreakv1alpha1.OpenstackCreds,
	vmwcreds *vjailbreakv1alpha1.VMwareCreds,
	arraycreds *vjailbreakv1alpha1.ArrayCreds,
	migrationtemplate *vjailbreakv1alpha1.MigrationTemplate,
	parallelvms []*vjailbreakv1alpha1.VMwareMachine,
) error {
	ctxlog := r.ctxlog.WithValues("migrationplan", migrationplan.Name)
	var (
		fbcm                 *corev1.ConfigMap
		baseFlavor           *flavors.Flavor
		hotplugFlavorMissing = false
	)

	// For flavorless migrations, check hotplug base flavor availability
	if migrationtemplate.Spec.UseFlavorless {
		ctxlog.Info("Flavorless migration detected, attempting to auto-discover base flavor.")

		osClients, err := utils.GetOpenStackClients(ctx, r.Client, openstackcreds)
		if err != nil {
			return errors.Wrap(err, "failed to get OpenStack clients for flavor discovery")
		}

		baseFlavor, err = utils.FindHotplugBaseFlavor(osClients.ComputeClient)
		if err != nil {
			ctxlog.Error(err, "Failed to discover hotplug base flavor")
			if updateErr := r.UpdateMigrationPlanStatus(ctx, migrationplan, corev1.PodFailed, "Failed to discover base flavor for flavorless migration"); updateErr != nil {
				ctxlog.Error(updateErr, "Failed to update migration plan status after flavor discovery failure")
			}
			hotplugFlavorMissing = true
		} else {
			ctxlog.Info("Successfully discovered base flavor", "flavorName", baseFlavor.Name, "flavorID", baseFlavor.ID)
		}
	}

	nodeList := &corev1.NodeList{}
	err := r.List(ctx, nodeList)
	if err != nil {
		return errors.Wrap(err, "failed to list nodes")
	}
	counter := len(nodeList.Items)
	for _, vmMachineObj := range parallelvms {
		if vmMachineObj == nil {
			return errors.Wrapf(err, "VM '%s' not found in VMwareMachine", vmMachineObj.Name)
		}
		vm := vmMachineObj.Spec.VMInfo.Name

		if migrationtemplate.Spec.UseFlavorless && !hotplugFlavorMissing {
			if vmMachineObj.Spec.TargetFlavorID != baseFlavor.ID {
				patch := client.MergeFrom(vmMachineObj.DeepCopy())
				vmMachineObj.Spec.TargetFlavorID = baseFlavor.ID
				if err := r.Patch(ctx, vmMachineObj, patch); err != nil {
					return errors.Wrap(err, "failed to automatically patch VMwareMachine with discovered base flavor ID")
				}
				ctxlog.Info("Patched VMwareMachine with base flavor ID", "vmwareMachine", vmMachineObj.Name, "flavorID", baseFlavor.ID)
			}
		}

		migrationobj, err := r.CreateMigration(ctx, migrationplan, vm, vmMachineObj)
		if err != nil {
			if apierrors.IsAlreadyExists(err) && migrationobj.Status.Phase == vjailbreakv1alpha1.VMMigrationPhaseSucceeded {
				r.ctxlog.Info(fmt.Sprintf("Migration for VM '%s' already exists", vm))
				continue
			}
			return errors.Wrapf(err, "failed to create Migration for VM %s", vm)
		}
		migrationobjs.Items = append(migrationobjs.Items, *migrationobj)

		if migrationtemplate.Spec.UseFlavorless && hotplugFlavorMissing {
			ctxlog.Info("Marking migration as Failed due to missing hotplug base flavor", "vm", vm)
			if err := r.markMigrationFailed(ctx, migrationobj, "Failed to discover base flavor for flavorless migration"); err != nil {
				ctxlog.Error(err, "Failed to mark migration as Failed", "vm", vm)
			}
			continue
		}
		_, err = r.CreateMigrationConfigMap(ctx, migrationplan, migrationtemplate, migrationobj, openstackcreds, vmwcreds, vm, vmMachineObj, arraycreds)
		if err != nil {
			return errors.Wrapf(err, "failed to create ConfigMap for VM %s", vm)
		}
		fbcm, err = r.CreateFirstbootConfigMap(ctx, migrationplan, migrationobj, vm)
		if err != nil {
			return errors.Wrapf(err, "failed to create Firstboot ConfigMap for VM %s", vm)
		}
		//nolint:gocritic // err is already declared above
		if err = r.validateVDDKPresence(ctx, migrationobj, ctxlog); err != nil {
			return err
		}

		arraycredsSecretRef := ""
		if arraycreds != nil {
			arraycredsSecretRef = arraycreds.Spec.SecretRef.Name
		}
		err = r.CreateJob(ctx,
			migrationplan,
			migrationtemplate,
			migrationobj,
			vm,
			fbcm.Name,
			vmwcreds.Spec.SecretRef.Name,
			openstackcreds.Spec.SecretRef.Name,
			vmMachineObj,
			arraycredsSecretRef)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("failed to create Job for VM %s", vm))
		}
		counter--

		if counter == 0 {
			// Control the number of VMs in parallel
			counter = len(nodeList.Items)
			time.Sleep(constants.MigrationTriggerDelay)
		}
	}

	if hotplugFlavorMissing {
		return nil
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *MigrationPlanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&vjailbreakv1alpha1.MigrationPlan{}).
		Owns(&vjailbreakv1alpha1.Migration{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.MaxConcurrentReconciles}).
		Complete(r)
}

func (r *MigrationPlanReconciler) validateVDDKPresence(
	ctx context.Context,
	migrationobj *vjailbreakv1alpha1.Migration,
	logger logr.Logger,
) error {
	currentUser, err := user.Current()
	whoami := "unknown"
	if err == nil {
		whoami = currentUser.Username
	}

	oldConditions := migrationobj.Status.Conditions

	files, err := os.ReadDir(VDDKDirectory)
	if err != nil {
		logger.Error(err, "VDDK directory could not be read")

		migrationobj.Status.Phase = vjailbreakv1alpha1.VMMigrationPhasePending
		setCondition := corev1.PodCondition{
			Type:               "VDDKCheck",
			Status:             corev1.ConditionFalse,
			Reason:             "VDDKDirectoryMissing",
			Message:            "VDDK directory is missing. Please create and upload the required files.",
			LastTransitionTime: metav1.Now(),
		}

		newConditions := []corev1.PodCondition{}
		for _, c := range migrationobj.Status.Conditions {
			if c.Type != "VDDKCheck" {
				newConditions = append(newConditions, c)
			}
		}
		newConditions = append(newConditions, setCondition)
		migrationobj.Status.Conditions = newConditions

		if err = r.Status().Update(ctx, migrationobj); err != nil {
			return errors.Wrap(err, "failed to update migration status after missing VDDK dir")
		}

		return errors.Wrap(err, "VDDK_MISSING: directory could not be read")
	}

	if len(files) == 0 {
		logger.Info("VDDK directory is empty, skipping Job creation. Will retry in 30s.",
			"path", VDDKDirectory,
			"whoami", whoami)

		migrationobj.Status.Phase = vjailbreakv1alpha1.VMMigrationPhasePending
		migrationobj.Status.Conditions = append(migrationobj.Status.Conditions, corev1.PodCondition{
			Type:               "VDDKCheck",
			Status:             corev1.ConditionFalse,
			Reason:             "VDDKDirectoryEmpty",
			Message:            "VDDK directory is empty. Please upload the required files.",
			LastTransitionTime: metav1.Now(),
		})

		if !reflect.DeepEqual(migrationobj.Status.Conditions, oldConditions) {
			if err = r.Status().Update(ctx, migrationobj); err != nil {
				return errors.Wrap(err, "failed to update migration status after empty VDDK dir")
			}
		}

		return errors.Wrap(err, "VDDK_MISSING: directory is empty")
	}

	// Clear previous VDDKCheck condition if directory is valid
	cleanedConditions := []corev1.PodCondition{}
	for _, c := range migrationobj.Status.Conditions {
		if c.Type != "VDDKCheck" {
			cleanedConditions = append(cleanedConditions, c)
		}
	}

	migrationobj.Status.Phase = vjailbreakv1alpha1.VMMigrationPhasePending
	migrationobj.Status.Conditions = cleanedConditions

	if !reflect.DeepEqual(migrationobj.Status.Conditions, oldConditions) {
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			currentMigration := &vjailbreakv1alpha1.Migration{}
			if getErr := r.Get(ctx, types.NamespacedName{Name: migrationobj.Name, Namespace: migrationobj.Namespace}, currentMigration); getErr != nil {
				return fmt.Errorf("failed to get Migration %s/%s during retry: %w", migrationobj.Namespace, migrationobj.Name, getErr)
			}

			currentMigration.Status.Phase = vjailbreakv1alpha1.VMMigrationPhasePending
			currentMigration.Status.Conditions = cleanedConditions

			return r.Status().Update(ctx, currentMigration)
		})
		if err != nil {
			return errors.Wrap(err, "failed to update migration status after validating VDDK presence")
		}
	}
	return nil
}

// MergeLabels combines two label maps into a single map, with values from b overriding values from a if keys conflict.
// This function is used to create a complete set of labels for Kubernetes resources created during migration.
func MergeLabels(a, b map[string]string) map[string]string {
	result := make(map[string]string)
	for k, v := range a {
		result[k] = v
	}
	for k, v := range b {
		result[k] = v
	}
	return result
}

// EnsureVMFolderExists ensures that the specified folder exists in the datacenter.
// If the folder does not exist, it creates a new folder with the specified name.
func EnsureVMFolderExists(ctx context.Context, finder *find.Finder, dc *object.Datacenter, folderName string) (*object.Folder, error) {
	finder.SetDatacenter(dc)

	// Check if folder exists
	folder, err := finder.Folder(ctx, folderName)
	if err == nil {
		return folder, nil
	}

	// Create folder if missing
	folders, err := dc.Folders(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get datacenter folders")
	}
	folder, err = folders.VmFolder.CreateFolder(ctx, folderName)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create folder '%s'", folderName)
	}
	return folder, nil
}

func (r *MigrationPlanReconciler) migrateRDMdisks(ctx context.Context, migrationplan *vjailbreakv1alpha1.MigrationPlan, vmMachines map[string]*vjailbreakv1alpha1.VMwareMachine, openstackcreds *vjailbreakv1alpha1.OpenstackCreds) error {
	allRDMDisks := []*vjailbreakv1alpha1.RDMDisk{}
	rdmDiskCRToBeUpdated := make([]vjailbreakv1alpha1.RDMDisk, 0)
	for _, vmMachine := range vmMachines {
		if len(vmMachine.Spec.VMInfo.RDMDisks) > 0 {
			// Check if VM is powered off
			if vmMachine.Status.PowerState != string(govmomitypes.VirtualMachineGuestStateNotRunning) {
				return fmt.Errorf("VM %s is not powered off, cannot migrate RDM disks", vmMachine.Name)
			}
			for _, rdmDisk := range vmMachine.Spec.VMInfo.RDMDisks {
				// Get RDMDisk CR
				rdmDiskCR := &vjailbreakv1alpha1.RDMDisk{}
				err := r.Get(ctx, types.NamespacedName{
					Name:      strings.TrimSpace(rdmDisk),
					Namespace: migrationplan.Namespace,
				}, rdmDiskCR)
				if err != nil {
					return fmt.Errorf("failed to get RDMDisk CR: %w", err)
				}
				// Validate that all ownerVMs are present in parallelVMs
				// This validation can be disabled via vjailbreak-settings ConfigMap
				vjailbreakSettings, err := k8sutils.GetVjailbreakSettings(ctx, r.Client)
				switch err {
				case nil:
					if vjailbreakSettings.ValidateRDMOwnerVMs {
						for _, ownerVM := range rdmDiskCR.Spec.OwnerVMs {
							if _, ok := vmMachines[ownerVM]; !ok {
								log.FromContext(ctx).Error(fmt.Errorf("ownerVM %q in RDM disk %s not found in migration plan", ownerVM, rdmDisk), "verify migration plan")
								return fmt.Errorf("ownerVM %q in RDM disk %s not found in migration plan", ownerVM, rdmDisk)
							}
						}
					} else {
						log.FromContext(ctx).Info("RDM disk owner VM validation disabled via vjailbreak-settings", "rdmDisk", rdmDisk)
					}
				default:
					// Successfully retrieved settings, proceed with validation
					log.FromContext(ctx).Error(err, "Failed to get vjailbreak settings, using default validation behavior")
					// Fall back to default behavior (validation enabled) if we can't get settings
					for _, ownerVM := range rdmDiskCR.Spec.OwnerVMs {
						if _, ok := vmMachines[ownerVM]; !ok {
							log.FromContext(ctx).Error(fmt.Errorf("ownerVM %q in RDM disk %s not found in migration plan", ownerVM, rdmDisk), "verify migration plan")
						}
					}
				}
				// Update existing RDMDisk CR
				err = ValidateRDMDiskFields(rdmDiskCR)
				if err != nil {
					return fmt.Errorf("failed to validate RDMDisk CR: %w", err)
				}
				// collect RDM disks that need to be updated (to be imported to Cinder)
				if !rdmDiskCR.Spec.ImportToCinder {
					rdmDiskCRToBeUpdated = append(rdmDiskCRToBeUpdated, *rdmDiskCR)
				}
				allRDMDisks = append(allRDMDisks, rdmDiskCR)
			}
		}
	}
	// Update all RDMDisk CRs that need to be updated
	for _, rdmDiskCR := range rdmDiskCRToBeUpdated {
		if rdmDiskCR.Status.Phase == RDMPhaseManaging || rdmDiskCR.Status.Phase == RDMPhaseManaged || rdmDiskCR.Status.Phase == RDMPhaseError {
			log.FromContext(ctx).Info("Skipping update for RDMDisk CR as it is already being managed or in error state", "rdmDiskName", rdmDiskCR.Name, "phase", rdmDiskCR.Status.Phase)
			continue
		}

		// Use retry to handle resourceVersion conflicts gracefully,
		// on error it will re-fetch the latest version and before
		// retrying update will check importToCinder flag again.
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			rdmDisk := &vjailbreakv1alpha1.RDMDisk{}
			if err := r.Get(ctx, types.NamespacedName{
				Name:      strings.TrimSpace(rdmDiskCR.Name),
				Namespace: migrationplan.Namespace,
			}, rdmDisk); err != nil {
				return err
			}
			// Idempotent guard - check if already marked for import
			if rdmDisk.Spec.ImportToCinder {
				log.FromContext(ctx).Info("Skipping update, already marked for import",
					"rdmDisk", rdmDisk.Name)
				return nil
			}

			// Migration Plan controller only sets ImportToCinder to
			// true, the RDMDisk controller ensures that RDM disk is
			// managed and imported to Cinder
			rdmDisk.Spec.ImportToCinder = true
			rdmDisk.Spec.OpenstackVolumeRef.OpenstackCreds = openstackcreds.GetName()

			if err := r.Update(ctx, rdmDisk); err != nil {
				if apierrors.IsConflict(err) {
					log.FromContext(ctx).Info("Conflict detected while updating RDMDisk — another controller updated it first",
						"rdmDisk", rdmDisk.Name)
					// RetryOnConflict will re-fetch and retry
					return err
				}
				return fmt.Errorf("failed to update RDMDisk CR: %w", err)
			}

			log.FromContext(ctx).Info("Updated RDMDisk CR to import into Cinder",
				"rdmDisk", rdmDisk.Name,
				"openstackCreds", openstackcreds.GetName())

			return nil
		})
		if err != nil {
			// if still failing after retries, bubble up
			return err
		}
	}

	// Check if all RDM disks are migrated after the delay
	for _, rdmDiskCR := range allRDMDisks {
		reFetchedRDMDiskCR := &vjailbreakv1alpha1.RDMDisk{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      strings.TrimSpace(rdmDiskCR.Name),
			Namespace: migrationplan.Namespace,
		}, reFetchedRDMDiskCR)
		if err != nil {
			return err
		}
		if reFetchedRDMDiskCR.Status.Phase != RDMPhaseManaged || reFetchedRDMDiskCR.Status.CinderVolumeID == "" {
			// Log which disk is not ready
			r.ctxlog.Info("RDM disk not yet managed, will retry", "diskName", reFetchedRDMDiskCR.Name, "phase", reFetchedRDMDiskCR.Status.Phase, "cinderVolumeID", reFetchedRDMDiskCR.Status.CinderVolumeID)
			return verrors.ErrRDMDiskNotMigrated
		}
	}
	return nil
}

// validates that the VM has a valid OS type
func (r *MigrationPlanReconciler) validateVMOS(vmMachine *vjailbreakv1alpha1.VMwareMachine) (bool, bool, error) {
	validOSTypes := []string{"windowsGuest", "linuxGuest"}
	osFamily := strings.TrimSpace(vmMachine.Spec.VMInfo.OSFamily)

	if osFamily == "" || osFamily == "unknown" {
		r.ctxlog.Info("VM has unknown or unspecified OS type and will be skipped",
			"vmName", vmMachine.Spec.VMInfo.Name)
		return false, true, nil
	}

	for _, validOS := range validOSTypes {
		if osFamily == validOS {
			return true, false, nil
		}
	}

	return false, false, fmt.Errorf("vm '%s' has an unsupported OS type: %s",
		vmMachine.Spec.VMInfo.Name, osFamily)
}

// validates all VMs in the migration plan
func (r *MigrationPlanReconciler) validateMigrationPlanVMs(
	ctx context.Context,
	migrationplan *vjailbreakv1alpha1.MigrationPlan,
	migrationtemplate *vjailbreakv1alpha1.MigrationTemplate,
	vmwcreds *vjailbreakv1alpha1.VMwareCreds,
) ([]*vjailbreakv1alpha1.VMwareMachine, []*vjailbreakv1alpha1.VMwareMachine, error) {
	var validVMs, skippedVMs []*vjailbreakv1alpha1.VMwareMachine

	if len(migrationplan.Spec.VirtualMachines) == 0 {
		return nil, nil, fmt.Errorf("no VMs to migrate in migration plan")
	}

	for _, vmGroup := range migrationplan.Spec.VirtualMachines {
		for _, vm := range vmGroup {
			vmMachine, err := GetVMwareMachineForVM(ctx, r, vm, migrationtemplate, vmwcreds)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get VMwareMachine for VM %s: %w", vm, err)
			}

			_, skipped, err := r.validateVMOS(vmMachine)
			if err != nil {
				return nil, nil, err
			}
			if skipped {
				skippedVMs = append(skippedVMs, vmMachine)
				continue
			}

			validVMs = append(validVMs, vmMachine)
		}
	}

	if len(validVMs) == 0 {
		if len(skippedVMs) > 0 {
			skippedVMNames := make([]string, len(skippedVMs))
			for i, vm := range skippedVMs {
				skippedVMNames[i] = vm.Spec.VMInfo.Name
			}
			msg := fmt.Sprintf("Skipped VMs due to unsupported or unknown OS: %v", skippedVMNames)
			r.ctxlog.Info(msg)
		}
		return nil, skippedVMs, fmt.Errorf("all VMs have unknown or unsupported OS types; no migrations to run")
	}

	if len(skippedVMs) > 0 {
		skippedVMNames := make([]string, len(skippedVMs))
		for i, vm := range skippedVMs {
			skippedVMNames[i] = vm.Spec.VMInfo.Name
		}
		msg := fmt.Sprintf("Skipped VMs due to unsupported or unknown OS: %v", skippedVMNames)
		r.ctxlog.Info(msg)
		if updateErr := r.UpdateMigrationPlanStatus(ctx, migrationplan, corev1.PodPending, msg); updateErr != nil {
			r.ctxlog.Error(updateErr, "Failed to update migration plan status for skipped VMs")
		}
	}

	return validVMs, skippedVMs, nil
}

// getDatacenterForVM retrieves the datacenter for a given VM from its VMwareMachine annotation
func (r *MigrationPlanReconciler) getDatacenterForVM(ctx context.Context, vm string, vmwcreds *vjailbreakv1alpha1.VMwareCreds, migrationtemplate *vjailbreakv1alpha1.MigrationTemplate) (string, error) {
	vmMachine, err := GetVMwareMachineForVM(ctx, r, vm, migrationtemplate, vmwcreds)
	if err != nil {
		return "", errors.Wrapf(err, "failed to get VMwareMachine for VM %s", vm)
	}

	if vmMachine.Annotations == nil {
		return "", fmt.Errorf("VMwareMachine %s has no annotations", vmMachine.Name)
	}

	datacenter, exists := vmMachine.Annotations[constants.VMwareDatacenterLabel]
	if !exists || datacenter == "" {
		return "", fmt.Errorf("VMwareMachine %s has no datacenter annotation", vmMachine.Name)
	}

	return datacenter, nil
}

// updateMigrationPhaseWithRetry updates a Migration's phase and condition with retry logic
func (r *MigrationPlanReconciler) updateMigrationPhaseWithRetry(
	ctx context.Context,
	migrationObj *vjailbreakv1alpha1.Migration,
	phase vjailbreakv1alpha1.VMMigrationPhase,
	condition corev1.PodCondition,
	identifier string,
) error {
	migration := &vjailbreakv1alpha1.Migration{}
	pollErr := wait.PollUntilContextTimeout(
		ctx,
		200*time.Millisecond,
		5*time.Second,
		true,
		func(pctx context.Context) (bool, error) {
			getErr := r.Get(pctx, types.NamespacedName{
				Name:      migrationObj.Name,
				Namespace: migrationObj.Namespace,
			}, migration)

			if apierrors.IsNotFound(getErr) {
				return false, nil
			}
			return getErr == nil, getErr
		},
	)

	if pollErr != nil {
		r.ctxlog.Error(pollErr, "Migration object never appeared in API server", "identifier", identifier)
		return pollErr
	}

	retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &vjailbreakv1alpha1.Migration{}
		if err := r.Get(ctx, types.NamespacedName{Name: migrationObj.Name, Namespace: migrationObj.Namespace}, latest); err != nil {
			return err
		}

		latest.Status.Phase = phase
		latest.Status.Conditions = append(latest.Status.Conditions, condition)
		return r.Status().Update(ctx, latest)
	})

	if retryErr != nil {
		r.ctxlog.Error(retryErr, "Failed to update migration phase after retries", "phase", phase, "identifier", identifier)
		return retryErr
	}

	return nil
}

// markMigrationValidationFailed updates a Migration status to ValidationFailed
func (r *MigrationPlanReconciler) markMigrationValidationFailed(ctx context.Context, migrationObj *vjailbreakv1alpha1.Migration, vmName string, message string) {
	condition := corev1.PodCondition{
		Type:               "Validated",
		Status:             corev1.ConditionFalse,
		Reason:             "VMValidationFailed",
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
	if err := r.updateMigrationPhaseWithRetry(ctx, migrationObj, vjailbreakv1alpha1.VMMigrationPhaseValidationFailed, condition, vmName); err != nil {
		r.ctxlog.Error(err, "Failed to mark migration as ValidationFailed", "vm", vmName)
	}
}

// markMigrationFailed updates a Migration status to Failed
func (r *MigrationPlanReconciler) markMigrationFailed(ctx context.Context, migrationObj *vjailbreakv1alpha1.Migration, message string) error {
	condition := corev1.PodCondition{
		Type:               "Failed",
		Status:             corev1.ConditionTrue,
		Reason:             "MigrationFailed",
		Message:            message,
		LastTransitionTime: metav1.Now(),
	}
	return r.updateMigrationPhaseWithRetry(ctx, migrationObj, vjailbreakv1alpha1.VMMigrationPhaseFailed, condition, migrationObj.Name)
}
