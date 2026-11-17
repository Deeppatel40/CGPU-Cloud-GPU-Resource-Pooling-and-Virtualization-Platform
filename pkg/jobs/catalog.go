package jobs

import (
	"fmt"
	"math"
	"sort"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
)

// WorkloadParameter describes one user-settable parameter of an approved workload.
type WorkloadParameter struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"` // integer | number | string | enum
	Default     interface{} `json:"default"`
	Min         *float64    `json:"min,omitempty"`
	Max         *float64    `json:"max,omitempty"`
	Options     []string    `json:"options,omitempty"`
	Description string      `json:"description"`
}

// Workload is an approved entry in the catalog. Only catalog workloads can be submitted,
// so users choose what runs but never supply an arbitrary image or command.
type Workload struct {
	WorkloadID     string              `json:"workload_id"`
	Name           string              `json:"name"`
	Description    string              `json:"description"`
	DefaultVRAMMiB int                 `json:"default_vram_mib"`
	MinVRAMMiB     int                 `json:"min_vram_mib"`
	MaxVRAMMiB     int                 `json:"max_vram_mib"`
	Parameters     []WorkloadParameter `json:"parameters"`

	Image   string   `json:"-"`
	Command []string `json:"-"`
	// GPU workloads get the nvidia runtime class and the CGPU allocation guard preloaded.
	GPU bool `json:"-"`
}

func f64(v float64) *float64 { return &v }

// DefaultCatalog returns the approved workloads. smoke adds a CPU-only pipeline check that
// exercises scheduling, the Kubernetes Job lifecycle, logs and artifact upload without a GPU.
func DefaultCatalog(smoke bool) map[string]*Workload {
	c := map[string]*Workload{
		"pytorch-resnet18-cifar10": {
			WorkloadID:     "pytorch-resnet18-cifar10",
			Name:           "ResNet-18 on CIFAR-10 (PyTorch)",
			Description:    "Trains a ResNet-18 image classifier on CIFAR-10 and saves a checkpoint every epoch.",
			DefaultVRAMMiB: 2048,
			MinVRAMMiB:     1024,
			MaxVRAMMiB:     5629, // largest GPU (RTX 4050, 6141 MiB) minus the 512 MiB driver reserve
			Parameters: []WorkloadParameter{
				{Name: "epochs", Type: "integer", Default: 3, Min: f64(1), Max: f64(50), Description: "Training epochs"},
				{Name: "batch_size", Type: "integer", Default: 64, Min: f64(8), Max: f64(512), Description: "Mini-batch size"},
				{Name: "learning_rate", Type: "number", Default: 0.001, Min: f64(0.000001), Max: f64(1), Description: "Optimizer learning rate"},
				{Name: "optimizer", Type: "enum", Default: "adam", Options: []string{"adam", "sgd"}, Description: "Optimizer"},
				{Name: "max_steps_per_epoch", Type: "integer", Default: 0, Min: f64(0), Max: f64(100000), Description: "Cap on training steps per epoch (0 = full epoch)"},
			},
			Image:   "cgpu/workloads/pytorch-resnet18-cifar10:1.0.0",
			Command: []string{"python", "/app/train.py"},
			GPU:     true,
		},
	}
	if smoke {
		c["cgpu-pipeline-smoke"] = &Workload{
			WorkloadID:     "cgpu-pipeline-smoke",
			Name:           "Pipeline smoke test (CPU only)",
			Description:    "Does not use a GPU. Prints progress, writes a checkpoint and metrics, and exits; used to verify scheduling, the Job lifecycle, logs and artifact download.",
			DefaultVRAMMiB: 256,
			MinVRAMMiB:     1,
			MaxVRAMMiB:     100000,
			Parameters: []WorkloadParameter{
				{Name: "steps", Type: "integer", Default: 5, Min: f64(1), Max: f64(600), Description: "Progress steps (1 per second)"},
				{Name: "exit_code", Type: "integer", Default: 0, Min: f64(0), Max: f64(255), Description: "Exit code to finish with (non-zero simulates a failure)"},
				{Name: "simulate_oom", Type: "enum", Default: "no", Options: []string{"no", "yes"}, Description: "Print a CUDA out-of-memory error and exit 1"},
			},
			Image:   "python:3.12-slim",
			Command: []string{"python", "/opt/cgpu-tools/smoke.py"},
			GPU:     false,
		}
	}
	return c
}

// SortedWorkloads returns the catalog ordered by ID.
func SortedWorkloads(c map[string]*Workload) []*Workload {
	out := make([]*Workload, 0, len(c))
	for _, w := range c {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].WorkloadID < out[j].WorkloadID })
	return out
}

// ResolveParameters validates user parameters against the workload schema and returns the
// full parameter set with defaults filled in. Unknown parameters are rejected so a typo
// does not silently run with defaults.
func (w *Workload) ResolveParameters(in map[string]interface{}) (map[string]interface{}, []v1.ValidationErrorDetail) {
	var errs []v1.ValidationErrorDetail
	known := map[string]bool{}
	out := map[string]interface{}{}
	for _, p := range w.Parameters {
		known[p.Name] = true
		val, ok := in[p.Name]
		if !ok || val == nil {
			out[p.Name] = p.Default
			continue
		}
		field := "parameters." + p.Name
		switch p.Type {
		case "integer", "number":
			n, isNum := toFloat(val)
			if !isNum {
				errs = append(errs, v1.ValidationErrorDetail{Field: field, RejectedValue: val, Message: "must be a number"})
				continue
			}
			if p.Type == "integer" && n != math.Trunc(n) {
				errs = append(errs, v1.ValidationErrorDetail{Field: field, RejectedValue: val, Message: "must be a whole number"})
				continue
			}
			if p.Min != nil && n < *p.Min || p.Max != nil && n > *p.Max {
				errs = append(errs, v1.ValidationErrorDetail{Field: field, RejectedValue: val,
					Message: fmt.Sprintf("must be between %v and %v", fmtNum(p.Min), fmtNum(p.Max))})
				continue
			}
			if p.Type == "integer" {
				out[p.Name] = int64(n)
			} else {
				out[p.Name] = n
			}
		case "enum":
			s, isStr := val.(string)
			valid := false
			for _, o := range p.Options {
				if isStr && s == o {
					valid = true
				}
			}
			if !valid {
				errs = append(errs, v1.ValidationErrorDetail{Field: field, RejectedValue: val, Message: fmt.Sprintf("must be one of %v", p.Options)})
				continue
			}
			out[p.Name] = s
		default:
			s, isStr := val.(string)
			if !isStr {
				errs = append(errs, v1.ValidationErrorDetail{Field: field, RejectedValue: val, Message: "must be a string"})
				continue
			}
			out[p.Name] = s
		}
	}
	for k, v := range in {
		if !known[k] {
			errs = append(errs, v1.ValidationErrorDetail{Field: "parameters." + k, RejectedValue: v, Message: "unknown parameter for workload " + w.WorkloadID})
		}
	}
	return out, errs
}

func toFloat(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func fmtNum(p *float64) string {
	if p == nil {
		return "any"
	}
	return fmt.Sprintf("%g", *p)
}
