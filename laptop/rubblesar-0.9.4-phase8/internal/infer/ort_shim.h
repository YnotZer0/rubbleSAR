// Minimal C interface over the ONNX Runtime C API used by ort.go.
#ifndef RUBBLESAR_ORT_SHIM_H
#define RUBBLESAR_ORT_SHIM_H

#include <stddef.h>
#include <stdint.h>

#define ORTS_MAX_OUT 4
#define ORTS_MAX_RANK 8

typedef struct orts_session orts_session;

typedef struct {
	int n;                                   // number of outputs
	int rank[ORTS_MAX_OUT];
	int64_t shape[ORTS_MAX_OUT][ORTS_MAX_RANK];
	float *data[ORTS_MAX_OUT];               // malloc'ed; free with orts_free_outputs
	char name[ORTS_MAX_OUT][64];
} orts_outputs;

// orts_version returns the runtime version string.
const char *orts_version(void);

// orts_open loads a model. Returns NULL and fills err on failure.
orts_session *orts_open(const char *path, int threads, char *err, size_t errlen);

// orts_input_name returns the model's first input name.
const char *orts_input_name(orts_session *s);

// orts_run runs the model on one float32 input of the given shape.
// Returns 0 on success.
int orts_run(orts_session *s, const float *input, const int64_t *shape, int rank, orts_outputs *out, char *err, size_t errlen);

void orts_free_outputs(orts_outputs *out);
void orts_close(orts_session *s);

#endif
