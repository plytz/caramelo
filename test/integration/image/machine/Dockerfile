FROM debian:trixie-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      systemd \
      systemd-sysv \
      dbus \
      sudo \
      curl \
      ca-certificates \
      openssh-server \
      iproute2 \
      iputils-ping \
      procps \
      kmod \
 && rm -rf /var/lib/apt/lists/*

RUN useradd -m -s /bin/bash debian \
 && printf 'debian ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/debian \
 && chmod 0440 /etc/sudoers.d/debian \
 && install -d -m 0700 -o debian -g debian /home/debian/.ssh \
 && install -m 0600 -o debian -g debian /dev/null /home/debian/.ssh/authorized_keys \
 && systemctl enable ssh \
 && systemctl mask getty@tty1.service console-getty.service getty.target

STOPSIGNAL SIGRTMIN+3

CMD ["/sbin/init"]
