# browser — Playwright 1.64 (Python package and Node, with Chromium, Firefox and WebKit) for
# headless browsing agents. Chromium runs inside the gVisor sandbox already, so launch it with
# --no-sandbox (chromium.launch(args=["--no-sandbox"])).
FROM mcr.microsoft.com/playwright/python@sha256:7d030ac0638b279ecc72493f8b6706fecef80a893679a23b02f912905c4915c0
USER root
RUN apt-get update && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends sudo python3-pip \
 && rm -rf /var/lib/apt/lists/* \
 && pip install --no-cache-dir --break-system-packages playwright==1.64.1 \
 && python3 -m pip install --no-cache-dir --break-system-packages --upgrade \
      certifi==2026.10.19 filelock==4.2.1 msgpack==1.2.3 setuptools==78.1.1 \
      urllib3==2.8.0 virtualenv==21.14.9 pip==26.3
COPY common/setup-user.sh /usr/local/sbin/proxcloud-setup-user
RUN proxcloud-setup-user
WORKDIR /home/user
