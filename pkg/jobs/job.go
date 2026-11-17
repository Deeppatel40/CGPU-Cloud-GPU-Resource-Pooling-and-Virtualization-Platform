// Package jobs implements the CGPU job manager behind the public API (issue B2 / #12):
// validation and idempotent admission, MongoDB persistence, hand-off to the CGPU scheduler,
// the Kubernetes Job lifecycle, log retrieval and durable artifact storage.
package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
)

// Job is the MongoDB document for one submission. It holds everything the public
// JobStatusResponse shows plus the private bookkeeping the lifecycle needs.
type Job struct {
	ID               string                 `bson:"_id"`
	RequestID        string                 `bson:"request_id"`
	Name             string                 `bson:"name"`
	WorkloadID       string                 `bson:"workload_id"`
	Status           v1.JobState            `bson:"status"`
	SubStatus        string                 `bson:"sub_status,omitempty"`
	PendingReason    v1.PendingReason       `bson:"pending_reason,omitempty"`
	FailureReason    v1.FailureReason       `bson:"failure_reason,omitempty"`
	FailureDetails   *v1.FailureDetails     `bson:"failure_details,omitempty"`
	RequestedVRAMMiB int                    `bson:"requested_vram_mib"`
	Priority         int                    `bson:"priority"`
	TimeoutSeconds   int                    `bson:"timeout_seconds"`
	Parameters       map[string]interface{} `bson:"parameters,omitempty"`
	Dataset          *v1.DatasetReference   `bson:"dataset,omitempty"`
	Placement        *v1.PlacementDetails   `bson:"placement,omitempty"`
	ExecutionMetrics *v1.ExecutionMetrics   `bson:"execution_metrics,omitempty"`
	Artifacts        []v1.ArtifactMetadata  `bson:"artifacts"`
	Timestamps       v1.JobTimestamps       `bson:"timestamps"`

	// KubeJobName is set once the Kubernetes Job has been created.
	KubeJobName string `bson:"kube_job_name,omitempty"`
	// UploadToken authenticates the workload container when it uploads its outputs.
	UploadToken string `bson:"upload_token"`
	// ReservationReleased records that the scheduler's VRAM reservation was given back.
	ReservationReleased bool      `bson:"reservation_released"`
	LastError           string    `bson:"last_error,omitempty"`
	UpdatedAt           time.Time `bson:"updated_at"`
}

// Terminal reports whether the job has reached SUCCEEDED or FAILED.
func (j *Job) Terminal() bool {
	return j.Status == v1.StateSucceeded || j.Status == v1.StateFailed
}

// Response converts the document to the public API shape.
func (j *Job) Response() *v1.JobStatusResponse {
	r := &v1.JobStatusResponse{
		JobID:            j.ID,
		RequestID:        j.RequestID,
		Name:             j.Name,
		WorkloadID:       j.WorkloadID,
		Status:           j.Status,
		SubStatus:        j.SubStatus,
		FailureDetails:   j.FailureDetails,
		RequestedVRAMMiB: j.RequestedVRAMMiB,
		Priority:         j.Priority,
		Parameters:       j.Parameters,
		Dataset:          j.Dataset,
		Placement:        j.Placement,
		ExecutionMetrics: j.ExecutionMetrics,
		Artifacts:        j.Artifacts,
		Timestamps:       j.Timestamps,
	}
	if r.Artifacts == nil {
		r.Artifacts = []v1.ArtifactMetadata{}
	}
	if j.PendingReason != "" && j.Status == v1.StatePending {
		pr := j.PendingReason
		r.PendingReason = &pr
	}
	if j.FailureReason != "" {
		fr := j.FailureReason
		r.FailureReason = &fr
	}
	return r
}

// Summary converts the document to the history-list shape.
func (j *Job) Summary() v1.JobSummary {
	s := v1.JobSummary{
		JobID:            j.ID,
		Name:             j.Name,
		WorkloadID:       j.WorkloadID,
		Status:           j.Status,
		RequestedVRAMMiB: j.RequestedVRAMMiB,
		SubmittedAt:      j.Timestamps.SubmittedAt,
	}
	if j.Placement != nil {
		n := j.Placement.NodeID
		s.NodeID = &n
	}
	if j.Timestamps.StartedAt != nil {
		end := time.Now().UTC()
		if j.Timestamps.CompletedAt != nil {
			end = *j.Timestamps.CompletedAt
		}
		d := end.Sub(*j.Timestamps.StartedAt).Seconds()
		s.DurationSeconds = &d
	}
	return s
}

// LogLine is one line of workload output.
type LogLine struct {
	Timestamp time.Time `json:"timestamp"`
	Stream    string    `json:"stream"`
	Message   string    `json:"message"`
}

// LogsResponse is the JSON body of GET /jobs/{id}/logs.
type LogsResponse struct {
	JobID string    `json:"job_id"`
	Lines []LogLine `json:"lines"`
}

// newUUID returns a random RFC 4122 version-4 UUID.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func nowUTC() time.Time { return time.Now().UTC() }

func newToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b[:])
}
