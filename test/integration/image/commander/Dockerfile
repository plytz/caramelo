FROM debian:trixie-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      git \
      openssh-client \
      curl \
      ca-certificates \
      util-linux \
      python3 \
      sudo \
      iproute2 \
      procps \
 && rm -rf /var/lib/apt/lists/*

RUN useradd -m -s /bin/bash debian \
 && printf 'debian ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/debian \
 && chmod 0440 /etc/sudoers.d/debian \
 && install -d -m 0700 -o debian -g debian /home/debian/.ssh

CMD ["sleep", "infinity"]
