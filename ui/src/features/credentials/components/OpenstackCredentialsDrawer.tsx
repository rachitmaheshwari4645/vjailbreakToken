import { Box } from '@mui/material'
import axios from 'axios'
import { useState, useCallback } from 'react'
import { useForm, SubmitHandler } from 'react-hook-form'
import {
  DrawerShell,
  DrawerHeader,
  DrawerFooter,
  ActionButton,
  Section,
  SectionHeader,
  OperationStatus,
  Row,
  FormGrid,
  SurfaceCard
} from 'src/components'
import {
  createOpenstackCredsWithSecretFlow,
  deleteOpenStackCredsWithSecretFlow
} from 'src/api/helpers'
import { useOpenstackCredentialsQuery } from 'src/hooks/api/useOpenstackCredentialsQuery'
import { THREE_SECONDS } from 'src/constants'
import { useInterval } from 'src/hooks/useInterval'
import { getOpenstackCredentials } from 'src/api/openstack-creds/openstackCreds'
import { isValidName } from 'src/utils'
import CheckIcon from '@mui/icons-material/Check'
import {
  DesignSystemForm,
  RHFTextField,
  RHFToggleField,
  RHFOpenstackRCFileField
} from 'src/shared/components/forms'
import { useErrorHandler } from 'src/hooks/useErrorHandler'
import { useAmplitude } from 'src/hooks/useAmplitude'
import { AMPLITUDE_EVENTS } from 'src/types/amplitude'

interface OpenstackCredentialsDrawerProps {
  open: boolean
  onClose: () => void
}

interface OpenstackCredentialsFormValues {
  credentialName: string
  rcFile?: File
  isPcd: boolean
  insecure: boolean
}

export default function OpenstackCredentialsDrawer({
  open,
  onClose
}: OpenstackCredentialsDrawerProps) {
  const { reportError } = useErrorHandler({ component: 'OpenstackCredentialsDrawer' })
  const { track } = useAmplitude({ component: 'OpenstackCredentialsDrawer' })

  const form = useForm<OpenstackCredentialsFormValues>({
    defaultValues: {
      credentialName: '',
      rcFile: undefined,
      isPcd: true,
      insecure: false
    }
  })

  const {
    watch,
    setValue,
    setError: setFormError,
    reset,
    formState: { errors }
  } = form

  const [validatingOpenstackCreds, setValidatingOpenstackCreds] = useState(false)
  const [openstackCredsValidated, setOpenstackCredsValidated] = useState<boolean | null>(null)
  const [operationError, setOperationError] = useState<string | null>(null)
  const [submitting, setSubmitting] = useState(false)
  const [createdCredentialName, setCreatedCredentialName] = useState<string | null>(null)
  const [createdCredentialIsPcd, setCreatedCredentialIsPcd] = useState(false)

  const watchedValues = watch()
  const credentialName = watchedValues.credentialName
  const rcFile = watchedValues.rcFile

  const { refetch: refetchOpenstackCreds } = useOpenstackCredentialsQuery()

  const resetDrawerState = useCallback(() => {
    reset({
      credentialName: '',
      rcFile: undefined,
      isPcd: false,
      insecure: false
    })
    setRcFileValues(null)
    setCreatedCredentialName(null)
    setCreatedCredentialIsPcd(false)
    setValidatingOpenstackCreds(false)
    setOpenstackCredsValidated(null)
    setOperationError(null)
    setSubmitting(false)

    onClose()
  }, [onClose, reset])

  const closeDrawer = useCallback(() => {
    if (createdCredentialName) {
      try {
        deleteOpenStackCredsWithSecretFlow(createdCredentialName)
          .then(() =>
            console.log(`Cancelled credential ${createdCredentialName} deleted successfully`)
          )
          .catch((err) =>
            console.error(`Error deleting cancelled credential: ${createdCredentialName}`, err)
          )
      } catch (err) {
        console.error(
          `Error initiating deletion of cancelled credential: ${createdCredentialName}`,
          err
        )
      }
    }

    resetDrawerState()
  }, [createdCredentialName, resetDrawerState])

  const [rcFileValues, setRcFileValues] = useState<Record<string, string> | null>(null)

  const handleRCFileParsed = useCallback(
    (values: Record<string, string>) => {
      setRcFileValues(values)
      setOpenstackCredsValidated(null)
      setOperationError(null)

      if (values.OS_INSECURE !== undefined) {
        const val = values.OS_INSECURE.toString().toLowerCase().trim()
        if (['true', '1', 'yes'].includes(val)) {
          setValue('insecure', true)
        } else if (['false', '0', 'no'].includes(val)) {
          setValue('insecure', false)
        }
      }
    },
    [setValue]
  )

  const getApiErrorMessage = (error: unknown): string => {
    if (axios.isAxiosError(error) && typeof error.response?.data?.message === 'string') {
      return error.response.data.message
    }
    if (error instanceof Error) {
      return error.message
    }
    return 'An unknown error occurred. Please try again.'
  }

  const handleSubmit: SubmitHandler<OpenstackCredentialsFormValues> = useCallback(
    async (values) => {
      if (!rcFileValues) {
        setFormError('rcFile', {
          type: 'manual',
          message: 'OpenStack RC file is required'
        })
        return
      }

      setSubmitting(true)
      setValidatingOpenstackCreds(true)
      setOperationError(null)

      try {
        const projectName = rcFileValues.OS_PROJECT_NAME || rcFileValues.OS_TENANT_NAME

        const response = await createOpenstackCredsWithSecretFlow(
          values.credentialName,
          {
            OS_AUTH_URL: rcFileValues.OS_AUTH_URL,
            OS_DOMAIN_NAME: rcFileValues.OS_DOMAIN_NAME,
            OS_USERNAME: rcFileValues.OS_USERNAME,
            OS_PASSWORD: rcFileValues.OS_PASSWORD,
            OS_REGION_NAME: rcFileValues.OS_REGION_NAME,
            OS_TOKEN:rcFileValues.OS_TOKEN,
            OS_TENANT_NAME: projectName,
            OS_INSECURE: values.insecure
          },
          values.isPcd,
          projectName
        )

        setCreatedCredentialName(response.metadata.name)
        setCreatedCredentialIsPcd(values.isPcd)

        track(AMPLITUDE_EVENTS.CREDENTIALS_ADDED, {
          credentialType: 'openstack',
          credentialName: values.credentialName,
          isPcd: values.isPcd,
          namespace: response.metadata.namespace
        })
      } catch (error: unknown) {
        console.error('Error creating OpenStack credentials:', error)

        track(AMPLITUDE_EVENTS.CREDENTIALS_FAILED, {
          credentialType: 'openstack',
          credentialName: values.credentialName,
          isPcd: values.isPcd,
          errorMessage: error instanceof Error ? error.message : String(error),
          stage: 'creation'
        })

        reportError(error as Error, {
          context: 'openstack-credential-creation',
          metadata: {
            credentialName: values.credentialName,
            isPcd: values.isPcd,
            action: 'create-openstack-credential'
          }
        })
        setOpenstackCredsValidated(false)
        setValidatingOpenstackCreds(false)

        const errorMessage = getApiErrorMessage(error)
        setOperationError(errorMessage)
        setSubmitting(false)
      }
    },
    [rcFileValues, reportError, setFormError, track]
  )

  const isSubmitDisabled =
    submitting ||
    validatingOpenstackCreds ||
    !!errors.credentialName ||
    !!errors.rcFile ||
    !credentialName ||
    !rcFile ||
    !rcFileValues

  const handleValidationStatus = (status: string, message?: string) => {
    if (status === 'Succeeded') {
      setOpenstackCredsValidated(true)
      setValidatingOpenstackCreds(false)

      track(AMPLITUDE_EVENTS.CREDENTIALS_ADDED, {
        credentialType: 'openstack',
        credentialName: createdCredentialName,
        isPcd: createdCredentialIsPcd,
        stage: 'validation_success'
      })

      setTimeout(() => {
        refetchOpenstackCreds()
        resetDrawerState()
      }, 1500)
    } else if (status === 'Failed') {
      setOpenstackCredsValidated(false)
      setValidatingOpenstackCreds(false)
      setOperationError(message || 'Validation failed')

      track(AMPLITUDE_EVENTS.CREDENTIALS_FAILED, {
        credentialType: 'openstack',
        credentialName: createdCredentialName,
        isPcd: createdCredentialIsPcd,
        errorMessage: message || 'Validation failed',
        stage: 'validation'
      })

      reportError(
        new Error(`OpenStack credential validation failed: ${message || 'Unknown reason'}`),
        {
          context: 'openstack-validation-failure',
          metadata: {
            credentialName: createdCredentialName,
            validationMessage: message,
            action: 'openstack-validation-failed'
          }
        }
      )

      if (createdCredentialName) {
        try {
          deleteOpenStackCredsWithSecretFlow(createdCredentialName)
            .then(() => console.log(`Failed credential ${createdCredentialName} deleted`))
            .catch((deleteErr) =>
              console.error(`Error deleting failed credential: ${createdCredentialName}`, deleteErr)
            )
        } catch (deleteErr) {
          console.error(`Error deleting failed credential: ${createdCredentialName}`, deleteErr)
          reportError(deleteErr as Error, {
            context: 'openstack-credential-deletion',
            metadata: {
              credentialName: createdCredentialName,
              action: 'delete-failed-credential'
            }
          })
        }
      }
    }
    setSubmitting(false)
  }

  const shouldPollOpenstackCreds = !!createdCredentialName && validatingOpenstackCreds

  useInterval(
    async () => {
      try {
        const response = await getOpenstackCredentials(createdCredentialName)
        if (response?.status?.openstackValidationStatus) {
          handleValidationStatus(
            response.status.openstackValidationStatus,
            response.status.openstackValidationMessage
          )
        }
      } catch (err) {
        console.error('Error validating OpenStack credentials', err)
        reportError(err as Error, {
          context: 'openstack-validation-polling',
          metadata: {
            credentialName: createdCredentialName,
            action: 'openstack-validation-status-polling'
          }
        })
        setOperationError('Error validating PCD credentials')
        setValidatingOpenstackCreds(false)
        setSubmitting(false)
      }
    },
    THREE_SECONDS,
    shouldPollOpenstackCreds
  )

  return (
    <DrawerShell
      open={open}
      onClose={closeDrawer}
      header={
        <DrawerHeader
          title="Add PCD Credentials"
          subtitle="Upload an RC file and validate access to your PCD environment"
          onClose={closeDrawer}
        />
      }
      footer={
        <DrawerFooter>
          <ActionButton tone="secondary" onClick={closeDrawer} data-testid="openstack-cred-cancel">
            Cancel
          </ActionButton>
          <ActionButton
            tone="primary"
            type="submit"
            form="openstack-cred-form"
            loading={submitting}
            disabled={isSubmitDisabled}
            data-testid="openstack-cred-submit"
          >
            Save Credential
          </ActionButton>
        </DrawerFooter>
      }
    >
      <DesignSystemForm
        form={form}
        id="openstack-cred-form"
        onSubmit={handleSubmit}
        keyboardSubmitProps={{
          open,
          onClose: closeDrawer,
          isSubmitDisabled
        }}
      >
        <SurfaceCard>
          <Section>
            <SectionHeader
              title="PCD Credential Details"
              subtitle="Give your credential a clear name and upload the RC file from your PCD environment."
            />

            <FormGrid minWidth={360} gap={2}>
              <RHFTextField
                name="credentialName"
                label="PCD Credential Name"
                placeholder="e.g. prod-pcd"
                rules={{
                  required: 'Credential name is required',
                  validate: (value: string) =>
                    isValidName(value) ||
                    'Credential name must start with a letter or number, followed by letters, numbers or hyphens, with a maximum length of 253 characters'
                }}
                fullWidth
                required
              />
            </FormGrid>

            <RHFOpenstackRCFileField
              name="rcFile"
              onParsed={handleRCFileParsed}
              size="small"
              required
            />

            <Row gap={3} flexWrap="wrap">
              <Box sx={{ flex: 1, minWidth: 260 }}>
                <RHFToggleField
                  name="isPcd"
                  label="Is PCD credential"
                  description="Mark this if the credential is for a Private Cloud Director (PCD)."
                />
              </Box>
              <Box sx={{ flex: 1, minWidth: 260 }}>
                <RHFToggleField
                  name="insecure"
                  label="Allow insecure TLS (skip SSL verification)"
                  description="Use only for testing or environments with self-signed certificates."
                />
              </Box>
            </Row>

            <OperationStatus
              mt={3}
              display="flex"
              flexDirection="column"
              gap={2}
              loading={validatingOpenstackCreds}
              loadingMessage="Validating PCD credentials…"
              success={openstackCredsValidated === true && Boolean(credentialName)}
              successMessage="PCD credentials created and validated."
              successIcon={<CheckIcon color="success" fontSize="small" />}
              error={operationError}
            />
          </Section>
        </SurfaceCard>
      </DesignSystemForm>
    </DrawerShell>
  )
}
