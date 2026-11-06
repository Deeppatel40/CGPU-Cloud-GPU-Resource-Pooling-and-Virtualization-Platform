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
 * It intercepts the CUDA allocation and free paths, keeps a running total of device memory
 * held by the process, and refuses any allocation that would push the total past the quota,
 * returning the same error the CUDA APIs return when the device is out of memory. The real
 * allocator is never called for a denied request, so no memory is reserved on the card.
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

/* ---- CUDA constants, declared locally so no toolkit headers are needed ---------------- */
#define CGPU_SUCCESS 0
#define CGPU_ERR_MEMORY_ALLOCATION 2 /* cudaErrorMemoryAllocation == CUDA_ERROR_OUT_OF_MEMORY */

typedef int cgpu_cuda_error_t;        /* cudaError_t  */
typedef int cgpu_cu_result_t;         /* CUresult     */
typedef unsigned long long cgpu_dev_ptr; /* CUdeviceptr */

/* ---- guard state --------------------------------------------------------------------- */

#define CGPU_TABLE_SIZE 16384 /* power of two; open addressing with linear probing */

struct cgpu_slot {
    void *ptr;
    size_t size;
};

static struct cgpu_slot g_table[CGPU_TABLE_SIZE];
static pthread_mutex_t g_lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_once_t g_once = PTHREAD_ONCE_INIT;

static size_t g_quota_bytes;   /* 0 means "no quota configured" -> pass-through */
static size_t g_used_bytes;    /* device memory currently held, as tracked by us */
static size_t g_peak_bytes;
static unsigned long g_allocs;
static unsigned long g_denials;
static int g_verbose;
static int g_enabled;
static const char *g_job_id = "-";
static const char *g_status_path;

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

    if (bytes && *bytes) {
        g_quota_bytes = (size_t)cgpu_parse_u64(bytes);
    } else if (mib && *mib) {
        g_quota_bytes = (size_t)cgpu_parse_u64(mib) * 1024u * 1024u;
    }

    g_enabled = (g_quota_bytes > 0);
    if (g_enabled) {
        cgpu_log("info", "guard active quota_bytes=%zu (%.1f MiB)",
                 g_quota_bytes, (double)g_quota_bytes / (1024.0 * 1024.0));
    } else {
        cgpu_log("info", "no quota configured (CGPU_ALLOCATED_VRAM_MIB unset) - pass-through");
    }
}

/*
 * Announce ourselves at load time. This is not just logging: it distinguishes "the guard
 * was never preloaded" from "the guard was preloaded but the workload's allocation calls
 * did not route through it" (which happens when a workload statically links the CUDA
 * runtime). Those two failures look identical without it.
 */
__attribute__((constructor)) static void cgpu_on_load(void)
{
    pthread_once(&g_once, cgpu_init);
    cgpu_log("info", "guard library loaded via LD_PRELOAD");
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
    /* Table full: we stop tracking this pointer rather than lie about the total. */
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
 * Returns 1 if an allocation of `size` is permitted, 0 if it must be denied.
 * On success the usage total is already updated, so the caller only needs to record
 * the pointer once the real allocator returns it.
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

/* ---- real symbol resolution ----------------------------------------------------------- */

#define CGPU_REAL(fn_type, name)                                   \
    static fn_type real = NULL;                                    \
    if (!real) real = (fn_type)dlsym(RTLD_NEXT, name);             \
    if (!real) {                                                   \
        cgpu_log("error", "cannot resolve real %s", name);         \
        return CGPU_ERR_MEMORY_ALLOCATION;                         \
    }

/* ---- CUDA Runtime API ----------------------------------------------------------------- */

typedef cgpu_cuda_error_t (*fn_cudaMalloc)(void **, size_t);
typedef cgpu_cuda_error_t (*fn_cudaMallocManaged)(void **, size_t, unsigned int);
typedef cgpu_cuda_error_t (*fn_cudaFree)(void *);

cgpu_cuda_error_t cudaMalloc(void **devPtr, size_t size)
{
    CGPU_REAL(fn_cudaMalloc, "cudaMalloc")
    if (!cgpu_try_reserve(size, "cudaMalloc")) return CGPU_ERR_MEMORY_ALLOCATION;

    cgpu_cuda_error_t rc = real(devPtr, size);
    if (rc == CGPU_SUCCESS && devPtr) cgpu_track(*devPtr, size);
    else cgpu_unreserve(size);
    return rc;
}

cgpu_cuda_error_t cudaMallocManaged(void **devPtr, size_t size, unsigned int flags)
{
    CGPU_REAL(fn_cudaMallocManaged, "cudaMallocManaged")
    if (!cgpu_try_reserve(size, "cudaMallocManaged")) return CGPU_ERR_MEMORY_ALLOCATION;

    cgpu_cuda_error_t rc = real(devPtr, size, flags);
    if (rc == CGPU_SUCCESS && devPtr) cgpu_track(*devPtr, size);
    else cgpu_unreserve(size);
    return rc;
}

cgpu_cuda_error_t cudaFree(void *devPtr)
{
    CGPU_REAL(fn_cudaFree, "cudaFree")
    cgpu_release(devPtr, "cudaFree");
    return real(devPtr);
}

/* ---- CUDA Driver API ------------------------------------------------------------------ */

typedef cgpu_cu_result_t (*fn_cuMemAlloc)(cgpu_dev_ptr *, size_t);
typedef cgpu_cu_result_t (*fn_cuMemAllocManaged)(cgpu_dev_ptr *, size_t, unsigned int);
typedef cgpu_cu_result_t (*fn_cuMemFree)(cgpu_dev_ptr);

cgpu_cu_result_t cuMemAlloc_v2(cgpu_dev_ptr *dptr, size_t bytesize)
{
    CGPU_REAL(fn_cuMemAlloc, "cuMemAlloc_v2")
    if (!cgpu_try_reserve(bytesize, "cuMemAlloc_v2")) return CGPU_ERR_MEMORY_ALLOCATION;

    cgpu_cu_result_t rc = real(dptr, bytesize);
    if (rc == CGPU_SUCCESS && dptr) cgpu_track((void *)(uintptr_t)*dptr, bytesize);
    else cgpu_unreserve(bytesize);
    return rc;
}

cgpu_cu_result_t cuMemAllocManaged(cgpu_dev_ptr *dptr, size_t bytesize, unsigned int flags)
{
    CGPU_REAL(fn_cuMemAllocManaged, "cuMemAllocManaged")
    if (!cgpu_try_reserve(bytesize, "cuMemAllocManaged")) return CGPU_ERR_MEMORY_ALLOCATION;

    cgpu_cu_result_t rc = real(dptr, bytesize, flags);
    if (rc == CGPU_SUCCESS && dptr) cgpu_track((void *)(uintptr_t)*dptr, bytesize);
    else cgpu_unreserve(bytesize);
    return rc;
}

cgpu_cu_result_t cuMemFree_v2(cgpu_dev_ptr dptr)
{
    CGPU_REAL(fn_cuMemFree, "cuMemFree_v2")
    cgpu_release((void *)(uintptr_t)dptr, "cuMemFree_v2");
    return real(dptr);
}
