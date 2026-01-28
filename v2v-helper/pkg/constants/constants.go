package constants

import "time"

const (
	HotplugCPUKey       = "HOTPLUG_CPU"
	HotplugMemoryKey    = "HOTPLUG_MEMORY"
	HotplugCPUMaxKey    = "HOTPLUG_CPU_MAX"
	HotplugMemoryMaxKey = "HOTPLUG_MEMORY_MAX"
	RhelFirstBootScript = `#!/bin/bash
set -e
LOG_FILE="/var/log/network_fix.log"
echo "$(date '+%Y-%m-%d %H:%M:%S') - Starting network fix script" >> "$LOG_FILE"

if ! systemctl is-active NetworkManager >/dev/null 2>&1; then
    echo "$(date '+%Y-%m-%d %H:%M:%S') - NetworkManager not active, attempting to start" >> "$LOG_FILE"
    systemctl start NetworkManager
    if ! systemctl is-active NetworkManager >/dev/null 2>&1; then
        echo "$(date '+%Y-%m-%d %H:%M:%S') - Failed to start NetworkManager, exiting" >> "$LOG_FILE"
        exit 1
    fi
fi
echo "$(date '+%Y-%m-%d %H:%M:%S') - NetworkManager is active" >> "$LOG_FILE"

nmcli con reload || {
    echo "$(date '+%Y-%m-%d %H:%M:%S') - Warning: Reload failed, restarting NM" >> "$LOG_FILE"
    systemctl restart NetworkManager
    sleep 5
    nmcli con reload || echo "$(date '+%Y-%m-%d %H:%M:%S') - Error: Reload still failed" >> "$LOG_FILE"
}

OLD_CONNS=$(nmcli -t -f NAME,TYPE connection show | grep -v ':loopback' | cut -d: -f1)
if [ -z "$OLD_CONNS" ]; then
    echo "$(date '+%Y-%m-%d %H:%M:%S') - No existing connections found" >> "$LOG_FILE"
else
    echo "$(date '+%Y-%m-%d %H:%M:%S') - Found connections: $OLD_CONNS" >> "$LOG_FILE"
fi

for conn in $OLD_CONNS; do
    echo "$(date '+%Y-%m-%d %H:%M:%S') - Processing connection: $conn" >> "$LOG_FILE"
    nmcli con mod "$conn" ipv4.method auto ipv4.addresses "" ipv4.gateway "" 2>>"$LOG_FILE" || \
        echo "$(date '+%Y-%m-%d %H:%M:%S') - Error: Failed to modify IPv4 for $conn" >> "$LOG_FILE"
    nmcli con mod "$conn" ipv6.method auto ipv6.addresses "" ipv6.gateway "" 2>>"$LOG_FILE" || \
        echo "$(date '+%Y-%m-%d %H:%M:%S') - Error: Failed to modify IPv6 for $conn" >> "$LOG_FILE"
    nmcli con up "$conn" 2>>"$LOG_FILE" || \
        echo "$(date '+%Y-%m-%d %H:%M:%S') - Warning: Failed to activate $conn" >> "$LOG_FILE"
done

NEW_IFACES=$(ip link show | grep -o '^[0-9]\+: [a-zA-Z0-9]\+:' | cut -d ' ' -f2 | cut -d ':' -f1 | grep -v lo)
if [ -z "$NEW_IFACES" ]; then
    echo "$(date '+%Y-%m-%d %H:%M:%S') - No new interfaces detected" >> "$LOG_FILE"
else
    echo "$(date '+%Y-%m-%d %H:%M:%S') - Detected interfaces: $NEW_IFACES" >> "$LOG_FILE"
fi

for iface in $NEW_IFACES; do
    echo "$(date '+%Y-%m-%d %H:%M:%S') - Processing new interface: $iface" >> "$LOG_FILE"
    conn_name="$iface"
    if ! nmcli con show "$conn_name" >/dev/null 2>&1; then
        echo "$(date '+%Y-%m-%d %H:%M:%S') - Creating new connection for $iface" >> "$LOG_FILE"
        nmcli con add type ethernet con-name "$conn_name" ifname "$iface" ipv4.method auto ipv6.method auto 2>>"$LOG_FILE" || \
            echo "$(date '+%Y-%m-%d %H:%M:%S') - Error: Failed to create connection for $iface" >> "$LOG_FILE"
    fi
    nmcli con up "$conn_name" 2>>"$LOG_FILE" || \
        echo "$(date '+%Y-%m-%d %H:%M:%S') - Error: Failed to activate $iface" >> "$LOG_FILE"
done
echo "$(date '+%Y-%m-%d %H:%M:%S') - Network fix script completed" >> "$LOG_FILE"`

	VMwareCleanupFirstBootScript = `#!/bin/bash 
	
log() { echo "[vjailbreak-vmware-cleanup] $*"; } 
 
cleanup_dir() { 
  local p="$1" 
  if [[ -e "$p" ]]; then 
    log "Deleting $p" 
    rm -rf --one-file-system "$p" || true 
  else 
    log "Not present: $p" 
  fi 
} 
 
cleanup_dir /etc/vmware-tools 
cleanup_dir /var/lib/vmware 
cleanup_dir /usr/lib/vmware-tools 
cleanup_dir /usr/lib/open-vm-tools 
 
if [[ -d /var/log ]]; then 
  log "Deleting /var/log/vmware-* files" 
  find /var/log -maxdepth 1 -type f -name 'vmware-*' -print -delete || true 
fi 
 
log "VMware leftovers cleanup complete" 
`

	MaxCPU = 9999999
	MaxRAM = 9999999

	// Number of intervals to wait for the volume to become available
	MaxIntervalCount = 60

	InspectOSCommand         = "inspect-os"
	LSBootCommand            = "ls /boot"
	XMLFileName              = "libxml.xml"
	MigrationSnapshotName    = "migration-snap"
	MaxHTTPRetryCount        = 5
	MaxVMActiveCheckCount    = 15
	VMActiveCheckInterval    = 20 * time.Second
	NamespaceMigrationSystem = "migration-system"
	TrueString               = "true"

	LogsDir = "/var/log/pf9"

	EventMessageConvertingDisk                    = "Converting disk"
	EventMessageWaitingForCutOverStart            = "Waiting for VM Cutover start time"
	EventMessageCopyingChangedBlocksWithIteration = "Copying changed blocks"
	EventMessageWaitingForDataCopyStart           = "Waiting for data copy start time"
	EventMessageDataCopyStart                     = "Data copy start time reached"
	EventMessageWaitingForAdminCutOver            = "Waiting for Admin Cutover conditions to be met"
	EventMessageMigrationSucessful                = "VM created successfully"
	EventMessageMigrationFailed                   = "Trying to perform cleanup"
	EventMessageCopyingDisk                       = "Copying disk"
	EventMessageFailed                            = "Failed to"
	EventDisconnect                               = "Disconnected network interfaces"

	// StorageAcceleratedCopy specific event messages
	EventMessageEsxiSSHConnect                       = "Connecting to ESXi"
	EventMessageEsxiSSHTest                          = "Testing ESXi connection"
	EventMessageEsxiConnected                        = "Connected to ESXi"
	EventMessageInitiatorGroup                       = "Creating/updating initiator group"
	EventMessageStorageAcceleratedCopyCreatingVolume = "Creating target volume"
	EventMessageStorageAcceleratedCopyCinderManage   = "Cinder managing the volume"
	EventMessageStorageAcceleratedCopyMappingVolume  = "Mapping target volume"
	EventMessageStorageAcceleratedCopyRescanStorage  = "Waiting for target volume"
	EventMessageStorageAcceleratedCopyTargetDevice   = "Target device is visible:"

	OSFamilyWindows = "windowsguest"
	OSFamilyLinux   = "linuxguest"

	PCDClusterNameNoCluster = "NO CLUSTER"

	// VCenterVMScanConcurrencyLimit is the limit for concurrency while scanning vCenter VMs
	VCenterVMScanConcurrencyLimit = 100

	// ConfigMap default values
	ChangedBlocksCopyIterationThreshold = 20
	PeriodicSyncInterval                = "1h"
	// VMActiveWaitIntervalSeconds is the interval to wait for vm to become active
	VMActiveWaitIntervalSeconds = 20

	// VMActiveWaitRetryLimit is the number of retries to wait for vm to become active
	VMActiveWaitRetryLimit = 15

	// VolumeAvailableWaitIntervalSeconds is the interval to wait for volume to become available
	VolumeAvailableWaitIntervalSeconds = 5

	// VolumeAvailableWaitRetryLimit is the number of retries to wait for volume to become available
	VolumeAvailableWaitRetryLimit = 15

	// DefaultMigrationMethod is the default migration method
	DefaultMigrationMethod = "cold"

	// VCenterScanConcurrencyLimit is the max number of vcenter scan pods
	VCenterScanConcurrencyLimit = 100

	// CleanupVolumesAfterConvertFailure is the default value for cleanup volumes after convert failure
	CleanupVolumesAfterConvertFailure = true

	// CleanupPortsAfterMigrationFailure is the default value for cleanup ports after migration failure
	CleanupPortsAfterMigrationFailure = false

	// PopulateVMwareMachineFlavors is the default value for populating VMwareMachine objects with OpenStack flavors
	PopulateVMwareMachineFlavors = true

	// ValidateRDMOwnerVMs is the default value for RDM owner VM validation
	ValidateRDMOwnerVMs = true

	// VjailbreakSettingsConfigMapName is the name of the vjailbreak settings configmap
	VjailbreakSettingsConfigMapName = "vjailbreak-settings"

	// VCenterLoginRetryLimit is the number of retries for vcenter login
	VCenterLoginRetryLimit = 1

	// OpenstackCredsRequeueAfterMinutes is the time to requeue after.
	OpenstackCredsRequeueAfterMinutes = 60

	// VMwareCredsRequeueAfterMinutes is the time to requeue after.
	VMwareCredsRequeueAfterMinutes = 60

	// PeriodicSyncMaxRetries is the max number of retries for CBT sync
	PeriodicSyncMaxRetries = 3

	// PeriodicSyncRetryCap is the max retry interval for CBT sync
	PeriodicSyncRetryCap = "3h"
	// ValidateRDMOwnerVMsKey is the key for enabling/disabling RDM owner VM validation
	ValidateRDMOwnerVMsKey = "VALIDATE_RDM_OWNER_VMS"

	// ESXiSSHSecretName is the name of the Kubernetes secret containing ESXi SSH private key
	ESXiSSHSecretName = "esxi-ssh-key"

	// AutoFstabUpdate is the default value for automatic fstab update
	AutoFstabUpdate = false
	// AutoFstabUpdateKey is the key for enabling/disabling automatic fstab update
	AutoFstabUpdateKey = "AUTO_FSTAB_UPDATE"

	// AutoPXEBootOnConversionDefault is the default value for automatic PXE boot during cluster conversion
	AutoPXEBootOnConversionDefault = false
	// AutoPXEBootOnConversionKey is the key for enabling/disabling automatic PXE boot during cluster conversion
	AutoPXEBootOnConversionKey = "AUTO_PXE_BOOT_ON_CONVERSION"

	// StorageCopyMethod is the default value for storage copy method
	StorageCopyMethod = "StorageAcceleratedCopy"
)
