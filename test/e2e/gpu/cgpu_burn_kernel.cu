/*
 * cgpu_burn_kernel.cu - the compute kernel used by cgpu_alloc_probe
 * ================================================================
 *
 * This file is NOT compiled during a normal build. It is compiled once, by hand, to PTX
 * (see gen_ptx.sh) and the result is committed as cgpu_burn_ptx.h. The probe then loads
 * that PTX at runtime through the CUDA driver API (cuModuleLoadData / cuLaunchKernel).
 *
 * Why go the long way round:
 *   - the probe must stay buildable with nothing but gcc and libcuda, because the GPU
 *     worker is a WSL node with no CUDA toolkit installed,
 *   - the probe must keep linking libcuda *dynamically*, which is the property that makes
 *     its allocations interposable by the CGPU guard. Linking the CUDA runtime (cudart) to
 *     get <<<...>>> launch syntax would risk the static-linkage trap documented in the A3
 *     evidence: a workload the guard cannot police at all.
 *
 * The kernel itself is a dependent chain of fused multiply-adds per thread. The chain is
 * dependent on purpose: each FMA needs the previous result, so the compiler cannot vectorise
 * or hoist it away and the SMs are genuinely occupied for the duration. It reads and writes
 * device memory the probe allocated, so the work is tied to the memory under quota.
 */

extern "C" __global__ void cgpu_burn(float *out, unsigned long long n, unsigned int iters)
{
    unsigned long long i = (unsigned long long)blockIdx.x * blockDim.x + threadIdx.x;
    if (i >= n) return;

    float a = out[i];
    const float b = 1.0000001f;
    const float c = 0.0000001f;
    for (unsigned int k = 0; k < iters; k++) a = __fmaf_rn(a, b, c);

    out[i] = a;
}
