/*
 * cgpu_matmul_kernel.cu - tiled SGEMM for the CGPU matmul workload
 * ================================================================
 *
 * Compiled once to PTX by build.sh and committed as cgpu_matmul_ptx.h, then loaded at
 * runtime through the CUDA driver API. The reasoning is the same as for the e2e probe:
 * the workload must build with nothing but gcc (the GPU workers have no CUDA toolkit) and
 * must link libcuda *dynamically*, because that is the property that lets the CGPU
 * allocation guard interpose its allocations. Linking cudart to get <<<>>> syntax would
 * walk straight into the static-linkage trap the guard cannot police.
 *
 * Why tiled rather than the naive triple loop:
 *   A naive SGEMM re-reads both operands from global memory for every output element, so it
 *   is memory-bound and lands around a few hundred GFLOP/s - the SMs spend their time
 *   waiting rather than computing. Staging TILE x TILE blocks in shared memory gives each
 *   loaded value TILE reuses, which makes the kernel arithmetic-bound and actually puts the
 *   SMs under load. That matters here because the whole point of this workload is to show
 *   real GPU utilisation, so the kernel has to be the sort of thing that saturates a card
 *   rather than one that merely looks busy while stalled on memory.
 *
 * C = A * B, all row-major, square N x N. One thread computes one element of C.
 */

#define TILE 16

extern "C" __global__ void cgpu_sgemm(const float *A, const float *B, float *C, int n)
{
    __shared__ float as[TILE][TILE];
    __shared__ float bs[TILE][TILE];

    const int row = blockIdx.y * TILE + threadIdx.y;
    const int col = blockIdx.x * TILE + threadIdx.x;

    float acc = 0.0f;

    /* Walk the tiles along the shared dimension. */
    for (int t = 0; t < (n + TILE - 1) / TILE; t++) {
        const int aCol = t * TILE + threadIdx.x;
        const int bRow = t * TILE + threadIdx.y;

        /* Guard the loads so a non-multiple-of-TILE n stays correct: out-of-range
         * contributions are zeroed rather than reading past the allocation. */
        as[threadIdx.y][threadIdx.x] = (row < n && aCol < n) ? A[(size_t)row * n + aCol] : 0.0f;
        bs[threadIdx.y][threadIdx.x] = (bRow < n && col < n) ? B[(size_t)bRow * n + col] : 0.0f;

        __syncthreads();

#pragma unroll
        for (int k = 0; k < TILE; k++) acc = __fmaf_rn(as[threadIdx.y][k], bs[k][threadIdx.x], acc);

        /* Both barriers are required: the second stops a fast warp overwriting the tile
         * while a slower one is still reading it. */
        __syncthreads();
    }

    if (row < n && col < n) C[(size_t)row * n + col] = acc;
}
