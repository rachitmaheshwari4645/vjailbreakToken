import { Box, Alert, Divider, Typography, useMediaQuery } from '@mui/material'
import MigrationIcon from '@mui/icons-material/SwapHoriz'
import { useQueryClient } from '@tanstack/react-query'
import axios from 'axios'
import { useEffect, useMemo, useRef, useState, useCallback } from 'react'
import { useNavigate } from 'react-router-dom'
import { postMigrationPlan } from 'src/features/migration/api/migration-plans/migrationPlans'
import { MigrationPlan } from 'src/features/migration/api/migration-plans/model'
import SecurityGroupAndServerGroupStep from './SecurityGroupAndServerGroup'
import {
  getMigrationTemplate,
  patchMigrationTemplate,
  postMigrationTemplate,
  deleteMigrationTemplate
} from 'src/features/migration/api/migration-templates/migrationTemplates'
import { MigrationTemplate, VmData } from 'src/features/migration/api/migration-templates/model'
import { createNetworkMappingJson } from 'src/api/network-mapping/helpers'
import { postNetworkMapping } from 'src/api/network-mapping/networkMappings'
import { OpenstackCreds } from 'src/api/openstack-creds/model'
import {
  getOpenstackCredentials,
  deleteOpenstackCredentials
} from 'src/api/openstack-creds/openstackCreds'
import { createStorageMappingJson } from 'src/api/storage-mappings/helpers'
import { postStorageMapping } from 'src/api/storage-mappings/storageMappings'
import { VMwareCreds } from 'src/api/vmware-creds/model'
import { getVmwareCredentials, deleteVmwareCredentials } from 'src/api/vmware-creds/vmwareCreds'
import { THREE_SECONDS } from 'src/constants'
import { MIGRATIONS_QUERY_KEY } from 'src/hooks/api/useMigrationsQuery'
import { VMWARE_MACHINES_BASE_KEY } from 'src/hooks/api/useVMwareMachinesQuery'
import { useInterval } from 'src/hooks/useInterval'
import useParams from 'src/hooks/useParams'
import { isNilOrEmpty } from 'src/utils'
import MigrationOptions from './MigrationOptionsAlt'
import NetworkAndStorageMappingStep from './NetworkAndStorageMappingStep'
import SourceDestinationClusterSelection from './SourceDestinationClusterSelection'
import VmsSelectionStep from './VmsSelectionStep'
import { CUTOVER_TYPES } from './constants'
import { uniq } from 'ramda'
import { flatten } from 'ramda'
import { useClusterData } from './useClusterData'
import { useErrorHandler } from 'src/hooks/useErrorHandler'
import { useRdmConfigValidation } from 'src/hooks/useRdmConfigValidation'
import { useRdmDisksQuery } from 'src/hooks/api/useRdmDisksQuery'
import { useAmplitude } from 'src/hooks/useAmplitude'
import { AMPLITUDE_EVENTS } from 'src/types/amplitude'
import { createMigrationTemplateJson } from 'src/features/migration/api/migration-templates/helpers'
import { createMigrationPlanJson } from 'src/features/migration/api/migration-plans/helpers'
import {
  ActionButton,
  DrawerFooter,
  DrawerHeader,
  DrawerShell,
  NavTab,
  NavTabs,
  SectionNav,
  SurfaceCard
} from 'src/components'
import type { SectionNavItem } from 'src/components'
import { useTheme } from '@mui/material/styles'
import { useForm, useWatch } from 'react-hook-form'
import { DesignSystemForm } from 'src/shared/components/forms'

const stringsCompareFn = (a: string, b: string) => a.toLowerCase().localeCompare(b.toLowerCase())

const drawerWidth = 1400

type MigrationDrawerRHFValues = {
  securityGroups: string[]
  serverGroup: string
  dataCopyStartTime: string
  cutoverStartTime: string
  cutoverEndTime: string
  postMigrationActionSuffix: string
  postMigrationActionFolderName: string
}

const stringArrayEqual = (a: string[] | undefined, b: string[] | undefined) => {
  if (a === b) return true
  if (!a || !b) return false
  if (a.length !== b.length) return false
  return a.every((v, i) => v === b[i])
}

export interface FormValues extends Record<string, unknown> {
  vmwareCreds?: {
    vcenterHost: string
    datacenter: string
    username: string
    password: string
    existingCredName?: string
    credentialName?: string
  }
  openstackCreds?: {
    OS_AUTH_URL: string
    OS_DOMAIN_NAME: string
    OS_USERNAME: string
    OS_PASSWORD: string
    OS_REGION_NAME: string
    OS_TENANT_NAME: string
    existingCredName?: string
    credentialName?: string
    OS_INSECURE?: boolean
    OS_TOKEN: string
  }
  vms?: VmData[]
  rdmConfigurations?: Array<{
    uuid: string
    diskName: string
    cinderBackendPool: string
    volumeType: string
    source: Record<string, string>
  }>
  networkMappings?: { source: string; target: string }[]
  storageMappings?: { source: string; target: string }[]
  // Cluster selection fields
  vmwareCluster?: string // Format: "credName:datacenter:clusterName"
  pcdCluster?: string // PCD cluster ID
  // Optional Params
  dataCopyMethod?: string
  dataCopyStartTime?: string
  cutoverOption?: string
  cutoverStartTime?: string
  cutoverEndTime?: string
  postMigrationScript?: string
  osFamily?: string
  // Add postMigrationAction with optional properties
  postMigrationAction?: {
    suffix?: string
    folderName?: string
    renameVm?: boolean
    moveToFolder?: boolean
  }
  disconnectSourceNetwork?: boolean
  securityGroups?: string[]
  serverGroup?: string
  fallbackToDHCP?: boolean
  useGPU?: boolean
  networkPersistence?: boolean
}

export interface SelectedMigrationOptionsType {
  dataCopyMethod: boolean
  dataCopyStartTime: boolean
  cutoverOption: boolean
  cutoverStartTime: boolean
  cutoverEndTime: boolean
  postMigrationScript: boolean
  periodicSyncEnabled?: boolean
  postMigrationAction?: {
    suffix?: boolean
    folderName?: boolean
    renameVm?: boolean
    moveToFolder?: boolean
  }
  [key: string]: unknown
}

// Default state for checkboxes
const defaultMigrationOptions = {
  dataCopyMethod: false,
  dataCopyStartTime: false,
  cutoverOption: false,
  cutoverStartTime: false,
  cutoverEndTime: false,
  postMigrationScript: false,
  postMigrationAction: {
    suffix: false,
    folderName: false,
    renameVm: false,
    moveToFolder: false
  }
}

const defaultValues: Partial<FormValues> = {}

export type FieldErrors = { [formId: string]: string }

interface MigrationFormDrawerProps {
  open: boolean
  onClose: () => void
  reloadMigrations?: () => void
  onSuccess?: (message: string) => void
}

export default function MigrationFormDrawer({
  open,
  onClose,
  onSuccess
}: MigrationFormDrawerProps) {
  const navigate = useNavigate()
  const { params, getParamsUpdater } = useParams<FormValues>(defaultValues)
  const { pcdData } = useClusterData()
  const { reportError } = useErrorHandler({ component: 'MigrationForm' })
  const { track } = useAmplitude({ component: 'MigrationForm' })
  const [, setError] = useState<{ title: string; message: string } | null>(null)
  // Theses are the errors that will be displayed on the form
  const { params: fieldErrors, getParamsUpdater: getFieldErrorsUpdater } = useParams<FieldErrors>(
    {}
  )
  const queryClient = useQueryClient()

  // Migration Options - Checked or Unchecked state
  const { params: selectedMigrationOptions, getParamsUpdater: updateSelectedMigrationOptions } =
    useParams<SelectedMigrationOptionsType>(defaultMigrationOptions)

  // Form Statuses
  const [submitting, setSubmitting] = useState(false)

  // Migration Resources
  const [vmwareCredentials, setVmwareCredentials] = useState<VMwareCreds | undefined>(undefined)
  const [openstackCredentials, setOpenstackCredentials] = useState<OpenstackCreds | undefined>(
    undefined
  )
  const [migrationTemplate, setMigrationTemplate] = useState<MigrationTemplate | undefined>(
    undefined
  )

  // Generate a unique session ID for this form instance
  const [sessionId] = useState(() => `form-session-${Date.now()}`)

  const form = useForm<MigrationDrawerRHFValues, any, MigrationDrawerRHFValues>({
    defaultValues: {
      securityGroups: params.securityGroups ?? [],
      serverGroup: params.serverGroup ?? '',
      dataCopyStartTime: params.dataCopyStartTime ?? '',
      cutoverStartTime: params.cutoverStartTime ?? '',
      cutoverEndTime: params.cutoverEndTime ?? '',
      postMigrationActionSuffix: params.postMigrationAction?.suffix ?? '',
      postMigrationActionFolderName: params.postMigrationAction?.folderName ?? ''
    }
  })

  const rhfSecurityGroups = useWatch({ control: form.control, name: 'securityGroups' })
  const rhfServerGroup = useWatch({ control: form.control, name: 'serverGroup' })
  const rhfDataCopyStartTime = useWatch({ control: form.control, name: 'dataCopyStartTime' })
  const rhfCutoverStartTime = useWatch({ control: form.control, name: 'cutoverStartTime' })
  const rhfCutoverEndTime = useWatch({ control: form.control, name: 'cutoverEndTime' })
  const rhfPostMigrationActionSuffix = useWatch({
    control: form.control,
    name: 'postMigrationActionSuffix'
  })
  const rhfPostMigrationActionFolderName = useWatch({
    control: form.control,
    name: 'postMigrationActionFolderName'
  })

  useEffect(() => {
    const nextSecurityGroups = params.securityGroups ?? []
    const nextServerGroup = params.serverGroup ?? ''

    const nextDataCopyStartTime = params.dataCopyStartTime ?? ''
    const nextCutoverStartTime = params.cutoverStartTime ?? ''
    const nextCutoverEndTime = params.cutoverEndTime ?? ''
    const nextPostMigrationActionSuffix = params.postMigrationAction?.suffix ?? ''
    const nextPostMigrationActionFolderName = params.postMigrationAction?.folderName ?? ''

    const currentSecurityGroups = form.getValues('securityGroups') ?? []
    const currentServerGroup = form.getValues('serverGroup') ?? ''
    const currentDataCopyStartTime = form.getValues('dataCopyStartTime') ?? ''
    const currentCutoverStartTime = form.getValues('cutoverStartTime') ?? ''
    const currentCutoverEndTime = form.getValues('cutoverEndTime') ?? ''
    const currentPostMigrationActionSuffix = form.getValues('postMigrationActionSuffix') ?? ''
    const currentPostMigrationActionFolderName =
      form.getValues('postMigrationActionFolderName') ?? ''

    if (!stringArrayEqual(currentSecurityGroups, nextSecurityGroups)) {
      form.setValue('securityGroups', nextSecurityGroups)
    }
    if (currentServerGroup !== nextServerGroup) {
      form.setValue('serverGroup', nextServerGroup)
    }

    if (currentDataCopyStartTime !== nextDataCopyStartTime) {
      form.setValue('dataCopyStartTime', nextDataCopyStartTime)
    }
    if (currentCutoverStartTime !== nextCutoverStartTime) {
      form.setValue('cutoverStartTime', nextCutoverStartTime)
    }
    if (currentCutoverEndTime !== nextCutoverEndTime) {
      form.setValue('cutoverEndTime', nextCutoverEndTime)
    }
    if (currentPostMigrationActionSuffix !== nextPostMigrationActionSuffix) {
      form.setValue('postMigrationActionSuffix', nextPostMigrationActionSuffix)
    }
    if (currentPostMigrationActionFolderName !== nextPostMigrationActionFolderName) {
      form.setValue('postMigrationActionFolderName', nextPostMigrationActionFolderName)
    }
  }, [
    form,
    params.securityGroups,
    params.serverGroup,
    params.dataCopyStartTime,
    params.cutoverStartTime,
    params.cutoverEndTime,
    params.postMigrationAction
  ])

  useEffect(() => {
    const nextDataCopyStartTime = (rhfDataCopyStartTime ?? '') as string
    if ((params.dataCopyStartTime ?? '') !== nextDataCopyStartTime) {
      getParamsUpdater('dataCopyStartTime')(nextDataCopyStartTime)
    }
  }, [getParamsUpdater, params.dataCopyStartTime, rhfDataCopyStartTime])

  useEffect(() => {
    const nextCutoverStartTime = (rhfCutoverStartTime ?? '') as string
    if ((params.cutoverStartTime ?? '') !== nextCutoverStartTime) {
      getParamsUpdater('cutoverStartTime')(nextCutoverStartTime)
    }
  }, [getParamsUpdater, params.cutoverStartTime, rhfCutoverStartTime])

  useEffect(() => {
    const nextCutoverEndTime = (rhfCutoverEndTime ?? '') as string
    if ((params.cutoverEndTime ?? '') !== nextCutoverEndTime) {
      getParamsUpdater('cutoverEndTime')(nextCutoverEndTime)
    }
  }, [getParamsUpdater, params.cutoverEndTime, rhfCutoverEndTime])

  useEffect(() => {
    const nextSuffix = String(rhfPostMigrationActionSuffix ?? '')
    const normalized = nextSuffix.trim() ? nextSuffix.trim() : ''
    const current = params.postMigrationAction?.suffix ?? ''

    const renameEnabled = !!selectedMigrationOptions.postMigrationAction?.renameVm
    if (!renameEnabled) return

    if (current !== normalized) {
      getParamsUpdater('postMigrationAction')({
        ...params.postMigrationAction,
        suffix: normalized ? normalized : undefined
      })
    }
  }, [
    getParamsUpdater,
    params.postMigrationAction,
    rhfPostMigrationActionSuffix,
    selectedMigrationOptions.postMigrationAction?.renameVm
  ])

  useEffect(() => {
    const nextFolderName = String(rhfPostMigrationActionFolderName ?? '')
    const normalized = nextFolderName.trim() ? nextFolderName.trim() : ''
    const current = params.postMigrationAction?.folderName ?? ''

    const moveToFolderEnabled = !!selectedMigrationOptions.postMigrationAction?.moveToFolder
    if (!moveToFolderEnabled) return

    if (current !== normalized) {
      getParamsUpdater('postMigrationAction')({
        ...params.postMigrationAction,
        folderName: normalized ? normalized : undefined
      })
    }
  }, [
    getParamsUpdater,
    params.postMigrationAction,
    rhfPostMigrationActionFolderName,
    selectedMigrationOptions.postMigrationAction?.moveToFolder
  ])

  useEffect(() => {
    const next = (rhfSecurityGroups ?? []) as string[]
    if (!stringArrayEqual(params.securityGroups ?? [], next)) {
      getParamsUpdater('securityGroups')(next)
    }
  }, [params.securityGroups, rhfSecurityGroups, getParamsUpdater])

  useEffect(() => {
    const next = (rhfServerGroup ?? '') as string
    if ((params.serverGroup ?? '') !== next) {
      getParamsUpdater('serverGroup')(next)
    }
  }, [params.serverGroup, rhfServerGroup, getParamsUpdater])

  const vmwareCredsValidated = vmwareCredentials?.status?.vmwareValidationStatus === 'Succeeded'

  const openstackCredsValidated =
    openstackCredentials?.status?.openstackValidationStatus === 'Succeeded'

  // Query RDM disks
  const { data: rdmDisks = [] } = useRdmDisksQuery({
    enabled: vmwareCredsValidated && openstackCredsValidated
  })

  // Polling Conditions - Poll when we have a migration template but it's not fully populated with networks/volumes
  const shouldPollMigrationTemplate =
    migrationTemplate?.metadata?.name &&
    (!migrationTemplate?.status?.openstack?.networks ||
      !migrationTemplate?.status?.openstack?.volumeTypes)

  // Update this effect to only handle existing credential selection
  useEffect(() => {
    const fetchCredentials = async () => {
      if (!params.vmwareCreds || !params.vmwareCreds.existingCredName) return

      try {
        const existingCredName = params.vmwareCreds.existingCredName
        const response = await getVmwareCredentials(existingCredName)
        setVmwareCredentials(response)
      } catch (error) {
        console.error('Error fetching existing VMware credentials:', error)
        getFieldErrorsUpdater('vmwareCreds')(
          'Error fetching VMware credentials: ' +
            (axios.isAxiosError(error) ? error?.response?.data?.message : error)
        )
      }
    }

    if (isNilOrEmpty(params.vmwareCreds)) return
    setVmwareCredentials(undefined)
    getFieldErrorsUpdater('vmwareCreds')('')
    fetchCredentials()
  }, [params.vmwareCreds, getFieldErrorsUpdater])

  // Update this effect to only handle existing credential selection
  useEffect(() => {
    const fetchCredentials = async () => {
      if (!params.openstackCreds || !params.openstackCreds.existingCredName) return

      try {
        const existingCredName = params.openstackCreds.existingCredName
        const response = await getOpenstackCredentials(existingCredName)
        setOpenstackCredentials(response)
      } catch (error) {
        console.error('Error fetching existing OpenStack credentials:', error)
        getFieldErrorsUpdater('openstackCreds')(
          'Error fetching PCD credentials: ' +
            (axios.isAxiosError(error) ? error?.response?.data?.message : error)
        )
      }
    }

    if (isNilOrEmpty(params.openstackCreds)) return
    // Reset the OpenstackCreds object if the user changes the credentials
    setOpenstackCredentials(undefined)
    getFieldErrorsUpdater('openstackCreds')('')
    fetchCredentials()
  }, [params.openstackCreds, getFieldErrorsUpdater])

  const targetPCDClusterName = useMemo(() => {
    if (!params.pcdCluster) return undefined
    const selectedPCD = pcdData.find((p) => p.id === params.pcdCluster)
    return selectedPCD?.name
  }, [params.pcdCluster, pcdData])

  useEffect(() => {
    if (!vmwareCredsValidated || !openstackCredsValidated) return

    const syncMigrationTemplate = async () => {
      try {
        // If a template already exists, update it instead of creating a new one
        if (migrationTemplate?.metadata?.name) {
          const patchBody = {
            spec: {
              source: {
                ...(params.vmwareCreds?.datacenter && {
                  datacenter: params.vmwareCreds.datacenter
                }),
                vmwareRef: vmwareCredentials?.metadata.name
              },
              destination: {
                openstackRef: openstackCredentials?.metadata.name
              },
              ...(targetPCDClusterName && {
                targetPCDClusterName
              }),
              useFlavorless: params.useFlavorless || false,
              useGPUFlavor: params.useGPU || false
            }
          }

          const updated = await patchMigrationTemplate(migrationTemplate.metadata.name, patchBody)
          setMigrationTemplate(updated)
          return
        }

        // Otherwise create a new template once
        const body = createMigrationTemplateJson({
          ...(params.vmwareCreds?.datacenter && { datacenter: params.vmwareCreds.datacenter }),
          vmwareRef: vmwareCredentials?.metadata.name,
          openstackRef: openstackCredentials?.metadata.name,
          targetPCDClusterName,
          useFlavorless: params.useFlavorless || false,
          useGPUFlavor: params.useGPU || false
        })
        const created = await postMigrationTemplate(body)
        setMigrationTemplate(created)
      } catch (err) {
        console.error('Error syncing migration template', err)
        getFieldErrorsUpdater('migrationTemplate')(
          'Error syncing migration template: ' +
            (axios.isAxiosError(err)
              ? err?.response?.data?.message
              : err instanceof Error
                ? err.message
                : String(err))
        )
      }
    }

    syncMigrationTemplate()
  }, [
    vmwareCredsValidated,
    openstackCredsValidated,
    params.vmwareCreds?.datacenter,
    vmwareCredentials?.metadata.name,
    openstackCredentials?.metadata.name,
    targetPCDClusterName,
    params.useFlavorless,
    params.useGPU,
    migrationTemplate?.metadata?.name,
    getFieldErrorsUpdater
  ])

  // Keep original fetchMigrationTemplate for fetching OpenStack networks and volume types
  const fetchMigrationTemplate = async () => {
    try {
      const updatedMigrationTemplate = await getMigrationTemplate(migrationTemplate!.metadata!.name)
      setMigrationTemplate(updatedMigrationTemplate)
    } catch (err) {
      console.error('Error retrieving migration templates', err)
      getFieldErrorsUpdater('migrationTemplate')('Error retrieving migration templates')
    }
  }

  useInterval(
    async () => {
      if (shouldPollMigrationTemplate) {
        try {
          fetchMigrationTemplate()
        } catch (err) {
          console.error('Error retrieving migration templates', err)
          getFieldErrorsUpdater('migrationTemplate')('Error retrieving migration templates')
        }
      }
    },
    THREE_SECONDS,
    shouldPollMigrationTemplate
  )

  useEffect(() => {
    if (vmwareCredsValidated && openstackCredsValidated) return
    // Reset all the migration resources if the user changes the credentials
    setMigrationTemplate(undefined)
  }, [vmwareCredsValidated, openstackCredsValidated])

  const availableVmwareNetworks = useMemo(() => {
    if (params.vms === undefined) return []
    return uniq(flatten(params.vms.map((vm) => vm.networks || []))).sort(stringsCompareFn) // Back to unique networks only
  }, [params.vms])

  const availableVmwareDatastores = useMemo(() => {
    if (params.vms === undefined) return []
    return uniq(flatten(params.vms.map((vm) => vm.datastores || []))).sort(stringsCompareFn)
  }, [params.vms])

  const createNetworkMapping = async (networkMappingParams) => {
    const body = createNetworkMappingJson({
      networkMappings: networkMappingParams
    })

    try {
      const data = postNetworkMapping(body)
      return data
    } catch (err) {
      setError({
        title: 'Error creating network mapping',
        message: axios.isAxiosError(err) ? err?.response?.data?.message : ''
      })
      getFieldErrorsUpdater('networksMapping')(
        'Error creating network mapping : ' +
          (axios.isAxiosError(err) ? err?.response?.data?.message : err)
      )
    }
  }

  const createStorageMapping = async (storageMappingsParams) => {
    const body = createStorageMappingJson({
      storageMappings: storageMappingsParams
    })
    try {
      const data = postStorageMapping(body)
      return data
    } catch (err) {
      console.error('Error creating storage mapping', err)
      reportError(err as Error, {
        context: 'storage-mapping-creation',
        metadata: {
          storageMappingsParams: storageMappingsParams,
          action: 'create-storage-mapping'
        }
      })
      setError({
        title: 'Error creating storage mapping',
        message: axios.isAxiosError(err) ? err?.response?.data?.message : ''
      })
      getFieldErrorsUpdater('storageMapping')(
        'Error creating storage mapping : ' +
          (axios.isAxiosError(err) ? err?.response?.data?.message : err)
      )
    }
  }

  const updateMigrationTemplate = async (migrationTemplate, networkMappings, storageMappings) => {
    const migrationTemplateName = migrationTemplate?.metadata?.name
    const updatedMigrationTemplateFields = {
      spec: {
        networkMapping: networkMappings.metadata.name,
        storageMapping: storageMappings.metadata.name
      }
    }
    try {
      const data = await patchMigrationTemplate(
        migrationTemplateName,
        updatedMigrationTemplateFields
      )
      return data
    } catch (err) {
      setError({
        title: 'Error updating migration template',
        message: axios.isAxiosError(err) ? err?.response?.data?.message : ''
      })
    }
  }

  const createMigrationPlan = async (
    updatedMigrationTemplate?: MigrationTemplate | null
  ): Promise<MigrationPlan> => {
    if (!updatedMigrationTemplate?.metadata?.name) {
      throw new Error('Migration template is not available')
    }

    const postMigrationAction = selectedMigrationOptions.postMigrationAction
      ? params.postMigrationAction
      : undefined

    const vmsToMigrate = (params.vms || []).map((vm) => vm.name)

    // Build AssignedIPsPerVM map for cold migration
    const assignedIPsPerVM: Record<string, string> = {}
    if (params.vms) {
      params.vms.forEach((vm) => {
        if (vm.assignedIPs && vm.assignedIPs.trim() !== '') {
          assignedIPsPerVM[vm.name] = vm.assignedIPs
        }
      })
    }

    const migrationFields = {
      migrationTemplateName: updatedMigrationTemplate?.metadata?.name,
      virtualMachines: vmsToMigrate,
      type: params.dataCopyMethod,
      ...(Object.keys(assignedIPsPerVM).length > 0 && { assignedIPsPerVM }),
      ...(selectedMigrationOptions.dataCopyStartTime &&
        params?.dataCopyStartTime && {
          dataCopyStart: params.dataCopyStartTime
        }),
      ...(selectedMigrationOptions.cutoverOption &&
        params.cutoverOption === CUTOVER_TYPES.ADMIN_INITIATED && {
          adminInitiatedCutOver: true
        }),
      ...(selectedMigrationOptions.cutoverOption &&
        params.cutoverOption === CUTOVER_TYPES.TIME_WINDOW &&
        params.cutoverStartTime && {
          vmCutoverStart: params.cutoverStartTime
        }),
      ...(selectedMigrationOptions.cutoverOption &&
        params.cutoverOption === CUTOVER_TYPES.TIME_WINDOW &&
        params.cutoverEndTime && {
          vmCutoverEnd: params.cutoverEndTime
        }),
      ...(postMigrationAction && { postMigrationAction }),
      ...(params.securityGroups &&
        params.securityGroups.length > 0 && {
          securityGroups: params.securityGroups
        }),
      ...(params.serverGroup && {
        serverGroup: params.serverGroup
      }),
      disconnectSourceNetwork: params.disconnectSourceNetwork || false,
      fallbackToDHCP: params.fallbackToDHCP || false,
      ...(selectedMigrationOptions.postMigrationScript &&
        params.postMigrationScript && {
          postMigrationScript: params.postMigrationScript
        }),
      ...(typeof params.networkPersistence === 'boolean' && {
        networkPersistence: params.networkPersistence
      }),
      periodicSyncInterval: params.periodicSyncInterval,
      periodicSyncEnabled: selectedMigrationOptions.periodicSyncEnabled
    }

    const body = createMigrationPlanJson(migrationFields)

    try {
      const data = await postMigrationPlan(body)

      // Track successful migration creation
      track(AMPLITUDE_EVENTS.MIGRATION_CREATED, {
        migrationName: data.metadata?.name,
        migrationTemplateName: updatedMigrationTemplate?.metadata?.name,
        virtualMachineCount: vmsToMigrate?.length || 0,
        migrationType: migrationFields.type,
        hasDataCopyStartTime: !!migrationFields.dataCopyStart,
        hasAdminInitiatedCutover: !!migrationFields.adminInitiatedCutOver,
        hasTimedCutover: !!(migrationFields.vmCutoverStart && migrationFields.vmCutoverEnd),
        postMigrationAction,
        namespace: data.metadata?.namespace
      })

      return data
    } catch (error: unknown) {
      console.error('Error creating migration plan', error)

      // Track migration creation failure
      track(AMPLITUDE_EVENTS.MIGRATION_CREATION_FAILED, {
        migrationTemplateName: updatedMigrationTemplate?.metadata?.name,
        virtualMachineCount: vmsToMigrate?.length || 0,
        migrationType: migrationFields.type,
        errorMessage: error instanceof Error ? error.message : String(error),
        stage: 'creation'
      })

      reportError(error as Error, {
        context: 'migration-plan-creation',
        metadata: {
          migrationFields: migrationFields,
          action: 'create-migration-plan'
        }
      })

      let errorMessage = 'An unknown error occurred'
      let errorResponse: {
        status?: number
        statusText?: string
        data?: unknown
        config?: {
          url?: string
          method?: string
          data?: unknown
        }
      } = {}

      if (axios.isAxiosError(error)) {
        errorMessage = error.response?.data?.message || error.message || String(error)
        errorResponse = {
          status: error.response?.status,
          statusText: error.response?.statusText,
          data: error.response?.data,
          config: {
            url: error.config?.url,
            method: error.config?.method,
            data: error.config?.data
          }
        }
      } else if (error instanceof Error) {
        errorMessage = error.message
      } else {
        errorMessage = String(error)
      }

      console.error('Error details:', errorResponse)

      setError({
        title: 'Error creating migration plan',
        message: errorMessage
      })

      getFieldErrorsUpdater('migrationPlan')(`Error creating migration plan: ${errorMessage}`)
      throw error
    }
  }

  const handleSubmit = useCallback(async () => {
    setSubmitting(true)
    setError(null)

    // Create NetworkMapping
    const networkMappings = await createNetworkMapping(params.networkMappings)

    // Create StorageMapping
    const storageMappings = await createStorageMapping(params.storageMappings)

    if (!networkMappings || !storageMappings) {
      setSubmitting(false)
      return
    }

    // Update MigrationTemplate with NetworkMapping and StorageMapping resource names
    const updatedMigrationTemplate = await updateMigrationTemplate(
      migrationTemplate,
      networkMappings,
      storageMappings
    )

    // Create MigrationPlan
    await createMigrationPlan(updatedMigrationTemplate)

    // Stop submitting state
    setSubmitting(false)
    queryClient.invalidateQueries({ queryKey: MIGRATIONS_QUERY_KEY })

    // Show success notification via callback
    onSuccess?.('Migration submitted successfully')

    // Close form and navigate
    onClose()
    navigate('/dashboard/migrations')
  }, [
    params.networkMappings,
    params.storageMappings,
    migrationTemplate,
    createNetworkMapping,
    createStorageMapping,
    updateMigrationTemplate,
    createMigrationPlan,
    queryClient,
    onClose,
    onSuccess,
    navigate
  ])

  const migrationOptionValidated = useMemo(() => {
    return Object.keys(selectedMigrationOptions).every((key) => {
      if (key === 'postMigrationAction') {
        // Post-migration actions are optional, so we don't validate them here
        return true
      }
      // TODO - Need to figure out a better way to add validation for periodic sync interval
      if (key === 'periodicSyncEnabled' && selectedMigrationOptions.periodicSyncEnabled) {
        return params?.periodicSyncInterval !== '' && fieldErrors['periodicSyncInterval'] === ''
      }
      if (selectedMigrationOptions[key as keyof typeof selectedMigrationOptions]) {
        return params?.[key as keyof typeof params] !== undefined && !fieldErrors[key]
      }
      return true
    })
  }, [selectedMigrationOptions, params, fieldErrors])

  // VM validation - ensure powered-off VMs have IP assigned and powered-on VMs have OS detected
  const vmValidation = useMemo(() => {
    if (!params.vms || params.vms.length === 0) {
      return { hasError: false, errorMessage: '' }
    }

    const poweredOffVMs = params.vms.filter((vm) => {
      // Determine power state - check different possible property names
      const powerState = vm.vmState === 'running' ? 'powered-on' : 'powered-off'
      return powerState === 'powered-off'
    })

    const poweredOnVMs = params.vms.filter((vm) => {
      // Determine power state - check different possible property names
      const powerState = vm.vmState === 'running' ? 'powered-on' : 'powered-off'
      return powerState === 'powered-on'
    })

    // Check for powered-off VMs without IP addresses
    const vmsWithoutIPs = poweredOffVMs.filter(
      (vm) => !vm.ipAddress || vm.ipAddress === '—' || vm.ipAddress.trim() === ''
    )

    // Check for VMs without OS assignment or with Unknown OS (any power state)
    const vmsWithoutOSAssigned = poweredOffVMs
      .filter((vm) => !vm.osFamily || vm.osFamily === 'Unknown' || vm.osFamily.trim() === '')
      .concat(
        poweredOnVMs.filter(
          (vm) => !vm.osFamily || vm.osFamily === 'Unknown' || vm.osFamily.trim() === ''
        )
      )

    if (vmsWithoutIPs.length > 0 || vmsWithoutOSAssigned.length > 0) {
      let errorMessage = 'Cannot proceed with migration: '
      const issues: string[] = []

      if (vmsWithoutIPs.length > 0) {
        issues.push(
          `${vmsWithoutIPs.length} powered-off VM${
            vmsWithoutIPs.length === 1 ? '' : 's'
          } missing IP address${vmsWithoutIPs.length === 1 ? '' : 'es'}`
        )
      }

      if (vmsWithoutOSAssigned.length > 0) {
        issues.push(
          `We could not detect the operating system for ${vmsWithoutOSAssigned.length} VM${
            vmsWithoutOSAssigned.length === 1 ? '' : 's'
          }`
        )
      }

      errorMessage +=
        issues.join(' and ') + '. Please assign the required information before continuing.'

      return { hasError: true, errorMessage }
    }

    return { hasError: false, errorMessage: '' }
  }, [params.vms])

  // RDM validation - check if RDM disks have missing required configuration
  const rdmValidation = useRdmConfigValidation({
    selectedVMs: params.vms || [],
    rdmDisks: rdmDisks
  })

  const disableSubmit =
    !vmwareCredsValidated ||
    !openstackCredsValidated ||
    isNilOrEmpty(params.vms) ||
    isNilOrEmpty(params.networkMappings) ||
    isNilOrEmpty(params.storageMappings) ||
    isNilOrEmpty(params.vmwareCluster) ||
    isNilOrEmpty(params.pcdCluster) ||
    // Check if all networks are mapped
    availableVmwareNetworks.some(
      (network) => !params.networkMappings?.some((mapping) => mapping.source === network)
    ) ||
    // Check if all datastores are mapped
    availableVmwareDatastores.some(
      (datastore) => !params.storageMappings?.some((mapping) => mapping.source === datastore)
    ) ||
    !migrationOptionValidated ||
    // VM validation - ensure powered-off VMs have IP and OS assigned
    vmValidation.hasError ||
    // RDM validation - ensure RDM disks are properly configured
    rdmValidation.hasValidationError

  const sortedOpenstackNetworks = useMemo(
    () => (openstackCredentials?.status?.openstack?.networks || []).sort(stringsCompareFn),
    [openstackCredentials?.status?.openstack?.networks]
  )
  const sortedOpenstackVolumeTypes = useMemo(
    () => (openstackCredentials?.status?.openstack?.volumeTypes || []).sort(stringsCompareFn),
    [openstackCredentials?.status?.openstack?.volumeTypes]
  )

  const handleClose = useCallback(async () => {
    try {
      setMigrationTemplate(undefined)
      setVmwareCredentials(undefined)
      setOpenstackCredentials(undefined)
      setError(null)

      // Invalidate and remove queries when form closes
      queryClient.invalidateQueries({ queryKey: [VMWARE_MACHINES_BASE_KEY, sessionId] })
      queryClient.removeQueries({ queryKey: [VMWARE_MACHINES_BASE_KEY, sessionId] })

      onClose()
      // Delete migration template if it exists
      if (migrationTemplate?.metadata?.name) {
        await deleteMigrationTemplate(migrationTemplate.metadata.name)
      }

      if (vmwareCredentials?.metadata?.name && !params.vmwareCreds?.existingCredName) {
        await deleteVmwareCredentials(vmwareCredentials.metadata.name)
      }

      if (openstackCredentials?.metadata?.name && !params.openstackCreds?.existingCredName) {
        await deleteOpenstackCredentials(openstackCredentials.metadata.name)
      }
    } catch (err) {
      console.error('Error cleaning up resources', err)
      reportError(err as Error, {
        context: 'resource-cleanup',
        metadata: {
          migrationTemplateName: migrationTemplate?.metadata?.name,
          vmwareCredentialsName: vmwareCredentials?.metadata?.name,
          openstackCredentialsName: openstackCredentials?.metadata?.name,
          action: 'cleanup-resources'
        }
      })
      onClose()
    }
  }, [
    migrationTemplate,
    vmwareCredentials,
    openstackCredentials,
    queryClient,
    sessionId,
    onClose,
    params.vmwareCreds,
    params.openstackCreds
  ])

  const contentRootRef = useRef<HTMLDivElement | null>(null)
  const section1Ref = useRef<HTMLDivElement | null>(null)
  const section2Ref = useRef<HTMLDivElement | null>(null)
  const section3Ref = useRef<HTMLDivElement | null>(null)
  const section4Ref = useRef<HTMLDivElement | null>(null)
  const section5Ref = useRef<HTMLDivElement | null>(null)
  const reviewRef = useRef<HTMLDivElement | null>(null)
  const [activeSectionId, setActiveSectionId] = useState<string>('source-destination')

  const [touchedSections, setTouchedSections] = useState({
    options: false
  })

  const markTouched = useCallback(
    (key: keyof typeof touchedSections) => {
      setTouchedSections((prev) => (prev[key] ? prev : { ...prev, [key]: true }))
    },
    [setTouchedSections]
  )

  useEffect(() => {
    if (!open) return
    setTouchedSections({
      options: false
    })
  }, [open])

  const isStep1Complete = Boolean(
    params.vmwareCluster &&
      params.pcdCluster &&
      !fieldErrors['vmwareCluster'] &&
      !fieldErrors['pcdCluster'] &&
      !fieldErrors['vmwareCreds'] &&
      !fieldErrors['openstackCreds']
  )

  const isStep2Complete = Boolean(
    (params.vms?.length || 0) > 0 &&
      !fieldErrors['vms'] &&
      !vmValidation.hasError &&
      !rdmValidation.hasConfigError
  )

  const isStep3Complete = useMemo(() => {
    if (!params.vms || params.vms.length === 0) return false
    if (fieldErrors['networksMapping'] || fieldErrors['storageMapping']) return false

    const networkMapped = availableVmwareNetworks.every((network) =>
      (params.networkMappings || []).some((m) => m.source === network)
    )
    const storageMapped = availableVmwareDatastores.every((datastore) =>
      (params.storageMappings || []).some((m) => m.source === datastore)
    )
    return networkMapped && storageMapped
  }, [
    params.vms,
    params.networkMappings,
    params.storageMappings,
    availableVmwareNetworks,
    availableVmwareDatastores,
    fieldErrors
  ])

  const unmappedNetworksCount = useMemo(() => {
    return availableVmwareNetworks.filter(
      (network) => !(params.networkMappings || []).some((m) => m.source === network)
    ).length
  }, [availableVmwareNetworks, params.networkMappings])

  const unmappedStorageCount = useMemo(() => {
    return availableVmwareDatastores.filter(
      (ds) => !(params.storageMappings || []).some((m) => m.source === ds)
    ).length
  }, [availableVmwareDatastores, params.storageMappings])

  const step1HasErrors = Boolean(
    fieldErrors['vmwareCluster'] ||
      fieldErrors['pcdCluster'] ||
      fieldErrors['vmwareCreds'] ||
      fieldErrors['openstackCreds']
  )

  const step2HasErrors = Boolean(
    fieldErrors['vms'] || vmValidation.hasError || rdmValidation.hasConfigError
  )

  const step3HasErrors = Boolean(fieldErrors['networksMapping'] || fieldErrors['storageMapping'])

  const step4Complete = Boolean(
    (params.securityGroups && params.securityGroups.length > 0) || params.serverGroup
  )

  const step5HasErrors = Boolean(
    (selectedMigrationOptions.dataCopyStartTime && fieldErrors['dataCopyStartTime']) ||
      (selectedMigrationOptions.cutoverOption &&
        (fieldErrors['cutoverOption'] ||
          (params.cutoverOption === CUTOVER_TYPES.TIME_WINDOW &&
            (fieldErrors['cutoverStartTime'] || fieldErrors['cutoverEndTime'])) ||
          (params.cutoverOption === CUTOVER_TYPES.ADMIN_INITIATED &&
            selectedMigrationOptions.periodicSyncEnabled &&
            fieldErrors['periodicSyncInterval']))) ||
      (selectedMigrationOptions.postMigrationScript && fieldErrors['postMigrationScript'])
  )

  const step5Complete = Boolean(touchedSections.options && !step5HasErrors)

  const sectionNavItems = useMemo<SectionNavItem[]>(
    () => [
      {
        id: 'source-destination',
        title: 'Source And Destination',
        description: 'Pick clusters and credentials',
        status: isStep1Complete ? 'complete' : step1HasErrors ? 'attention' : 'incomplete'
      },
      {
        id: 'select-vms',
        title: 'Select VMs',
        description: 'Choose VMs and assign required fields',
        status: isStep2Complete ? 'complete' : step2HasErrors ? 'attention' : 'incomplete'
      },
      {
        id: 'map-resources',
        title: 'Map Networks And Storage',
        description: 'Map VMware networks/datastores to PCD',
        status: isStep3Complete ? 'complete' : step3HasErrors ? 'attention' : 'incomplete'
      },
      {
        id: 'security',
        title: 'Security And Placement',
        description: 'Security groups and server group',
        status: step4Complete ? 'complete' : 'incomplete'
      },
      {
        id: 'options',
        title: 'Migration Options',
        description: 'Scheduling and advanced behavior',
        status: step5HasErrors ? 'attention' : step5Complete ? 'complete' : 'incomplete'
      }
    ],
    [
      isStep1Complete,
      isStep2Complete,
      isStep3Complete,
      step4Complete,
      step1HasErrors,
      step2HasErrors,
      step3HasErrors,
      step5HasErrors,
      step5Complete
    ]
  )

  const scrollToSection = useCallback((id: string) => {
    const map: Record<string, React.RefObject<HTMLDivElement | null>> = {
      'source-destination': section1Ref,
      'select-vms': section2Ref,
      'map-resources': section3Ref,
      security: section4Ref,
      options: section5Ref
    }

    const el = map[id]?.current
    if (!el) return
    el.scrollIntoView({ behavior: 'smooth', block: 'start' })
    setActiveSectionId(id)
  }, [])

  useEffect(() => {
    if (!open) return

    let cancelled = false
    let observer: IntersectionObserver | undefined
    let rafId: number | undefined

    const init = () => {
      if (cancelled) {
        if (rafId) cancelAnimationFrame(rafId)
        return
      }

      const root = contentRootRef.current?.parentElement ?? undefined
      const nodes = [
        section1Ref.current,
        section2Ref.current,
        section3Ref.current,
        section4Ref.current,
        section5Ref.current
      ].filter(Boolean) as HTMLDivElement[]

      if (!root || nodes.length === 0) {
        rafId = requestAnimationFrame(init)
        return
      }

      const idByNode = new Map<Element, string>([
        [section1Ref.current as HTMLDivElement, 'source-destination'],
        [section2Ref.current as HTMLDivElement, 'select-vms'],
        [section3Ref.current as HTMLDivElement, 'map-resources'],
        [section4Ref.current as HTMLDivElement, 'security'],
        [section5Ref.current as HTMLDivElement, 'options']
      ])

      observer = new IntersectionObserver(
        (entries) => {
          const visible = entries
            .filter((e) => e.isIntersecting)
            .sort((a, b) => (b.intersectionRatio ?? 0) - (a.intersectionRatio ?? 0))[0]

          if (!visible) return
          const id = idByNode.get(visible.target)
          if (id) setActiveSectionId(id)
        },
        {
          root,
          threshold: [0.2, 0.35, 0.5, 0.65]
        }
      )

      nodes.forEach((n) => observer?.observe(n))
    }

    rafId = requestAnimationFrame(init)

    return () => {
      cancelled = true
      if (rafId) cancelAnimationFrame(rafId)
      if (observer) observer.disconnect()
    }
  }, [open])

  const submitDisabled = disableSubmit || submitting

  const theme = useTheme()
  const isSmallNav = useMediaQuery(theme.breakpoints.down('md'))

  return (
    <DrawerShell
      data-testid="migration-form-drawer"
      open={open}
      onClose={handleClose}
      width={drawerWidth}
      ModalProps={{
        keepMounted: false,
        style: { zIndex: 1300 }
      }}
      header={
        <DrawerHeader
          data-testid="migration-form-header"
          title="Start Migration"
          subtitle="Configure source/destination, select VMs, and map resources before starting"
          icon={<MigrationIcon />}
          onClose={handleClose}
        />
      }
      footer={
        <DrawerFooter data-testid="migration-form-footer">
          <ActionButton tone="secondary" onClick={handleClose} data-testid="migration-form-cancel">
            Cancel
          </ActionButton>
          <ActionButton
            tone="primary"
            onClick={handleSubmit}
            disabled={submitDisabled}
            loading={submitting}
            data-testid="migration-form-submit"
          >
            Start Migration
          </ActionButton>
        </DrawerFooter>
      }
    >
      <DesignSystemForm
        form={form}
        onSubmit={async () => {
          await handleSubmit()
        }}
        keyboardSubmitProps={{
          open,
          onClose: handleClose,
          isSubmitDisabled: disableSubmit || submitting
        }}
      >
        <Box
          ref={contentRootRef}
          data-testid="migration-form-content"
          sx={{
            display: 'grid',
            gridTemplateColumns: isSmallNav ? '1fr' : '56px 1fr',
            gap: 3
          }}
        >
          {!isSmallNav ? (
            <SectionNav
              data-testid="migration-form-section-nav"
              items={sectionNavItems}
              activeId={activeSectionId}
              onSelect={scrollToSection}
              dense
              showDescriptions={false}
            />
          ) : null}

          <Box sx={{ display: 'grid', gap: 3 }}>
            {isSmallNav ? (
              <SurfaceCard
                title="Steps"
                subtitle="Jump to any section"
                data-testid="migration-form-steps-card"
              >
                <NavTabs
                  value={activeSectionId}
                  onChange={(_e, value) => scrollToSection(value as string)}
                  data-testid="migration-form-steps-tabs"
                >
                  {sectionNavItems.map((item) => (
                    <NavTab
                      key={item.id}
                      value={item.id}
                      label={item.title}
                      description={item.description}
                      data-testid={`migration-form-steps-tab-${item.id}`}
                    />
                  ))}
                </NavTabs>
              </SurfaceCard>
            ) : null}

            {/* Step 1 */}
            <Box ref={section1Ref} data-testid="migration-form-step-source-destination">
              <SurfaceCard
                variant="section"
                title="Source And Destination"
                subtitle="Choose where you migrate from and where you migrate to"
                data-testid="migration-form-step1-card"
              >
                <SourceDestinationClusterSelection
                  onChange={getParamsUpdater}
                  errors={fieldErrors}
                  vmwareCluster={params.vmwareCluster}
                  pcdCluster={params.pcdCluster}
                  showHeader={false}
                />
              </SurfaceCard>
            </Box>

            <Divider />

            {/* Step 2 - VM selection now manages its own data fetching with unique session ID */}
            <Box ref={section2Ref} data-testid="migration-form-step-select-vms">
              <SurfaceCard
                variant="section"
                title="Select VMs"
                subtitle="Pick the virtual machines you want to migrate"
                data-testid="migration-form-step2-card"
              >
                <VmsSelectionStep
                  onChange={getParamsUpdater}
                  error={fieldErrors['vms']}
                  open={open}
                  vmwareCredsValidated={vmwareCredsValidated}
                  openstackCredsValidated={openstackCredsValidated}
                  sessionId={sessionId}
                  openstackFlavors={openstackCredentials?.spec?.flavors}
                  vmwareCredName={params.vmwareCreds?.existingCredName}
                  openstackCredName={params.openstackCreds?.existingCredName}
                  openstackCredentials={openstackCredentials}
                  vmwareCluster={params.vmwareCluster}
                  useGPU={params.useGPU}
                  showHeader={false}
                />
                {vmValidation.hasError && (
                  <Alert severity="warning">{vmValidation.errorMessage}</Alert>
                )}
                {rdmValidation.hasConfigError && (
                  <Alert severity="error">{rdmValidation.configErrorMessage}</Alert>
                )}
              </SurfaceCard>
            </Box>
            <Divider />

            {/* Step 3 */}
            <Box ref={section3Ref} data-testid="migration-form-step-map-resources">
              <SurfaceCard
                variant="section"
                title="Map Networks And Storage"
                subtitle="Ensure all VMware networks and datastores have PCD targets"
                data-testid="migration-form-step3-card"
              >
                <NetworkAndStorageMappingStep
                  vmwareNetworks={availableVmwareNetworks}
                  vmWareStorage={availableVmwareDatastores}
                  openstackNetworks={sortedOpenstackNetworks}
                  openstackStorage={sortedOpenstackVolumeTypes}
                  params={params}
                  onChange={getParamsUpdater}
                  networkMappingError={fieldErrors['networksMapping']}
                  storageMappingError={fieldErrors['storageMapping']}
                  showHeader={false}
                />
              </SurfaceCard>
            </Box>
            <Divider />

            {/* Step 4 */}
            <Box ref={section4Ref} data-testid="migration-form-step-security">
              <SurfaceCard
                variant="section"
                title="Security groups and server group"
                subtitle="Optional placement and security settings"
                data-testid="migration-form-step4-card"
              >
                <SecurityGroupAndServerGroupStep
                  params={params}
                  onChange={getParamsUpdater}
                  openstackCredentials={openstackCredentials}
                  stepNumber="4"
                  showHeader={false}
                />
              </SurfaceCard>
            </Box>
            <Divider />

            {/* Step 5 */}
            <Box
              ref={section5Ref}
              data-testid="migration-form-step-options"
              onChangeCapture={() => markTouched('options')}
              onInputCapture={() => markTouched('options')}
              onClickCapture={() => markTouched('options')}
              onKeyDownCapture={() => markTouched('options')}
            >
              <SurfaceCard
                variant="section"
                title="Migration Options"
                subtitle="Optional scheduling, cutover behavior, and advanced settings"
                data-testid="migration-form-step5-card"
              >
                <MigrationOptions
                  params={params}
                  onChange={getParamsUpdater}
                  openstackCredentials={openstackCredentials}
                  selectedMigrationOptions={selectedMigrationOptions}
                  updateSelectedMigrationOptions={updateSelectedMigrationOptions}
                  errors={fieldErrors}
                  getErrorsUpdater={getFieldErrorsUpdater}
                  stepNumber="5"
                  showHeader={false}
                />
              </SurfaceCard>
            </Box>
            <Divider />

            <Box ref={reviewRef} data-testid="migration-form-step-review">
              <SurfaceCard
                variant="section"
                title="Preview"
                subtitle="Verify your selections before starting the migration"
                data-testid="migration-form-step6-card"
              >
                <Box sx={{ display: 'grid', gap: 1.5 }}>
                  <Typography variant="subtitle2">Summary</Typography>
                  <Divider />

                  <Box sx={{ display: 'grid', gap: 1 }}>
                    <Box sx={{ display: 'flex', justifyContent: 'space-between', gap: 2 }}>
                      <Typography variant="body2" color="text.secondary">
                        Source
                      </Typography>
                      <Typography variant="body2">{params.vmwareCluster || '—'}</Typography>
                    </Box>

                    <Box sx={{ display: 'flex', justifyContent: 'space-between', gap: 2 }}>
                      <Typography variant="body2" color="text.secondary">
                        Destination
                      </Typography>
                      <Typography variant="body2">
                        {targetPCDClusterName || params.pcdCluster || '—'}
                      </Typography>
                    </Box>

                    <Box sx={{ display: 'flex', justifyContent: 'space-between', gap: 2 }}>
                      <Typography variant="body2" color="text.secondary">
                        VMs selected
                      </Typography>
                      <Typography variant="body2">{params.vms?.length || 0}</Typography>
                    </Box>

                    <Box sx={{ display: 'flex', justifyContent: 'space-between', gap: 2 }}>
                      <Typography variant="body2" color="text.secondary">
                        Network mappings
                      </Typography>
                      <Typography variant="body2">
                        {availableVmwareNetworks.length === 0
                          ? '—'
                          : unmappedNetworksCount === 0
                            ? 'All mapped'
                            : `${unmappedNetworksCount} unmapped`}
                      </Typography>
                    </Box>

                    <Box sx={{ display: 'flex', justifyContent: 'space-between', gap: 2 }}>
                      <Typography variant="body2" color="text.secondary">
                        Storage mappings
                      </Typography>
                      <Typography variant="body2">
                        {availableVmwareDatastores.length === 0
                          ? '—'
                          : unmappedStorageCount === 0
                            ? 'All mapped'
                            : `${unmappedStorageCount} unmapped`}
                      </Typography>
                    </Box>
                  </Box>
                </Box>
              </SurfaceCard>
            </Box>
          </Box>
        </Box>
      </DesignSystemForm>
    </DrawerShell>
  )
}
