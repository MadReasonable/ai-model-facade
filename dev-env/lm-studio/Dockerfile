# LM Studio's headless daemon, llmster, which LM Studio publishes no image
# for. This installs the archive its install script (lmstudio.ai/install.sh)
# downloads for Linux x86_64 without an NVIDIA GPU, pinned to one version
# and checked against its SHA-512, then runs the archive's own bootstrap,
# which installs llmster and lms under /root/.lmstudio. LM Studio is
# proprietary, so the image is built locally and not published.
FROM debian:trixie-20261005-slim

# libatomic1 and libgomp1 are what the install script checks for.
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl libatomic1 libgomp1 \
 && rm -rf /var/lib/apt/lists/*

ARG LLMSTER_VERSION=0.0.25-1
ARG LLMSTER_SHA512=4d119af740379fc7509894d761a85183420a6dd3cb994596a1b6e4b37208a5d3e8cb94ffe583115b1473a6722bab5fffc09b90eb22c755f5dde1458774ab4951
RUN curl -fsSL -o /tmp/llmster.tar.gz "https://llmster.lmstudio.ai/download/${LLMSTER_VERSION}-linux-x64.full.tar.gz" \
 && echo "${LLMSTER_SHA512}  /tmp/llmster.tar.gz" | sha512sum -c - \
 && mkdir /tmp/llmster \
 && tar xf /tmp/llmster.tar.gz -C /tmp/llmster \
 && LMS_BOOTSTRAP_INSTALL_SH=1 /tmp/llmster/llmster bootstrap \
 && rm -rf /tmp/llmster /tmp/llmster.tar.gz

ENV PATH=/root/.lmstudio/bin:$PATH
COPY start.sh /start.sh
ENTRYPOINT ["/bin/sh", "/start.sh"]
