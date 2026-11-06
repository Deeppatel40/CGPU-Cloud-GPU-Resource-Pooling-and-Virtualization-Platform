/*
 * cgpu_alloc_probe - CGPU's controlled GPU test workload
 * ======================================================
 *
 * A small, dependency-free CUDA workload used to exercise and prove the CGPU allocation
 * guard and the scheduler's VRAM quotas (Issue A3, criteria 6 and 7).
 *
 * Why this exists rather than using an off-the-shelf sample: the cuda-sample images
 * statically link the CUDA runtime and reach the driver through dlsym(), so their allocation
 * calls cannot be interposed with LD_PRELOAD at all. This probe links libcuda dynamically,
 * which is how real framework workloads (PyTorch, TensorFlow) link it, so it exercises the
 * same code path the guard governs in production.
 *
 * It needs no CUDA toolkit to build: the handful of driver API entry points are declared
 * here and resolved by the dynamic linker against libcuda at load time.
 *
 * Behaviour:
 *   - reports the physical GPU identity (name and UUID) so concurrent runs can be proven to
 *     be sharing one card,
 *   - allocates device memory in fixed chunks up to a target, printing a timestamped line
 *     per allocation with the driver's verdict,
 *   - then holds that memory and performs repeated host-to-device copies for a while, which
 *     is observable CUDA device activity, printing timestamped progress,
 *   - frees everything and reports the outcome.
 *
 * Usage:
 *   cgpu_alloc_probe --label NAME [--target-mib N] [--chunk-mib M] [--seconds S]
 *                    [--expect-denial]
 *
 * Exit codes:
 *   0  finished as expected
 *   3  an allocation was refused and --expect-denial was NOT given
 *   4  --expect-denial was given but every allocation succeeded
 */

#define _GNU_SOURCE
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

/* ---- CUDA driver API, declared locally so no toolkit headers are needed -------------- */
typedef int CUresult;
typedef int CUdevice;
typedef void *CUcontext;
typedef unsigned long long CUdeviceptr;

#define CUDA_OK 0
#define CUDA_ERROR_OUT_OF_MEMORY 2

extern CUresult cuInit(unsigned int flags);
extern CUresult cuDeviceGet(CUdevice *dev, int ordinal);
extern CUresult cuDeviceGetName(char *name, int len, CUdevice dev);
extern CUresult cuDeviceGetUuid(char uuid[16], CUdevice dev);
extern CUresult cuCtxCreate_v2(CUcontext *ctx, unsigned int flags, CUdevice dev);
extern CUresult cuCtxDestroy_v2(CUcontext ctx);
extern CUresult cuMemAlloc_v2(CUdeviceptr *dptr, size_t bytes);
extern CUresult cuMemFree_v2(CUdeviceptr dptr);
extern CUresult cuMemcpyHtoD_v2(CUdeviceptr dst, const void *src, size_t bytes);
extern CUresult cuMemGetInfo_v2(size_t *freeB, size_t *totalB);
extern CUresult cuCtxSynchronize(void);

/* ---- helpers -------------------------------------------------------------------------- */

#define MAX_CHUNKS 4096

static const char *g_label = "probe";

static void stamp(char *buf, size_t n)
{
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    struct tm tm;
    gmtime_r(&ts.tv_sec, &tm);
    size_t k = strftime(buf, n, "%H:%M:%S", &tm);
    snprintf(buf + k, n - k, ".%03ld", ts.tv_nsec / 1000000);
}

static void say(const char *fmt, ...)
{
    char ts[32];
    stamp(ts, sizeof ts);
    va_list ap;
    printf("[%s] [%s] ", ts, g_label);
    va_start(ap, fmt);
    vprintf(fmt, ap);
    va_end(ap);
    putchar('\n');
    fflush(stdout);
}

/*
 * Local integer parser instead of atol()/strtol(). Modern glibc headers redirect those to
 * __isoc23_* symbols that carry a GLIBC_2.38 requirement, which would make this binary
 * unloadable in the Ubuntu 22.04 (glibc 2.35) CUDA workload images it has to run in.
 */
static long cgpu_atol(const char *s)
{
    long v = 0;
    int neg = 0;
    if (!s) return 0;
    while (*s == ' ' || *s == '\t') s++;
    if (*s == '-') { neg = 1; s++; }
    else if (*s == '+') s++;
    while (*s >= '0' && *s <= '9') { v = v * 10 + (*s - '0'); s++; }
    return neg ? -v : v;
}

static double now_sec(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}

int main(int argc, char **argv)
{
    long target_mib = 256, chunk_mib = 64, seconds = 0;
    int expect_denial = 0;

    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "--label") && i + 1 < argc) g_label = argv[++i];
        else if (!strcmp(argv[i], "--target-mib") && i + 1 < argc) target_mib = cgpu_atol(argv[++i]);
        else if (!strcmp(argv[i], "--chunk-mib") && i + 1 < argc) chunk_mib = cgpu_atol(argv[++i]);
        else if (!strcmp(argv[i], "--seconds") && i + 1 < argc) seconds = cgpu_atol(argv[++i]);
        else if (!strcmp(argv[i], "--expect-denial")) expect_denial = 1;
        else { fprintf(stderr, "unknown argument: %s\n", argv[i]); return 2; }
    }
    if (chunk_mib <= 0) chunk_mib = 1;

    CUresult rc;
    if ((rc = cuInit(0)) != CUDA_OK) { say("cuInit failed rc=%d", rc); return 1; }

    CUdevice dev;
    if ((rc = cuDeviceGet(&dev, 0)) != CUDA_OK) { say("cuDeviceGet failed rc=%d", rc); return 1; }

    char name[128] = {0};
    cuDeviceGetName(name, (int)sizeof name, dev);

    unsigned char uu[16] = {0};
    char uuid[48] = {0};
    if (cuDeviceGetUuid((char *)uu, dev) == CUDA_OK) {
        snprintf(uuid, sizeof uuid,
                 "GPU-%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
                 uu[0], uu[1], uu[2], uu[3], uu[4], uu[5], uu[6], uu[7],
                 uu[8], uu[9], uu[10], uu[11], uu[12], uu[13], uu[14], uu[15]);
    }

    CUcontext ctx;
    if ((rc = cuCtxCreate_v2(&ctx, 0, dev)) != CUDA_OK) {
        say("cuCtxCreate failed rc=%d", rc);
        return 1;
    }

    size_t freeB = 0, totalB = 0;
    cuMemGetInfo_v2(&freeB, &totalB);

    say("device=\"%s\"", name);
    say("uuid=%s", uuid);
    say("device_memory total=%zu MiB free=%zu MiB", totalB >> 20, freeB >> 20);
    say("plan target=%ld MiB in %ld MiB chunks, then %ld s of device traffic",
        target_mib, chunk_mib, seconds);

    /* ---- allocation phase ---- */
    CUdeviceptr chunks[MAX_CHUNKS];
    int held = 0;
    long got_mib = 0;
    int denied = 0;

    while (got_mib < target_mib && held < MAX_CHUNKS) {
        long this_mib = chunk_mib;
        if (got_mib + this_mib > target_mib) this_mib = target_mib - got_mib;
        size_t bytes = (size_t)this_mib << 20;

        rc = cuMemAlloc_v2(&chunks[held], bytes);
        if (rc == CUDA_OK) {
            held++;
            got_mib += this_mib;
            say("alloc  %4ld MiB  OK            total_held=%ld MiB", this_mib, got_mib);
        } else {
            denied = 1;
            say("alloc  %4ld MiB  REFUSED rc=%d  total_held=%ld MiB%s",
                this_mib, rc, got_mib,
                rc == CUDA_ERROR_OUT_OF_MEMORY ? " (out of memory)" : "");
            break;
        }
    }

    /* ---- activity phase: prove the workload keeps making CUDA progress ---- */
    if (held > 0 && seconds > 0) {
        size_t buf_bytes = (size_t)chunk_mib << 20;
        void *host = malloc(buf_bytes);
        if (host) {
            memset(host, 0x5a, buf_bytes);
            double t0 = now_sec();
            unsigned long iters = 0;
            double next_report = 1.0;
            while (now_sec() - t0 < (double)seconds) {
                if (cuMemcpyHtoD_v2(chunks[0], host, buf_bytes) != CUDA_OK) break;
                cuCtxSynchronize();
                iters++;
                double el = now_sec() - t0;
                if (el >= next_report) {
                    say("progress t+%.0fs copies=%lu holding=%ld MiB", el, iters, got_mib);
                    next_report += 1.0;
                }
            }
            say("activity done copies=%lu in %ld s", iters, seconds);
            free(host);
        }
    }

    /* ---- teardown ---- */
    for (int i = 0; i < held; i++) cuMemFree_v2(chunks[i]);
    say("freed %d chunks (%ld MiB)", held, got_mib);
    cuMemGetInfo_v2(&freeB, &totalB);
    say("device_memory free_after=%zu MiB", freeB >> 20);
    cuCtxDestroy_v2(ctx);

    if (denied && !expect_denial) {
        say("RESULT=UNEXPECTED_DENIAL");
        return 3;
    }
    if (!denied && expect_denial) {
        say("RESULT=DENIAL_EXPECTED_BUT_NONE");
        return 4;
    }
    say("RESULT=OK%s", expect_denial ? " (denial observed as expected)" : "");
    return 0;
}
