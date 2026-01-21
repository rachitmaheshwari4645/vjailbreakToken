import { v4 as uuidv4 } from 'uuid'

// Helper function to parse OS_INSECURE from string to boolean
const getBooleanValue = (value: string | undefined): boolean | undefined => {
  if (value === undefined) return undefined
  return value.toLowerCase() === 'true'
}

interface OpenstackCredsParams {
  name?: string
  namespace?: string
  OS_AUTH_URL?: string
  OS_DOMAIN_NAME?: string
  OS_USERNAME?: string
  OS_PASSWORD?: string
  OS_REGION_NAME?: string
  OS_TENANT_NAME?: string
  OS_INSECURE?: string
  existingCredName?: string
  OS_TOKEN?:string
}

export const createOpenstackCredsJson = (params: OpenstackCredsParams) => {
  const {
    name,
    namespace = 'migration-system',
    OS_AUTH_URL,
    OS_DOMAIN_NAME,
    OS_USERNAME,
    OS_PASSWORD,
    OS_REGION_NAME,
    OS_TENANT_NAME,
    OS_INSECURE,
    OS_TOKEN,
    existingCredName
  } = params || {}

  // If existingCredName is provided, we're using an existing credential
  // and don't need to create a new one
  if (existingCredName) {
    return null
  }

  return {
    apiVersion: 'vjailbreak.k8s.pf9.io/v1alpha1',
    kind: 'OpenstackCreds',
    metadata: {
      name: name || uuidv4(),
      namespace
    },
    spec: {
      OS_AUTH_URL,
      OS_DOMAIN_NAME,
      OS_USERNAME,
      OS_PASSWORD,
      OS_REGION_NAME,
      OS_TENANT_NAME,
      OS_INSECURE: getBooleanValue(OS_INSECURE),
      OS_TOKEN,
    }
  }
}

interface OpenstackCreds {
  OS_USERNAME: string
  OS_USER_DOMAIN_NAME?: string
  OS_PASSWORD: string
  OS_PROJECT_NAME?: string
  OS_PROJECT_DOMAIN_NAME?: string
  OS_TOKEN: string
}

export const createOpenstackTokenRequestBody = (creds: OpenstackCreds) => {
  // If token exists — do NOT require username/password
  if (creds.OS_TOKEN && creds.OS_TOKEN.trim() !== '') {
    return {
      auth: {
        identity: {
          methods: ['token'],
          token: {
            id: creds.OS_TOKEN
          }
        },
        scope: {
          project: {
            name: creds.OS_PROJECT_NAME || 'service',
            domain: { name: creds.OS_PROJECT_DOMAIN_NAME || 'default' }
          }
        }
      }
    }
  }else {
    return {
      auth: {
        identity: {
          methods: ['password'],
          password: {
            user: {
              name: creds.OS_USERNAME,
              domain: { name: creds.OS_USER_DOMAIN_NAME || 'default' },
              password: creds.OS_PASSWORD
            }
          }
        },
        scope: {
          project: {
            name: creds.OS_PROJECT_NAME || 'service',
            domain: { name: creds.OS_PROJECT_DOMAIN_NAME || 'default' }
          }
        }
      }
    }
  }
}
