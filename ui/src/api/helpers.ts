import { deleteMigrationPlan, getMigrationPlansList } from './migration-plans/migrationPlans'
import {
  deleteMigrationTemplate,
  getMigrationTemplatesList
} from './migration-templates/migrationTemplates'
import { deleteNetworkMapping, getNetworkMappingList } from './network-mapping/networkMappings'
import {
  deleteOpenstackCredentials,
  getOpenstackCredentialsList,
  postOpenstackCredentials
} from './openstack-creds/openstackCreds'
import { deleteStorageMapping, getStorageMappingsList } from './storage-mappings/storageMappings'
import { deleteVmwareCredentials, getVmwareCredentialsList } from './vmware-creds/vmwareCreds'
import {
  createOpenstackCredsSecret,
  createVMwareCredsSecret,
  deleteSecret
} from './secrets/secrets'
import { createVMwareCredsWithSecret } from './vmware-creds/vmwareCreds'
import { VJAILBREAK_DEFAULT_NAMESPACE } from './constants'
import { AMPLITUDE_EVENTS, EventProperties } from 'src/types/amplitude'
import { enrichEventProperties, getTrackingBehavior } from 'src/config/amplitude'
import { trackEvent } from 'src/services/amplitudeService'
import axios from './axios'
import { get } from './axios'

export interface TrackingContext {
  component?: string
  userId?: string
  userEmail?: string
}

export const cleanupAllResources = async () => {
  // Clean up vmware creds
  try {
    const vmwareCreds = await getVmwareCredentialsList()
    for (const vmwareCred of vmwareCreds) {
      await deleteVmwareCredentials(vmwareCred.metadata.name)
    }
  } catch (e) {
    console.error('Error cleaning up vmware creds', e)
  }

  // Clean up PCD creds
  try {
    const openstackCreds = await getOpenstackCredentialsList()
    for (const openstackCred of openstackCreds) {
      await deleteOpenstackCredentials(openstackCred.metadata.name)
    }
  } catch (e) {
    console.error('Error cleaning up PCD creds', e)
  }

  // Clean up network mappings
  try {
    const networkMappings = await getNetworkMappingList()
    for (const networkMapping of networkMappings) {
      await deleteNetworkMapping(networkMapping.metadata.name)
    }
  } catch (e) {
    console.error('Error cleaning up network mappings', e)
  }

  // Clean up storage mappings
  try {
    const storageMappings = await getStorageMappingsList()
    for (const storageMapping of storageMappings) {
      await deleteStorageMapping(storageMapping.metadata.name)
    }
  } catch (e) {
    console.error('Error cleaning up storage mappings', e)
  }

  // Clean up migration templates
  try {
    const migrationTemplates = await getMigrationTemplatesList()
    for (const migrationTemplate of migrationTemplates) {
      await deleteMigrationTemplate(migrationTemplate.metadata.name)
    }
  } catch (e) {
    console.error('Error cleaning up migration templates', e)
  }

  // Clean up migration plans. This will also clean up migrations
  try {
    const migrationPlans = await getMigrationPlansList()
    for (const migrationPlan of migrationPlans) {
      await deleteMigrationPlan(migrationPlan.metadata.name)
    }
  } catch (e) {
    console.error('Error cleaning up migration plans', e)
  }
}

// Create OpenStack credentials with secret
export const createOpenstackCredsWithSecretFlow = async (
  credName: string,
  credentials: {
    OS_USERNAME: string
    OS_PASSWORD: string
    OS_AUTH_URL: string
    OS_PROJECT_NAME?: string
    OS_TENANT_NAME?: string
    OS_DOMAIN_NAME: string
    OS_REGION_NAME?: string
    OS_INSECURE?: boolean
    OS_TOKEN:string
  },
  isPcd: boolean = false,
  projectName: string,
  namespace = VJAILBREAK_DEFAULT_NAMESPACE
) => {
  const secretName = `${credName}-openstack-secret`

  // First create the secret
  await createOpenstackCredsSecret(secretName, credentials, namespace)

  // Then create the OpenStack credentials with the label
  const credBody = {
    apiVersion: 'vjailbreak.k8s.pf9.io/v1alpha1',
    kind: 'OpenstackCreds',
    metadata: {
      name: credName,
      namespace,
      labels: {
        'vjailbreak.k8s.pf9.io/is-pcd': isPcd ? 'true' : 'false'
      }
    },
    spec: {
      secretRef: {
        name: secretName
      },
      projectName: projectName
    }
  }

  return postOpenstackCredentials(credBody, namespace)
}

// Create VMware credentials with secret
export const createVMwareCredsWithSecretFlow = async (
  credName: string,
  credentials,
  namespace = VJAILBREAK_DEFAULT_NAMESPACE
) => {
  const secretName = `${credName}-vmware-secret`

  await createVMwareCredsSecret(secretName, credentials, namespace)

  return createVMwareCredsWithSecret(
    credName,
    secretName,
    namespace,
    credentials.VCENTER_DATACENTER
  )
}

export const deleteVMwareCredsWithSecretFlow = async (
  credName: string,
  namespace = VJAILBREAK_DEFAULT_NAMESPACE
) => {
  try {
    const secretName = `${credName}-vmware-secret`
    await deleteVmwareCredentials(credName, namespace)
    await deleteSecret(secretName, namespace)
    return { success: true }
  } catch (error) {
    console.error(`Error deleting VMware credential ${credName}:`, error)
    throw error
  }
}

// Delete OpenStack credentials and associated secret
export const deleteOpenStackCredsWithSecretFlow = async (
  credName: string,
  namespace = VJAILBREAK_DEFAULT_NAMESPACE
) => {
  try {
    await deleteOpenstackCredentials(credName, namespace)
    return { success: true }
  } catch (error) {
    console.error(`Error deleting OpenStack credential ${credName}:`, error)
    throw error
  }
}

export interface RevalidateCredentialsRequest {
  name: string
  namespace: string
  kind: 'VmwareCreds' | 'OpenstackCreds'
}

export interface RevalidateCredentialsResponse {
  message: string
}

export const revalidateCredentials = (data: RevalidateCredentialsRequest) => {
  return axios.post<RevalidateCredentialsResponse>({
    endpoint: '/dev-api/sdk/vpw/v1/revalidate_credentials',
    data
  })
}

export interface InjectEnvVariablesRequest {
  http_proxy?: string
  https_proxy?: string
  no_proxy?: string
}

export interface InjectEnvVariablesResponse {
  message: string
}

export const injectEnvVariables = (data: InjectEnvVariablesRequest) => {
  return axios.post<InjectEnvVariablesResponse>({
    endpoint: '/dev-api/sdk/vpw/v1/inject_env_variables',
    data
  })
}

export interface Pf9EnvConfigMap {
  apiVersion: string
  kind: string
  metadata: {
    name: string
    namespace: string
    [key: string]: any
  }
  data?: Record<string, string>
}

export const getPf9EnvConfig = async (): Promise<Pf9EnvConfigMap> => {
  const endpoint = '/api/v1/namespaces/migration-system/configmaps/pf9-env'
  return get<Pf9EnvConfigMap>({
    endpoint,
    config: { mock: false }
  })
}

// Simplified API call tracking
export const trackApiCall = async <T>(
  operation: () => Promise<T>,
  successEvent: keyof typeof AMPLITUDE_EVENTS,
  failureEvent: keyof typeof AMPLITUDE_EVENTS,
  baseProperties: EventProperties = {},
  context: TrackingContext = {}
): Promise<T> => {
  const behavior = getTrackingBehavior()

  // If tracking disabled, just execute operation
  if (!behavior.enabled) {
    return operation()
  }

  try {
    const result = await operation()

    // Track success
    const successProperties = enrichEventProperties(baseProperties, {
      component: context.component || behavior.defaultComponent,
      userId: context.userId,
      userEmail: context.userEmail
    })

    trackEvent(AMPLITUDE_EVENTS[successEvent], successProperties)
    return result
  } catch (error) {
    // Track failure
    const errorMessage = error instanceof Error ? error.message : String(error)
    const failureProperties = enrichEventProperties(baseProperties, {
      component: context.component || behavior.defaultComponent,
      userId: context.userId,
      userEmail: context.userEmail,
      errorMessage
    })

    trackEvent(AMPLITUDE_EVENTS[failureEvent], failureProperties)
    throw error
  }
}
