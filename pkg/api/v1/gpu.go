package v1

import (
	"time"
)

// NodeStatus indicates the operating state of a worker node.
type NodeStatus string

const (
	NodeOnline      NodeStatus = "ONLINE"
	NodeOffline     NodeStatus = "OFFLINE"
	NodeMaintenance NodeStatus = "MAINTENANCE"
)

// GPUHealth indicates the device health reported by NVML.
type GPUHealth string

const (
	HealthHealthy   GPUHealth = "HEALTHY"
	HealthDegraded  GPUHealth = "DEGRADED"
	HealthUnhealthy GPUHealth = "UNHEALTHY"
)

// ActiveAllocation tracks a workload slice currently consuming VRAM on a GPU.
type ActiveAllocation struct {
	JobID           string    `json:"job_id" bson:"job_id"`
	JobName         string    `json:"job_name" bson:"job_name"`
	AllocatedVRAMMiB int       `json:"allocated_vram_mib" bson:"allocated_vram_mib"`
	ContainerID     string    `json:"container_id,omitempty" bson:"container_id,omitempty"`
	AllocatedAt     time.Time `json:"allocated_at" bson:"allocated_at"`
}

// GPUDevice represents a physical GPU attached to a node.
type GPUDevice struct {
	GPUID              string              `json:"gpu_id" bson:"gpu_id"`
	Index              int                 `json:"index" bson:"index"`
	Name               string              `json:"name" bson:"name"`
	PCIBusID           string              `json:"pci_bus_id,omitempty" bson:"pci_bus_id,omitempty"`
	DriverVersion      string              `json:"driver_version,omitempty" bson:"driver_version,omitempty"`
	CUDAVersion        string              `json:"cuda_version,omitempty" bson:"cuda_version,omitempty"`
	ComputeCapability  string              `json:"compute_capability,omitempty" bson:"compute_capability,omitempty"`
	TotalVRAMMiB       int                 `json:"total_vram_mib" bson:"total_vram_mib"`
	AllocatedVRAMMiB   int                 `json:"allocated_vram_mib" bson:"allocated_vram_mib"`
	FreeVRAMMiB        int                 `json:"free_vram_mib" bson:"free_vram_mib"`
	HealthStatus       GPUHealth           `json:"health_status" bson:"health_status"`
	Telemetry          *GPUTelemetrySample `json:"telemetry,omitempty" bson:"telemetry,omitempty"`
	ActiveAllocations  []ActiveAllocation  `json:"active_allocations" bson:"active_allocations"`
}

// GPUNode represents a host machine / Kubernetes worker.
type GPUNode struct {
	NodeID         string            `json:"node_id" bson:"_id"`
	Hostname       string            `json:"hostname" bson:"hostname"`
	Status         NodeStatus        `json:"status" bson:"status"`
	IPAddress      string            `json:"ip_address,omitempty" bson:"ip_address,omitempty"`
	KubernetesNode string            `json:"kubernetes_node" bson:"kubernetes_node"`
	Labels         map[string]string `json:"labels,omitempty" bson:"labels,omitempty"`
	GPUs           []GPUDevice       `json:"gpus" bson:"gpus"`
}

// GPUInventoryResponse aggregates the entire cluster's GPU state.
type GPUInventoryResponse struct {
	TotalNodes       int       `json:"total_nodes"`
	TotalGPUs        int       `json:"total_gpus"`
	AggregateVRAMMiB int       `json:"aggregate_vram_mib"`
	AllocatedVRAMMiB int       `json:"allocated_vram_mib"`
	FreeVRAMMiB      int       `json:"free_vram_mib"`
	Nodes            []GPUNode `json:"nodes"`
}

