"""Stand-in for torch so the training script's own logic can be tested without a GPU."""
__version__ = "0.0-fake"


class _Version:
    cuda = "12.8"


version = _Version()


class cuda:
    available = True
    oom_batches = set()

    @staticmethod
    def is_available():
        return cuda.available

    @staticmethod
    def get_device_name(i=0):
        return "Fake Quadro RTX 4000"

    @staticmethod
    def empty_cache():
        pass


class OutOfMemoryError(RuntimeError):
    pass
