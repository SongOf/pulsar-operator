// Reserve 16 characters for the longest Service suffix: -broker-headless.
// Keep this contract aligned with the Pulsar and BookKeeper CRD admission rules.
export const CLUSTER_NAME_MAX_LENGTH = 47
export const BOOKKEEPER_NAME_MAX_LENGTH = 51
export const CLUSTER_NAME_PATTERN = '[a-z]([-a-z0-9]*[a-z0-9])?'

export function validateClusterName(name, kind = 'PulsarCluster') {
  const limit = kind === 'BookKeeperCluster' ? BOOKKEEPER_NAME_MAX_LENGTH : CLUSTER_NAME_MAX_LENGTH
  if (typeof name !== 'string' || name.length > limit ||
      !new RegExp(`^${CLUSTER_NAME_PATTERN}$`).test(name)) {
    throw new Error(`集群名称须为 1–${limit} 位小写字母、数字或连字符，以字母开头，以字母或数字结尾。`)
  }
}
