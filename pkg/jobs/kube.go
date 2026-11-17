package jobs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/template"
	"time"
)

// KubeState is what Kubernetes currently says about one CGPU job: its batch/v1 Job and the
// newest Pod it created. Both are nil when nothing exists.
type KubeState struct {
	JobExists     bool
	JobFailed     bool   // Job condition Failed=True (e.g. DeadlineExceeded, BackoffLimitExceeded)
	JobFailReason string // condition reason/message
	Pod           *PodInfo
}

// PodInfo is the subset of Pod status the lifecycle needs.
type PodInfo struct {
	Name           string
	Phase          string // Pending | Running | Succeeded | Failed | Unknown
	Node           string
	StartedAt      *time.Time
	WaitingReason  string
	WaitingMessage string
	Terminated     *Termination
	CreatedAt      time.Time
}

// Termination is the workload container's final state.
type Termination struct {
	ExitCode   int
	Reason     string
	Message    string
	FinishedAt time.Time
}

// Kube is the Kubernetes surface the job manager uses. The kubectl implementation runs
// with a kubeconfig for the cgpu-api ServiceAccount, whose Role only allows these calls
// in the jobs namespace (see deploy/k8s/cgpu-api-rbac.yaml).
type Kube interface {
	Create(ctx context.Context, manifest []byte) error
	State(ctx context.Context, jobID string) (*KubeState, error)
	Logs(ctx context.Context, podName string, tail int) ([]LogLine, error)
	StreamLogs(ctx context.Context, podName string, sinceTail int, emit func(LogLine) error) error
	Delete(ctx context.Context, jobID string) error
}

// KubeName is the name of the Kubernetes Job and Secret for a CGPU job ID.
func KubeName(jobID string) string { return "cgpu-" + jobID }

// Kubectl implements Kube by running kubectl.
type Kubectl struct {
	Binary     string
	Kubeconfig string
	Namespace  string
}

func (k *Kubectl) cmd(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{"--namespace", k.Namespace}, args...)
	c := exec.CommandContext(ctx, k.Binary, full...)
	c.Env = os.Environ()
	if k.Kubeconfig != "" {
		c.Env = append(c.Env, "KUBECONFIG="+k.Kubeconfig)
	}
	return c
}

func (k *Kubectl) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c := k.cmd(ctx, args...)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Create uses `kubectl create` (not apply) so the ServiceAccount needs only create rights.
func (k *Kubectl) Create(ctx context.Context, manifest []byte) error {
	_, err := k.run(ctx, manifest, "create", "-f", "-")
	return err
}

type k8sList struct {
	Items []struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name              string    `json:"name"`
			CreationTimestamp time.Time `json:"creationTimestamp"`
		} `json:"metadata"`
		Spec struct {
			NodeName string `json:"nodeName"`
		} `json:"spec"`
		Status struct {
			Phase      string `json:"phase"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
			ContainerStatuses []struct {
				Name  string `json:"name"`
				State struct {
					Waiting *struct {
						Reason  string `json:"reason"`
						Message string `json:"message"`
					} `json:"waiting"`
					Running *struct {
						StartedAt time.Time `json:"startedAt"`
					} `json:"running"`
					Terminated *struct {
						ExitCode   int       `json:"exitCode"`
						Reason     string    `json:"reason"`
						Message    string    `json:"message"`
						StartedAt  time.Time `json:"startedAt"`
						FinishedAt time.Time `json:"finishedAt"`
					} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

func (k *Kubectl) State(ctx context.Context, jobID string) (*KubeState, error) {
	out, err := k.run(ctx, nil, "get", "jobs,pods", "-l", "cgpu.io/job-id="+jobID, "-o", "json")
	if err != nil {
		return nil, err
	}
	return parseKubeState(out)
}

func parseKubeState(out []byte) (*KubeState, error) {
	var l k8sList
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, fmt.Errorf("parse kubectl output: %w", err)
	}
	st := &KubeState{}
	var pods []*PodInfo
	for _, it := range l.Items {
		switch it.Kind {
		case "Job":
			st.JobExists = true
			for _, c := range it.Status.Conditions {
				if c.Type == "Failed" && c.Status == "True" {
					st.JobFailed = true
					st.JobFailReason = strings.TrimSpace(c.Reason + ": " + c.Message)
				}
			}
		case "Pod":
			p := &PodInfo{Name: it.Metadata.Name, Phase: it.Status.Phase, Node: it.Spec.NodeName, CreatedAt: it.Metadata.CreationTimestamp}
			for _, cs := range it.Status.ContainerStatuses {
				if cs.Name != "workload" {
					continue
				}
				if w := cs.State.Waiting; w != nil {
					p.WaitingReason, p.WaitingMessage = w.Reason, w.Message
				}
				if r := cs.State.Running; r != nil {
					t := r.StartedAt
					p.StartedAt = &t
				}
				if t := cs.State.Terminated; t != nil {
					s := t.StartedAt
					p.StartedAt = &s
					p.Terminated = &Termination{ExitCode: t.ExitCode, Reason: t.Reason, Message: t.Message, FinishedAt: t.FinishedAt}
				}
			}
			pods = append(pods, p)
		}
	}
	if len(pods) > 0 {
		sort.Slice(pods, func(i, j int) bool { return pods[i].CreatedAt.After(pods[j].CreatedAt) })
		st.Pod = pods[0]
	}
	return st, nil
}

// parseLogLine splits a `kubectl logs --timestamps` line ("<RFC3339Nano> <message>").
func parseLogLine(s string) LogLine {
	if i := strings.IndexByte(s, ' '); i > 0 {
		if t, err := time.Parse(time.RFC3339Nano, s[:i]); err == nil {
			return LogLine{Timestamp: t.UTC(), Stream: "stdout", Message: s[i+1:]}
		}
	}
	return LogLine{Timestamp: time.Now().UTC(), Stream: "stdout", Message: s}
}

func (k *Kubectl) Logs(ctx context.Context, pod string, tail int) ([]LogLine, error) {
	args := []string{"logs", pod, "-c", "workload", "--timestamps"}
	if tail > 0 {
		args = append(args, fmt.Sprintf("--tail=%d", tail))
	}
	out, err := k.run(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	var lines []LogLine
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		lines = append(lines, parseLogLine(sc.Text()))
	}
	return lines, sc.Err()
}

// StreamLogs follows the pod's log until the container exits or ctx is cancelled.
func (k *Kubectl) StreamLogs(ctx context.Context, pod string, tail int, emit func(LogLine) error) error {
	c := k.cmd(ctx, "logs", pod, "-c", "workload", "--timestamps", "--follow", fmt.Sprintf("--tail=%d", tail))
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if err := emit(parseLogLine(sc.Text())); err != nil {
			_ = c.Process.Kill()
			_ = c.Wait()
			return err
		}
	}
	return c.Wait()
}

// Delete removes the Job (and its pods) and the per-job Secret, by name only, so the
// ServiceAccount needs delete but not list rights on Secrets.
func (k *Kubectl) Delete(ctx context.Context, jobID string) error {
	name := KubeName(jobID)
	_, err1 := k.run(ctx, nil, "delete", "job", name, "--ignore-not-found", "--wait=false", "--cascade=background")
	_, err2 := k.run(ctx, nil, "delete", "secret", name, "--ignore-not-found", "--wait=false")
	if err1 != nil {
		return err1
	}
	return err2
}

// TemplateData is everything deploy/templates/cgpu-workload-template.yaml can reference.
type TemplateData struct {
	JobID            string
	Name             string // Kubernetes object name for the Job and Secret
	Namespace        string
	WorkloadID       string
	Image            string
	CommandShell     string // POSIX-shell-quoted workload command
	AllocatedVRAMMiB int
	AssignedGPUID    string
	AssignedNode     string
	TimeoutSeconds   int
	APIURL           string
	ParamsJSON       string
	UploadToken      string
	GPU              bool
}

// LoadJobTemplate parses the shared Job template (a Go text/template producing YAML).
func LoadJobTemplate(path string) (*template.Template, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return template.New("job").Funcs(template.FuncMap{"quote": yamlQuote}).Option("missingkey=error").Parse(string(b))
}

// yamlQuote renders s as a double-quoted YAML scalar (JSON strings are valid YAML).
func yamlQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// shellQuote joins argv into a POSIX shell command line.
func shellQuote(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

// RenderJob executes the template and returns the manifest.
func RenderJob(t *template.Template, d TemplateData) ([]byte, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
