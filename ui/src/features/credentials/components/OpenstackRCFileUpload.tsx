import { Box, FormControl, FormHelperText, styled } from '@mui/material'
import React, { useState, useImperativeHandle, forwardRef } from 'react'
import { ActionButton, FieldLabel } from 'src/components'
import { TextField } from 'src/shared/components/forms'

const baseRequiredFields = ['OS_AUTH_URL','OS_DOMAIN_NAME','OS_REGION_NAME','OS_TENANT_NAME'];
const passwordAuthFields = ['OS_USERNAME','OS_PASSWORD'];

const FileUploadFieldContainer = styled('div')(({ theme }) => ({
  display: 'grid',
  gridTemplateColumns: '1fr auto',
  alignItems: 'center',
  gap: theme.spacing(2),
  marginTop: theme.spacing(2)
}))

interface OpenstackRCFileUploaderProps {
  onChange: (values: unknown) => void
  openstackCredsError?: string
  size?: 'small' | 'medium'
}

export interface OpenstackRCFileUploaderRef {
  reset: () => void
}

const OpenstackRCFileUploader = forwardRef<
  OpenstackRCFileUploaderRef,
  OpenstackRCFileUploaderProps
>(({ onChange, openstackCredsError, size = 'medium' }, ref) => {
  const [fileName, setFileName] = useState<string>('')
  const [error, setError] = useState<string | null>(null)
  const [fileInputKey, setFileInputKey] = useState<number>(0)

  useImperativeHandle(ref, () => ({
    reset: () => {
      setFileName('')
      setError(null)
      setFileInputKey((prev) => prev + 1)
    }
  }))

  const handleFileChange = (event: React.ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0]
    if (file) {
      setFileName(file.name)
      parseRCFile(file)
    }

    // Reset the file input so the same file can be selected again if needed
    event.target.value = ''
  }

  const parseRCFile = (file: File) => {
    const reader = new FileReader()
    reader.onload = (e) => {
      try {
        setError(null)
        const content = e.target?.result as string
        const parsedFields = parseFields(content)
        const isValid = validateFields(parsedFields)
        if (isValid) {
          onChange(parsedFields)
        }
      } catch {
        setError('Failed to parse the file. Please check the file format.')
      }
    }
    reader.onerror = () => {
      setError('Failed to read the file. Please try again.')
    }
    reader.readAsText(file)
  }

  const parseFields = (content: string) => {
    // Remove 'export' keywords from each line
    const cleanedContent = content.replace(/^export\s+/gm, '')
    const parsedFields: Record<string, string> = {}
    const lines = cleanedContent.split('\n')
    // Parse each line as key=value, handling special cases:
    for (const line of lines) {
      const trimmed = line.trim()
      if (!trimmed || trimmed.startsWith('#')) continue // Skip empty lines and lines starting with '#' (treated as comments)
      const eqIdx = trimmed.indexOf('=') // Only process lines containing '='
      if (eqIdx === -1) continue
      const key = trimmed.slice(0, eqIdx).trim()
      let value = trimmed.slice(eqIdx + 1).trim()

      // Remove surrounding quotes from values, if present
      if (
        (value.startsWith('"') && value.endsWith('"')) ||
        (value.startsWith("'") && value.endsWith("'"))
      ) {
        value = value.slice(1, -1)
      }
      parsedFields[key] = value
    }
    if (parsedFields.OS_PROJECT_DOMAIN_NAME && !parsedFields.OS_DOMAIN_NAME) {
      parsedFields.OS_DOMAIN_NAME = parsedFields.OS_PROJECT_DOMAIN_NAME
    }
    if (parsedFields.OS_PROJECT_NAME && !parsedFields.OS_TENANT_NAME) {
      parsedFields.OS_TENANT_NAME = parsedFields.OS_PROJECT_NAME
    }
    return parsedFields
  }

const validateFields = (fields: Record<string, string>) => {
  const hasToken = fields['OS_TOKEN'] && fields['OS_TOKEN'].trim() !== '';

  const dynamicRequiredFields = hasToken
    ? baseRequiredFields                      // Token auth
    : [...baseRequiredFields, ...passwordAuthFields]; // Username/password auth

  const missing = dynamicRequiredFields.filter(
    (f) => !fields[f] || fields[f].trim() === ''
  );

  return missing;
};


  return (
    <FileUploadFieldContainer>
      <FormControl error={!!(error || openstackCredsError)} fullWidth>
        <Box sx={{ display: 'flex', flexDirection: 'column', gap: 1 }}>
          <FieldLabel
            label="PCD RC File"
            required
            align="flex-start"
            helperText="Upload the RC file exported from your PCD environment."
          />
          <Box
            sx={{
              display: 'grid',
              gridTemplateColumns: 'minmax(0, 1fr) auto',
              gridGap: (theme) => theme.spacing(2)
            }}
          >
            <TextField
              value={fileName}
              placeholder="No file selected"
              InputProps={{ readOnly: true }}
              size={size}
              required
              error={!!(error || openstackCredsError)}
            />
            <ActionButton
              tone="primary"
              component="label"
              size={size === 'small' ? 'small' : 'medium'}
            >
              Choose File
              <input type="file" hidden onChange={handleFileChange} key={fileInputKey} />
            </ActionButton>
          </Box>
        </Box>
        <FormHelperText error={!!(error || openstackCredsError)}>
          {error || openstackCredsError || ' '}
        </FormHelperText>
      </FormControl>
    </FileUploadFieldContainer>
  )
})

export default OpenstackRCFileUploader
