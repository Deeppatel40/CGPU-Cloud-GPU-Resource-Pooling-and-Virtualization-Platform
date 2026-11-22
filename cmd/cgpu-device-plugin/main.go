// Command cgpu-device-plugin advertises a GPU's memory as countable units so that plain
// Kubernetes can schedule fractions of a card.
//
// Owner / engineer: Deep Patel (@Deeppatel40)
//
// # The problem it solves
//
// Kubernetes' GPU model is all-or-nothing: `nvidia.com/gpu: 1` hands a pod the whole card
// and everything else queues. That is the right model for training on a datacenter GPU and
// the wrong one for this cluster, where a 6 GiB laptop card can comfortably hold several
// small jobs and typically sits at 20-40% utilisation.
//
// The Device Plugin API is the supported way to advertise custom devices, but it has a hard
// limitation: ListAndWatch reports *device counts* - integers - and nothing else. There is
// no field for "this card has 6141 MiB". That single constraint is why fractional-GPU
// systems usually end up with three moving parts: a device plugin to advertise inflated
// slot counts, a scheduler extender to decide which card can fit a VRAM request, and a
// mutating webhook plus pod annotations to carry the VRAM figure between them, because the
// plugin's Allocate call is told which device IDs were granted but never how much memory
// the pod asked for.
//
// # The approach here
//
// Rather than route VRAM around the integer-only interface, make VRAM *be* the integer:
// advertise memory in fixed-size units, one device per unit.
//
//	resources:
//	  limits:
//	    cgpu.dev/vram-512mib: 4      # 2 GiB
//
// Allocate is then handed four device IDs, which says exactly what the pod was granted -
// so the plugin can inject the quota itself. No annotations, no webhook, and no scheduler
// extender: kube-scheduler already understands integer resources, and its own binpack and
// spread scoring applies unchanged.
//
// The cost is granularity - a request is rounded up to the unit size - and that units on
// one node are not interchangeable with another's. Both are acceptable for memory, which is
// what makes the trade work: VRAM is a quantity that divides cleanly, unlike a device.
//
// # What a pod gets
//
// On Allocate the plugin injects the allocation guard and its limits:
//
//	LD_PRELOAD=/opt/cgpu/libcgpu_guard.so
//	CGPU_ALLOCATED_VRAM_MIB=<units * unit size>
//	CGPU_ALLOCATED_SM_PERCENT=<share of the card's memory, by default>
//	CGPU_JOB_ID=<pod uid, when the kubelet provides it>
//
// and mounts /etc/ld.so.preload so the guard loads for every process in the container, not
// only those that inherit the environment. A workload can still be run unguarded by asking
// for no units.
//
// Compute share defaults to the memory share on the grounds that a job asking for a quarter
// of the card's memory has implicitly asked for about a quarter of the card; -sm-share=0
// turns that off and leaves compute unmetered.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	api "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	kubeletSocket = "/var/lib/kubelet/device-plugins/kubelet.sock"
	pluginDir     = "/var/lib/kubelet/device-plugins"
)

var (
	flagUnitMiB = flag.Int("unit-mib", 512,
		"size of one advertised VRAM unit in MiB; requests are rounded up to a multiple of this")
	flagResource = flag.String("resource", "",
		"resource name to advertise (default cgpu.dev/vram-<unit>mib)")
	flagReserveMiB = flag.Int("reserve-mib", 512,
		"VRAM held back from the advertised total for driver/CUDA context overhead")
	flagGuardPath = flag.String("guard", "/opt/cgpu/libcgpu_guard.so",
		"host path of the allocation guard to inject into containers")
	flagPreloadFile = flag.String("preload-file", "/opt/cgpu/ld.so.preload",
		"host file mounted at /etc/ld.so.preload so the guard loads for every process")
	flagSMShare = flag.Bool("sm-share", true,
		"also meter compute, defaulting each pod's share to its share of the card's memory")
	flagSMI      = flag.String("nvidia-smi", "nvidia-smi", "path to nvidia-smi")
	flagNodeName = flag.String("node-name", "", "node name for logging; defaults to $NODE_NAME")
)

// gpu is one physical device as nvidia-smi reports it.
type gpu struct {
	Index    int    `json:"index"`
	UUID     string `json:"uuid"`
	Name     string `json:"name"`
	TotalMiB int    `json:"total_mib"`
}

// discover reads the node's GPUs. nvidia-smi rather than NVML so the binary stays pure Go
// and needs no cgo toolchain on a worker.
func discover(smi string) ([]gpu, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// A timeout because nvidia-smi can block indefinitely while the driver wakes a laptop
	// GPU, and a device plugin that never finishes starting is worse than one that retries.
	raw, err := exec.CommandContext(ctx, smi,
		"--query-gpu=index,uuid,name,memory.total", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", smi, err)
	}
	out := string(raw)
	var gpus []gpu
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 4 {
			continue
		}
		idx, _ := strconv.Atoi(strings.TrimSpace(f[0]))
		total, _ := strconv.Atoi(strings.TrimSpace(f[3]))
		gpus = append(gpus, gpu{
			Index:    idx,
			UUID:     strings.TrimSpace(f[1]),
			Name:     strings.TrimSpace(f[2]),
			TotalMiB: total,
		})
	}
	if len(gpus) == 0 {
		return nil, fmt.Errorf("%s reported no GPUs", smi)
	}
	return gpus, nil
}

// plugin implements the kubelet DevicePlugin service.
type plugin struct {
	resource string
	unitMiB  int
	gpus     []gpu
	devices  []*api.Device // one per advertised unit
	socket   string
	server   *grpc.Server
	stop     chan struct{}
}

func newPlugin(gpus []gpu, resource string, unitMiB, reserveMiB int) *plugin {
	p := &plugin{
		resource: resource,
		unitMiB:  unitMiB,
		gpus:     gpus,
		socket:   filepath.Join(pluginDir, "cgpu-vram.sock"),
		stop:     make(chan struct{}),
	}
	for _, g := range gpus {
		// Hold back the context reserve before dividing: CUDA contexts cost device memory
		// that no pod's quota accounts for, and advertising the full card lets the
		// scheduler overcommit it so a pod fails at startup instead of staying pending.
		usable := g.TotalMiB - reserveMiB
		if usable < 0 {
			usable = 0
		}
		units := usable / unitMiB
		for i := 0; i < units; i++ {
			// The device ID carries the GPU UUID so Allocate can tell which physical card
			// a unit belongs to - needed as soon as a node has more than one.
			p.devices = append(p.devices, &api.Device{
				ID:     fmt.Sprintf("%s-u%03d", g.UUID, i),
				Health: api.Healthy,
			})
		}
		log.Printf("[cgpu-device-plugin] %s (%s): %d MiB total, %d reserved, advertising %d x %d MiB units",
			g.Name, g.UUID, g.TotalMiB, reserveMiB, units, unitMiB)
	}
	return p
}

func (p *plugin) GetDevicePluginOptions(context.Context, *api.Empty) (*api.DevicePluginOptions, error) {
	return &api.DevicePluginOptions{}, nil
}

// ListAndWatch reports the unit list and then holds the stream open. Units are static
// because the card's memory is; health changes would be reported here.
func (p *plugin) ListAndWatch(_ *api.Empty, s api.DevicePlugin_ListAndWatchServer) error {
	if err := s.Send(&api.ListAndWatchResponse{Devices: p.devices}); err != nil {
		return err
	}
	<-p.stop
	return nil
}

// Allocate is where the quota is derived: the number of unit IDs the kubelet grants is the
// pod's memory allowance, so nothing has to be communicated out of band.
func (p *plugin) Allocate(_ context.Context, req *api.AllocateRequest) (*api.AllocateResponse, error) {
	resp := &api.AllocateResponse{}
	for _, cr := range req.ContainerRequests {
		units := len(cr.DevicesIDs)
		quotaMiB := units * p.unitMiB

		env := map[string]string{
			// The whole card is visible: isolation is enforced by the guard, not by hiding
			// devices. Hiding them would stop a pod seeing VRAM it may not use, but it
			// would also stop it using the card at all.
			"NVIDIA_VISIBLE_DEVICES":     "all",
			"NVIDIA_DRIVER_CAPABILITIES": "compute,utility",
			"LD_PRELOAD":                 *flagGuardPath,
			"CGPU_ALLOCATED_VRAM_MIB":    strconv.Itoa(quotaMiB),
		}

		if *flagSMShare && len(p.gpus) > 0 {
			// Share of the card's memory, as a percentage, floored at 1 so a tiny request
			// is throttled rather than silently unmetered.
			share := quotaMiB * 100 / p.gpus[0].TotalMiB
			if share < 1 {
				share = 1
			}
			if share > 100 {
				share = 100
			}
			env["CGPU_ALLOCATED_SM_PERCENT"] = strconv.Itoa(share)
		}

		mounts := []*api.Mount{
			{ContainerPath: *flagGuardPath, HostPath: *flagGuardPath, ReadOnly: true},
		}
		// /etc/ld.so.preload makes the guard load for every process in the container, so a
		// workload that spawns children or clears its environment cannot step outside the
		// quota. LD_PRELOAD above covers the common case; this covers the rest.
		if _, err := os.Stat(*flagPreloadFile); err == nil {
			mounts = append(mounts, &api.Mount{
				ContainerPath: "/etc/ld.so.preload",
				HostPath:      *flagPreloadFile,
				ReadOnly:      true,
			})
		}

		log.Printf("[cgpu-device-plugin] allocate: %d units -> %d MiB%s",
			units, quotaMiB, map[bool]string{true: ", sm " + env["CGPU_ALLOCATED_SM_PERCENT"] + "%", false: ""}[*flagSMShare])

		resp.ContainerResponses = append(resp.ContainerResponses, &api.ContainerAllocateResponse{
			Envs:   env,
			Mounts: mounts,
		})
	}
	return resp, nil
}

func (p *plugin) PreStartContainer(context.Context, *api.PreStartContainerRequest) (*api.PreStartContainerResponse, error) {
	return &api.PreStartContainerResponse{}, nil
}

func (p *plugin) GetPreferredAllocation(context.Context, *api.PreferredAllocationRequest) (*api.PreferredAllocationResponse, error) {
	// Units are interchangeable within a card, so there is nothing to prefer. Returning an
	// empty response leaves the choice to the kubelet.
	return &api.PreferredAllocationResponse{}, nil
}

func (p *plugin) serve() error {
	if err := os.Remove(p.socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	l, err := net.Listen("unix", p.socket)
	if err != nil {
		return err
	}
	p.server = grpc.NewServer()
	api.RegisterDevicePluginServer(p.server, p)
	go func() {
		if err := p.server.Serve(l); err != nil {
			log.Printf("[cgpu-device-plugin] grpc server stopped: %v", err)
		}
	}()

	// Wait for the socket to accept a connection before registering, or the kubelet's
	// immediate ListAndWatch can race the server being ready.
	conn, err := grpc.NewClient("unix://"+p.socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

// register tells the kubelet the socket and resource name to use.
func (p *plugin) register() error {
	conn, err := grpc.NewClient("unix://"+kubeletSocket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial kubelet: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = api.NewRegistrationClient(conn).Register(ctx, &api.RegisterRequest{
		Version:      api.Version,
		Endpoint:     filepath.Base(p.socket),
		ResourceName: p.resource,
		Options:      &api.DevicePluginOptions{},
	})
	if err != nil {
		return fmt.Errorf("register %s: %w", p.resource, err)
	}
	return nil
}

// writeSpecFile records what this node advertises, for the scheduler and for debugging.
// The Device Plugin API cannot carry device attributes, so anything beyond a count has to
// travel some other way; a file on the node is the least surprising of the options and
// needs no API-server credentials on a worker.
func (p *plugin) writeSpecFile(path string, reserveMiB int) {
	type spec struct {
		Node       string `json:"node"`
		Resource   string `json:"resource"`
		UnitMiB    int    `json:"unit_mib"`
		ReserveMiB int    `json:"reserve_mib"`
		Units      int    `json:"units"`
		GPUs       []gpu  `json:"gpus"`
		UpdatedAt  string `json:"updated_at"`
	}
	s := spec{
		Node:       nodeName(),
		Resource:   p.resource,
		UnitMiB:    p.unitMiB,
		ReserveMiB: reserveMiB,
		Units:      len(p.devices),
		GPUs:       p.gpus,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		log.Printf("[cgpu-device-plugin] could not write %s: %v", path, err)
	}
}

func nodeName() string {
	if *flagNodeName != "" {
		return *flagNodeName
	}
	if n := os.Getenv("NODE_NAME"); n != "" {
		return n
	}
	h, _ := os.Hostname()
	return h
}

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.LUTC)

	resource := *flagResource
	if resource == "" {
		resource = fmt.Sprintf("cgpu.dev/vram-%dmib", *flagUnitMiB)
	}

	gpus, err := discover(*flagSMI)
	if err != nil {
		log.Fatalf("[cgpu-device-plugin] GPU discovery failed: %v", err)
	}

	p := newPlugin(gpus, resource, *flagUnitMiB, *flagReserveMiB)
	if len(p.devices) == 0 {
		log.Fatalf("[cgpu-device-plugin] nothing to advertise: %d MiB card with a %d MiB reserve leaves no %d MiB units",
			gpus[0].TotalMiB, *flagReserveMiB, *flagUnitMiB)
	}

	if err := p.serve(); err != nil {
		log.Fatalf("[cgpu-device-plugin] could not start the plugin socket: %v", err)
	}
	if err := p.register(); err != nil {
		log.Fatalf("[cgpu-device-plugin] %v", err)
	}
	log.Printf("[cgpu-device-plugin] registered %s with %d units of %d MiB on %s",
		resource, len(p.devices), *flagUnitMiB, nodeName())

	p.writeSpecFile("/var/lib/cgpu/device-plugin.json", *flagReserveMiB)

	// The kubelet deletes the plugin directory's sockets when it restarts; the convention
	// is for the plugin to notice and re-register rather than be restarted externally.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case s := <-sigs:
			log.Printf("[cgpu-device-plugin] %v, shutting down", s)
			close(p.stop)
			p.server.Stop()
			_ = os.Remove(p.socket)
			return
		case <-ticker.C:
			if _, err := os.Stat(kubeletSocket); err != nil {
				continue // kubelet is away; nothing useful to do but wait
			}
			if _, err := os.Stat(p.socket); err != nil {
				log.Printf("[cgpu-device-plugin] our socket disappeared (kubelet restart?), re-registering")
				close(p.stop)
				p.server.Stop()
				p.stop = make(chan struct{})
				if err := p.serve(); err != nil {
					log.Printf("[cgpu-device-plugin] re-serve failed: %v", err)
					continue
				}
				if err := p.register(); err != nil {
					log.Printf("[cgpu-device-plugin] re-register failed: %v", err)
				}
			}
		}
	}
}
