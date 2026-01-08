import { Box, FormControl, FormHelperText } from '@mui/material'
import CredentialSelector from './CredentialSelector'

export interface OpenstackCredential {
  metadata: {
    name: string
    namespace?: string
  }
  spec: {
    // Support for both direct credentials and secretRef
    OS_AUTH_URL?: string
    OS_DOMAIN_NAME?: string
    OS_USERNAME?: string
    OS_PASSWORD?: string
    OS_REGION_NAME?: string
    OS_TENANT_NAME?: string
    secretRef?: {
      name: string
    }
    OS_TOKEN?: string
  }
  status?: {
    openstackValidationStatus?: string
    openstackValidationMessage?: string
  }
}

export interface OpenstackCredentialsFormProps {
  credentialsList?: OpenstackCredential[]
  loadingCredentials?: boolean
  error?: string
  onCredentialSelect?: (credId: string | null) => void
  selectedCredential?: string | null
  showCredentialNameField?: boolean
  showCredentialSelector?: boolean
  fullWidth?: boolean
  size?: 'small' | 'medium'
}

export default function OpenstackCredentialsForm({
  credentialsList = [],
  loadingCredentials = false,
  error,
  size = 'small',
  onCredentialSelect,
  selectedCredential = null,
  showCredentialSelector = true
}: OpenstackCredentialsFormProps) {
  // Format credentials for the selector
  const credentialOptions = credentialsList.map((cred) => ({
    label: cred.metadata.name,
    value: cred.metadata.name,
    metadata: cred.metadata,
    status: {
      validationStatus: cred.status?.openstackValidationStatus,
      validationMessage: cred.status?.openstackValidationMessage
    }
  }))

  return (
    <div>
      {showCredentialSelector && (
        <FormControl fullWidth error={!!error}>
          <CredentialSelector
            placeholder="Select PCD Credentials"
            options={credentialOptions}
            value={selectedCredential}
            onChange={onCredentialSelect || (() => {})}
            loading={loadingCredentials}
            size={size}
            emptyMessage="No PCD credentials found. Please use the credential drawer to create new ones."
            showAddNewButton={false}
          />
          {error && (
            <Box sx={{ mt: 1 }}>
              <FormHelperText error>{error}</FormHelperText>
            </Box>
          )}
        </FormControl>
      )}
    </div>
  )
}
