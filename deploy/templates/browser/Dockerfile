# browser — Playwright 1.64 (Python package and Node, with Chromium, Firefox and WebKit) for
# headless browsing agents. Chromium runs inside the gVisor sandbox already, so launch it with
# --no-sandbox (chromium.launch(args=["--no-sandbox"])).
FROM mcr.microsoft.com/playwright/python@sha256:7d030ac0638b279ecc72493f8b6706fecef80a893679a23b02f912905c4915c0
USER root
# setuptools 84.2.1 vendors wheel 0.46.3 and jaraco.context 6.1.0 (it ships no pkg_resources; Ubuntu's
# python3-pkg-resources still provides one). virtualenv 21.15.6 bundles pip 26.3 and setuptools 84.2.1.
# /root/.cache/virtualenv is the upstream image's virtualenv seed cache, not installed software: it holds
# an unpacked pip 26.2.1 whose vendored SBOM lists old certifi/msgpack/urllib3/setuptools. virtualenv
# rebuilds it on demand from its bundled wheels, so drop it rather than ship the stale copy.
RUN apt-get update && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends sudo python3-pip \
 && rm -rf /var/lib/apt/lists/* \
 && pip install --no-cache-dir --break-system-packages playwright==1.64.1 \
 && python3 -m pip install --no-cache-dir --break-system-packages --upgrade \
      certifi==2026.10.23 filelock==4.2.1 msgpack==1.2.3 setuptools==84.2.1 \
      urllib3==2.8.0 virtualenv==21.15.6 pip==26.3 \
 && rm -rf /root/.cache/virtualenv
COPY common/setup-user.sh /usr/local/sbin/proxcloud-setup-user
RUN proxcloud-setup-user
WORKDIR /home/user
