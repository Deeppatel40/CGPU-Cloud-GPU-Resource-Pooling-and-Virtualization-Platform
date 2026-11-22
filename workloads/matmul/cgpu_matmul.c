/*
 * cgpu_matmul - CGPU's lightweight GPU-saturating workload
 * ========================================================
 *
 * A sustained tiled SGEMM loop whose job is to make GPU utilisation, VRAM occupancy and
 * achieved FLOP/s plainly visible, for demonstrating pooling and quota enforcement.
 *
 * Why this exists next to workloads/demonstration (ResNet-18 on CIFAR-10):
 *   The ResNet workload is an 11.9 GB image and needs a 170 MB dataset that its upstream
 *   host serves at a few hundred bytes per second, so standing it up takes hours. For
 *   showing that two jobs share one physical GPU under separate VRAM quotas, none of that
 *   weight earns anything: a matmul saturates the SMs, holds a controllable amount of VRAM,
 *   needs no dataset at all, and fits in a ~450 MB image on the CUDA base layer.
 *
 *   It is also easier to read as evidence. Training throughput depends on the model, the
 *   batch size and the data pipeline; GEMM throughput is 2*N^3 FLOPs per iteration, so the
 *   reported GFLOP/s can be checked by hand against the card's spec sheet.
 *
 * Properties that matter for CGPU:
 *   - links libcuda dynamically, so the allocation guard can interpose and enforce a quota,
 *   - allocates through cuMemAlloc, the path the guard policies,
 *   - sizes itself to the quota the guard reports via cuMemGetInfo rather than to the
 *     physical card, so it cooperates with capacity virtualisation instead of fighting it,
 *   - needs no CUDA toolkit at build or run time: the kernel travels as committed PTX.
 *
 * Parameters, in increasing precedence: defaults, /etc/cgpu/params.json, CLI flags.
 * Writes /outputs/metrics.json on exit so the platform can collect a result.
 *
 * Usage:
 *   cgpu_matmul [--size N] [--seconds S] [--vram-mib M] [--label NAME] [--output DIR]
 *
 * Exit codes:
 *   0 ran to completion
 *   1 CUDA/driver failure
 *   2 bad arguments
 *   3 could not fit even one working set in the quota
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
typedef void *CUmodule;
typedef void *CUfunction;
typedef void *CUstream;
typedef unsigned long long CUdeviceptr;

#define CUDA_OK 0

extern CUresult cuInit(unsigned int flags);
extern CUresult cuDeviceGet(CUdevice *dev, int ordinal);
extern CUresult cuDeviceGetName(char *name, int len, CUdevice dev);
extern CUresult cuDeviceGetUuid(char uuid[16], CUdevice dev);
extern CUresult cuCtxCreate_v2(CUcontext *ctx, unsigned int flags, CUdevice dev);
extern CUresult cuCtxDestroy_v2(CUcontext ctx);
extern CUresult cuCtxSynchronize(void);
extern CUresult cuMemAlloc_v2(CUdeviceptr *dptr, size_t bytes);
extern CUresult cuMemFree_v2(CUdeviceptr dptr);
extern CUresult cuMemsetD32_v2(CUdeviceptr dst, unsigned int ui, size_t n);
extern CUresult cuMemcpyDtoH_v2(void *dst, CUdeviceptr src, size_t bytes);
extern CUresult cuMemGetInfo_v2(size_t *freeB, size_t *totalB);
extern CUresult cuGetErrorName(CUresult err, const char **str);
extern CUresult cuModuleLoadData(CUmodule *mod, const void *image);
extern CUresult cuModuleGetFunction(CUfunction *fn, CUmodule mod, const char *name);
extern CUresult cuModuleUnload(CUmodule mod);
extern CUresult cuLaunchKernel(CUfunction f,
                               unsigned int gx, unsigned int gy, unsigned int gz,
                               unsigned int bx, unsigned int by, unsigned int bz,
                               unsigned int shmem, CUstream stream, void **params, void **extra);

#include "cgpu_matmul_ptx.h"

#define TILE 16

/* ---- small helpers ------------------------------------------------------------------- */

static const char *g_label = "matmul";

static void say(const char *fmt, ...)
{
    struct timespec ts;
    clock_gettime(CLOCK_REALTIME, &ts);
    struct tm tm;
    gmtime_r(&ts.tv_sec, &tm);
    char stamp[32];
    size_t k = strftime(stamp, sizeof stamp, "%H:%M:%S", &tm);
    snprintf(stamp + k, sizeof stamp - k, ".%03ld", ts.tv_nsec / 1000000);

    printf("[%s] [%s] ", stamp, g_label);
    va_list ap;
    va_start(ap, fmt);
    vprintf(fmt, ap);
    va_end(ap);
    putchar('\n');
    fflush(stdout);
}

static double now_sec(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (double)ts.tv_sec + (double)ts.tv_nsec / 1e9;
}

/*
 * Hand-rolled integer parsing instead of atol()/strtoll(). Modern glibc headers redirect
 * those to __isoc23_* symbols carrying a GLIBC_2.38 requirement, which would make this
 * binary unloadable in the Ubuntu 22.04 (glibc 2.35) images it has to run in.
 */
static long cgpu_atol(const char *s)
{
    long v = 0;
    int neg = 0;
    if (!s) return 0;
    while (*s == ' ' || *s == '\t' || *s == '"') s++;
    if (*s == '-') { neg = 1; s++; }
    while (*s >= '0' && *s <= '9') v = v * 10 + (*s++ - '0');
    return neg ? -v : v;
}

/*
 * Minimal scalar lookup in a flat JSON object: finds "key" and returns the integer that
 * follows it. Deliberately not a JSON parser - the platform writes this file itself and it
 * is a flat object of scalars, so a dependency-free scan is the proportionate tool. Returns
 * `fallback` when the key is absent.
 */
static long json_long(const char *json, const char *key, long fallback)
{
    if (!json) return fallback;
    char pat[64];
    snprintf(pat, sizeof pat, "\"%s\"", key);
    const char *p = strstr(json, pat);
    if (!p) return fallback;
    p = strchr(p + strlen(pat), ':');
    if (!p) return fallback;
    p++;
    while (*p == ' ' || *p == '\t') p++;
    if (*p != '-' && *p != '"' && (*p < '0' || *p > '9')) return fallback;
    return cgpu_atol(p);
}

static char *read_file(const char *path)
{
    FILE *f = fopen(path, "rb");
    if (!f) return NULL;
    if (fseek(f, 0, SEEK_END) != 0) { fclose(f); return NULL; }
    long n = ftell(f);
    if (n < 0 || n > (1 << 20)) { fclose(f); return NULL; }
    rewind(f);
    char *buf = malloc((size_t)n + 1);
    if (!buf) { fclose(f); return NULL; }
    size_t got = fread(buf, 1, (size_t)n, f);
    buf[got] = '\0';
    fclose(f);
    return buf;
}

static const char *cu_err(CUresult rc)
{
    const char *s = NULL;
    cuGetErrorName(rc, &s);
    return s ? s : "UNKNOWN";
}

int main(int argc, char **argv)
{
    /* Defaults: 4096x4096 is ~192 MiB of working set and a few TFLOPs per iteration, which
     * keeps a laptop GPU busy without monopolising it for minutes at a time. */
    long size = 4096, seconds = 60, vram_mib = 0;
    const char *outdir = "/outputs";

    /* The platform's parameters, if the Job mounted them. */
    char *cfg = read_file("/etc/cgpu/params.json");
    if (cfg) {
        size = json_long(cfg, "size", size);
        size = json_long(cfg, "matrix_size", size);
        seconds = json_long(cfg, "seconds", seconds);
        vram_mib = json_long(cfg, "vram_mib", vram_mib);
        say("loaded parameters from /etc/cgpu/params.json");
    }

    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "--size") && i + 1 < argc) size = cgpu_atol(argv[++i]);
        else if (!strcmp(argv[i], "--seconds") && i + 1 < argc) seconds = cgpu_atol(argv[++i]);
        else if (!strcmp(argv[i], "--vram-mib") && i + 1 < argc) vram_mib = cgpu_atol(argv[++i]);
        else if (!strcmp(argv[i], "--label") && i + 1 < argc) g_label = argv[++i];
        else if (!strcmp(argv[i], "--output") && i + 1 < argc) outdir = argv[++i];
        else { fprintf(stderr, "unknown argument: %s\n", argv[i]); return 2; }
    }
    if (size < TILE) size = TILE;
    /* Round up to the tile size: the kernel handles ragged edges, but a clean multiple
     * keeps the reported FLOP/s honest rather than counting padded work. */
    size = ((size + TILE - 1) / TILE) * TILE;

    const char *job = getenv("CGPU_JOB_ID");
    if (job) g_label = job;

    CUresult rc;
    if ((rc = cuInit(0)) != CUDA_OK) { say("cuInit failed: %s", cu_err(rc)); return 1; }

    CUdevice dev;
    if ((rc = cuDeviceGet(&dev, 0)) != CUDA_OK) { say("cuDeviceGet failed: %s", cu_err(rc)); return 1; }

    char name[128] = {0};
    cuDeviceGetName(name, (int)sizeof name, dev);
    unsigned char uu[16] = {0};
    char uuid[48] = "unknown";
    if (cuDeviceGetUuid((char *)uu, dev) == CUDA_OK) {
        snprintf(uuid, sizeof uuid,
                 "GPU-%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
                 uu[0], uu[1], uu[2], uu[3], uu[4], uu[5], uu[6], uu[7],
                 uu[8], uu[9], uu[10], uu[11], uu[12], uu[13], uu[14], uu[15]);
    }

    CUcontext ctx;
    if ((rc = cuCtxCreate_v2(&ctx, 0, dev)) != CUDA_OK) { say("cuCtxCreate failed: %s", cu_err(rc)); return 1; }

    /* Under the guard this reports the job's quota rather than the card, which is exactly
     * what we want to size against - the workload should fit its reservation, not the GPU. */
    size_t freeB = 0, totalB = 0;
    cuMemGetInfo_v2(&freeB, &totalB);
    const char *quota_env = getenv("CGPU_ALLOCATED_VRAM_MIB");
    say("device=%s uuid=%s", name, uuid);
    say("visible memory: free=%zu MiB total=%zu MiB (quota env=%s)",
        freeB >> 20, totalB >> 20, quota_env ? quota_env : "unset");

    /* Three N x N float matrices. Shrink N until the working set fits what we can see,
     * keeping a margin for the CUDA context's own allocations. */
    size_t elems = (size_t)size * (size_t)size;
    size_t need = elems * sizeof(float) * 3;
    size_t budget = freeB > (96u << 20) ? freeB - (96u << 20) : freeB / 2;
    if (vram_mib > 0) {
        size_t want = (size_t)vram_mib << 20;
        if (want < budget) budget = want;
    }
    while (need > budget && size > TILE * 4) {
        size -= TILE * 8;
        elems = (size_t)size * (size_t)size;
        need = elems * sizeof(float) * 3;
    }
    if (need > budget) {
        say("RESULT=NO_FIT working set %zu MiB exceeds the %zu MiB available",
            need >> 20, budget >> 20);
        cuCtxDestroy_v2(ctx);
        return 3;
    }
    say("plan: %ldx%ld SGEMM, working set %zu MiB, running %ld s",
        size, size, need >> 20, seconds);

    CUdeviceptr dA = 0, dB = 0, dC = 0;
    if ((rc = cuMemAlloc_v2(&dA, elems * sizeof(float))) != CUDA_OK ||
        (rc = cuMemAlloc_v2(&dB, elems * sizeof(float))) != CUDA_OK ||
        (rc = cuMemAlloc_v2(&dC, elems * sizeof(float))) != CUDA_OK) {
        /* A denial here is the guard enforcing the quota, which is a legitimate outcome
         * worth naming rather than reporting as a generic CUDA error. */
        say("allocation refused after %zu MiB: %s (quota enforcement, or the device is full)",
            need >> 20, cu_err(rc));
        if (dA) cuMemFree_v2(dA);
        if (dB) cuMemFree_v2(dB);
        cuCtxDestroy_v2(ctx);
        return 3;
    }
    say("allocated 3 x %zu MiB on the device", (elems * sizeof(float)) >> 20);

    /* 1.0f everywhere: every element of C then equals n, so the result is checkable. */
    const unsigned int one = 0x3f800000u;
    cuMemsetD32_v2(dA, one, elems);
    cuMemsetD32_v2(dB, one, elems);
    cuMemsetD32_v2(dC, 0, elems);
    cuCtxSynchronize();

    CUmodule mod = NULL;
    CUfunction fn = NULL;
    if ((rc = cuModuleLoadData(&mod, cgpu_matmul_ptx)) != CUDA_OK) {
        say("could not load PTX: %s", cu_err(rc));
        goto cleanup;
    }
    if ((rc = cuModuleGetFunction(&fn, mod, "cgpu_sgemm")) != CUDA_OK) {
        say("kernel cgpu_sgemm missing: %s", cu_err(rc));
        goto cleanup;
    }

    {
        const unsigned int grid = (unsigned int)((size + TILE - 1) / TILE);
        /* 2*N^3: one multiply and one add per inner-loop step. */
        const double flops_per_iter = 2.0 * (double)size * (double)size * (double)size;

        double t0 = now_sec(), next_report = 5.0;
        unsigned long iters = 0;
        int n_arg = (int)size;

        while (now_sec() - t0 < (double)sec onds) {
            void *args[] = { &dA, &dB, &dC, &n_arg };
            if ((rc = cuLaunchKernel(fn, grid, grid, 1, TILE, TILE, 1, 0, NULL, args, NULL)) != CUDA_OK) {
                say("kernel launch failed: %s", cu_err(rc));
                goto cleanup;
            }
            if ((rc = cuCtxSynchronize()) != CUDA_OK) {
                say("synchronize failed: %s", cu_err(rc));
                goto cleanup;
            }
            iters++;

            double el = now_sec() - t0;
            if (el >= next_report) {
                double gf = (flops_per_iter * (double)iters) / el / 1e9;
                say("progress t+%.0fs iters=%lu %.1f GFLOP/s holding=%zu MiB",
                    el, iters, gf, need >> 20);
                next_report += 5.0;
            }
        }

        double elapsed = now_sec() - t0;
        double gf = iters ? (flops_per_iter * (double)iters) / elapsed / 1e9 : 0.0;

        /* Verify the arithmetic: with A and B all ones, every C element must equal n. */
        float probe = 0.0f;
        cuMemcpyDtoH_v2(&probe, dC, sizeof probe);
        int correct = (probe > (float)size - 0.5f) && (probe < (float)size + 0.5f);

        say("done iters=%lu elapsed=%.1fs %.1f GFLOP/s element0=%.1f expected=%ld verified=%s",
            iters, elapsed, gf, (double)probe, size, correct ? "yes" : "NO");

        char path[512];
        snprintf(path, sizeof path, "%s/metrics.json", outdir);
        FILE *m = fopen(path, "w");
        if (m) {
            fprintf(m,
                    "{\n"
                    "  \"workload\": \"cgpu-matmul\",\n"
                    "  \"job_id\": \"%s\",\n"
                    "  \"device\": \"%s\",\n"
                    "  \"gpu_uuid\": \"%s\",\n"
                    "  \"matrix_size\": %ld,\n"
                    "  \"iterations\": %lu,\n"
                    "  \"elapsed_seconds\": %.3f,\n"
                    "  \"gflops\": %.2f,\n"
                    "  \"working_set_mib\": %zu,\n"
                    "  \"result_verified\": %s\n"
                    "}\n",
                    job ? job : g_label, name, uuid, size, iters, elapsed, gf,
                    need >> 20, correct ? "true" : "false");
            fclose(m);
            say("wrote %s", path);
        } else {
            /* Not fatal: the run itself succeeded and the figures are in the log. */
            say("note: could not write %s (is /outputs mounted?)", path);
        }

        if (!correct) {
            say("RESULT=WRONG_RESULT");
            cuModuleUnload(mod);
            cuMemFree_v2(dA); cuMemFree_v2(dB); cuMemFree_v2(dC);
            cuCtxDestroy_v2(ctx);
            return 1;
        }
    }

    cuModuleUnload(mod);
    cuMemFree_v2(dA);
    cuMemFree_v2(dB);
    cuMemFree_v2(dC);
    cuCtxDestroy_v2(ctx);
    say("RESULT=OK");
    return 0;

cleanup:
    if (mod) cuModuleUnload(mod);
    if (dA) cuMemFree_v2(dA);
    if (dB) cuMemFree_v2(dB);
    if (dC) cuMemFree_v2(dC);
    cuCtxDestroy_v2(ctx);
    return 1;
}
