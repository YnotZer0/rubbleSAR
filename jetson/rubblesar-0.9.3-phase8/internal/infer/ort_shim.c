//go:build ort

#include "ort_shim.h"

#include <onnxruntime_c_api.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static const OrtApi *api(void) {
	static const OrtApi *a = NULL;
	if (!a) {
		const OrtApiBase *b = OrtGetApiBase();
		if (b) a = b->GetApi(ORT_API_VERSION);
	}
	return a;
}

struct orts_session {
	OrtEnv *env;
	OrtSession *sess;
	OrtMemoryInfo *mem;
	char in_name[128];
	int nout;
	char out_names[ORTS_MAX_OUT][64];
};

const char *orts_version(void) {
	const OrtApiBase *b = OrtGetApiBase();
	return b ? b->GetVersionString() : "unknown";
}

// fail copies an OrtStatus message into err and releases the status.
static int fail(OrtStatus *st, const char *what, char *err, size_t errlen) {
	if (!st) return 0;
	snprintf(err, errlen, "%s: %s", what, api()->GetErrorMessage(st));
	api()->ReleaseStatus(st);
	return 1;
}

static void free_name(const OrtApi *a, OrtAllocator *alloc, char *name) {
	OrtStatus *st = a->AllocatorFree(alloc, name);
	if (st) a->ReleaseStatus(st);
}

orts_session *orts_open(const char *path, int threads, char *err, size_t errlen) {
	const OrtApi *a = api();
	if (!a) {
		snprintf(err, errlen, "ONNX Runtime does not provide API version %d (library too old?)", ORT_API_VERSION);
		return NULL;
	}
	orts_session *s = calloc(1, sizeof(*s));
	OrtSessionOptions *so = NULL;
	OrtAllocator *alloc = NULL;
	if (fail(a->CreateEnv(ORT_LOGGING_LEVEL_WARNING, "rubblesar", &s->env), "CreateEnv", err, errlen)) goto bad;
	if (fail(a->CreateSessionOptions(&so), "CreateSessionOptions", err, errlen)) goto bad;
	if (threads > 0 && fail(a->SetIntraOpNumThreads(so, threads), "SetIntraOpNumThreads", err, errlen)) goto bad;
	if (fail(a->SetSessionGraphOptimizationLevel(so, ORT_ENABLE_ALL), "SetSessionGraphOptimizationLevel", err, errlen)) goto bad;
	if (fail(a->CreateSession(s->env, path, so, &s->sess), "CreateSession", err, errlen)) goto bad;
	a->ReleaseSessionOptions(so);
	so = NULL;
	if (fail(a->CreateCpuMemoryInfo(OrtArenaAllocator, OrtMemTypeDefault, &s->mem), "CreateCpuMemoryInfo", err, errlen)) goto bad;
	if (fail(a->GetAllocatorWithDefaultOptions(&alloc), "GetAllocator", err, errlen)) goto bad;
	char *name = NULL;
	if (fail(a->SessionGetInputName(s->sess, 0, alloc, &name), "SessionGetInputName", err, errlen)) goto bad;
	snprintf(s->in_name, sizeof(s->in_name), "%s", name);
	free_name(a, alloc, name);
	size_t n = 0;
	if (fail(a->SessionGetOutputCount(s->sess, &n), "SessionGetOutputCount", err, errlen)) goto bad;
	if (n < 1 || n > ORTS_MAX_OUT) {
		snprintf(err, errlen, "model has %zu outputs (expected 1 to %d)", n, ORTS_MAX_OUT);
		goto bad;
	}
	s->nout = (int)n;
	for (size_t i = 0; i < n; i++) {
		if (fail(a->SessionGetOutputName(s->sess, i, alloc, &name), "SessionGetOutputName", err, errlen)) goto bad;
		snprintf(s->out_names[i], sizeof(s->out_names[i]), "%s", name);
		free_name(a, alloc, name);
	}
	return s;
bad:
	if (so) a->ReleaseSessionOptions(so);
	orts_close(s);
	return NULL;
}

const char *orts_input_name(orts_session *s) { return s->in_name; }

int orts_run(orts_session *s, const float *input, const int64_t *shape, int rank, orts_outputs *out, char *err, size_t errlen) {
	const OrtApi *a = api();
	memset(out, 0, sizeof(*out));
	size_t count = 1;
	for (int i = 0; i < rank; i++) count *= (size_t)shape[i];
	OrtValue *in = NULL;
	OrtValue *outs[ORTS_MAX_OUT] = {0};
	int rc = 1;
	if (fail(a->CreateTensorWithDataAsOrtValue(s->mem, (void *)input, count * sizeof(float), shape, (size_t)rank,
	                                           ONNX_TENSOR_ELEMENT_DATA_TYPE_FLOAT, &in), "CreateTensor", err, errlen)) goto done;
	const char *in_names[1] = {s->in_name};
	const char *out_names[ORTS_MAX_OUT];
	for (int i = 0; i < s->nout; i++) out_names[i] = s->out_names[i];
	if (fail(a->Run(s->sess, NULL, in_names, (const OrtValue *const *)&in, 1, out_names, (size_t)s->nout, outs), "Run", err, errlen)) goto done;
	out->n = s->nout;
	for (int i = 0; i < s->nout; i++) {
		OrtTensorTypeAndShapeInfo *info = NULL;
		if (fail(a->GetTensorTypeAndShape(outs[i], &info), "GetTensorTypeAndShape", err, errlen)) goto done;
		ONNXTensorElementDataType t;
		size_t dims = 0, elems = 0;
		OrtStatus *st = a->GetTensorElementType(info, &t);
		if (!st) st = a->GetDimensionsCount(info, &dims);
		if (!st && dims > ORTS_MAX_RANK) {
			a->ReleaseTensorTypeAndShapeInfo(info);
			snprintf(err, errlen, "output %d has rank %zu", i, dims);
			goto done;
		}
		if (!st) st = a->GetDimensions(info, out->shape[i], dims);
		if (!st) st = a->GetTensorShapeElementCount(info, &elems);
		a->ReleaseTensorTypeAndShapeInfo(info);
		if (fail(st, "output shape", err, errlen)) goto done;
		if (t != ONNX_TENSOR_ELEMENT_DATA_TYPE_FLOAT) {
			snprintf(err, errlen, "output %s is not float32 (type %d)", s->out_names[i], (int)t);
			goto done;
		}
		out->rank[i] = (int)dims;
		snprintf(out->name[i], sizeof(out->name[i]), "%s", s->out_names[i]);
		float *src = NULL;
		if (fail(a->GetTensorMutableData(outs[i], (void **)&src), "GetTensorMutableData", err, errlen)) goto done;
		out->data[i] = malloc(elems * sizeof(float) + 1);
		memcpy(out->data[i], src, elems * sizeof(float));
	}
	rc = 0;
done:
	if (in) a->ReleaseValue(in);
	for (int i = 0; i < ORTS_MAX_OUT; i++)
		if (outs[i]) a->ReleaseValue(outs[i]);
	if (rc) orts_free_outputs(out);
	return rc;
}

void orts_free_outputs(orts_outputs *out) {
	for (int i = 0; i < ORTS_MAX_OUT; i++) {
		free(out->data[i]);
		out->data[i] = NULL;
	}
}

void orts_close(orts_session *s) {
	if (!s) return;
	const OrtApi *a = api();
	if (s->mem) a->ReleaseMemoryInfo(s->mem);
	if (s->sess) a->ReleaseSession(s->sess);
	if (s->env) a->ReleaseEnv(s->env);
	free(s);
}
