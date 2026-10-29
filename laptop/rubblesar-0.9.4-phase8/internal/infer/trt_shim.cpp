//go:build trt

// TensorRT 10 runtime shim: deserialise an engine, run it on one CUDA
// stream, and hand float32 outputs back to Go.
#include "trt_shim.h"

#include <NvInfer.h>
#include <cuda_runtime_api.h>

#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <exception>
#include <fstream>
#include <mutex>
#include <string>
#include <vector>

namespace {

class Logger : public nvinfer1::ILogger {
public:
	void log(Severity severity, nvinfer1::AsciiChar const *msg) noexcept override {
		if (severity > Severity::kWARNING) return;
		std::lock_guard<std::mutex> lk(mu);
		if (severity <= Severity::kERROR) errors = true;
		if (!last.empty()) last += " | ";
		last += msg;
		if (last.size() > 2000) last.erase(0, last.size() - 2000);
	}
	std::string take() {
		std::lock_guard<std::mutex> lk(mu);
		std::string s = last;
		last.clear();
		return s;
	}
	// failed reports (and clears) whether TensorRT logged an error since the last call.
	bool failed() {
		std::lock_guard<std::mutex> lk(mu);
		bool e = errors;
		errors = false;
		return e;
	}

private:
	std::mutex mu;
	std::string last;
	bool errors = false;
};

int64_t volume(nvinfer1::Dims const &d) {
	int64_t v = 1;
	for (int i = 0; i < d.nbDims; i++) v *= d.d[i] < 0 ? 0 : d.d[i];
	return v;
}

} // namespace

struct trts_engine {
	Logger logger;
	nvinfer1::IRuntime *runtime = nullptr;
	nvinfer1::ICudaEngine *engine = nullptr;
	nvinfer1::IExecutionContext *ctx = nullptr;
	cudaStream_t stream = nullptr;
	trts_info info{};
	void *d_in = nullptr;
	size_t in_bytes = 0;
	void *d_out[TRTS_MAX_OUT] = {};
	size_t out_bytes[TRTS_MAX_OUT] = {};
};

static void set_err(char *err, size_t n, std::string const &msg) { snprintf(err, n, "%s", msg.c_str()); }

static bool cuda_ok(cudaError_t rc, char const *what, char *err, size_t n) {
	if (rc == cudaSuccess) return true;
	snprintf(err, n, "%s: %s", what, cudaGetErrorString(rc));
	return false;
}

static trts_engine *open_engine(const char *path, char *err, size_t errlen);

// cuda_usable checks that this process can use the GPU before TensorRT is
// touched: when CUDA cannot start (on a Jetson usually because the user may
// not open /dev/nvmap or /dev/nvgpu), TensorRT itself may crash.
static bool cuda_usable(char *err, size_t n) {
	int count = 0;
	cudaError_t rc = cudaGetDeviceCount(&count);
	if (rc == cudaSuccess && count == 0) {
		snprintf(err, n, "CUDA is not usable by this process: no GPU found");
		return false;
	}
	if (rc == cudaSuccess) rc = cudaFree(nullptr); // creates the CUDA context
	void *probe = nullptr;
	if (rc == cudaSuccess) rc = cudaMalloc(&probe, 1 << 20); // GPU memory (nvmap on a Jetson)
	if (probe) cudaFree(probe);
	if (rc != cudaSuccess) {
		snprintf(err, n, "CUDA is not usable by this process: %s (%s)", cudaGetErrorString(rc), cudaGetErrorName(rc));
		return false;
	}
	return true;
}

extern "C" trts_engine *trts_open(const char *path, char *err, size_t errlen) {
	// No C++ exception may escape into Go: it would abort the whole program.
	try {
		if (!cuda_usable(err, errlen)) return nullptr;
		return open_engine(path, err, errlen);
	} catch (std::exception const &ex) {
		set_err(err, errlen, std::string("TensorRT threw an exception: ") + ex.what());
	} catch (...) {
		set_err(err, errlen, "TensorRT threw an unknown exception");
	}
	return nullptr;
}

static trts_engine *open_engine(const char *path, char *err, size_t errlen) {
	std::ifstream f(path, std::ios::binary | std::ios::ate);
	if (!f) {
		set_err(err, errlen, std::string("cannot open engine file ") + path);
		return nullptr;
	}
	std::streamsize size = f.tellg();
	f.seekg(0, std::ios::beg);
	std::vector<char> blob(static_cast<size_t>(size));
	if (!f.read(blob.data(), size)) {
		set_err(err, errlen, std::string("cannot read engine file ") + path);
		return nullptr;
	}
	auto *e = new trts_engine();
	cudaDeviceProp prop{};
	if (cudaGetDeviceProperties(&prop, 0) == cudaSuccess) snprintf(e->info.device, sizeof(e->info.device), "%.127s", prop.name);
	e->info.trt_version = NV_TENSORRT_VERSION;
	e->runtime = nvinfer1::createInferRuntime(e->logger);
	if (!e->runtime || e->logger.failed()) {
		set_err(err, errlen, "createInferRuntime failed: " + e->logger.take());
		trts_close(e);
		return nullptr;
	}
	e->engine = e->runtime->deserializeCudaEngine(blob.data(), blob.size());
	if (!e->engine || e->logger.failed()) {
		set_err(err, errlen, "deserializeCudaEngine failed (engine built with another TensorRT version or GPU?): " + e->logger.take());
		trts_close(e);
		return nullptr;
	}
	e->ctx = e->engine->createExecutionContext();
	if (!e->ctx || e->logger.failed()) {
		set_err(err, errlen, "createExecutionContext failed: " + e->logger.take());
		trts_close(e);
		return nullptr;
	}
	if (!cuda_ok(cudaStreamCreate(&e->stream), "cudaStreamCreate", err, errlen)) {
		trts_close(e);
		return nullptr;
	}
	int n = e->engine->getNbIOTensors();
	int nin = 0;
	for (int i = 0; i < n; i++) {
		char const *name = e->engine->getIOTensorName(i);
		bool input = e->engine->getTensorIOMode(name) == nvinfer1::TensorIOMode::kINPUT;
		if (e->engine->getTensorDataType(name) != nvinfer1::DataType::kFLOAT) {
			set_err(err, errlen, std::string("tensor ") + name + " is not float32; build the engine without --inputIOFormats/--outputIOFormats");
			trts_close(e);
			return nullptr;
		}
		if (input) {
			if (nin++ > 0) {
				set_err(err, errlen, "engine has more than one input");
				trts_close(e);
				return nullptr;
			}
			snprintf(e->info.in_name, sizeof(e->info.in_name), "%s", name);
			nvinfer1::Dims d = e->engine->getTensorShape(name);
			nvinfer1::Dims mx = d;
			if (e->engine->getNbOptimizationProfiles() > 0 && d.d[0] < 0) {
				mx = e->engine->getProfileShape(name, 0, nvinfer1::OptProfileSelector::kMAX);
			}
			if (mx.nbDims != 4 || mx.d[1] != 3 || mx.d[2] <= 0 || mx.d[3] <= 0) {
				set_err(err, errlen, "unexpected input shape (want N x 3 x H x W)");
				trts_close(e);
				return nullptr;
			}
			e->info.max_batch = static_cast<int>(mx.d[0]);
			e->info.channels = static_cast<int>(mx.d[1]);
			e->info.height = static_cast<int>(mx.d[2]);
			e->info.width = static_cast<int>(mx.d[3]);
		} else {
			if (e->info.n_out >= TRTS_MAX_OUT) {
				set_err(err, errlen, "engine has too many outputs");
				trts_close(e);
				return nullptr;
			}
			snprintf(e->info.out_name[e->info.n_out], 64, "%s", name);
			e->info.n_out++;
		}
	}
	if (nin != 1 || e->info.n_out < 1) {
		set_err(err, errlen, "engine must have one input and at least one output");
		trts_close(e);
		return nullptr;
	}
	// Size every buffer for the largest batch.
	nvinfer1::Dims in{4, {e->info.max_batch, 3, e->info.height, e->info.width}};
	if (!e->ctx->setInputShape(e->info.in_name, in)) {
		set_err(err, errlen, "setInputShape(max batch) failed: " + e->logger.take());
		trts_close(e);
		return nullptr;
	}
	e->in_bytes = static_cast<size_t>(volume(in)) * sizeof(float);
	if (!cuda_ok(cudaMalloc(&e->d_in, e->in_bytes), "cudaMalloc(input)", err, errlen)) {
		trts_close(e);
		return nullptr;
	}
	for (int i = 0; i < e->info.n_out; i++) {
		nvinfer1::Dims d = e->ctx->getTensorShape(e->info.out_name[i]);
		e->out_bytes[i] = static_cast<size_t>(volume(d)) * sizeof(float);
		if (e->out_bytes[i] == 0 || !cuda_ok(cudaMalloc(&e->d_out[i], e->out_bytes[i]), "cudaMalloc(output)", err, errlen)) {
			if (e->out_bytes[i] == 0) set_err(err, errlen, std::string("output ") + e->info.out_name[i] + " has an unknown size");
			trts_close(e);
			return nullptr;
		}
	}
	return e;
}

extern "C" void trts_info_get(trts_engine *e, trts_info *info) {
	if (e) *info = e->info;
}

static int run_engine(trts_engine *e, const float *input, int batch, trts_outputs *out, char *err, size_t errlen);

extern "C" int trts_run(trts_engine *e, const float *input, int batch, trts_outputs *out, char *err, size_t errlen) {
	std::memset(out, 0, sizeof(*out));
	if (!e) {
		set_err(err, errlen, "engine is not open");
		return 1;
	}
	try {
		return run_engine(e, input, batch, out, err, errlen);
	} catch (std::exception const &ex) {
		set_err(err, errlen, std::string("TensorRT threw an exception: ") + ex.what());
	} catch (...) {
		set_err(err, errlen, "TensorRT threw an unknown exception");
	}
	trts_free_outputs(out);
	return 1;
}

static int run_engine(trts_engine *e, const float *input, int batch, trts_outputs *out, char *err, size_t errlen) {
	if (batch < 1 || batch > e->info.max_batch) {
		snprintf(err, errlen, "batch %d outside 1..%d", batch, e->info.max_batch);
		return 1;
	}
	nvinfer1::Dims in{4, {batch, 3, e->info.height, e->info.width}};
	if (!e->ctx->setInputShape(e->info.in_name, in)) {
		set_err(err, errlen, "setInputShape failed: " + e->logger.take());
		return 1;
	}
	size_t bytes = static_cast<size_t>(volume(in)) * sizeof(float);
	if (!cuda_ok(cudaMemcpyAsync(e->d_in, input, bytes, cudaMemcpyHostToDevice, e->stream), "copy to GPU", err, errlen)) return 1;
	e->ctx->setTensorAddress(e->info.in_name, e->d_in);
	for (int i = 0; i < e->info.n_out; i++) e->ctx->setTensorAddress(e->info.out_name[i], e->d_out[i]);
	if (!e->ctx->enqueueV3(e->stream)) {
		set_err(err, errlen, "enqueueV3 failed: " + e->logger.take());
		return 1;
	}
	for (int i = 0; i < e->info.n_out; i++) {
		nvinfer1::Dims d = e->ctx->getTensorShape(e->info.out_name[i]);
		out->rank[i] = d.nbDims;
		for (int k = 0; k < d.nbDims && k < TRTS_MAX_RANK; k++) out->shape[i][k] = d.d[k];
		size_t ob = static_cast<size_t>(volume(d)) * sizeof(float);
		out->data[i] = static_cast<float *>(std::malloc(ob + 1));
		if (!cuda_ok(cudaMemcpyAsync(out->data[i], e->d_out[i], ob, cudaMemcpyDeviceToHost, e->stream), "copy from GPU", err, errlen)) {
			trts_free_outputs(out);
			return 1;
		}
	}
	if (!cuda_ok(cudaStreamSynchronize(e->stream), "cudaStreamSynchronize", err, errlen)) {
		trts_free_outputs(out);
		return 1;
	}
	return 0;
}

extern "C" void trts_free_outputs(trts_outputs *out) {
	for (int i = 0; i < TRTS_MAX_OUT; i++) {
		std::free(out->data[i]);
		out->data[i] = nullptr;
	}
}

extern "C" void trts_close(trts_engine *e) {
	if (!e) return;
	if (e->stream) cudaStreamSynchronize(e->stream);
	for (int i = 0; i < TRTS_MAX_OUT; i++)
		if (e->d_out[i]) cudaFree(e->d_out[i]);
	if (e->d_in) cudaFree(e->d_in);
	if (e->stream) cudaStreamDestroy(e->stream);
	delete e->ctx;
	delete e->engine;
	delete e->runtime;
	delete e;
}
