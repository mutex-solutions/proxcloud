# desktop — what the e2b-desktop SDKs start: Ubuntu 22.04 with Xvfb, XFCE, x11vnc, E2B's noVNC
# (port 6080), xdotool/scrot, Firefox ESR and LibreOffice. Built like e2b-dev/desktop
# template/template.py (Apache-2.0; config files from the pinned "upstream" context), without
# Google Chrome and VS Code (their licences forbid redistributing them in an image).
FROM docker.io/library/ubuntu@sha256:8a2321a92b438238f93b4346fff61bc4259c99fdd4d9b52ee86f153fee0c344f
ENV DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=high \
    PIP_DEFAULT_TIMEOUT=100 PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_CACHE_DIR=1
USER root
# Launchpad answers 503 for minutes at a time (its API in add-apt-repository, the PPA index and
# the package pool), so the PPA step is retried as a whole; "Error-Mode=any" makes a failed index
# fetch fail apt-get update instead of surfacing later as "no installation candidate".
RUN apt-get update && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends \
      xserver-xorg x11-xserver-utils xvfb x11-utils xauth xfce4 xfce4-goodies util-linux sudo \
      curl git wget python3-pip xdotool scrot ffmpeg x11vnc net-tools netcat x11-apps libreoffice \
      xpdf gedit xpaint tint2 galculator pcmanfm software-properties-common apt-transport-https \
      libgtk-3-bin dbus-x11 ca-certificates gnupg gpg-agent \
 && for i in 1 2 3 4 5; do \
      add-apt-repository -y ppa:mozillateam/ppa \
      && apt-get update -o APT::Update::Error-Mode=any \
      && apt-get install -y --no-install-recommends firefox-esr && break; \
      [ "$i" -lt 5 ] || exit 1; echo "Launchpad PPA step failed (attempt $i of 5), retrying" >&2; sleep $((i * 30)); \
    done \
 && rm -rf /var/lib/apt/lists/* \
 && pip install --no-cache-dir numpy \
 && git clone --depth 1 --branch e2b-desktop https://github.com/e2b-dev/noVNC.git /opt/noVNC \
 && ln -s /opt/noVNC/vnc.html /opt/noVNC/index.html \
 && git clone --depth 1 --branch v0.12.0 https://github.com/novnc/websockify.git /opt/noVNC/utils/websockify \
 && ln -sf /usr/bin/xfce4-terminal.wrapper /etc/alternatives/x-terminal-emulator \
 && update-alternatives --set x-www-browser /usr/bin/firefox-esr \
 && update-desktop-database /usr/share/applications/
COPY --from=upstream template/files/wallpaper.png /usr/share/backgrounds/xfce/wallpaper.png
COPY --from=upstream template/files/xfce4-desktop.xml /home/user/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-desktop.xml
COPY --from=upstream template/files/screensaver.desktop /home/user/.config/autostart/screensaver.desktop
COPY --from=upstream template/files/firefox-policies.json /usr/lib/firefox-esr/distribution/policies.json
COPY --from=upstream template/files/firefox-autoconfig.js /usr/lib/firefox-esr/defaults/pref/autoconfig.js
COPY --from=upstream template/files/firefox.cfg /usr/lib/firefox-esr/firefox.cfg
COPY common/setup-user.sh /usr/local/sbin/proxcloud-setup-user
RUN proxcloud-setup-user
WORKDIR /home/user
