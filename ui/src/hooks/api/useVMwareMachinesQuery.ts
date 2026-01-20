import { useQuery } from '@tanstack/react-query'
import { getVMwareMachines } from 'src/api/vmware-machines/vmwareMachines'
import { VmData } from 'src/features/migration/api/migration-templates/model'
import { fetchRdmDisksMap, mapToVmDataWithRdm } from 'src/api/rdm-disks/rdmDiskUtils'
import { VMwareMachine } from 'src/api/vmware-machines/model'
import { getVMwareClusters } from 'src/api/vmware-clusters/vmwareClusters'

export const VMWARE_MACHINES_BASE_KEY = 'vmwaremachines'

interface UseVMwareMachinesQueryProps {
  vmwareCredsValidated?: boolean
  openstackCredsValidated?: boolean
  enabled?: boolean
  sessionId?: string
  vmwareCredName?: string
  clusterName?: string
  datacenterName?: string
}

export const useVMwareMachinesQuery = ({
  vmwareCredsValidated = false,
  openstackCredsValidated = false,
  enabled = true,
  sessionId = 'default',
  vmwareCredName,
  clusterName,
  datacenterName
}: UseVMwareMachinesQueryProps = {}) => {
  const areCredsValidated = vmwareCredsValidated && openstackCredsValidated
  const queryEnabled = enabled && areCredsValidated

  const queryKey = [
    VMWARE_MACHINES_BASE_KEY,
    sessionId,
    vmwareCredsValidated,
    openstackCredsValidated,
    vmwareCredName,
    clusterName,
    datacenterName
  ]

  return useQuery({
    queryKey,
    queryFn: async (): Promise<VmData[]> => {
      if (!areCredsValidated) {
        return []
      }
      const [vmResponse, rdmDisksMap, clustersResponse] = await Promise.all([
        getVMwareMachines(undefined, vmwareCredName),
        fetchRdmDisksMap(),
        getVMwareClusters(undefined, vmwareCredName)
      ])

      let filteredItems: VMwareMachine[] = vmResponse.items
      if(clusterName){
        console.warn('Filtering VMs by cluster:', clusterName)
      }else{
        console.warn('No cluster filter applied')
      }
      if(datacenterName){
        console.warn('Filtering VMs by datacenter:', datacenterName)
      }else{
        console.warn('No datacenter filter applied')
      }
      if (clusterName && datacenterName) {
        console.warn('Filtering VMs by cluster and datacenter:', clusterName, datacenterName)
        const isNoCluster = clusterName.startsWith('no-cluster-')
        if (isNoCluster) {
          const datacenterClusterNames = new Set<string>()
          console.log('Identifying clusters in datacenter with no cluster specified:', datacenterName)
          clustersResponse.items.forEach((cluster) => {
            const annotations = (cluster.metadata as any)?.annotations || {}
            const clusterDC =
              annotations['vjailbreak.k8s.pf9.io/datacenter'] || datacenterName || ''
            if (clusterDC === datacenterName) {
              datacenterClusterNames.add(cluster.metadata.name)
            }
          })
          console.log('Clusters found in datacenter:', Array.from(datacenterClusterNames))
          filteredItems = vmResponse.items.filter((vm) => {
            const vmClusterLabel =
              vm.metadata?.labels?.['vjailbreak.k8s.pf9.io/vmware-cluster'] || ''
            return datacenterClusterNames.has(vmClusterLabel)
          })
        } else {
          const selectedClusterResource = clustersResponse.items.find((cluster) => {
            const annotations = (cluster.metadata as any)?.annotations || {}
            const clusterDC = annotations['vjailbreak.k8s.pf9.io/datacenter'] || ''
            const dcMatches = !clusterDC || clusterDC === datacenterName
            const matchesDisplayName = cluster.spec.name === clusterName && dcMatches
            const matchesK8sName = cluster.metadata.name === clusterName && dcMatches

            return matchesDisplayName || matchesK8sName
          })

          const expectedClusterLabel = selectedClusterResource?.metadata.name
          console.log('Expected cluster label for filtering VMs:', expectedClusterLabel)
          if (expectedClusterLabel) {
            filteredItems = vmResponse.items.filter((vm) => {
              const vmClusterLabel = vm.metadata?.labels?.['vjailbreak.k8s.pf9.io/vmware-cluster']
              console.log('Cluster Label:', vmClusterLabel)
              return vmClusterLabel === expectedClusterLabel
            })
          } else {
            filteredItems = vmResponse.items
          }
        }
      }

      // Use RDM-aware mapping function
      return mapToVmDataWithRdm(filteredItems, rdmDisksMap)
    },
    enabled: queryEnabled,
    refetchOnWindowFocus: false,
    staleTime: 0, // Consider data immediately stale to ensure fresh fetch on new sessions
    // Don't keep previous data when credentials change or form reopens
    placeholderData: []
  })
}
