FROM golang:1.26
RUN sed -i 's|http://deb.debian.org|https://deb.debian.org|g' /etc/apt/sources.list.d/debian.sources \
    && timeout 120 apt-get -o Acquire::https::Timeout=15 -o Acquire::Retries=2 update -qq \
    && DEBIAN_FRONTEND=noninteractive timeout 120 apt-get -o Acquire::https::Timeout=15 -o Acquire::Retries=2 install -y -qq --no-install-recommends libopus-dev libopusfile-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*
