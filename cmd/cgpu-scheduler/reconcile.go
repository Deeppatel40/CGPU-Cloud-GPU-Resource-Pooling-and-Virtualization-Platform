package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os/exec"
	"time"

	v1 "github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/api/v1"
	"github.com/Deeppatel40/CGPU-Cloud-GPU-Resource-Pooling-and-Virtualization-Platform/pkg/scheduler"
)

// The reconciler keeps the scheduler's view in line with the cluster:
//
//   - node readiness: a laptop that sleeps or closes WSL shows NotReady in Kubernetes; the
//     scheduler marks it OFFLINE so nothing new is placed there, and ONLINE again when it
//     returns (which also dispatches queued jobs that now fit).
//   - orphaned reservations: a reservation whose job has no Pending/Running pod (finished,
//     crashed, deleted, or never created) is released after a grace period, so VRAM does not
//     leak when the API misses a /release call.
//
// It shells out to kubectl rather than linking client-go: the control plane already has a
// working kubectl, and this keeps the scheduler a dependency-free binary. If kubectl fails
// the pass is skipped entirely - an API outage must never be read as "no pods" and wipe
// every reservation.

type reconciler struct {
	engine   *scheduler.PlacementEngine
	kubectl  string
	interval time.Duration
	grace    time.Duration
}

type podList struct {
	Items []struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	} `json:"items"`
}

type nodeList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			// Addresses carries the node's current InternalIP. The GPU workers are
			// laptops on DHCP Wi-Fi, so the address the scheduler was started with
			// goes stale on every rejoin - and a stale address is not cosmetic: the
			// API probes <ip>:9400 for telemetry, so the portal reports
			// AGENT_UNREACHABLE and every measured figure renders "unavailable"
			// while the worker is in fact healthy and reporting.
			Addresses []struct {
				Type    string `json:"type"`
				Address string `json:"address"`
			} `json:"addresses"`
		} `json:"status"`
	} `json:"items"`
}

func (r *reconciler) run(ctx context.Context) {
	r.once(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.once(ctx)
		}
	}
}

func (r *reconciler) once(ctx context.Context) {
	if err := r.syncNodes(ctx); err != nil {
		log.Printf("[CGPU-Reconciler] node sync skipped: %v", err)
	}
	if err := r.syncPods(ctx); err != nil {
		log.Printf("[CGPU-Reconciler] pod sync skipped: %v", err)
	}
}

func (r *reconciler) syncNodes(ctx context.Context) error {
	var nl nodeList
	if err := r.kubectlJSON(ctx, &nl, "get", "nodes", "-o", "json"); err != nil {
		return err
	}
	ready := make(map[string]bool, len(nl.Items))
	addr := make(map[string]string, len(nl.Items))
	for _, n := range nl.Items {
		for _, c := range n.Status.Conditions {
			if c.Type == "Ready" {
				ready[n.Metadata.Name] = c.Status == "True"
			}
		}
		for _, a := range n.Status.Addresses {
			if a.Type == "InternalIP" && a.Address != "" {
				addr[n.Metadata.Name] = a.Address
			}
		}
	}
	for _, node := range r.engine.GetNodes() {
		if node.Status == v1.NodeMaintenance {
			continue
		}
		// Adopt the address Kubernetes reports. Kubernetes learns it from the
		// kubelet itself, so it is correct by construction where the value the
		// scheduler was bootstrapped with is only correct until a laptop rejoins
		// on a different one.
		if ip := addr[node.KubernetesNode]; ip != "" && ip != node.IPAddress {
			log.Printf("[CGPU-Reconciler] node %s address %s -> %s", node.NodeID, node.IPAddress, ip)
			updated := node
			updated.IPAddress = ip
			r.engine.RegisterOrUpdateNode(&updated)
		}
		want := v1.NodeOffline
		if ready[node.KubernetesNode] {
			want = v1.NodeOnline
		}
		if changed, dispatched := r.engine.SetNodeStatus(node.NodeID, want); changed {
			log.Printf("[CGPU-Reconciler] node %s -> %s", node.NodeID, want)
			logDispatched(dispatched)
		}
	}
	return nil
}

func (r *reconciler) syncPods(ctx context.Context) error {
	var pl podList
	if err := r.kubectlJSON(ctx, &pl, "get", "pods", "--all-namespaces", "-l", "cgpu.io/job-id", "-o", "json"); err != nil {
		return err
	}
	live := make(map[string]bool)
	for _, p := range pl.Items {
		id := p.Metadata.Labels["cgpu.io/job-id"]
		if id == "" {
			continue
		}
		switch p.Status.Phase {
		case "Pending", "Running", "Unknown":
			// Unknown = node unreachable; the job may still be running on the GPU.
			live[id] = true
		}
	}
	released, dispatched := r.engine.ReconcileOrphans(live, r.grace, time.Now().UTC())
	for _, id := range released {
		log.Printf("[CGPU-Reconciler] released orphaned reservation for job %s (no live pod)", id)
	}
	logDispatched(dispatched)
	return nil
}

func (r *reconciler) kubectlJSON(ctx context.Context, into any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.kubectl, args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return fmt.Errorf("kubectl %v: %v: %s", args, err, ee.Stderr)
		}
		return fmt.Errorf("kubectl %v: %w", args, err)
	}
	return json.Unmarshal(out, into)
}

func logDispatched(ds []*v1.PlacementDecision) {
	for _, d := range ds {
		log.Printf("[CGPU-Scheduler] dispatched queued job %s -> %s (%s)", d.JobID, d.AssignedNode, d.AssignedGPUID)
	}
}
