// Minimal C interface over the TensorRT C++ runtime used by trt.go.
#ifndef RUBBLESAR_TRT_SHIM_H
#define RUBBLESAR_TRT_SHIM_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define TRTS_MAX_OUT 4
#define TRTS_MAX_RANK 8

typedef struct trts_engine trts_engine;

typedef struct {
	int max_batch;            // from the optimisation profile (or the fixed batch)
	int channels, height, width;
	int n_out;
	char in_name[64];
	char out_name[TRTS_MAX_OUT][64];
	char device[128];         // GPU name
	int trt_version;          // e.g. 100300 for 10.3.0
} trts_info;

typedef struct {
	int rank[TRTS_MAX_OUT];
	int64_t shape[TRTS_MAX_OUT][TRTS_MAX_RANK];
	float *data[TRTS_MAX_OUT]; // malloc'ed; free with trts_free_outputs
} trts_outputs;

// trts_open deserialises an engine file and allocates buffers for the
// largest batch its profile allows. NULL and err on failure.
trts_engine *trts_open(const char *path, char *err, size_t errlen);
void trts_info_get(trts_engine *e, trts_info *info);

// trts_run copies batch images (float32 NCHW) to the GPU, runs the engine and
// copies every output back. Returns 0 on success.
int trts_run(trts_engine *e, const float *input, int batch, trts_outputs *out, char *err, size_t errlen);

void trts_free_outputs(trts_outputs *out);
void trts_close(trts_engine *e);

#ifdef __cplusplus
}
#endif
#endif
