/**
 * Utility functions for parsing and validating OpenStack RC files
 */

const baseRequiredFields = ['OS_AUTH_URL','OS_DOMAIN_NAME','OS_REGION_NAME','OS_TENANT_NAME'];
const passwordAuthFields = ['OS_USERNAME','OS_PASSWORD'];

export interface ParseRCFileResult {
  success: boolean
  fields?: Record<string, string>
  error?: string
}

/**
 * Parses the content of an OpenStack RC file and extracts environment variables
 */
export function parseRCFileContent(content: string): Record<string, string> {
  // Remove 'export' keywords from each line
  const cleanedContent = content.replace(/^export\s+/gm, '')
  const parsedFields: Record<string, string> = {}
  const lines = cleanedContent.split('\n')

  // Parse each line as key=value, handling special cases
  for (const line of lines) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) continue // Skip empty lines and comments
    const eqIdx = trimmed.indexOf('=')
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

  // Handle aliases: OS_PROJECT_DOMAIN_NAME -> OS_DOMAIN_NAME
  if (parsedFields.OS_PROJECT_DOMAIN_NAME && !parsedFields.OS_DOMAIN_NAME) {
    parsedFields.OS_DOMAIN_NAME = parsedFields.OS_PROJECT_DOMAIN_NAME
  }
  // Handle aliases: OS_PROJECT_NAME -> OS_TENANT_NAME
  if (parsedFields.OS_PROJECT_NAME && !parsedFields.OS_TENANT_NAME) {
    parsedFields.OS_TENANT_NAME = parsedFields.OS_PROJECT_NAME
  }

  return parsedFields
}

/**
 * Validates that all required fields are present in the parsed fields
 */
export function validateRCFileFields(fields: Record<string, string>): {
  valid: boolean
  missingFields: string[]
} {
  const hasToken = fields['OS_TOKEN'] && fields['OS_TOKEN'].trim() !== '';

  const dynamicRequiredFields = hasToken
    ? baseRequiredFields                      // Token auth
    : [...baseRequiredFields, ...passwordAuthFields]; // Username/password auth

  const missingFields = dynamicRequiredFields.filter(
    (f) => !fields[f] || fields[f].trim() === ''
  );

  return {
    valid: missingFields.length === 0,
     missingFields: missingFields
  }
}

/**
 * Reads a File and parses it as an OpenStack RC file
 */
export function parseRCFile(file: File): Promise<ParseRCFileResult> {
  return new Promise((resolve) => {
    const reader = new FileReader()

    reader.onload = (e) => {
      try {
        const content = e.target?.result as string
        const parsedFields = parseRCFileContent(content)
        const validation = validateRCFileFields(parsedFields)

        if (validation.valid) {
          resolve({
            success: true,
            fields: parsedFields
          })
        } else {
          resolve({
            success: false,
            error: `Missing required fields: ${validation.missingFields.join(', ')}`
          })
        }
      } catch (error) {
        resolve({
          success: false,
          error: 'Failed to parse the file. Please check the file format.'
        })
      }
    }

    reader.onerror = () => {
      resolve({
        success: false,
        error: 'Failed to read the file. Please try again.'
      })
    }

    reader.readAsText(file)
  })
}
