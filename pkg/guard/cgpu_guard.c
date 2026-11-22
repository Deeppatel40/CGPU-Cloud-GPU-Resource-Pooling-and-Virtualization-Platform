/*
 * CGPU CUDA Allocation Guard
 * ==========================
 *
 * A CGPU-owned LD_PRELOAD interposer that enforces a per-workload VRAM quota.
 *
 * Why this exists: on the project's WSL GPU workers the NVIDIA driver does not report
 * per-process VRAM usage (`--query-compute-apps used_memory` returns [N/A], `pmon` is
 * unusable, accounting mode is Disabled - see docs/gpu-worker-4050.md). There is therefore
 * no external way to observe, let alone enforce, what a single job is using. The guard's own
 * tally is the source of truth, and device-level `nvidia-smi` is only a sanity check.
 *
 * It does three things:
 *   1. intercepts the CUDA allocation and free paths and refuses any allocation that would
 *      push the process past its quota, returning the usual out-of-memory error without
 *      calling the real allocator, so nothing is reserved on the card for a denied request;
 *   2. interposes dlsym, so a workload that looks driver entry points up by name still gets
 *      the guarded versions;
 *   3. reports the quota as the device's capacity, so frameworks size their memory pools to
 *      the quota instead of to the physical card.
 *
 * Deliberately NOT derived from HAMi or HAMi-Core: this is CGPU's own implementation, no
 * HAMi code, headers or resource names are used.
 *
 * No CUDA toolkit is required to build this. The handful of CUDA symbols and error codes are
 * declared locally, and the real implementations are resolved at runtime with dlsym().
 *
 * Configuration (environment):
 *   CGPU_ALLOCATED_VRAM_MIB    quota in MiB, as handed to the workload by the scheduler
 *   CGPU_ALLOCATED_VRAM_BYTES  exact-byte quota, overrides the above (used by the e2e tests)
 *   CGPU_JOB_ID                job identifier, included in log lines
 *   CGPU_GUARD_STATUS_FILE     if set, current usage is written here for telemetry
 *   CGPU_GUARD_VERBOSE         1 to log every allocation and free, not just denials
 *
 * With no quota configured the guard stays in pass-through mode and only logs that fact, so
 * preloading it can never break an unquotaed workload.
 */

#define _GNU_SOURCE
#include <dlfcn.h>
#include <pthread.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

/* ---- CUDA constants, declared locally so no toolkit headers are needed ---------------- */
#define CGPU_SUCCESS 0
#define CGPU_ERR_MEMORY_ALLOCATION 2 /* cudaErrorMemoryAllocation == CUDA_ERROR_OUT_OF_MEMORY */

typedef int cgpu_cuda_error_t;           /* cudaError_t  */
typedef int cgpu_cu_result_t;            /* CUresult     */
typedef unsigned long long cgpu_dev_ptr; /* CUdeviceptr  */

/* ---- guard state --------------------------------------------------------------------- */

#define CGPU_TABLE_SIZE 16384 /* power of two; open addressing with linear probing */

struct cgpu_slot {
    void *ptr;
    size_t size;
};

static struct cgpu_slot g_table[CGPU_TABLE_SIZE];
static pthread_mutex_t g_lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_once_t g_once = PTHREAD_ONCE_INIT;

static size_t g_quota_bytes; /* 0 means "no quota configured" -> pass-through */
static size_t g_used_bytes;  /* device memory currently held, as tracked by us */
static size_t g_peak_bytes;
static unsigned long g_allocs;
static unsigned long g_denials;
static int g_verbose;
static int g_enabled;
static const char *g_job_id = "-";
static const char *g_status_path;

/*
 * Two behaviours beyond plain accounting:
 *
 *   g_virt_meminfo (ON by default) - report the quota as the device capacity. Safe and
 *   necessary: without it a framework sizes its pools from the physical card and burns its
 *   whole quota before failing.
 *
 *   g_hook_dlsym (OFF by default) - also hand guarded wrappers to runtime symbol lookups.
 *   This would catch workloads that resolve driver entry points by name, but it is EXPERI-
 *   MENTAL and off for a reason: the driver resolves its own internals the same way, so the
 *   hook makes libcuda's internal calls re-enter this library. Measured on the RTX 4050 it
 *   first caused every allocation to be charged twice, and once that was fixed it hung the
 *   workload outright, while the same probe ran cleanly with the hook disabled. Enabling it
 *   is not advisable without a way to distinguish driver-internal lookups from workload ones.
 */
static int g_virt_meminfo = 1;
static int g_hook_dlsym;

/* Compute share, declared here because cgpu_init() sets it; the limiter that uses it is
 * further down. 0 means unlimited. */
static int g_sm_percent;

static int cgpu_env_flag(const char *name, int dflt)
{
    const char *v = getenv(name);
    if (!v || !*v) return dflt;
    return !(v[0] == '0' || v[0] == 'n' || v[0] == 'N' || v[0] == 'f' || v[0] == 'F');
}

/*
 * Re-entrancy depth, per thread.
 *
 * This is not an optimisation, it is a correctness requirement. The driver implements the
 * malloc-style APIs on top of the lower-level ones: a single cuMemAlloc_v2 from the workload
 * turns into cuMemCreate calls inside libcuda, and both are interposed here. Without this
 * counter each allocation was charged to the quota more than once - measured on the RTX 4050,
 * one 64 MiB request consumed 128 MiB of quota and the third request was wrongly refused.
 *
 * Only the outermost call accounts; nested calls made by the driver's own implementation pass
 * straight through, because the memory they commit has already been charged.
 */
static __thread int g_depth;

#define CGPU_ENTER() const int cgpu_outer = (g_depth == 0); g_depth++
#define CGPU_LEAVE() g_depth--

static void cgpu_log(const char *level, const char *fmt, ...)
{
    va_list ap;
    fprintf(stderr, "[cgpu-guard] %s job=%s ", level, g_job_id);
    va_start(ap, fmt);
    vfprintf(stderr, fmt, ap);
    va_end(ap);
    fputc('\n', stderr);
    fflush(stderr);
}

/*
 * Minimal decimal parser, used instead of strtoull().
 *
 * This is not stylistic. Modern glibc headers redirect strtoull() to __isoc23_strtoull(),
 * which carries a GLIBC_2.38 version requirement. The CUDA workload images this guard is
 * preloaded into are Ubuntu 22.04 based (glibc 2.35), so linking that symbol makes the
 * library unloadable in the very containers it exists to govern. Parsing the two integer
 * environment variables by hand keeps the minimum glibc at 2.34.
 */
static unsigned long long cgpu_parse_u64(const char *s)
{
    unsigned long long v = 0;
    if (!s) return 0;
    while (*s == ' ' || *s == '\t') s++;
    while (*s >= '0' && *s <= '9') {
        v = v * 10ull + (unsigned long long)(*s - '0');
        s++;
    }
    return v;
}

static void cgpu_init(void)
{
    const char *mib = getenv("CGPU_ALLOCATED_VRAM_MIB");
    const char *bytes = getenv("CGPU_ALLOCATED_VRAM_BYTES");
    const char *job = getenv("CGPU_JOB_ID");
    const char *verbose = getenv("CGPU_GUARD_VERBOSE");

    if (job && *job) g_job_id = job;
    g_verbose = (verbose && *verbose && strcmp(verbose, "0") != 0);
    g_status_path = getenv("CGPU_GUARD_STATUS_FILE");
    g_virt_meminfo = cgpu_env_flag("CGPU_GUARD_VIRTUALIZE_MEMINFO", 1);
    g_hook_dlsym = cgpu_env_flag("CGPU_GUARD_HOOK_DLSYM", 0);

    if (bytes && *bytes) {
        g_quota_bytes = (size_t)cgpu_parse_u64(bytes);
    } else if (mib && *mib) {
        g_quota_bytes = (size_t)cgpu_parse_u64(mib) * 1024u * 1024u;
    }

    /* Compute share. Independent of the VRAM quota: a job may be given a slice of the SMs
     * without a memory limit, or the reverse. */
    const char *sm = getenv("CGPU_ALLOCATED_SM_PERCENT");
    if (!sm || !*sm) sm = getenv("CGPU_ALLOCATED_GPU_CORES");
    if (sm && *sm) {
        long v = (long)cgpu_parse_u64(sm);
        if (v > 0 && v < 100) {
            g_sm_percent = (int)v;
        } else if (v >= 100) {
            g_sm_percent = 0; /* a full share is the same as no limit - skip the gate */
        }
    }

    g_enabled = (g_quota_bytes > 0);
    if (g_enabled) {
        cgpu_log("info", "guard active quota_bytes=%zu (%.1f MiB)",
                 g_quota_bytes, (double)g_quota_bytes / (1024.0 * 1024.0));
    } else {
        cgpu_log("info", "no quota configured (CGPU_ALLOCATED_VRAM_MIB unset) - pass-through");
    }
    if (g_sm_percent > 0) {
        cgpu_log("info", "compute share %d%% of the GPU", g_sm_percent);
    }
}

/*
 * Announce ourselves at load time. This distinguishes "the guard was never preloaded" from
 * "the guard was preloaded but the workload's allocation calls did not route through it"
 * (which happens when a workload statically links the CUDA runtime). Those two failures look
 * identical without it.
 */
__attribute__((constructor)) static void cgpu_on_load(void)
{
    pthread_once(&g_once, cgpu_init);
    cgpu_log("info", "guard library loaded via LD_PRELOAD");
}

/*
 * Remove our status file when the process exits normally.
 *
 * Without this, a finished job's last status reading stays on disk forever and the worker
 * agent keeps reporting it as an active job holding VRAM. Observed in practice: a status
 * file from a test run two hours earlier was still being exported as 256 MiB in use, which
 * would have A4's dashboard showing phantom usage and any capacity calculation reading it
 * double-counting memory that nobody holds.
 *
 * This covers normal exit only. A job killed by SIGKILL, or one whose container is torn
 * down, never runs a destructor - so the agent must ALSO treat a status file whose mtime has
 * stopped advancing as stale. Both halves are needed; neither alone is sufficient.
 */
__attribute__((destructor)) static void cgpu_on_unload(void)
{
    if (g_status_path) unlink(g_status_path);
}

/* =====================================================================================
 * Compute (SM) limiting
 * =====================================================================================
 * A VRAM quota stops one job exhausting another's memory, but it says nothing about who
 * gets the SMs: a single job issuing back-to-back kernels keeps the card at 100% and every
 * co-tenant waits behind it. Time-slicing between processes is the driver's job and it is
 * fair only in the sense that everyone suffers equally, so a job that should have a third
 * of the card can still monopolise it.
 *
 * So the guard also meters kernel launches. The workload declares a share with
 * CGPU_ALLOCATED_SM_PERCENT (1-100); a watcher thread samples how much of the GPU this
 * process is actually getting and adjusts a pre-launch delay until the measured share
 * converges on the declared one. Feedback rather than a fixed budget, because the cost of
 * a kernel is not knowable in advance - grid size says nothing about how long a kernel
 * runs, so any static "tokens per launch" scheme mis-meters by orders of magnitude between
 * a trivial elementwise kernel and a large GEMM.
 *
 * Honest limits of this approach, all shared with every software-level GPU partitioner:
 *   - It throttles *submission*, not execution. A single already-running kernel cannot be
 *     preempted, so the share is only respected on average over several launches, and a
 *     workload that submits one very long kernel will overshoot for its duration.
 *   - Per-process accounting needs NVML. Where it is unavailable the watcher falls back to
 *     device-wide utilisation, which over-counts when co-tenants are busy and therefore
 *     throttles this job more than its share. Conservative in the right direction, but
 *     worth knowing.
 *   - It is a share of time, not a hardware partition. Hardware partitioning (MIG) gives
 *     true isolation but only on datacenter cards; neither of this project's laptop GPUs
 *     supports it.
 */


static pthread_once_t g_sm_once = PTHREAD_ONCE_INIT;
static int g_sm_sampled_self;         /* 1 when per-process utilisation is available */

/* ---- NVML, resolved at runtime so the guard never links against it ------------------- */
typedef int (*fn_nvmlInit)(void);
typedef int (*fn_nvmlDeviceGetHandleByIndex)(unsigned int, void **);
typedef int (*fn_nvmlDeviceGetUtilizationRates)(void *, void *);
typedef int (*fn_nvmlDeviceGetProcessUtilization)(void *, void *, unsigned int *, unsigned long long);

struct cgpu_nvml_util { unsigned int gpu, memory; };
/* nvmlProcessUtilizationSample_t */
struct cgpu_nvml_proc { unsigned int pid; unsigned long long ts; unsigned int sm, mem, enc, dec; };

static fn_nvmlDeviceGetUtilizationRates   g_nvml_util;
static fn_nvmlDeviceGetProcessUtilization g_nvml_proc_util;
static void *g_nvml_dev;

static void cgpu_nvml_open(void)
{
    void *h = dlopen("libnvidia-ml.so.1", RTLD_LAZY);
    if (!h) h = dlopen("libnvidia-ml.so", RTLD_LAZY);
    if (!h) return;

    fn_nvmlInit init = (fn_nvmlInit)dlsym(h, "nvmlInit_v2");
    if (!init) init = (fn_nvmlInit)dlsym(h, "nvmlInit");
    fn_nvmlDeviceGetHandleByIndex byidx =
        (fn_nvmlDeviceGetHandleByIndex)dlsym(h, "nvmlDeviceGetHandleByIndex_v2");
    if (!byidx) byidx = (fn_nvmlDeviceGetHandleByIndex)dlsym(h, "nvmlDeviceGetHandleByIndex");
    if (!init || !byidx || init() != 0) return;
    if (byidx(0, &g_nvml_dev) != 0) { g_nvml_dev = NULL; return; }

    g_nvml_util = (fn_nvmlDeviceGetUtilizationRates)dlsym(h, "nvmlDeviceGetUtilizationRates");
    g_nvml_proc_util =
        (fn_nvmlDeviceGetProcessUtilization)dlsym(h, "nvmlDeviceGetProcessUtilization");
}

/* Returns this process's SM utilisation percentage, or -1 when unknown. */
static int cgpu_sample_utilisation(void)
{
    if (!g_nvml_dev) return -1;

    /* Preferred: our own share, so a co-tenant's load does not throttle us. */
    if (g_nvml_proc_util) {
        struct cgpu_nvml_proc samples[32];
        unsigned int count = 32;
        /* A 1s look-back: shorter windows frequently return "not found" between samples. */
        struct timespec ts;
        clock_gettime(CLOCK_REALTIME, &ts);
        unsigned long long since = (unsigned long long)(ts.tv_sec - 1) * 1000000ull;
        if (g_nvml_proc_util(g_nvml_dev, samples, &count, since) == 0) {
            const unsigned int me = (unsigned int)getpid();
            for (unsigned int i = 0; i < count; i++) {
                if (samples[i].pid == me) {
                    g_sm_sampled_self = 1;
                    return (int)samples[i].sm;
                }
            }
            /* Reported successfully but we are not in the list: we used nothing. */
            g_sm_sampled_self = 1;
            return 0;
        }
    }

    if (g_nvml_util) {
        struct cgpu_nvml_util u = {0, 0};
        if (g_nvml_util(g_nvml_dev, &u) == 0) return (int)u.gpu;
    }
    return -1;
}

/*
 * Adjusts the pre-launch delay until measured utilisation meets the declared share.
 *
 * Deliberately asymmetric: overshoot is corrected faster than undershoot is relaxed. The
 * cost of being slightly under budget is some idle GPU; the cost of being over is that a
 * co-tenant misses its share, which is the thing the quota exists to prevent.
 */
static void *cgpu_sm_watcher(void *arg)
{
    (void)arg;
    cgpu_nvml_open();

    /*
     * Reporting only. Enforcement is in cgpu_sm_gate(), which needs no measurement: the
     * share is held by accounting the job's own work against wall time, so it works
     * identically with or without NVML and is unaffected by what co-tenants do.
     *
     * This thread exists because the measured number is worth having when a share looks
     * wrong - it distinguishes "the gate is not throttling" from "the gate is throttling
     * and the GPU is busy with someone else". It only runs under CGPU_GUARD_VERBOSE.
     */
    if (!g_verbose) return NULL;

    if (!g_nvml_dev) {
        cgpu_log("info", "sm limit %d%% active (NVML unavailable - no utilisation reporting)",
                 g_sm_percent);
        return NULL;
    }
    cgpu_log("info", "sm limit %d%% active (%s utilisation reporting)", g_sm_percent,
             g_nvml_proc_util ? "per-process" : "device-wide");

    for (;;) {
        struct timespec req = { 2, 0 }; /* every 2s: this is diagnostics, not a control loop */
        nanosleep(&req, NULL);
        int util = cgpu_sample_utilisation();
        if (util >= 0) {
            cgpu_log("info", "sm share declared=%d%% measured=%d%%%s", g_sm_percent, util,
                     g_sm_sampled_self ? "" : " (device-wide, includes co-tenants)");
        }
    }
    return NULL;
}

static void cgpu_sm_start(void)
{
    pthread_t t;
    if (pthread_create(&t, NULL, cgpu_sm_watcher, NULL) == 0) pthread_detach(t);
}

/*
 * Called before every intercepted kernel launch.
 *
 * Duty cycling, not utilisation feedback, is what actually produces proportional shares.
 * The first version of this throttled on the process's own measured utilisation, and it
 * enforced a single job's share correctly (declared 30% -> 31% of solo throughput) but
 * failed the case the whole feature exists for. Measured with two co-tenants declaring 60%
 * and 20%:
 *
 *     tenant-A (60%)  200.2 GFLOP/s
 *     tenant-B (20%)  190.7 GFLOP/s     <- should have been about a third of A
 *
 * The signal was the problem. A process's own utilisation falls both when it throttles
 * itself and when someone else is competing, and those are indistinguishable from inside.
 * Under contention both tenants read well under target, both concluded they had room, and
 * neither throttled - so the card was split by the driver's own round-robin instead of by
 * the declared shares.
 *
 * A duty cycle has no such coupling: each window, a job may submit for its declared
 * fraction and must then stand down, regardless of what anyone else is doing. Two jobs at
 * 60% and 20% submit for 60% and 20% of wall time, and the ratio between them holds
 * whether they run alone or together.
 *
 * Kept cheap: one clock read and at most one sleep. A workload can launch tens of thousands
 * of kernels a second, so anything heavier here would become the bottleneck it is measuring.
 * The window state is deliberately unsynchronised - concurrent launches may race on it, and
 * the worst outcome is a slightly early window rollover, which is not worth a mutex on this
 * path.
 */
/* Ignore gaps longer than this as the job being idle rather than computing, so a workload
 * that pauses is not billed for the pause. */
#define CGPU_SM_IDLE_GAP_US 1000000LL
/* Ceiling on a single stand-down, so a long kernel cannot make the job unresponsive. */
#define CGPU_SM_MAX_SLEEP_US 250000LL

static volatile long long g_sm_last_gate_us; /* previous gate call, to charge elapsed work */

static long long cgpu_now_us(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (long long)ts.tv_sec * 1000000LL + ts.tv_nsec / 1000;
}

static inline void cgpu_sm_gate(void)
{
    if (g_sm_percent <= 0 || g_sm_percent >= 100) return;
    pthread_once(&g_sm_once, cgpu_sm_start);

    const long long now = cgpu_now_us();
    const long long last = g_sm_last_gate_us;

    /*
     * Charge the interval since the previous launch as work, then stand down for however
     * long that work "cost" at the declared share:
     *
     *     share p  =>  work w must be followed by idle w * (100 - p) / p
     *
     * so the job occupies the GPU for p percent of its own wall time. There is no window:
     * a kernel longer than any fixed window is charged in full and paid for in full, which
     * is what the earlier window version got wrong - it discarded intervals longer than the
     * window as "idle", so a job whose kernels ran 88 ms against a 20 ms window was never
     * throttled at all, and two tenants declaring 60% and 20% both ran flat out.
     *
     * For a workload that synchronises after each launch, the charged interval is
     * essentially the kernel's own run time. For one that pipelines without synchronising,
     * it is the submission gap and therefore an undercharge - the share is then a bound on
     * submission rather than on occupancy, which is the honest limit of metering from
     * inside the process.
     */
    long long work = (last == 0) ? 0 : now - last;
    if (work < 0 || work > CGPU_SM_IDLE_GAP_US) work = 0; /* first call, or genuinely idle */

    if (work > 0) {
        long long sleep_us = work * (100 - g_sm_percent) / g_sm_percent;
        if (sleep_us > CGPU_SM_MAX_SLEEP_US) sleep_us = CGPU_SM_MAX_SLEEP_US;
        if (sleep_us > 0) {
            struct timespec req = { sleep_us / 1000000LL, (sleep_us % 1000000LL) * 1000L };
            nanosleep(&req, NULL);
        }
    }

    /* Billing restarts after the stand-down, so the idle time is not itself charged. */
    g_sm_last_gate_us = cgpu_now_us();
}

static size_t cgpu_hash(void *p)
{
    uintptr_t x = (uintptr_t)p;
    x ^= x >> 33;
    x *= 0xff51afd7ed558ccdULL;
    x ^= x >> 33;
    return (size_t)(x & (CGPU_TABLE_SIZE - 1));
}

/* caller holds g_lock */
static void cgpu_remember(void *p, size_t size)
{
    size_t i = cgpu_hash(p);
    for (size_t n = 0; n < CGPU_TABLE_SIZE; n++) {
        size_t s = (i + n) & (CGPU_TABLE_SIZE - 1);
        if (g_table[s].ptr == NULL || g_table[s].ptr == p) {
            g_table[s].ptr = p;
            g_table[s].size = size;
            return;
        }
    }
    /* Table full: stop tracking this pointer rather than lie about the total. */
    cgpu_log("warn", "allocation table full, pointer %p untracked", p);
}

/* caller holds g_lock; returns the size that was tracked, or 0 if unknown */
static size_t cgpu_forget(void *p)
{
    size_t i = cgpu_hash(p);
    for (size_t n = 0; n < CGPU_TABLE_SIZE; n++) {
        size_t s = (i + n) & (CGPU_TABLE_SIZE - 1);
        if (g_table[s].ptr == NULL) return 0;
        if (g_table[s].ptr == p) {
            size_t size = g_table[s].size;
            g_table[s].ptr = NULL;
            g_table[s].size = 0;
            return size;
        }
    }
    return 0;
}

/* caller holds g_lock */
static void cgpu_write_status(void)
{
    if (!g_status_path) return;
    FILE *f = fopen(g_status_path, "w");
    if (!f) return;
    fprintf(f,
            "job_id %s\nused_bytes %zu\npeak_bytes %zu\nquota_bytes %zu\nallocations %lu\ndenials %lu\n",
            g_job_id, g_used_bytes, g_peak_bytes, g_quota_bytes, g_allocs, g_denials);
    fclose(f);
}

/*
 * Returns 1 if an allocation of `size` is permitted, 0 if it must be denied. On success the
 * usage total is already updated, so the caller only records the pointer once the real
 * allocator returns it.
 */
static int cgpu_try_reserve(size_t size, const char *api)
{
    pthread_once(&g_once, cgpu_init);
    if (!g_enabled) return 1;

    pthread_mutex_lock(&g_lock);
    if (g_used_bytes + size > g_quota_bytes) {
        g_denials++;
        size_t used = g_used_bytes, quota = g_quota_bytes;
        cgpu_write_status();
        pthread_mutex_unlock(&g_lock);
        cgpu_log("deny",
                 "%s size=%zu used=%zu quota=%zu would_exceed_by=%zu -> returning out-of-memory",
                 api, size, used, quota, (used + size) - quota);
        return 0;
    }
    g_used_bytes += size;
    if (g_used_bytes > g_peak_bytes) g_peak_bytes = g_used_bytes;
    g_allocs++;
    size_t used = g_used_bytes;
    cgpu_write_status();
    pthread_mutex_unlock(&g_lock);

    if (g_verbose)
        cgpu_log("allow", "%s size=%zu used=%zu quota=%zu", api, size, used, g_quota_bytes);
    return 1;
}

/* Roll back a reservation when the real allocator fails for its own reasons. */
static void cgpu_unreserve(size_t size)
{
    if (!g_enabled) return;
    pthread_mutex_lock(&g_lock);
    g_used_bytes = (g_used_bytes >= size) ? g_used_bytes - size : 0;
    if (g_allocs) g_allocs--;
    cgpu_write_status();
    pthread_mutex_unlock(&g_lock);
}

static void cgpu_track(void *p, size_t size)
{
    if (!g_enabled || p == NULL) return;
    pthread_mutex_lock(&g_lock);
    cgpu_remember(p, size);
    cgpu_write_status();
    pthread_mutex_unlock(&g_lock);
}

static void cgpu_release(void *p, const char *api)
{
    pthread_once(&g_once, cgpu_init);
    if (!g_enabled || p == NULL) return;

    pthread_mutex_lock(&g_lock);
    size_t size = cgpu_forget(p);
    g_used_bytes = (g_used_bytes >= size) ? g_used_bytes - size : 0;
    size_t used = g_used_bytes;
    cgpu_write_status();
    pthread_mutex_unlock(&g_lock);

    if (g_verbose)
        cgpu_log("free", "%s size=%zu used=%zu", api, size, used);
}

/* ---- real symbol resolution -----------------------------------------------------------
 *
 * All internal resolution goes through cgpu_next(), which calls the *real* dlsym directly.
 * It must not go through plain dlsym(), because this library interposes dlsym() itself and
 * would otherwise hand itself its own wrappers and recurse forever.
 */

typedef void *(*fn_dlsym_t)(void *, const char *);
static fn_dlsym_t g_real_dlsym;

static void cgpu_resolve_dlsym(void)
{
    if (g_real_dlsym) return;
    /* dlsym moved from libdl into libc in glibc 2.34, so try that version first. */
    g_real_dlsym = (fn_dlsym_t)dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.34");
    if (!g_real_dlsym) g_real_dlsym = (fn_dlsym_t)dlvsym(RTLD_NEXT, "dlsym", "GLIBC_2.2.5");
}

/*
 * Resolve the real implementation of a CUDA entry point.
 *
 * RTLD_NEXT covers the normal case, where the workload links libcudart or libcuda and the
 * loader placed them after us. It is not enough on its own: a workload that dlopen()s the
 * driver itself - PyTorch does this for the virtual memory API - leaves those libraries
 * outside our RTLD_NEXT chain. Without the fallback the guard would refuse allocations it was
 * only supposed to account for, turning a quota check into a hard failure.
 */
static void *cgpu_next(const char *name)
{
    cgpu_resolve_dlsym();
    if (!g_real_dlsym) return NULL;

    void *p = g_real_dlsym(RTLD_NEXT, name);
    if (p) return p;

    static void *h_driver, *h_runtime;
    if (!h_driver) {
        /* NOLOAD first: reuse the workload's handle rather than forcing another load. */
        h_driver = dlopen("libcuda.so.1", RTLD_LAZY | RTLD_NOLOAD);
        if (!h_driver) h_driver = dlopen("libcuda.so.1", RTLD_LAZY);
    }
    if (h_driver) {
        p = g_real_dlsym(h_driver, name);
        if (p) return p;
    }
    if (!h_runtime) {
        h_runtime = dlopen("libcudart.so.12", RTLD_LAZY | RTLD_NOLOAD);
        if (!h_runtime) h_runtime = dlopen("libcudart.so", RTLD_LAZY | RTLD_NOLOAD);
    }
    if (h_runtime) {
        p = g_real_dlsym(h_runtime, name);
        if (p) return p;
    }
    return NULL;
}

#define CGPU_REAL(fn_type, name)                            \
    static fn_type real = NULL;                             \
    if (!real) real = (fn_type)cgpu_next(name);             \
    if (!real) {                                            \
        cgpu_log("error", "cannot resolve real %s", name);  \
        return CGPU_ERR_MEMORY_ALLOCATION;                  \
    }

/*
 * Shared body for the allocation wrappers. Accounting happens only in the outermost call
 * (see g_depth); nested driver-internal calls pass through already charged.
 */
#define CGPU_ALLOC_BODY(api, call, store)                                    \
    CGPU_ENTER();                                                            \
    if (cgpu_outer && !cgpu_try_reserve(size_arg, api)) {                    \
        CGPU_LEAVE();                                                        \
        return CGPU_ERR_MEMORY_ALLOCATION;                                   \
    }                                                                        \
    int rc = (call);                                                         \
    CGPU_LEAVE();                                                            \
    if (cgpu_outer) {                                                        \
        if (rc == CGPU_SUCCESS) { store; }                                   \
        else cgpu_unreserve(size_arg);                                       \
    }                                                                        \
    return rc;

#define CGPU_FREE_BODY(api, ptr_expr, call)        \
    CGPU_ENTER();                                  \
    if (cgpu_outer) cgpu_release(ptr_expr, api);   \
    int rc = (call);                               \
    CGPU_LEAVE();                                  \
    return rc;

/* ---- CUDA Runtime API ----------------------------------------------------------------- */

typedef cgpu_cuda_error_t (*fn_cudaMalloc)(void **, size_t);
typedef cgpu_cuda_error_t (*fn_cudaMallocManaged)(void **, size_t, unsigned int);
typedef cgpu_cuda_error_t (*fn_cudaMallocAsync)(void **, size_t, void *);
typedef cgpu_cuda_error_t (*fn_cudaFree)(void *);
typedef cgpu_cuda_error_t (*fn_cudaFreeAsync)(void *, void *);

cgpu_cuda_error_t cudaMalloc(void **devPtr, size_t size)
{
    const size_t size_arg = size;
    CGPU_REAL(fn_cudaMalloc, "cudaMalloc")
    CGPU_ALLOC_BODY("cudaMalloc", real(devPtr, size),
                    if (devPtr) cgpu_track(*devPtr, size_arg))
}

cgpu_cuda_error_t cudaMallocManaged(void **devPtr, size_t size, unsigned int flags)
{
    const size_t size_arg = size;
    CGPU_REAL(fn_cudaMallocManaged, "cudaMallocManaged")
    CGPU_ALLOC_BODY("cudaMallocManaged", real(devPtr, size, flags),
                    if (devPtr) cgpu_track(*devPtr, size_arg))
}

/*
 * Stream-ordered (pool) allocations. These matter for enforcement, not completeness:
 * measured on the RTX 4050, PYTORCH_CUDA_ALLOC_CONF=backend:cudaMallocAsync stopped calling
 * cudaMalloc altogether and a 1024 MiB allocation against a 512 MiB quota succeeded. Without
 * these wrappers the quota could be bypassed by setting one environment variable.
 */
cgpu_cuda_error_t cudaMallocAsync(void **devPtr, size_t size, void *stream)
{
    const size_t size_arg = size;
    CGPU_REAL(fn_cudaMallocAsync, "cudaMallocAsync")
    CGPU_ALLOC_BODY("cudaMallocAsync", real(devPtr, size, stream),
                    if (devPtr) cgpu_track(*devPtr, size_arg))
}

cgpu_cuda_error_t cudaFree(void *devPtr)
{
    CGPU_REAL(fn_cudaFree, "cudaFree")
    CGPU_FREE_BODY("cudaFree", devPtr, real(devPtr))
}

cgpu_cuda_error_t cudaFreeAsync(void *devPtr, void *stream)
{
    CGPU_REAL(fn_cudaFreeAsync, "cudaFreeAsync")
    CGPU_FREE_BODY("cudaFreeAsync", devPtr, real(devPtr, stream))
}

/* ---- CUDA Driver API ------------------------------------------------------------------ */

typedef cgpu_cu_result_t (*fn_cuMemAlloc)(cgpu_dev_ptr *, size_t);
typedef cgpu_cu_result_t (*fn_cuMemAllocManaged)(cgpu_dev_ptr *, size_t, unsigned int);
typedef cgpu_cu_result_t (*fn_cuMemAllocAsync)(cgpu_dev_ptr *, size_t, void *);
typedef cgpu_cu_result_t (*fn_cuMemAllocFromPoolAsync)(cgpu_dev_ptr *, size_t, void *, void *);
typedef cgpu_cu_result_t (*fn_cuMemFree)(cgpu_dev_ptr);
typedef cgpu_cu_result_t (*fn_cuMemFreeAsync)(cgpu_dev_ptr, void *);

cgpu_cu_result_t cuMemAlloc_v2(cgpu_dev_ptr *dptr, size_t bytesize)
{
    const size_t size_arg = bytesize;
    CGPU_REAL(fn_cuMemAlloc, "cuMemAlloc_v2")
    CGPU_ALLOC_BODY("cuMemAlloc_v2", real(dptr, bytesize),
                    if (dptr) cgpu_track((void *)(uintptr_t)*dptr, size_arg))
}

cgpu_cu_result_t cuMemAllocManaged(cgpu_dev_ptr *dptr, size_t bytesize, unsigned int flags)
{
    const size_t size_arg = bytesize;
    CGPU_REAL(fn_cuMemAllocManaged, "cuMemAllocManaged")
    CGPU_ALLOC_BODY("cuMemAllocManaged", real(dptr, bytesize, flags),
                    if (dptr) cgpu_track((void *)(uintptr_t)*dptr, size_arg))
}

cgpu_cu_result_t cuMemAllocAsync(cgpu_dev_ptr *dptr, size_t bytesize, void *stream)
{
    const size_t size_arg = bytesize;
    CGPU_REAL(fn_cuMemAllocAsync, "cuMemAllocAsync")
    CGPU_ALLOC_BODY("cuMemAllocAsync", real(dptr, bytesize, stream),
                    if (dptr) cgpu_track((void *)(uintptr_t)*dptr, size_arg))
}

cgpu_cu_result_t cuMemAllocFromPoolAsync(cgpu_dev_ptr *dptr, size_t bytesize, void *pool, void *stream)
{
    const size_t size_arg = bytesize;
    CGPU_REAL(fn_cuMemAllocFromPoolAsync, "cuMemAllocFromPoolAsync")
    CGPU_ALLOC_BODY("cuMemAllocFromPoolAsync", real(dptr, bytesize, pool, stream),
                    if (dptr) cgpu_track((void *)(uintptr_t)*dptr, size_arg))
}

cgpu_cu_result_t cuMemFree_v2(cgpu_dev_ptr dptr)
{
    CGPU_REAL(fn_cuMemFree, "cuMemFree_v2")
    CGPU_FREE_BODY("cuMemFree_v2", (void *)(uintptr_t)dptr, real(dptr))
}

cgpu_cu_result_t cuMemFreeAsync(cgpu_dev_ptr dptr, void *stream)
{
    CGPU_REAL(fn_cuMemFreeAsync, "cuMemFreeAsync")
    CGPU_FREE_BODY("cuMemFreeAsync", (void *)(uintptr_t)dptr, real(dptr, stream))
}

/*
 * Virtual memory management API. PyTorch's expandable_segments:True commits through
 * cuMemCreate rather than any malloc-style call, which is a second way to sidestep a quota.
 * cuMemCreate is where physical memory is committed, so that is the point to charge.
 *
 * Note this is also the API the driver uses internally to satisfy cuMemAlloc_v2, which is
 * exactly why the re-entrancy counter above exists.
 */

typedef cgpu_cu_result_t (*fn_cuMemCreate)(unsigned long long *, size_t, const void *, unsigned long long);
typedef cgpu_cu_result_t (*fn_cuMemRelease)(unsigned long long);

cgpu_cu_result_t cuMemCreate(unsigned long long *handle, size_t size, const void *prop,
                             unsigned long long flags)
{
    const size_t size_arg = size;
    CGPU_REAL(fn_cuMemCreate, "cuMemCreate")
    CGPU_ALLOC_BODY("cuMemCreate", real(handle, size, prop, flags),
                    if (handle) cgpu_track((void *)(uintptr_t)*handle, size_arg))
}

cgpu_cu_result_t cuMemRelease(unsigned long long handle)
{
    CGPU_REAL(fn_cuMemRelease, "cuMemRelease")
    CGPU_FREE_BODY("cuMemRelease", (void *)(uintptr_t)handle, real(handle))
}

/* ---- Memory-info virtualisation -------------------------------------------------------
 *
 * Enforcement alone is not enough: a framework that asks the device how much memory it has
 * will size its pools from the physical card, not from its quota, and then spend the whole
 * quota before failing. Measured with PyTorch's expandable_segments:True, a 256 MiB tensor
 * committed ~500 MiB against a 512 MiB quota; raising the quota to 2048 MiB made the same
 * tensor commit ~2040 MiB and still fail. The allocator simply grows until refused.
 *
 * So the guard reports the quota as the device's capacity. A workload under a 512 MiB quota
 * sees a 512 MiB GPU with 512 MiB free and sizes itself accordingly. Unquotaed workloads see
 * the real device values, unchanged.
 */

typedef cgpu_cu_result_t (*fn_cuMemGetInfo)(size_t *, size_t *);
typedef cgpu_cuda_error_t (*fn_cudaMemGetInfo)(size_t *, size_t *);
typedef cgpu_cu_result_t (*fn_cuDeviceTotalMem)(size_t *, int);

static void cgpu_quota_mem(size_t *freeB, size_t *totalB)
{
    pthread_mutex_lock(&g_lock);
    size_t used = g_used_bytes;
    size_t quota = g_quota_bytes;
    pthread_mutex_unlock(&g_lock);

    if (totalB) *totalB = quota;
    if (freeB) *freeB = (quota > used) ? (quota - used) : 0;
}

cgpu_cu_result_t cuMemGetInfo_v2(size_t *freeB, size_t *totalB)
{
    pthread_once(&g_once, cgpu_init);
    if (g_enabled && g_virt_meminfo) {
        cgpu_quota_mem(freeB, totalB);
        return CGPU_SUCCESS;
    }
    CGPU_REAL(fn_cuMemGetInfo, "cuMemGetInfo_v2")
    return real(freeB, totalB);
}

cgpu_cuda_error_t cudaMemGetInfo(size_t *freeB, size_t *totalB)
{
    pthread_once(&g_once, cgpu_init);
    if (g_enabled && g_virt_meminfo) {
        cgpu_quota_mem(freeB, totalB);
        return CGPU_SUCCESS;
    }
    CGPU_REAL(fn_cudaMemGetInfo, "cudaMemGetInfo")
    return real(freeB, totalB);
}

cgpu_cu_result_t cuDeviceTotalMem_v2(size_t *bytes, int dev)
{
    pthread_once(&g_once, cgpu_init);
    if (g_enabled && g_virt_meminfo) {
        if (bytes) *bytes = g_quota_bytes;
        return CGPU_SUCCESS;
    }
    CGPU_REAL(fn_cuDeviceTotalMem, "cuDeviceTotalMem_v2")
    return real(bytes, dev);
}

/* ---- kernel launches: the compute-share gate -----------------------------------------
 *
 * Both API levels are intercepted because they are reached differently. A workload using
 * the driver API calls cuLaunchKernel directly, so the dynamic linker routes it here. A
 * framework on the CUDA runtime calls cudaLaunchKernel, and libcudart reaches the driver
 * through its own internal reference - which our wrapper never sees. Interposing both
 * covers either style; a workload that statically links the runtime still escapes, the
 * same limitation that applies to the allocation wrappers.
 *
 * The gate runs before the real call so a throttled job delays its own submission rather
 * than being starved after the work is already queued.
 */

/* dim3 as the CUDA runtime lays it out; passed by value, so the shape must match. */
typedef struct { unsigned int x, y, z; } cgpu_dim3;

typedef cgpu_cu_result_t (*fn_cuLaunchKernel)(void *, unsigned int, unsigned int, unsigned int,
                                             unsigned int, unsigned int, unsigned int,
                                             unsigned int, void *, void **, void **);
typedef cgpu_cu_result_t (*fn_cuLaunchKernelEx)(void *, void *, void **, void **);
typedef cgpu_cuda_error_t (*fn_cudaLaunchKernel)(const void *, cgpu_dim3, cgpu_dim3,
                                                 void **, size_t, void *);

cgpu_cu_result_t cuLaunchKernel(void *f,
                                unsigned int gx, unsigned int gy, unsigned int gz,
                                unsigned int bx, unsigned int by, unsigned int bz,
                                unsigned int shmem, void *stream, void **params, void **extra)
{
    pthread_once(&g_once, cgpu_init);
    cgpu_sm_gate();
    CGPU_REAL(fn_cuLaunchKernel, "cuLaunchKernel")
    return real(f, gx, gy, gz, bx, by, bz, shmem, stream, params, extra);
}

/* The config-struct form, used by newer toolkits for cluster launches. */
cgpu_cu_result_t cuLaunchKernelEx(void *cfg, void *f, void **params, void **extra)
{
    pthread_once(&g_once, cgpu_init);
    cgpu_sm_gate();
    CGPU_REAL(fn_cuLaunchKernelEx, "cuLaunchKernelEx")
    return real(cfg, f, params, extra);
}

cgpu_cuda_error_t cudaLaunchKernel(const void *func, cgpu_dim3 gridDim, cgpu_dim3 blockDim,
                                   void **args, size_t sharedMem, void *stream)
{
    pthread_once(&g_once, cgpu_init);
    cgpu_sm_gate();
    CGPU_REAL(fn_cudaLaunchKernel, "cudaLaunchKernel")
    return real(func, gridDim, blockDim, args, sharedMem, stream);
}

/* ---- dlsym interposition --------------------------------------------------------------
 *
 * Symbol interposition via LD_PRELOAD only works for symbols the dynamic linker resolves. A
 * workload that opens the driver itself and looks entry points up by name - as PyTorch does
 * for the virtual memory API - gets the driver's real function and never touches our
 * wrappers. Measured: with expandable_segments:True a 1024 MiB allocation against a 512 MiB
 * quota succeeded and the guard saw nothing at all.
 *
 * So we interpose dlsym too and hand back our own wrappers for the paths we govern.
 *
 * This does NOT make the guard a security boundary: a workload determined to escape can reach
 * the driver's symbols by other means. The guard enforces quotas for cooperative and approved
 * workloads; containing a hostile one is not something an in-process interposer can promise,
 * and the scheduler must sanitise job environments as well.
 */
void *dlsym(void *handle, const char *name)
{
    cgpu_resolve_dlsym();
    if (!g_real_dlsym) return NULL;

    /* Cheap guard on the hot path: only CUDA names can match. */
    pthread_once(&g_once, cgpu_init);
    if (g_hook_dlsym && name && name[0] == 'c' && name[1] == 'u') {
        if (!strcmp(name, "cuMemAlloc_v2"))           return (void *)cuMemAlloc_v2;
        if (!strcmp(name, "cuMemAllocManaged"))       return (void *)cuMemAllocManaged;
        if (!strcmp(name, "cuMemFree_v2"))            return (void *)cuMemFree_v2;
        if (!strcmp(name, "cuMemAllocAsync"))         return (void *)cuMemAllocAsync;
        if (!strcmp(name, "cuMemAllocFromPoolAsync")) return (void *)cuMemAllocFromPoolAsync;
        if (!strcmp(name, "cuMemFreeAsync"))          return (void *)cuMemFreeAsync;
        if (!strcmp(name, "cuMemCreate"))             return (void *)cuMemCreate;
        if (!strcmp(name, "cuMemRelease"))            return (void *)cuMemRelease;
        if (!strcmp(name, "cuMemGetInfo_v2"))         return (void *)cuMemGetInfo_v2;
        if (!strcmp(name, "cuDeviceTotalMem_v2"))     return (void *)cuDeviceTotalMem_v2;
        if (!strcmp(name, "cudaMalloc"))              return (void *)cudaMalloc;
        if (!strcmp(name, "cudaMallocManaged"))       return (void *)cudaMallocManaged;
        if (!strcmp(name, "cudaMallocAsync"))         return (void *)cudaMallocAsync;
        if (!strcmp(name, "cudaFree"))                return (void *)cudaFree;
        if (!strcmp(name, "cudaFreeAsync"))           return (void *)cudaFreeAsync;
        if (!strcmp(name, "cudaMemGetInfo"))          return (void *)cudaMemGetInfo;
    }
    return g_real_dlsym(handle, name);
}
