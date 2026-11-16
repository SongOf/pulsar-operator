package lvm

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"

	storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"
	"github.com/SongOf/pulsar-operator/internal/storage/poolstate"
)

type fioIO struct {
	Bytes     *int64   `json:"io_bytes"`
	Bandwidth *int64   `json:"bw_bytes"`
	IOPS      *float64 `json:"iops"`
	Runtime   *int64   `json:"runtime"`
	Latency   struct {
		Mean        *float64           `json:"mean"`
		Percentiles map[string]float64 `json:"percentile"`
	} `json:"lat_ns"`
}

// fio's lat_ns is total latency, including submission and completion. Explicit
// lat_percentiles=1 ensures both percentiles have the same meaning as the mean.
// fio 3.33 stat.c:add_ddir_lat_json emits percentile keys with "%f", including
// six fractional digits: https://github.com/axboe/fio/blob/fio-3.33/stat.c
func parseFIO(data []byte, workload benchmarkWorkload) (storagev1.DiskBenchmarkWorkload, error) {
	out, err := parseFIOSample(data, workload)
	if err != nil {
		return out, err
	}
	if out.Errors != 0 || out.DurationSeconds < poolstate.DurationSeconds {
		return out, errors.New("fio workload failed or ended before its full duration")
	}
	return out, nil
}

// A failed fio command can still produce a valid JSON sample. Preserve those
// measured values and error counts, but never treat them as a completed test.
// Missing metrics are omitted by rejecting the sample, not fabricated as zero.
func parseFIOSample(data []byte, workload benchmarkWorkload) (storagev1.DiskBenchmarkWorkload, error) {
	var out storagev1.DiskBenchmarkWorkload
	invalid := errors.New("fio metrics are incomplete or invalid")
	if len(data) > 1<<20 {
		return out, invalid
	}
	var report struct {
		Jobs []struct {
			Name        string `json:"jobname"`
			Error       *int64 `json:"error"`
			TotalErrors int64  `json:"total_err"`
			Read        fioIO  `json:"read"`
			Write       fioIO  `json:"write"`
		} `json:"jobs"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&report); err != nil || len(report.Jobs) != 1 {
		return out, invalid
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return out, invalid
	}
	job := report.Jobs[0]
	if job.Name != workload.name || job.Error == nil || *job.Error < 0 || job.TotalErrors < 0 {
		return out, invalid
	}
	metric := job.Read
	if workload.rw == "write" || workload.rw == "randwrite" {
		metric = job.Write
	}
	p95, ok95 := metric.Latency.Percentiles["95.000000"]
	p99, ok99 := metric.Latency.Percentiles["99.000000"]
	if metric.Bytes == nil || metric.Bandwidth == nil || metric.IOPS == nil || metric.Runtime == nil || metric.Latency.Mean == nil || !ok95 || !ok99 {
		return out, invalid
	}
	positive := func(n float64) bool { return n > 0 && !math.IsNaN(n) && !math.IsInf(n, 0) }
	if *metric.Bytes <= 0 || *metric.Bandwidth <= 0 || *metric.Runtime <= 0 || !positive(*metric.IOPS) || !positive(*metric.Latency.Mean) || !positive(p95) || !positive(p99) || p99 < p95 {
		return out, invalid
	}
	errorCount := job.TotalErrors
	if *job.Error != 0 && errorCount == 0 {
		errorCount = 1
	}
	return storagev1.DiskBenchmarkWorkload{Name: workload.name, BlockSizeBytes: workload.block, QueueDepth: poolstate.QueueDepth, DurationSeconds: float64(*metric.Runtime) / 1000, Bytes: *metric.Bytes, BandwidthBytesPerSecond: *metric.Bandwidth, IOPS: *metric.IOPS, LatencyMeanNS: *metric.Latency.Mean, LatencyP95NS: p95, LatencyP99NS: p99, Errors: errorCount}, nil
}
