// Copyright © 2024 The vjailbreak authors

package migrate

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/pkg/errors"
	"github.com/platform9/vjailbreak/pkg/vpwned/sdk/storage"
	_ "github.com/platform9/vjailbreak/pkg/vpwned/sdk/storage/pure"
	"github.com/platform9/vjailbreak/v2v-helper/nbd"
	"github.com/platform9/vjailbreak/v2v-helper/openstack"
	"github.com/platform9/vjailbreak/v2v-helper/pkg/constants"
	"github.com/platform9/vjailbreak/v2v-helper/pkg/k8sutils"
	"github.com/platform9/vjailbreak/v2v-helper/pkg/utils"
	"github.com/platform9/vjailbreak/v2v-helper/pkg/utils/vmutils"
	"github.com/platform9/vjailbreak/v2v-helper/reporter"
	"github.com/platform9/vjailbreak/v2v-helper/vcenter"
	"github.com/platform9/vjailbreak/v2v-helper/virtv2v"
	"github.com/platform9/vjailbreak/v2v-helper/vm"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netutils "github.com/platform9/vjailbreak/pkg/common/utils"
	probing "github.com/prometheus-community/pro-bing"
	"github.com/vmware/govmomi/vim25/types"
)

type Migrate struct {
	URL                     string
	UserName                string
	Password                string
	Insecure                bool
	Networknames            []string
	Networkports            []string
	Volumetypes             []string
	Virtiowin               string
	Ostype                  string
	Thumbprint              string
	Convert                 bool
	DisconnectSourceNetwork bool
	Openstackclients        openstack.OpenstackOperations
	Vcclient                vcenter.VCenterOperations
	VMops                   vm.VMOperations
	Nbdops                  []nbd.NBDOperations
	EventReporter           chan string
	PodLabelWatcher         chan string
	InPod                   bool
	MigrationTimes          MigrationTimes
	MigrationType           string
	PerformHealthChecks     bool
	HealthCheckPort         string
	K8sClient               client.Client
	TargetFlavorId          string
	TargetAvailabilityZone  string
	AssignedIP              string
	SecurityGroups          []string
	ServerGroup             string
	RDMDisks                []string
	UseFlavorless           bool
	TenantName              string
	Reporter                *reporter.Reporter
	FallbackToDHCP          bool
	StorageCopyMethod       string
	// Array credentials for StorageAcceleratedCopy storage migration
	ArrayHost         string
	ArrayUser         string
	ArrayPassword     string
	ArrayInsecure     bool
	VendorType        string
	ArrayCredsMapping string
	StorageProvider   storage.StorageProvider
	ESXiSSHPrivateKey []byte
	ESXiSSHSecretName string // Name of the Kubernetes secret containing ESXi SSH private key
}

type MigrationTimes struct {
	DataCopyStart  time.Time
	VMCutoverStart time.Time
	VMCutoverEnd   time.Time
}

type PeriodicSyncStates int

const (
	initial PeriodicSyncStates = iota
	cleanedSnapshot
	TookSnapshot
	SyncCompleted
)

// disconnects the source VM's network interfaces
func (migobj *Migrate) DisconnectSourceNetworkIfRequested() error {
	if !migobj.DisconnectSourceNetwork {
		return nil
	}

	migobj.logMessage(fmt.Sprintf("Disconnecting source VM network interfaces (DisconnectSourceNetwork=%v)", migobj.DisconnectSourceNetwork))

	if err := migobj.VMops.DisconnectNetworkInterfaces(); err != nil {
		errMsg := fmt.Sprintf("Failed to disconnect source VM network interfaces: %v", err)
		migobj.logMessage("ERROR: " + errMsg)
		return fmt.Errorf("failed to disconnect network interfaces: %w", err)
	}

	migobj.logMessage("Successfully disconnected source VM network interfaces")
	return nil
}

func (migobj *Migrate) logMessage(message string) {
	if migobj.InPod {
		migobj.EventReporter <- message
	}
	utils.PrintLog(message)
}

// This function creates volumes in OpenStack and attaches them to the helper vm
func (migobj *Migrate) CreateVolumes(ctx context.Context, vminfo vm.VMInfo) (vm.VMInfo, error) {
	openstackops := migobj.Openstackclients
	migobj.logMessage("Creating volumes in OpenStack")

	for idx, vmdisk := range vminfo.VMDisks {
		setRDMLabel := false
		if len(vminfo.RDMDisks) > 0 {
			setRDMLabel = true
		}
		volume, err := openstackops.CreateVolume(ctx, vminfo.Name+"-"+vmdisk.Name, vmdisk.Size, vminfo.OSType, vminfo.UEFI, migobj.Volumetypes[idx], setRDMLabel)
		if err != nil {
			return vminfo, errors.Wrap(err, "failed to create volume")
		}
		vminfo.VMDisks[idx].OpenstackVol = volume
		if vminfo.VMDisks[idx].Boot {
			err = openstackops.SetVolumeBootable(ctx, volume)
			if err != nil {
				return vminfo, errors.Wrap(err, "failed to set volume as bootable")
			}
		}
	}
	migobj.logMessage("Volumes created successfully")
	return vminfo, nil
}

func (migobj *Migrate) AttachVolume(ctx context.Context, disk vm.VMDisk) (string, error) {
	openstackops := migobj.Openstackclients
	migobj.logMessage(fmt.Sprintf("Attaching volumes to VM: %s", disk.Name))
	if disk.OpenstackVol == nil {
		return "", errors.Wrap(fmt.Errorf("OpenStack volume is nil"), "failed to attach volume to VM")
	}
	volumeID := disk.OpenstackVol.ID
	if err := openstackops.AttachVolumeToVM(ctx, volumeID); err != nil {
		return "", errors.Wrap(err, "failed to attach volume to VM")
	}

	// Get the Path of the attached volume
	devicePath, err := openstackops.FindDevice(volumeID)
	if err != nil {
		return "", errors.Wrap(err, "failed to find device")
	}
	return devicePath, nil
}

func (migobj *Migrate) DetachVolume(ctx context.Context, disk vm.VMDisk) error {
	openstackops := migobj.Openstackclients

	if err := openstackops.DetachVolumeFromVM(ctx, disk.OpenstackVol.ID); err != nil {
		return errors.Wrap(err, "failed to detach volume from VM")
	}

	err := openstackops.WaitForVolume(ctx, disk.OpenstackVol.ID)
	if err != nil {
		return errors.Wrap(err, "failed to wait for volume to become available")
	}
	return nil
}

func (migobj *Migrate) DetachAllVolumes(ctx context.Context, vminfo vm.VMInfo) error {
	openstackops := migobj.Openstackclients
	for _, vmdisk := range vminfo.VMDisks {
		migobj.logMessage(fmt.Sprintf("Detaching volume %s from VM", vmdisk.Name))
		if err := openstackops.DetachVolumeFromVM(ctx, vmdisk.OpenstackVol.ID); err != nil && !strings.Contains(err.Error(), "is not attached to volume") {
			return errors.Wrap(err, "failed to detach volume from VM")
		}

		err := openstackops.WaitForVolume(ctx, vmdisk.OpenstackVol.ID)
		if err != nil {
			return errors.Wrap(err, "failed to wait for volume to become available")
		}
		migobj.logMessage(fmt.Sprintf("Volume %s detached from VM", vmdisk.Name))
	}
	time.Sleep(1 * time.Second)
	return nil
}

func (migobj *Migrate) DeleteAllVolumes(ctx context.Context, vminfo vm.VMInfo) error {
	openstackops := migobj.Openstackclients
	for _, vmdisk := range vminfo.VMDisks {
		err := openstackops.DeleteVolume(ctx, vmdisk.OpenstackVol.ID)
		if err != nil {
			return errors.Wrap(err, "failed to delete volume")
		}
		migobj.logMessage(fmt.Sprintf("Volume %s deleted", vmdisk.Name))
	}
	return nil
}

// extractFileName extracts the file name from a full VMDK path
func extractFileName(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return path
}

// logDiskCopyPlan logs the disk copy plan showing source to target mapping
// Helps with debugging by showing exactly which VMDK goes to which volume
func (migobj *Migrate) logDiskCopyPlan(vminfo vm.VMInfo) {
	migobj.logMessage("=== Disk Copy Plan ===")
	for idx, disk := range vminfo.VMDisks {
		migobj.logMessage(fmt.Sprintf("[%d] %s (DeviceKey=%d): %s -> Volume %s (%s)",
			idx,
			disk.Name,
			disk.Disk.Key,
			extractFileName(disk.SnapBackingDisk),
			disk.OpenstackVol.Name,
			disk.Path))
	}
}

// validateDiskMapping validates that disk mapping is correct before starting copy
// Cross-checks vminfo data with nbdops to ensure correct source-to-target mapping
func (migobj *Migrate) validateDiskMapping(vminfo vm.VMInfo) error {
	migobj.logMessage("Validating disk mapping before copy operation...")

	// Verify number of disks matches number of NBD servers
	if len(vminfo.VMDisks) != len(migobj.Nbdops) {
		return fmt.Errorf("disk count mismatch: vminfo has %d disks but %d NBD servers configured", len(vminfo.VMDisks), len(migobj.Nbdops))
	}

	for idx, vmdisk := range vminfo.VMDisks {
		// Validate volume exists
		if vmdisk.OpenstackVol == nil {
			return fmt.Errorf("OpenStack volume is nil for disk %s (DeviceKey=%d)", vmdisk.Name, vmdisk.Disk.Key)
		}

		// Validate device path exists
		if vmdisk.Path == "" {
			return fmt.Errorf("device path is empty for disk %s (DeviceKey=%d)", vmdisk.Name, vmdisk.Disk.Key)
		}

		// Validate snapshot backing disk exists
		if vmdisk.SnapBackingDisk == "" {
			return fmt.Errorf("snapshot backing disk is empty for disk %s (DeviceKey=%d)", vmdisk.Name, vmdisk.Disk.Key)
		}

		// Cross-check: verify NBD server at this index is initialized
		if migobj.Nbdops[idx] == nil {
			return fmt.Errorf("NBD server not initialized for disk %d (%s)", idx, vmdisk.Name)
		}

		// Log validation details for this disk
		utils.PrintLog(fmt.Sprintf("[%d] Validated %s (DeviceKey=%d): SnapFile=%s, Volume=%s, Path=%s",
			idx, vmdisk.Name, vmdisk.Disk.Key,
			extractFileName(vmdisk.SnapBackingDisk),
			vmdisk.OpenstackVol.ID, vmdisk.Path))
	}

	migobj.logMessage("Disk mapping validation passed")
	return nil
}

// This function enables CBT on the VM if it is not enabled and takes a snapshot for initializing CBT
func (migobj *Migrate) EnableCBTWrapper() error {
	vmops := migobj.VMops
	cbt, err := vmops.IsCBTEnabled()
	if err != nil {
		return errors.Wrap(err, "failed to check if CBT is enabled")
	}
	migobj.logMessage(fmt.Sprintf("CBT Enabled: %t", cbt))

	if !cbt {
		// 7.5. Enable CBT
		migobj.logMessage("CBT is not enabled. Enabling CBT")
		err = vmops.EnableCBT()
		if err != nil {
			return errors.Wrap(err, "failed to enable CBT")
		}
		_, err := vmops.IsCBTEnabled()
		if err != nil {
			return errors.Wrap(err, "failed to check if CBT is enabled")
		}
		migobj.logMessage("Creating temporary snapshot of the source VM")
		err = vmops.TakeSnapshot("tmp-snap")
		if err != nil {
			return errors.Wrap(err, "failed to take snapshot of source VM")
		}
		utils.PrintLog("Snapshot created successfully")
		err = vmops.DeleteSnapshot("tmp-snap")
		if err != nil {
			return errors.Wrap(err, "failed to delete snapshot of source VM")
		}
		utils.PrintLog("Snapshot deleted successfully")
		migobj.logMessage("CBT enabled successfully")
	}
	return nil
}

func (migobj *Migrate) WaitforCutover() error {
	var zerotime time.Time
	if !migobj.MigrationTimes.VMCutoverStart.Equal(zerotime) && migobj.MigrationTimes.VMCutoverStart.After(time.Now()) {
		migobj.logMessage("Waiting for VM Cutover start time")
		time.Sleep(time.Until(migobj.MigrationTimes.VMCutoverStart))
		migobj.logMessage("VM Cutover start time reached")
	} else {
		if !migobj.MigrationTimes.VMCutoverEnd.Equal(zerotime) && migobj.MigrationTimes.VMCutoverEnd.Before(time.Now()) {
			return errors.New("VM Cutover End time has already passed")
		}
	}
	return nil
}
func (migobj *Migrate) SyncCBT(ctx context.Context, vminfo vm.VMInfo) error {
	migobj.logMessage("Starting Periodic sync process")
	defer migobj.logMessage("Periodic sync process completed")
	vmops := migobj.VMops
	nbdops := migobj.Nbdops
	envURL := migobj.URL
	envUserName := migobj.UserName
	envPassword := migobj.Password
	thumbprint := migobj.Thumbprint
	migration_snapshot, err := vmops.GetSnapshot(constants.MigrationSnapshotName)
	if err != nil {
		return errors.Wrap(err, "failed to get snapshot")
	}

	var changedAreas types.DiskChangeInfo

	for idx := range vminfo.VMDisks {
		changedAreas, err = vmops.CustomQueryChangedDiskAreas(vminfo.VMDisks[idx].ChangeID, migration_snapshot, vminfo.VMDisks[idx].Disk, 0)
		if err != nil {
			return errors.Wrap(err, "failed to get changed disk areas")
		}

		if len(changedAreas.ChangedArea) == 0 {
			migobj.logMessage(fmt.Sprintf("Periodic Sync: Disk %d: No changed blocks found. Skipping copy", idx))
		} else {
			migobj.logMessage(fmt.Sprintf("Periodic Sync: Disk %d: Blocks have Changed.", idx))

			utils.PrintLog("Restarting NBD server")
			err = nbdops[idx].StopNBDServer()
			if err != nil {
				return errors.Wrap(err, "failed to stop NBD server")
			}

			err = nbdops[idx].StartNBDServer(vmops.GetVMObj(), envURL, envUserName, envPassword, thumbprint, vminfo.VMDisks[idx].Snapname, vminfo.VMDisks[idx].SnapBackingDisk, migobj.EventReporter)
			if err != nil {
				return errors.Wrap(err, "failed to start NBD server")
			}
			// sleep for 2 seconds to allow the NBD server to start
			time.Sleep(2 * time.Second)

			// 11. Copy Changed Blocks over
			changedBlockCopySuccess := true
			startTime := time.Now()
			migobj.logMessage(fmt.Sprintf("Periodic Sync: Starting incremental block copy for disk %d at %s", idx, startTime))
			err = nbdops[idx].CopyChangedBlocks(ctx, changedAreas, vminfo.VMDisks[idx].Path)
			if err != nil {
				select {
				case <-ctx.Done():
					err = vmops.CleanUpSnapshots(false)
					changedBlockCopySuccess = false
					if err != nil {
						return errors.Wrap(err, "failed to cleanup snapshot of source VM")
					}
				default:
					return errors.Wrap(err, "failed to copy changed blocks")
				}
			}

			duration := time.Since(startTime)

			migobj.logMessage(fmt.Sprintf("Periodic Sync: Incremental block copy for disk %d completed in %s", idx, duration))

			err = vmops.UpdateDiskInfo(&vminfo, vminfo.VMDisks[idx], changedBlockCopySuccess)
			if err != nil {
				return errors.Wrap(err, "failed to update disk info")
			}
			if !changedBlockCopySuccess {
				migobj.logMessage(fmt.Sprintf("Periodic Sync: Failed to copy changed blocks: %s", err))
				migobj.logMessage(fmt.Sprintf("Periodic Sync: Since full copy has completed, Retrying copy of changed blocks for disk: %d", idx))
			}
		}
	}
	// Cleanup the snapshot taken for incremental copy
	return nil
}
func (migobj *Migrate) getSyncEnabled() bool {
	var enabled bool
	enabled = false
	migrationParams, err := utils.GetMigrationParams(context.Background(), migobj.K8sClient)
	if err != nil {
		return enabled
	}
	if migrationParams.PeriodicSyncEnabled {
		enabled = true
	}
	return enabled
}
func (migobj *Migrate) getSyncDuration() time.Duration {
	const defaultInterval = "1h"

	migobj.logMessage("Periodic Sync: Setting up sync interval")

	migrationParams, err := utils.GetMigrationParams(context.Background(), migobj.K8sClient)
	if err != nil {
		migobj.logMessage(fmt.Sprintf("WARNING: Failed to get migration params: %v, using default interval (%s)",
			err, defaultInterval))
	}
	// Get sync interval settings
	interval := migrationParams.PeriodicSyncInterval
	if interval == "" {
		vjailbreakSettings, err := k8sutils.GetVjailbreakSettings(context.Background(), migobj.K8sClient)
		if err != nil {
			migobj.logMessage(fmt.Sprintf("WARNING: Failed to get vjailbreak settings: %v, using default interval (%s)",
				err, defaultInterval))
		}
		interval = vjailbreakSettings.PeriodicSyncInterval
		if interval == "" {
			interval = defaultInterval
		}
	}
	// Calculate wait time based on unit
	waitTime, err := time.ParseDuration(interval)
	if err != nil {
		migobj.logMessage(fmt.Sprintf("WARNING: Failed to parse interval %s, using default interval (%s)", interval, defaultInterval))
		interval = defaultInterval
		waitTime, _ = time.ParseDuration(interval)
	} else if waitTime < 5*time.Minute {
		migobj.logMessage(fmt.Sprintf("WARNING: Interval %s is less than 5 minutes, falling back to 5m", interval))
		waitTime = 5 * time.Minute
	}
	return waitTime
}

func (migobj *Migrate) WaitforAdminCutover(ctx context.Context, vminfo vm.VMInfo) error {
	var syncInterval time.Duration
	var maxRetries uint64
	var capInterval time.Duration
	currentState := initial
	vmops := migobj.VMops
	maxRetries, capInterval = utils.GetRetryLimits()
	migobj.logMessage(constants.EventMessageWaitingForAdminCutOver)
	elapsed := time.Duration(0)
	for {
		syncEnabled := migobj.getSyncEnabled()
		if syncEnabled {
			syncInterval = migobj.getSyncDuration()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case label := <-migobj.PodLabelWatcher:
			if label == "yes" && currentState == initial {
				migobj.logMessage("Admin cutover triggered")
				return nil
			}
		default:
			if !syncEnabled {
				continue
			}
			if elapsed >= syncInterval {
				migobj.logMessage("Periodic Sync: Previous sync took longer than interval, starting next cycle immediately")
				elapsed = syncInterval
			}
			// Otherwise wait remaining time
			waitTime := syncInterval - elapsed
			migobj.logMessage(fmt.Sprintf("Periodic Sync: Waiting %s before next sync cycle", waitTime))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-migobj.PodLabelWatcher:
				return nil // admin triggered cutover during wait
			case <-time.After(waitTime):
				// wait completed → loop and sync again
			}
			// Perform sync
			migobj.logMessage(fmt.Sprintf("Periodic Sync: Starting sync cycle (interval: %s)", syncInterval))
			start := time.Now()
			if currentState == initial {

				err := utils.DoRetryWithExponentialBackoff(ctx, func() error {
					return vmops.CleanUpSnapshots(false)
				}, maxRetries, capInterval)
				if err != nil {
					migobj.logMessage(fmt.Sprintf("Periodic Sync: Failed to clean up snapshots after %d retries: %v", maxRetries, err))
					continue
				} else {
					currentState = cleanedSnapshot
				}
			}
			if currentState == cleanedSnapshot {
				err := utils.DoRetryWithExponentialBackoff(ctx, func() error {
					return vmops.TakeSnapshot(constants.MigrationSnapshotName)
				}, maxRetries, capInterval)
				if err != nil {
					migobj.logMessage(fmt.Sprintf("Periodic Sync: Failed to take snapshot '%s' after %d retries: %v", constants.MigrationSnapshotName, maxRetries, err))
					continue
				} else {
					currentState = TookSnapshot
				}
			}
			if currentState == TookSnapshot {
				if err := migobj.SyncCBT(ctx, vminfo); err != nil {
					migobj.logMessage(fmt.Sprintf("Periodic Sync: Failed to sync Changed Block Tracking (CBT): %v", err))
					continue
				} else {
					currentState = initial
				}
			}
			elapsed = time.Since(start)
		}
	}
}

func (migobj *Migrate) CheckIfAdminCutoverSelected() bool {
	if migobj.Reporter == nil {
		return false
	}
	value, err := migobj.Reporter.GetCutoverLabel()
	if err != nil {
		utils.PrintLog(fmt.Sprintf("Failed to get pod labels: %v", err))
		return false
	}
	// If label is set to no, return true. because that time the admin has initiated cutover
	if value == "no" {
		return true
	}
	return false
}

func (migobj *Migrate) LiveReplicateDisks(ctx context.Context, vminfo vm.VMInfo) (vm.VMInfo, error) {
	vmops := migobj.VMops
	nbdops := migobj.Nbdops
	envURL := migobj.URL
	envUserName := migobj.UserName
	envPassword := migobj.Password
	thumbprint := migobj.Thumbprint

	if migobj.MigrationType == "cold" && !migobj.CheckIfAdminCutoverSelected() {
		if err := vmops.VMPowerOff(); err != nil {
			return vminfo, errors.Wrap(err, "failed to power off VM")
		}
	}

	// clean up snapshots
	utils.PrintLog("Cleaning up snapshots before copy")
	err := vmops.CleanUpSnapshots(false)
	if err != nil {
		return vminfo, errors.Wrap(err, "failed to clean up snapshots: %s, please delete manually before starting again")
	}

	utils.PrintLog("Starting NBD server")
	err = vmops.TakeSnapshot(constants.MigrationSnapshotName)
	if err != nil {
		return vminfo, errors.Wrap(err, "failed to take snapshot of source VM")
	}

	err = vmops.UpdateDisksInfo(&vminfo)
	if err != nil {
		return vminfo, errors.Wrap(err, "failed to update disk info")
	}

	for idx, vmdisk := range vminfo.VMDisks {
		migobj.logMessage(fmt.Sprintf("Copying disk %d, Completed: 0%%", idx))
		err = nbdops[idx].StartNBDServer(vmops.GetVMObj(), envURL, envUserName, envPassword, thumbprint, vmdisk.Snapname, vmdisk.SnapBackingDisk, migobj.EventReporter)
		if err != nil {
			return vminfo, errors.Wrap(err, "failed to start NBD server")
		}
	}
	// sleep for 2 seconds to allow the NBD server to start
	time.Sleep(2 * time.Second)
	final := false

	for idx, vmdisk := range vminfo.VMDisks {
		vminfo.VMDisks[idx].Path, err = migobj.AttachVolume(ctx, vmdisk)
		if err != nil {
			return vminfo, errors.Wrap(err, "failed to attach volume")
		}
	}

	// Validate disk mapping before starting copy
	if err := migobj.validateDiskMapping(vminfo); err != nil {
		return vminfo, errors.Wrap(err, "disk mapping validation failed")
	}

	// Log the disk copy plan for debugging
	migobj.logDiskCopyPlan(vminfo)

	vcenterSettings, err := k8sutils.GetVjailbreakSettings(ctx, migobj.K8sClient)
	if err != nil {
		return vminfo, errors.Wrap(err, "failed to get vcenter settings")
	}
	utils.PrintLog(fmt.Sprintf("Fetched vjailbreak settings for Changed Blocks Copy Iteration Threshold: %d", vcenterSettings.ChangedBlocksCopyIterationThreshold))

	// Check if migration has admin cutover if so don't copy any more changed blocks
	adminInitiatedCutover := migobj.CheckIfAdminCutoverSelected()

	incrementalCopyCount := 0
	for {
		// If its the first copy, copy the entire disk
		if incrementalCopyCount == 0 {
			for idx := range vminfo.VMDisks {
				startTime := time.Now()
				disk := vminfo.VMDisks[idx]

				migobj.logMessage(fmt.Sprintf("Starting full disk copy [%d/%d]: %s (DeviceKey=%d)",
					idx+1, len(vminfo.VMDisks), disk.Name, disk.Disk.Key))
				migobj.logMessage(fmt.Sprintf("  Source: %s", extractFileName(disk.SnapBackingDisk)))
				migobj.logMessage(fmt.Sprintf("  Target: %s (Volume ID: %s)", disk.Path, disk.OpenstackVol.ID))

				err = nbdops[idx].CopyDisk(ctx, disk.Path, idx)
				if err != nil {
					return vminfo, errors.Wrap(err, fmt.Sprintf("failed to copy disk %s (DeviceKey=%d)", disk.Name, disk.Disk.Key))
				}
				duration := time.Since(startTime)
				if migobj.MigrationType == "cold" {
					migobj.logMessage(fmt.Sprintf("✓ Disk %d (%s) copied successfully in %s", idx, disk.Name, duration))
				} else {
					migobj.logMessage(fmt.Sprintf("✓ Disk %d (%s) copied successfully in %s, copying changed blocks now", idx, disk.Name, duration))
				}
			}

			if adminInitiatedCutover {
				utils.PrintLog("Admin initiated cutover detected, skipping changed blocks copy")
				if err := migobj.WaitforAdminCutover(ctx, vminfo); err != nil {
					return vminfo, errors.Wrap(err, "failed to start VM Cutover")
				}
				utils.PrintLog("Shutting down source VM and performing final copy")
				err = vmops.VMPowerOff()
				if err != nil {
					return vminfo, errors.Wrap(err, "failed to power off VM")
				}
			}
			if err := migobj.WaitforCutover(); err != nil {
				return vminfo, errors.Wrap(err, "failed to start VM Cutover")
			}
		} else {
			migration_snapshot, err := vmops.GetSnapshot(constants.MigrationSnapshotName)
			if err != nil {
				return vminfo, errors.Wrap(err, "failed to get snapshot")
			}

			var changedAreas types.DiskChangeInfo
			done := true

			for idx := range vminfo.VMDisks {
				changedAreas, err = vmops.CustomQueryChangedDiskAreas(vminfo.VMDisks[idx].ChangeID, migration_snapshot, vminfo.VMDisks[idx].Disk, 0)
				if err != nil {
					return vminfo, errors.Wrap(err, "failed to get changed disk areas")
				}

				if len(changedAreas.ChangedArea) == 0 {
					if migobj.MigrationType != "cold" {
						migobj.logMessage(fmt.Sprintf("Disk %d: No changed blocks found. Skipping copy", idx))
					}
				} else {
					migobj.logMessage(fmt.Sprintf("Disk %d: Blocks have Changed.", idx))

					utils.PrintLog("Restarting NBD server")
					err = nbdops[idx].StopNBDServer()
					if err != nil {
						return vminfo, errors.Wrap(err, "failed to stop NBD server")
					}

					err = nbdops[idx].StartNBDServer(vmops.GetVMObj(), envURL, envUserName, envPassword, thumbprint, vminfo.VMDisks[idx].Snapname, vminfo.VMDisks[idx].SnapBackingDisk, migobj.EventReporter)
					if err != nil {
						return vminfo, errors.Wrap(err, "failed to start NBD server")
					}
					// sleep for 2 seconds to allow the NBD server to start
					time.Sleep(2 * time.Second)

					// 11. Copy Changed Blocks over
					done = false
					changedBlockCopySuccess := true
					migobj.logMessage("Copying changed blocks")

					// incremental block copy

					startTime := time.Now()
					migobj.logMessage(fmt.Sprintf("Starting incremental block copy for disk %d at %s", idx, startTime))

					err = nbdops[idx].CopyChangedBlocks(ctx, changedAreas, vminfo.VMDisks[idx].Path)
					if err != nil {
						changedBlockCopySuccess = false
					}

					duration := time.Since(startTime)

					migobj.logMessage(fmt.Sprintf("Incremental block copy for disk %d completed in %s", idx, duration))

					err = vmops.UpdateDiskInfo(&vminfo, vminfo.VMDisks[idx], changedBlockCopySuccess)
					if err != nil {
						return vminfo, errors.Wrap(err, "failed to update disk info")
					}
					if !changedBlockCopySuccess {
						migobj.logMessage(fmt.Sprintf("Failed to copy changed blocks: %s", err))
						migobj.logMessage(fmt.Sprintf("Since full copy has completed, Retrying copy of changed blocks for disk: %d", idx))
					}
					migobj.logMessage(fmt.Sprintf("Finished copying and syncing changed blocks for disk %d in %s [Progress: %d/20]", idx, duration, incrementalCopyCount))
				}
			}
			if final {
				break
			}
			if done || incrementalCopyCount > vcenterSettings.ChangedBlocksCopyIterationThreshold {
				utils.PrintLog("Shutting down source VM and performing final copy")
				err = vmops.VMPowerOff()
				if err != nil {
					return vminfo, errors.Wrap(err, "failed to power off VM")
				}
				final = true
			}
		}

		// Update old change id to the new base change id value
		// Only do this after you have gone through all disks with old change id.
		// If you dont, only your first disk will have the updated changes

		err = vmops.CleanUpSnapshots(false)
		if err != nil {
			return vminfo, errors.Wrap(err, "failed to cleanup snapshot of source VM")
		}
		err = vmops.TakeSnapshot(constants.MigrationSnapshotName)
		if err != nil {
			return vminfo, errors.Wrap(err, "failed to take snapshot of source VM")
		}

		incrementalCopyCount += 1

	}

	err = migobj.DetachAllVolumes(ctx, vminfo)
	if err != nil {
		return vminfo, errors.Wrap(err, "Failed to detach all volumes from VM")
	}

	utils.PrintLog("Stopping NBD server")
	for _, nbdserver := range nbdops {
		err = nbdserver.StopNBDServer()
		if err != nil {
			return vminfo, errors.Wrap(err, "failed to stop NBD server")
		}
	}

	utils.PrintLog("Deleting migration snapshot")
	err = vmops.CleanUpSnapshots(true)
	if err != nil {
		migobj.logMessage(fmt.Sprintf(`Failed to cleanup snapshot of source VM: %s, since copy is completed, 
        continuing with the migration`, err))
	}
	return vminfo, nil
}

// getBootCommand returns the appropriate command to detect boot volume based on OS type
func (migobj *Migrate) getBootCommand(osType string) string {
	switch strings.ToLower(osType) {
	case constants.OSFamilyWindows:
		return "ls /Windows"
	case constants.OSFamilyLinux:
		return "ls /boot"
	default:
		return "inspect-os"
	}
}

// attachAllVolumes attaches all volumes and updates their paths in vminfo
func (migobj *Migrate) attachAllVolumes(ctx context.Context, vminfo *vm.VMInfo) error {
	for idx, vmdisk := range vminfo.VMDisks {
		path, err := migobj.AttachVolume(ctx, vmdisk)
		if err != nil {
			return errors.Wrap(err, "failed to attach volume")
		}
		vminfo.VMDisks[idx].Path = path
	}
	return nil
}

// detectBootVolume identifies which volume contains the boot partition
func (migobj *Migrate) detectBootVolume(vminfo vm.VMInfo, getBootCommand string) (bootVolumeIndex int, osPath string, useSingleDisk bool, err error) {
	bootVolumeIndex = -1

	for idx := range vminfo.VMDisks {
		ans, cmdErr := virtv2v.RunCommandInGuest(vminfo.VMDisks[idx].Path, getBootCommand, false)
		if cmdErr != nil {
			utils.PrintLog(fmt.Sprintf("Error running '%s'. Error: '%s', Output: %s\n", getBootCommand, cmdErr, strings.TrimSpace(ans)))
			continue
		}

		if ans == "" {
			continue
		}

		utils.PrintLog(fmt.Sprintf("Output from '%s' - '%s'\n", getBootCommand, strings.TrimSpace(ans)))
		osPath = strings.TrimSpace(ans)
		bootVolumeIndex = idx
		useSingleDisk = true
		break
	}

	return bootVolumeIndex, osPath, useSingleDisk, nil
}

// handleLinuxOSDetection handles OS detection and validation for Linux systems
func (migobj *Migrate) handleLinuxOSDetection(vminfo vm.VMInfo, bootVolumeIndex int, useSingleDisk bool, osPath string, autoFstabUpdate bool) (finalBootIndex int, finalOsPath string, osRelease string, err error) {
	finalBootIndex = bootVolumeIndex
	finalOsPath = osPath

	if useSingleDisk {
		osRelease, err = virtv2v.GetOsRelease(vminfo.VMDisks[bootVolumeIndex].Path)
		if err != nil {
			return -1, "", "", errors.Wrap(err, "failed to get os release")
		}
	} else {
		// Run get-bootable-partition.sh script
		migobj.logMessage("Running get-bootable-partition.sh script")
		var ans string
		var cmdErr error

		if ans, cmdErr = virtv2v.RunGetBootablePartitionScript(vminfo.VMDisks); cmdErr != nil {
			migobj.logMessage(fmt.Sprintf("Warning: Failed to run get-bootable-partition.sh: %v", cmdErr))
			// Don't fail the migration, just log the warning
		} else {
			if ans == "" {
				migobj.logMessage("Failed to run get-bootable-partition.sh script, empty output")
			} else {
				migobj.logMessage(fmt.Sprintf("Successfully ran get-bootable-partition.sh script with output: %s", ans))
			}
		}

		if ans == "" {
			return -1, "", "", errors.New("empty bootable partition from the script")
		}

		index, err := virtv2v.RunCommandInGuestAllVolumes(vminfo.VMDisks, "device-index", false, strings.TrimSpace(ans))
		if err != nil {
			fmt.Printf("failed to run command (%s): %v: %s\n", index, err, strings.TrimSpace(index))
			return -1, "", "", err
		}

		finalBootIndex, err = strconv.Atoi(strings.TrimSpace(index))
		if err != nil {
			return -1, "", "", errors.Wrap(err, "failed to convert bootable partition index to int")
		}
		migobj.logMessage(fmt.Sprintf("Bootable partition index: %d", finalBootIndex))

		lvm, lvmErr := virtv2v.CheckForLVM(vminfo.VMDisks)
		if lvmErr != nil || lvm == "" {
			return -1, "", "", errors.Wrap(lvmErr, "OS install location not found, Failed to check for LVM")
		}
		finalOsPath = strings.TrimSpace(lvm)
		if finalBootIndex < 0 {
			finalBootIndex, err = virtv2v.GetBootableVolumeIndex(vminfo.VMDisks)
			if err != nil {
				return -1, "", "", errors.Wrap(err, "Failed to get bootable volume index")
			}
		}

		osRelease, err = virtv2v.GetOsReleaseAllVolumes(vminfo.VMDisks)
		if err != nil {
			return -1, "", "", errors.Wrapf(err, "failed to get os release: %s", strings.TrimSpace(osRelease))
		}
	}

	if err := migobj.validateLinuxOS(osRelease); err != nil {
		return -1, "", "", err
	}

	// Run generate-mount-persistence.sh script with --force-uuid option based on AUTO_FSTAB_UPDATE setting
	if autoFstabUpdate {
		migobj.logMessage("Running generate-mount-persistence.sh script with --force-uuid option")
		if err := virtv2v.RunMountPersistenceScript(vminfo.VMDisks, useSingleDisk, vminfo.VMDisks[finalBootIndex].Path); err != nil {
			migobj.logMessage(fmt.Sprintf("Warning: Failed to run generate-mount-persistence.sh: %v", err))
			// Don't fail the migration, just log the warning
		} else {
			migobj.logMessage("Successfully ran generate-mount-persistence.sh script")
		}
	} else {
		migobj.logMessage("Skipping generate-mount-persistence.sh script (AUTO_FSTAB_UPDATE is disabled)")
	}

	return finalBootIndex, finalOsPath, osRelease, nil
}

// validateLinuxOS checks if the detected Linux OS is supported
func (migobj *Migrate) validateLinuxOS(osRelease string) error {
	osDetected := strings.ToLower(strings.TrimSpace(osRelease))
	utils.PrintLog(fmt.Sprintf("OS detected by guestfish: %s", osDetected))

	supportedOS := []string{
		"redhat", "red hat", "rhel", "centos", "scientific linux",
		"oracle linux", "fedora", "sles", "sled", "opensuse",
		"alt linux", "debian", "ubuntu", "rocky linux",
		"suse linux enterprise server", "alma linux",
	}

	for _, s := range supportedOS {
		if strings.Contains(osDetected, s) {
			utils.PrintLog("operating system compatibility check passed")
			return nil
		}
	}

	return errors.Errorf("unsupported OS detected by guestfish: %s", osDetected)
}

// handleWindowsBootDetection handles boot volume detection for Windows systems
func (migobj *Migrate) handleWindowsBootDetection(vminfo vm.VMInfo, bootVolumeIndex int, useSingleDisk bool) (int, error) {
	utils.PrintLog("operating system compatibility check passed")

	if !useSingleDisk {
		utils.PrintLog("checking for bootable volume in case of LDM")
		finalBootIndex, err := virtv2v.GetBootableVolumeIndex(vminfo.VMDisks)
		if err != nil {
			return -1, errors.Wrap(err, "Failed to get bootable volume index")
		}
		return finalBootIndex, nil
	}

	return bootVolumeIndex, nil
}

// performDiskConversion runs virt-v2v conversion on the boot disk
func (migobj *Migrate) performDiskConversion(ctx context.Context, vminfo vm.VMInfo, bootVolumeIndex int, osPath, osRelease string, useSingleDisk bool) error {

	persisNetwork := utils.GetNetworkPersistance(ctx, migobj.K8sClient)

	if !migobj.Convert {
		return nil
	}

	firstbootscripts := []string{}

	// Fix NTFS for Windows
	if strings.ToLower(vminfo.OSType) == constants.OSFamilyWindows {
		if err := virtv2v.NTFSFix(vminfo.VMDisks[bootVolumeIndex].Path); err != nil {
			return errors.Wrap(err, "failed to run ntfsfix")
		}
	}

	// Add first boot scripts for RHEL family
	if virtv2v.IsRHELFamily(osRelease) {
		versionID := parseVersionID(osRelease)
		if versionID == "" {
			return errors.Errorf("failed to get version ID")
		}
		if !persisNetwork {

			majorVersion, err := strconv.Atoi(strings.Split(versionID, ".")[0])
			if err != nil {
				return fmt.Errorf("failed to parse major version: %v", err)
			}

			if majorVersion >= 7 {
				firstbootscriptname := "rhel_enable_dhcp"
				firstbootscript := constants.RhelFirstBootScript
				firstbootscripts = append(firstbootscripts, firstbootscriptname)

				if err := virtv2v.AddFirstBootScript(firstbootscript, firstbootscriptname); err != nil {
					return errors.Wrap(err, "failed to add first boot script")
				}
				utils.PrintLog("First boot script added successfully")
			}
		}
	}

	if strings.ToLower(vminfo.OSType) == constants.OSFamilyLinux {
		firstbootscriptname := "vmware_cleanup"
		firstbootscript := constants.VMwareCleanupFirstBootScript
		firstbootscripts = append(firstbootscripts, firstbootscriptname)

		if err := virtv2v.AddFirstBootScript(firstbootscript, firstbootscriptname); err != nil {
			return errors.Wrap(err, "failed to add VMware cleanup first boot script")
		}
		utils.PrintLog("VMware cleanup first boot script added successfully")
	}

	// Run virt-v2v conversion
	if err := virtv2v.ConvertDisk(ctx, constants.XMLFileName, osPath, vminfo.OSType, migobj.Virtiowin, firstbootscripts, useSingleDisk, vminfo.VMDisks[bootVolumeIndex].Path); err != nil {
		return errors.Wrap(err, "failed to run virt-v2v")
	}

	// Set volume as bootable
	if err := migobj.Openstackclients.SetVolumeBootable(ctx, vminfo.VMDisks[bootVolumeIndex].OpenstackVol); err != nil {
		return errors.Wrap(err, "failed to set volume as bootable")
	}

	return nil
}

// configureLinuxNetwork handles network configuration for Linux systems
func (migobj *Migrate) configureLinuxNetwork(ctx context.Context, vminfo vm.VMInfo, bootVolumeIndex int, osRelease string, useSingleDisk bool) error {
	persisNetwork := utils.GetNetworkPersistance(ctx, migobj.K8sClient)
	if persisNetwork {
		if err := virtv2v.InjectMacToIps(vminfo.VMDisks, useSingleDisk, vminfo.VMDisks[bootVolumeIndex].Path, vminfo.GuestNetworks, vminfo.GatewayIP, vminfo.IPperMac); err != nil {
			return errors.Wrap(err, "failed to inject mac to ips")
		}
		utils.PrintLog("Mac to ips injection completed successfully")
		versionID := parseVersionID(osRelease)
		if versionID == "" {
			return errors.Errorf("failed to get version ID")
		}
		isNetplan := isNetplanSupported(versionID) && strings.Contains(osRelease, "ubuntu")
		utils.PrintLog(fmt.Sprintf("Is netplan: %v", isNetplan))
		utils.PrintLog("Running network persistence script")
		if err := virtv2v.RunNetworkPersistence(vminfo.VMDisks, useSingleDisk, vminfo.VMDisks[bootVolumeIndex].Path, vminfo.OSType, isNetplan); err != nil {
			utils.PrintLog(fmt.Sprintf("Warning: Network persistence script failed: %v", err))
		} else {
			utils.PrintLog("Network persistence script executed successfully")
		}
	} else {
		if strings.Contains(osRelease, "ubuntu") {
			return migobj.configureUbuntuNetwork(vminfo, bootVolumeIndex, osRelease, useSingleDisk)
		}

		if virtv2v.IsRHELFamily(osRelease) {
			return migobj.configureRHELNetwork(vminfo, bootVolumeIndex, osRelease)
		}
	}

	return nil
}

// configureUbuntuNetwork handles Ubuntu-specific network configuration
func (migobj *Migrate) configureUbuntuNetwork(vminfo vm.VMInfo, bootVolumeIndex int, osRelease string, useSingleDisk bool) error {
	versionID := parseVersionID(osRelease)
	utils.PrintLog(fmt.Sprintf("Version ID: %s", versionID))

	if versionID == "" {
		return errors.Errorf("failed to get version ID")
	}

	if isNetplanSupported(versionID) {
		utils.PrintLog("Adding wildcard netplan")
		if err := virtv2v.AddWildcardNetplan(vminfo.VMDisks, useSingleDisk, vminfo.VMDisks[bootVolumeIndex].Path, vminfo.GuestNetworks, vminfo.GatewayIP, vminfo.IPperMac); err != nil {
			return errors.Wrap(err, "failed to add wildcard netplan")
		}
		utils.PrintLog("Wildcard netplan added successfully")
		return nil
	}

	return migobj.addUdevRulesForUbuntu(vminfo, bootVolumeIndex, useSingleDisk)
}

// addUdevRulesForUbuntu adds udev rules for older Ubuntu versions
func (migobj *Migrate) addUdevRulesForUbuntu(vminfo vm.VMInfo, bootVolumeIndex int, useSingleDisk bool) error {
	utils.PrintLog("Ubuntu version does not support netplan, going to use udev rules")

	interfaces, err := virtv2v.GetNetworkInterfaceNames(vminfo.VMDisks[bootVolumeIndex].Path)
	if err != nil {
		return errors.Wrap(err, "failed to get network interface names")
	}

	if len(interfaces) == 0 {
		log.Printf("Failed to get network interface names, cannot add udev rules, network might not come up post migration, please check the network configuration post migration")
		return nil
	}

	utils.PrintLog("Adding udev rules")
	utils.PrintLog(fmt.Sprintf("Interfaces: %v", interfaces))

	macs := []string{}
	for _, nic := range vminfo.NetworkInterfaces {
		macs = append(macs, nic.MAC)
	}
	utils.PrintLog(fmt.Sprintf("MACs: %v", macs))

	if err := virtv2v.AddUdevRules(vminfo.VMDisks, useSingleDisk, vminfo.VMDisks[bootVolumeIndex].Path, interfaces, macs); err != nil {
		log.Printf(`Warning Failed to add udev rules: %s, incase of interface name mismatch,
                        network might not come up post migration, please check the network configuration post migration`, err)
		log.Println("Continuing with migration")
	}

	return nil
}

// configureRHELNetwork handles RHEL-specific network configuration
func (migobj *Migrate) configureRHELNetwork(vminfo vm.VMInfo, bootVolumeIndex int, osRelease string) error {
	versionID := parseVersionID(osRelease)
	majorVersion, err := strconv.Atoi(strings.Split(versionID, ".")[0])
	if err != nil {
		return fmt.Errorf("failed to parse major version: %v", err)
	}

	if majorVersion < 7 {
		diskPath := vminfo.VMDisks[bootVolumeIndex].Path
		if err := DetectAndHandleNetwork(diskPath, osRelease, vminfo); err != nil {
			utils.PrintLog(fmt.Sprintf(`Warning: Failed to handle network: %v,Continuing with migration, 
                    network might not come up post migration, please check the network configuration post migration`, err))
		}
	}

	return nil
}

func (migobj *Migrate) ConvertVolumes(ctx context.Context, vminfo vm.VMInfo) error {
	migobj.logMessage("Converting disk")

	// Step 1: Determine boot command based on OS type
	getBootCommand := migobj.getBootCommand(vminfo.OSType)

	// Step 2: Attach all volumes
	if err := migobj.attachAllVolumes(ctx, &vminfo); err != nil {
		return err
	}

	// Step 3: Generate XML configuration for conversion
	if err := vmutils.GenerateXMLConfig(vminfo); err != nil {
		return errors.Wrap(err, "failed to generate XML")
	}

	// Step 3.5: Get vjailbreak settings
	vjailbreakSettings, err := k8sutils.GetVjailbreakSettings(ctx, migobj.K8sClient)
	if err != nil {
		return errors.Wrap(err, "failed to get vjailbreak settings")
	}

	// Step 4: Detect boot volume
	bootVolumeIndex, osPath, useSingleDisk, err := migobj.detectBootVolume(vminfo, getBootCommand)
	if err != nil {
		return err
	}

	// Step 5: Handle OS-specific detection and validation
	var osRelease string
	osType := strings.ToLower(vminfo.OSType)

	switch osType {
	case constants.OSFamilyLinux:
		bootVolumeIndex, osPath, osRelease, err = migobj.handleLinuxOSDetection(vminfo, bootVolumeIndex, useSingleDisk, osPath, vjailbreakSettings.AutoFstabUpdate)
		if err != nil {
			return err
		}

	case constants.OSFamilyWindows:
		bootVolumeIndex, err = migobj.handleWindowsBootDetection(vminfo, bootVolumeIndex, useSingleDisk)
		if err != nil {
			return err
		}

	default:
		return errors.Errorf("unsupported OS type: %s", vminfo.OSType)
	}

	// Step 6: Validate boot volume was found
	if bootVolumeIndex == -1 {
		return errors.Errorf("boot volume not found, cannot create target VM")
	}

	// Step 7: Mark boot volume
	utils.PrintLog(fmt.Sprintf("Setting up boot volume as: %s", vminfo.VMDisks[bootVolumeIndex].Name))
	vminfo.VMDisks[bootVolumeIndex].Boot = true

	// Step 8: Perform disk conversion
	if err := migobj.performDiskConversion(ctx, vminfo, bootVolumeIndex, osPath, osRelease, useSingleDisk); err != nil {
		return err
	}

	// Step 9: Configure network for Linux systems
	if osType == constants.OSFamilyLinux {
		if err := migobj.configureLinuxNetwork(ctx, vminfo, bootVolumeIndex, osRelease, useSingleDisk); err != nil {
			return err
		}
	}

	// Step 10: Detach all volumes
	if err := migobj.DetachAllVolumes(ctx, vminfo); err != nil {
		return errors.Wrap(err, "Failed to detach all volumes from VM")
	}

	migobj.logMessage("Successfully converted disk")
	return nil
}

// DetectAndHandleNetwork: Checks if RHEL family, then detects NM presence offline.
// If NM (nmcli exists), injects first-boot nmcli script for DHCP force.
// If not, adds udev rules to pin names without forcing DHCP.
func DetectAndHandleNetwork(diskPath string, osRelease string, vmInfo vm.VMInfo) error {

	// No NM: Add udev rules to pin names
	interfaces, err := virtv2v.GetInterfaceNames(diskPath)
	if err != nil {
		utils.PrintLog(fmt.Sprintf("Warning: Failed to get interfaces: %v", err))
	}
	if len(interfaces) == 0 {
		utils.PrintLog(`No network interfaces found, cannot add udev rules, network might not
            come up post migration, please check the network configuration post migration`)
		return nil
	}
	macs := []string{}
	// By default the network interfaces macs are in the same order as the interfaces
	for _, nic := range vmInfo.NetworkInterfaces {
		macs = append(macs, nic.MAC)
	}
	utils.PrintLog(fmt.Sprintf("Interfaces: %v", interfaces))
	utils.PrintLog(fmt.Sprintf("MACs: %v", macs))
	if len(interfaces) != len(macs) {
		utils.PrintLog("Mismatch between number of interfaces and MACs")
		return fmt.Errorf("mismatch between number of interfaces and MACs")
	}
	// Add udev rules to pin names without forcing DHCP
	utils.PrintLog("Adding udev rules to pin interface names")

	// This will ensure that the network interfaces are named consistently after migration
	// and they get the correct IP address.
	// This is important because RHEL family uses NetworkManager by default and it does not
	// automatically configure the network interfaces to use DHCP after migration.
	// So we need to add udev rules to pin the names of the network interfaces
	// to the MAC addresses so that they are consistent after migration.
	// This will ensure that the network interfaces are named consistently after migration
	// and they get the correct IP address.
	err = virtv2v.AddUdevRules([]vm.VMDisk{{Path: diskPath}}, false, diskPath, interfaces, macs)
	if err != nil {
		utils.PrintLog(fmt.Sprintf("Warning: Failed to add udev: %v", err))
	}
	return nil
}

func (migobj *Migrate) CreateTargetInstance(ctx context.Context, vminfo vm.VMInfo, networkids, portids []string, ipaddresses []string) error {
	migobj.logMessage("Creating target instance")
	openstackops := migobj.Openstackclients
	var flavor *flavors.Flavor
	var err error

	if migobj.UseFlavorless {
		if migobj.TargetFlavorId == "" {
			err = fmt.Errorf("flavorless creation is enabled, but TargetFlavorId in vmwaremachine %s is empty. Please set it to the ID of your base flavor (e.g., '0-0-x')", vminfo.Name)
			return errors.Wrap(err, "failed to create target instance")
		}
		migobj.logMessage(fmt.Sprintf("Using flavorless creation with base flavor ID: %s", migobj.TargetFlavorId))
		flavor, err = openstackops.GetFlavor(ctx, migobj.TargetFlavorId)
		if err != nil {
			return errors.Wrap(err, "failed to get the specified base flavor for flavorless creation")
		}
	} else if migobj.TargetFlavorId != "" {
		flavor, err = openstackops.GetFlavor(ctx, migobj.TargetFlavorId)
		if err != nil {
			return errors.Wrap(err, "failed to get OpenStack flavor")
		}
	} else {
		flavor, err = openstackops.GetClosestFlavour(ctx, vminfo.CPU, vminfo.Memory)
		if err != nil {
			return errors.Wrap(err, "failed to get closest OpenStack flavor")
		}
		utils.PrintLog(fmt.Sprintf("Closest OpenStack flavor: %s: CPU: %dvCPUs\tMemory: %dMB\n", flavor.Name, flavor.VCPUs, flavor.RAM))
	}
	// Get security group IDs
	securityGroupIDs, err := openstackops.GetSecurityGroupIDs(ctx, migobj.SecurityGroups, migobj.TenantName)
	if err != nil {
		return errors.Wrap(err, "failed to resolve security group names to IDs")
	}
	utils.PrintLog(fmt.Sprintf("Using security group IDs: %v", securityGroupIDs))

	if migobj.ServerGroup != "" {
		utils.PrintLog(fmt.Sprintf("Using server group ID: %s", migobj.ServerGroup))
	} else {
		utils.PrintLog("No server group specified - VMs will be placed based on default scheduling")
	}

	// Get vjailbreak settings
	vjailbreakSettings, err := k8sutils.GetVjailbreakSettings(context.Background(), migobj.K8sClient)
	if err != nil {
		return errors.Wrap(err, "failed to get vjailbreak settings")
	}
	utils.PrintLog(fmt.Sprintf("Fetched vjailbreak settings for VM active wait retry limit: %d, VM active wait interval seconds: %d", vjailbreakSettings.VMActiveWaitRetryLimit, vjailbreakSettings.VMActiveWaitIntervalSeconds))

	// Create a new VM in OpenStack
	newVM, err := openstackops.CreateVM(ctx, flavor, networkids, portids, vminfo, migobj.TargetAvailabilityZone, securityGroupIDs, migobj.ServerGroup, *vjailbreakSettings, migobj.UseFlavorless)
	if err != nil {
		return errors.Wrap(err, "failed to create VM")
	}

	// Wait for VM to become active
	for i := 0; i < vjailbreakSettings.VMActiveWaitRetryLimit; i++ {
		migobj.logMessage(fmt.Sprintf("Waiting for VM to become active: %d/%d retries\n", i+1, vjailbreakSettings.VMActiveWaitRetryLimit))
		active, err := openstackops.WaitUntilVMActive(ctx, newVM.ID)
		if err != nil {
			return errors.Wrap(err, "failed to wait for VM to become active")
		}
		if active {
			break
		}
		if i == vjailbreakSettings.VMActiveWaitRetryLimit-1 {
			return errors.Errorf("VM is not active after %d retries", vjailbreakSettings.VMActiveWaitRetryLimit)
		}
		time.Sleep(time.Duration(vjailbreakSettings.VMActiveWaitIntervalSeconds) * time.Second)
	}

	migobj.logMessage(fmt.Sprintf("VM created successfully: ID: %s", newVM.ID))

	if migobj.PerformHealthChecks {
		err = migobj.HealthCheck(vminfo, ipaddresses)
		if err != nil {
			migobj.logMessage(fmt.Sprintf("Health Check failed: %s", err))
		}
	} else {
		migobj.logMessage("Skipping Health Checks")
	}

	return nil
}

// parseVersionID parses the VERSION_ID from /etc/os-release or /etc/redhat-release format.
// It returns the version ID as a string, or an empty string if not found.
func parseVersionID(osRelease string) string {
	osRelease = strings.TrimSpace(osRelease)

	// Key-value style (os-release, SuSE-release, etc.)
	if strings.Contains(osRelease, "=") {
		var version, patchlevel string
		for _, line := range strings.Split(osRelease, "\n") {
			kv := strings.SplitN(line, "=", 2)
			if len(kv) != 2 {
				continue
			}
			key := strings.TrimSpace(strings.ToUpper(kv[0]))
			val := strings.TrimSpace(strings.Trim(kv[1], `"`)) // Remove quotes and spaces
			switch key {
			case "VERSION_ID":
				return val
			case "VERSION":
				version = val
			case "PATCHLEVEL":
				patchlevel = val
			}
		}
		// If it's SLES style, combine VERSION + PATCHLEVEL if available
		if version != "" {
			if patchlevel != "" {
				return version + "." + patchlevel
			}
			return version
		}
	} else {
		// /etc/redhat-release style
		re := regexp.MustCompile(`release\s+([0-9]+(\.[0-9]+)?)`)
		matches := re.FindStringSubmatch(strings.ToLower(osRelease))
		if len(matches) > 1 {
			return matches[1]
		}
	}

	return ""
}

func isNetplanSupported(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		log.Printf("Warning: unexpected VERSION_ID format: %q", version)
		return true // assume modern if uncertain
	}

	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		log.Printf("Warning: failed to parse VERSION_ID %q: %v %v", version, err1, err2)
		return true
	}

	// Compare with 17.10
	if major > 17 {
		return true
	}
	if major == 17 && minor >= 10 {
		return true
	}
	return false
}

func (migobj *Migrate) pingVM(ips []string) error {
	for _, ip := range ips {
		migobj.logMessage(fmt.Sprintf("Pinging VM: %s", ip))
		pinger, err := probing.NewPinger(ip)
		if err != nil {
			return errors.Wrap(err, "failed to create pinger")
		}
		pinger.Count = 1
		pinger.Timeout = time.Second * 10
		err = pinger.Run()
		if err != nil {
			return errors.Wrap(err, "failed to run pinger")
		}
		if pinger.Statistics().PacketLoss == 0 {
			migobj.logMessage("Ping succeeded")
		} else {
			return errors.Errorf("Ping failed")
		}
	}
	return nil
}

func (migobj *Migrate) checkHTTPGet(ips []string, port string) error {
	var client *http.Client
	vjbNet := netutils.NewVjbNet()
	if migobj.Insecure {
		vjbNet.Insecure = true
	}
	if vjbNet.CreateSecureHTTPClient() == nil {
		client = vjbNet.GetClient()
	} else {
		return errors.Errorf("Both HTTP and HTTPS failed ")
	}
	for _, ip := range ips {
		// Try HTTP first
		httpURL := fmt.Sprintf("http://%s:%s", ip, port)
		if err := migobj.tryConnection(client, httpURL); err == nil {
			migobj.logMessage("HTTP succeeded")
			continue // Success with HTTP, move to next IP
		}

		// If HTTP fails, try HTTPS
		httpsURL := fmt.Sprintf("https://%s:%s", ip, port)
		if err := migobj.tryConnection(client, httpsURL); err == nil {
			migobj.logMessage("HTTPS succeeded")
			continue // Success with HTTPS, move to next IP
		}

		// Both HTTP and HTTPS failed
		return errors.Errorf("Both HTTP and HTTPS failed for %s:%s", ip, port)
	}

	return nil
}

func (migobj *Migrate) tryConnection(client *http.Client, url string) error {
	resp, err := client.Get(url)
	if err != nil {
		migobj.logMessage(fmt.Sprintf("GET failed for %s: %v", url, err))
		return errors.Wrap(err, "failed to get url")
	}
	defer resp.Body.Close()

	migobj.logMessage(fmt.Sprintf("GET response for %s: %d", url, resp.StatusCode))

	if resp.StatusCode != http.StatusOK {
		return errors.Errorf("GET returned non-OK status for %s: %d", url, resp.StatusCode)
	}

	return nil
}

func (migobj *Migrate) HealthCheck(vminfo vm.VMInfo, ips []string) error {
	migobj.logMessage("Performing Health Checks")
	healthChecks := make(map[string]bool)
	healthChecks["Ping"] = false
	healthChecks["HTTP Get"] = false
	for i := 0; i < 10; i++ {
		migobj.logMessage(fmt.Sprintf("Health Check Attempt %d", i+1))
		// 1. Ping
		if !healthChecks["Ping"] {
			err := migobj.pingVM(ips)
			if err != nil {
				migobj.logMessage(fmt.Sprintf("Ping(s) failed: %s", err))
			} else {
				healthChecks["Ping"] = true
			}
		}
		// 2. HTTP GET check
		if !healthChecks["HTTP Get"] {
			err := migobj.checkHTTPGet(ips, migobj.HealthCheckPort)
			if err != nil {
				migobj.logMessage(fmt.Sprintf("HTTP Get failed: %s", err))
			} else {
				healthChecks["HTTP Get"] = true
			}
		}
		if healthChecks["Ping"] && healthChecks["HTTP Get"] {
			break
		}
		migobj.logMessage("Waiting for 60 seconds before retrying health checks")
		time.Sleep(60 * time.Second)
	}
	for key, value := range healthChecks {
		if !value {
			migobj.logMessage(fmt.Sprintf("Health Check %s failed", key))
		} else {
			migobj.logMessage(fmt.Sprintf("Health Check %s succeeded", key))
		}
	}
	return nil
}

func (migobj *Migrate) gracefulTerminate(ctx context.Context, vminfo vm.VMInfo, cancel context.CancelFunc) {
	gracefulShutdown := make(chan os.Signal, 1)
	// Handle SIGTERM
	signal.Notify(gracefulShutdown, syscall.SIGTERM, syscall.SIGINT)
	<-gracefulShutdown
	migobj.logMessage("Gracefully terminating")
	cancel()
	migobj.cleanup(ctx, vminfo, "Migration terminated", nil, nil)
	os.Exit(0)
}

func (migobj *Migrate) MigrateVM(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Wait until the data copy start time
	var zerotime time.Time
	if !migobj.MigrationTimes.DataCopyStart.Equal(zerotime) && migobj.MigrationTimes.DataCopyStart.After(time.Now()) {
		migobj.logMessage("Waiting for data copy start time")
		time.Sleep(time.Until(migobj.MigrationTimes.DataCopyStart))
		migobj.logMessage("Data copy start time reached")
	}
	fmt.Println("Starting VM Migration with RDM disks : ", migobj.RDMDisks)
	// Get Info about VM
	vminfo, err := migobj.VMops.GetVMInfo(migobj.Ostype, migobj.RDMDisks)

	if err != nil {
		cancel()
		return errors.Wrap(err, "failed to get all info")
	}
	if (len(vminfo.VMDisks) != len(migobj.Volumetypes)) && migobj.StorageCopyMethod != constants.StorageCopyMethod {
		return errors.Errorf("number of volume types does not match number of disks vm(%d) volume(%d)", len(vminfo.VMDisks), len(migobj.Volumetypes))
	}
	if len(vminfo.Mac) != len(migobj.Networknames) {
		return errors.Errorf("number of mac addresses does not match number of network names mac(%d) network(%d)", len(vminfo.Mac), len(migobj.Networknames))
	}
	// Graceful Termination clean-up volumes and snapshots
	go migobj.gracefulTerminate(ctx, vminfo, cancel)

	// Reserve ports for VM
	networkids, portids, ipaddresses, err := migobj.ReservePortsForVM(ctx, &vminfo)
	if err != nil {
		return errors.Wrap(err, "failed to reserve ports for VM")
	}
	vcenterSettings, err := k8sutils.GetVjailbreakSettings(ctx, migobj.K8sClient)
	if err != nil {
		return errors.Wrap(err, "failed to get vcenter settings")
	}

	if migobj.StorageCopyMethod == constants.StorageCopyMethod {
		// Initialize storage provider if using StorageAcceleratedCopy migration
		if err := migobj.InitializeStorageProvider(ctx); err != nil {
			return errors.Wrap(err, "failed to initialize storage provider")
		}
		defer func() {
			if migobj.StorageProvider != nil {
				migobj.StorageProvider.Disconnect()
			}
		}()
		if err := migobj.ValidateStorageAcceleratedCopyPrerequisites(ctx); err != nil {
			return errors.Wrap(err, "StorageAcceleratedCopy prerequisites validation failed")
		}

		// Perform the copy here.
		if _, err := migobj.StorageAcceleratedCopyCopyDisks(ctx, vminfo); err != nil {
			return errors.Wrap(err, "failed to perform StorageAcceleratedCopy copy")
		}

	} else {

		// Create and Add Volumes to Host
		vminfo, err = migobj.CreateVolumes(ctx, vminfo)
		if err != nil {
			return errors.Wrap(err, "failed to add volumes to host")
		}
		// Enable CBT
		err = migobj.EnableCBTWrapper()
		if err != nil {
			migobj.cleanup(ctx, vminfo, fmt.Sprintf("CBT Failure: %s", err), portids, nil)
			return errors.Wrap(err, "CBT Failure")
		}

		// Create NBD servers
		for range vminfo.VMDisks {
			migobj.Nbdops = append(migobj.Nbdops, &nbd.NBDServer{})
		}

		// Live Replicate Disks
		vminfo, err = migobj.LiveReplicateDisks(ctx, vminfo)
		if err != nil {
			if cleanuperror := migobj.cleanup(ctx, vminfo, fmt.Sprintf("failed to live replicate disks: %s", err), portids, nil); cleanuperror != nil {
				// combine both errors
				return errors.Wrapf(err, "failed to cleanup disks: %s", cleanuperror)
			}
			return errors.Wrap(err, "failed to live replicate disks")
		}
	}
	// Convert the Boot Disk to raw format
	err = migobj.ConvertVolumes(ctx, vminfo)
	if err != nil {
		if !vcenterSettings.CleanupVolumesAfterConvertFailure {
			migobj.logMessage("Cleanup volumes after convert failure is disabled, detaching volumes and cleaning up snapshots")
			detachErr := migobj.DetachAllVolumes(ctx, vminfo)
			if detachErr != nil {
				utils.PrintLog(fmt.Sprintf("Failed to detach all volumes from VM: %s\n", detachErr))
			}

			cleanUpErr := migobj.VMops.CleanUpSnapshots(true)
			if cleanUpErr != nil {
				utils.PrintLog(fmt.Sprintf("Failed to cleanup snapshot of source VM: %s\n", cleanUpErr))
				return errors.Wrap(cleanUpErr, "Failed to cleanup snapshot of source VM")
			}
			return errors.Wrap(err, "failed to convert disks")
		}
		if cleanuperror := migobj.cleanup(ctx, vminfo, fmt.Sprintf("failed to convert volumes: %s", err), portids, vcenterSettings); cleanuperror != nil {
			// combine both errors
			return errors.Wrapf(err, "failed to cleanup disks: %s", cleanuperror)
		}
		return errors.Wrap(err, "failed to convert disks")
	}

	err = migobj.CreateTargetInstance(ctx, vminfo, networkids, portids, ipaddresses)
	if err != nil {
		if cleanuperror := migobj.cleanup(ctx, vminfo, fmt.Sprintf("failed to create target instance: %s", err), portids, vcenterSettings); cleanuperror != nil {
			// combine both errors
			return errors.Wrapf(err, "failed to cleanup disks: %s", cleanuperror)
		}
		return errors.Wrap(err, "failed to create target instance")
	}

	if err := migobj.DisconnectSourceNetworkIfRequested(); err != nil {
		migobj.logMessage(fmt.Sprintf("Warning: Failed to disconnect source VM network interfaces: %v", err))
	}

	return nil
}

func (migobj *Migrate) cleanup(ctx context.Context, vminfo vm.VMInfo, message string, portids []string, vcenterSettings *k8sutils.VjailbreakSettings) error {
	migobj.logMessage(fmt.Sprintf("%s. Trying to perform cleanup", message))
	err := migobj.DetachAllVolumes(ctx, vminfo)
	if err != nil {
		utils.PrintLog(fmt.Sprintf("Failed to detach all volumes from VM: %s\n", err))
	}
	err = migobj.DeleteAllVolumes(ctx, vminfo)
	if err != nil {
		utils.PrintLog(fmt.Sprintf("Failed to delete all volumes from host: %s\n", err))
	}
	err = migobj.VMops.CleanUpSnapshots(true)
	if err != nil {
		utils.PrintLog(fmt.Sprintf("Failed to cleanup snapshot of source VM: %s\n", err))
		return errors.Wrap(err, fmt.Sprintf("Failed to cleanup snapshot of source VM: %s\n", err))
	}

	// Delete ports if cleanup is enabled
	if vcenterSettings != nil && vcenterSettings.CleanupPortsAfterMigrationFailure && len(portids) > 0 {
		migobj.logMessage("Cleanup ports after migration failure is enabled, deleting ports")
		if portCleanupErr := migobj.DeleteAllPorts(ctx, portids); portCleanupErr != nil {
			utils.PrintLog(fmt.Sprintf("Failed to delete ports: %s\n", portCleanupErr))
		}
	} else if vcenterSettings != nil && !vcenterSettings.CleanupPortsAfterMigrationFailure {
		migobj.logMessage("Cleanup ports after migration failure is disabled, ports will not be deleted")
	}

	return nil
}

func (migobj *Migrate) DeleteAllPorts(ctx context.Context, portids []string) error {
	migobj.logMessage("Deleting all ports")
	openstackops := migobj.Openstackclients
	var deletionErrors []error
	successCount := 0

	for _, portID := range portids {
		err := openstackops.DeletePort(ctx, portID)
		if err != nil {
			utils.PrintLog(fmt.Sprintf("Failed to delete port %s: %s\n", portID, err))
			deletionErrors = append(deletionErrors, errors.Wrapf(err, "failed to delete port %s", portID))
		} else {
			utils.PrintLog(fmt.Sprintf("Successfully deleted port %s\n", portID))
			successCount++
		}
	}

	if len(deletionErrors) > 0 {
		migobj.logMessage(fmt.Sprintf("Port deletion completed with errors: %d succeeded, %d failed out of %d total", successCount, len(deletionErrors), len(portids)))
		// Combine all errors into a single error message
		errMsg := fmt.Sprintf("failed to delete %d port(s):", len(deletionErrors))
		for _, err := range deletionErrors {
			errMsg += fmt.Sprintf("\n  - %s", err.Error())
		}
		return errors.New(errMsg)
	}

	migobj.logMessage(fmt.Sprintf("Successfully deleted all %d ports", successCount))
	return nil
}

func (migobj *Migrate) ReservePortsForVM(ctx context.Context, vminfo *vm.VMInfo) ([]string, []string, []string, error) {
	networkids := []string{}
	ipaddresses := []string{}
	portids := []string{}
	openstackops := migobj.Openstackclients
	networknames := migobj.Networknames

	// Get security group IDs
	securityGroupIDs, err := openstackops.GetSecurityGroupIDs(ctx, migobj.SecurityGroups, migobj.TenantName)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "failed to resolve security group names to IDs")
	}
	utils.PrintLog(fmt.Sprintf("Using provided security group IDs %v", securityGroupIDs))

	// Log server group
	if migobj.ServerGroup != "" {
		utils.PrintLog(fmt.Sprintf("Server group ID for VM placement: %s", migobj.ServerGroup))
	}

	// Create ports
	if len(migobj.Networkports) != 0 {
		if len(migobj.Networkports) != len(networknames) {
			return nil, nil, nil, errors.Errorf("number of network ports does not match number of network names")
		}
		for _, port := range migobj.Networkports {
			retrPort, err := openstackops.GetPort(ctx, port)
			if err != nil {
				return nil, nil, nil, errors.Wrap(err, "failed to get port")
			}
			networkids = append(networkids, retrPort.NetworkID)
			portids = append(portids, retrPort.ID)
			for _, fixedIP := range retrPort.FixedIPs {
				ipaddresses = append(ipaddresses, fixedIP.IPAddress)
			}
		}
	} else {

		for idx, networkname := range networknames {
			// Create Port Group with the same mac address as the source VM
			// Find the network with the given ID
			network, err := openstackops.GetNetwork(ctx, networkname)
			if err != nil {
				return nil, nil, nil, errors.Wrap(err, "failed to get network")
			}

			if network == nil {
				return nil, nil, nil, errors.Errorf("network not found")
			}

			var ippm []string

			// VMware Tools detected IPs
			if detectedIPs, ok := vminfo.IPperMac[vminfo.Mac[idx]]; ok && len(detectedIPs) > 0 {
				for _, detectedIP := range detectedIPs {
					ippm = append(ippm, detectedIP.IP)
				}
				utils.PrintLog(fmt.Sprintf("Detected IPs from VMware Tools for MAC %s: %v", vminfo.Mac[idx], detectedIPs))
			}

			// User-assigned IP from ConfigMap
			if migobj.AssignedIP != "" {
				assignedIPs := strings.Split(migobj.AssignedIP, ",")
				if idx < len(assignedIPs) {
					ip := strings.TrimSpace(assignedIPs[idx])
					if ip != "" {
						ippm = []string{ip}
						vminfo.IPperMac[vminfo.Mac[idx]] = []vm.IpEntry{
							vm.IpEntry{
								IP:     ip,
								Prefix: 0,
							},
						}
						utils.PrintLog(fmt.Sprintf("User-Assigned IP[%d] for MAC %s: %s", idx, vminfo.Mac[idx], ip))
					} else {
						utils.PrintLog(fmt.Sprintf("User-Assigned IP[%d] is empty for MAC %s, using previously determined IP", idx, vminfo.Mac[idx]))
					}
				}
			}

			utils.PrintLog(fmt.Sprintf("Using IPs for MAC %s: %v", vminfo.Mac[idx], ippm))
			port, err := openstackops.ValidateAndCreatePort(ctx, network, vminfo.Mac[idx], vminfo.IPperMac, vminfo.Name, securityGroupIDs, migobj.FallbackToDHCP, vminfo.GatewayIP)
			if err != nil {
				return nil, nil, nil, errors.Wrap(err, "failed to create port group")
			}
			addressesOfPort := []string{}
			for _, fixedIP := range port.FixedIPs {
				addressesOfPort = append(addressesOfPort, fixedIP.IPAddress)
			}
			utils.PrintLog(fmt.Sprintf("Port created successfully: MAC:%s IP:%s\n", port.MACAddress, addressesOfPort))
			networkids = append(networkids, network.ID)
			portids = append(portids, port.ID)
			for _, fixedIP := range port.FixedIPs {
				ipaddresses = append(ipaddresses, fixedIP.IPAddress)
			}
		}
		utils.PrintLog(fmt.Sprintf("Gateways : %v", vminfo.GatewayIP))
	}
	return networkids, portids, ipaddresses, nil
}

// LogMessage is an exported wrapper for logMessage that satisfies the esxissh.ProgressLogger interface.
func (migobj *Migrate) LogMessage(message string) {
	migobj.logMessage(message)
}

// InitializeStorageProvider initializes and validates the storage provider for StorageAcceleratedCopy migration
func (migobj *Migrate) InitializeStorageProvider(ctx context.Context) error {
	if migobj.StorageCopyMethod != constants.StorageCopyMethod {
		migobj.logMessage("Storage copy method is not StorageAcceleratedCopy, skipping storage provider initialization")
		return nil
	}

	migobj.logMessage("Initializing storage provider for StorageAcceleratedCopy migration")

	// Validate required credentials
	if migobj.ArrayHost == "" {
		return fmt.Errorf("ARRAY_HOST is required for StorageAcceleratedCopy storage migration")
	}
	if migobj.ArrayUser == "" {
		return fmt.Errorf("ARRAY_USER is required for StorageAcceleratedCopy storage migration")
	}
	if migobj.ArrayPassword == "" {
		return fmt.Errorf("ARRAY_PASSWORD is required for StorageAcceleratedCopy storage migration")
	}

	// Create storage access info
	accessInfo := storage.StorageAccessInfo{
		Hostname:            migobj.ArrayHost,
		Username:            migobj.ArrayUser,
		Password:            migobj.ArrayPassword,
		SkipSSLVerification: migobj.ArrayInsecure,
		VendorType:          migobj.VendorType,
	}

	// Create storage provider
	provider, err := storage.NewStorageProvider(accessInfo.VendorType)
	if err != nil {
		return fmt.Errorf("failed to create storage provider: %w", err)
	}

	// Connect to storage array
	migobj.logMessage(fmt.Sprintf("Connecting to storage array: %s", migobj.ArrayHost))
	if err := provider.Connect(ctx, accessInfo); err != nil {
		return fmt.Errorf("failed to connect to storage array: %w", err)
	}

	// Validate credentials
	migobj.logMessage("Validating storage array credentials...")
	if err := provider.ValidateCredentials(ctx); err != nil {
		return fmt.Errorf("storage array credential validation failed: %w", err)
	}

	migobj.StorageProvider = provider
	migobj.logMessage(fmt.Sprintf("Storage provider initialized successfully: %s", provider.WhoAmI()))

	return nil
}

// LoadESXiSSHKey loads the ESXi SSH private key from the Kubernetes secret
func (migobj *Migrate) LoadESXiSSHKey(ctx context.Context) error {

	migobj.logMessage(fmt.Sprintf("Loading ESXi SSH private key from secret: %s", constants.ESXiSSHSecretName))

	privateKey, err := k8sutils.GetESXiSSHPrivateKey(ctx, migobj.K8sClient, constants.ESXiSSHSecretName)
	if err != nil {
		return errors.Wrapf(err, "failed to load ESXi SSH private key from secret %s", constants.ESXiSSHSecretName)
	}

	migobj.ESXiSSHPrivateKey = privateKey
	migobj.logMessage("ESXi SSH private key loaded successfully")

	return nil
}
