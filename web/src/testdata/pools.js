// Fictional reports only; these numbers are not measurements from a real disk.
export function benchmarkPool() {
  return {
    id: 'pool-one', name: 'ssd-one', node: 'worker-1', uid: 'pool-uid', nodeUID: 'node-uid', devices: [{ path: '/dev/disk/by-id/wwn-one', serial: 'SER1', sizeBytes: 100 * 1024 ** 3 }], resourceVersion: '17', vgName: 'df_one', state: 'AwaitingReview', allocationState: 'AwaitingReview', canReview: true, canCreateVolume: false, allocationReasons: ['等待人工审核'],
    capacityBytes: 100 * 1024 ** 3, allocatedBytes: 1024 ** 3, availableBytes: 99 * 1024 ** 3,
    benchmark: {
      poolUID: 'pool-uid', nodeUID: 'node-uid', vgName: 'df_one', runID: 'run-one', profile: 'temporary-lv-v1', phase: 'Succeeded',
      device: { path: '/dev/disk/by-id/wwn-one', serial: 'SER1', sizeBytes: 100 * 1024 ** 3 },
      startedAt: '2026-11-16T08:00:00Z', completedAt: '2026-11-16T08:02:02Z', testSizeBytes: 1024 ** 3,
      workloads: ['seq-write', 'seq-read', 'rand-write', 'rand-read'].map(name => ({ name, blockSizeBytes: name.startsWith('seq-') ? 1024 ** 2 : 4096, queueDepth: 16, durationSeconds: 30, bytes: 8 * 1024 ** 3, bandwidthBytesPerSecond: 256 * 1024 ** 2, iops: 256, latencyMeanNs: 1e6, latencyP95Ns: 2e6, latencyP99Ns: 3e6, errors: 0 })),
      cleanupVerified: true, resultDigest: 'sha256-report-one',
    },
  }
}

export function approvedPool() {
  const pool = benchmarkPool()
  return { ...pool, state: 'Ready', allocationState: 'Approved', canReview: false, canCreateVolume: true, allocationReasons: [], allocationApproval: { poolUID: pool.uid, runID: pool.benchmark.runID, resultDigest: pool.benchmark.resultDigest, decision: 'Approved', reviewedAt: '2026-11-16T08:03:00Z', note: 'fixture only' } }
}
