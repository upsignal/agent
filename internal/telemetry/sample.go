// Package telemetry defines the payload shapes the agent sends to the backend.
//
// This is the proposed DTO from the spec (section 6), not a published API contract yet.
// Missing metrics are encoded as JSON null, never as zero. An empty list means "collected, nothing to report";
// null means "collection failed".
package telemetry

import "time"

type Host struct {
	Hostname     *string `json:"hostname"`
	OS           *string `json:"os"`
	OSName       *string `json:"osName"`
	OSVersion    *string `json:"osVersion"`
	Kernel       *string `json:"kernel"`
	Architecture string  `json:"architecture"`
	LogicalCPUs  *int    `json:"logicalCpus"`
}

type Sample struct {
	ObservedAt time.Time `json:"observedAt"`
	// BootID changes on every reboot, so the backend can detect resets of cumulative counters.
	BootID            *string            `json:"bootId"`
	CPU               *CPU               `json:"cpu"`
	Memory            *Memory            `json:"memory"`
	Load              *Load              `json:"load"`
	UptimeSeconds     *int64             `json:"uptimeSeconds"`
	Filesystems       []Filesystem       `json:"filesystems"`
	NetworkInterfaces []NetworkInterface `json:"networkInterfaces"`
}

// CPU percentages are 0–100 over the interval since the previous sample.
type CPU struct {
	UsagePercent  float64 `json:"usagePercent"`
	UserPercent   float64 `json:"userPercent"`
	SystemPercent float64 `json:"systemPercent"`
	IOWaitPercent float64 `json:"iowaitPercent"`
	// StealPercent is time the hypervisor gave this VM's vCPUs to someone else.
	StealPercent float64 `json:"stealPercent"`
}

type Memory struct {
	TotalBytes      uint64   `json:"totalBytes"`
	AvailableBytes  uint64   `json:"availableBytes"`
	UsedBytes       uint64   `json:"usedBytes"`
	UsedPercent     float64  `json:"usedPercent"`
	SwapTotalBytes  uint64   `json:"swapTotalBytes"`
	SwapFreeBytes   uint64   `json:"swapFreeBytes"`
	SwapUsedBytes   uint64   `json:"swapUsedBytes"`
	SwapUsedPercent *float64 `json:"swapUsedPercent"` // null when there is no swap
}

type Load struct {
	OneMinute      float64 `json:"oneMinute"`
	FiveMinutes    float64 `json:"fiveMinutes"`
	FifteenMinutes float64 `json:"fifteenMinutes"`
}

type Filesystem struct {
	MountPoint     string   `json:"mountPoint"`
	Device         *string  `json:"device"`
	Type           string   `json:"type"`
	TotalBytes     uint64   `json:"totalBytes"`
	AvailableBytes uint64   `json:"availableBytes"`
	UsedBytes      uint64   `json:"usedBytes"`
	UsedPercent    *float64 `json:"usedPercent"`
	// Inode fields are null on filesystems that do not report inode counts (e.g. btrfs).
	InodesTotal       *uint64  `json:"inodesTotal"`
	InodesAvailable   *uint64  `json:"inodesAvailable"`
	InodesUsedPercent *float64 `json:"inodesUsedPercent"`
}

// NetworkInterface counters are cumulative since boot; the backend derives rates.
type NetworkInterface struct {
	Name      string `json:"name"`
	RxBytes   uint64 `json:"rxBytes"`
	TxBytes   uint64 `json:"txBytes"`
	RxPackets uint64 `json:"rxPackets"`
	TxPackets uint64 `json:"txPackets"`
	RxErrors  uint64 `json:"rxErrors"`
	TxErrors  uint64 `json:"txErrors"`
	RxDropped uint64 `json:"rxDropped"`
	TxDropped uint64 `json:"txDropped"`
}

type CollectorError struct {
	Collector string `json:"collector"`
	Message   string `json:"message"`
}
