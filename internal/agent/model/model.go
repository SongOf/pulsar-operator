// Package model defines the node-agent's device and storage backend contracts.
package model

import storagev1 "github.com/SongOf/pulsar-operator/api/storage/v1alpha1"

// Device is the immutable identity approved in DiskPool.spec.devices.
type Device = storagev1.Device

// Disk is an observation of a whole physical disk, never authorization to write it.
type Disk struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	DevicePath string   `json:"devicePath"`
	Serial     string   `json:"serial"`
	WWN        string   `json:"wwn"`
	SizeBytes  int64    `json:"sizeBytes"`
	Model      string   `json:"model"`
	Media      string   `json:"media"`
	State      string   `json:"state"`
	Reasons    []string `json:"reasons"`
	PoolID     *string  `json:"poolId"`
}

type PoolResult struct {
	VGName         string `json:"vgName"`
	CapacityBytes  int64  `json:"capacityBytes"`
	AvailableBytes int64  `json:"availableBytes"`
}
