# code-interpreter-v1 — what the e2b-code-interpreter SDKs start: a Jupyter server (Unix socket)
# behind E2B's FastAPI server on :49999, with Python, JavaScript and Bash kernels. Built like
# e2b-dev/code-interpreter template/template.py with is_docker=True (Apache-2.0; its files come
# from the pinned "upstream" build context). R and Java kernels are left out (size, start time).
# BASE_IMAGE=a CUDA PyTorch image builds code-interpreter-gpu.
ARG BASE_IMAGE=docker.io/library/python@sha256:bcaf104ef2827b827bdd0b8b351742783fc050296e1dc150113e6ec869e652d0
FROM ${BASE_IMAGE}
ENV PIP_DEFAULT_TIMEOUT=100 \
    NODE_OPTIONS=--use-openssl-ca \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1 \
    DEBIAN_FRONTEND=noninteractive
USER root
WORKDIR /root
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential curl git util-linux jq sudo fonts-noto-cjk ca-certificates gnupg \
 && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/* \
 && echo 'export NODE_OPTIONS="--use-openssl-ca${NODE_OPTIONS:+ $NODE_OPTIONS}"' > /etc/profile.d/node-ca.sh
RUN python -m pip install --upgrade pip==26.2.2
COPY --from=upstream template/charts charts
COPY --from=upstream template/requirements.txt requirements.txt
RUN pip install --no-cache-dir -r requirements.txt \
 && ipython kernel install --name python3 --user
RUN npm install -g --unsafe-perm git+https://github.com/e2b-dev/ijavascript.git \
 && ijsinstall --install=global \
 && pip install --no-cache-dir bash_kernel && python -m bash_kernel.install
COPY --from=upstream template/server .server
RUN python -m venv .server/.venv \
 && .server/.venv/bin/python -m pip install --upgrade pip==26.2.2 \
 && .server/.venv/bin/pip install --no-cache-dir -r .server/requirements.txt
COPY --from=upstream template/matplotlibrc .config/matplotlib/.matplotlibrc
COPY --from=upstream template/jupyter-healthcheck.sh template/start-up.sh template/jupyter_server_config.py .jupyter/
COPY --from=upstream template/ipython_kernel_config.py .ipython/profile_default/
COPY --from=upstream template/startup_scripts .ipython/profile_default/startup
RUN chmod +x .jupyter/jupyter-healthcheck.sh .jupyter/start-up.sh
COPY common/setup-user.sh /usr/local/sbin/proxcloud-setup-user
RUN proxcloud-setup-user
WORKDIR /home/user
