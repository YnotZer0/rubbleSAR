# RubbleSAR TAK server: taky (https://github.com/tkuester/taky, MIT licence),
# a small TAK server and CoT router, on plain TCP 8087 (plan decision D10).
# Built and run by scripts/zbook/34-taky.sh; the service is rubblesar-taky.
ARG TAKY_BASE_IMAGE=python:3.11-slim
FROM ${TAKY_BASE_IMAGE}
ARG TAKY_VERSION=0.10
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_DISABLE_PIP_VERSION_CHECK=1
RUN pip install --no-cache-dir "taky==${TAKY_VERSION}" \
 && useradd --system --uid 8087 --home-dir /var/taky --shell /usr/sbin/nologin taky \
 && mkdir -p /var/taky /etc/taky \
 && chown taky:taky /var/taky
USER taky
WORKDIR /var/taky
EXPOSE 8087
# No /etc/taky/taky.conf: taky's defaults are plain TCP on 0.0.0.0:8087, no SSL.
CMD ["taky", "-l", "info"]
