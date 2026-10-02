FROM ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server tmux ca-certificates procps git rsync && rm -rf /var/lib/apt/lists/* && useradd -m -s /bin/bash developer && mkdir -p /run/sshd /home/developer/.ssh /home/developer/project && chown -R developer:developer /home/developer
RUN printf '\nPasswordAuthentication no\nPermitRootLogin no\nAllowUsers developer\nAllowStreamLocalForwarding yes\nStreamLocalBindUnlink yes\n' >> /etc/ssh/sshd_config
COPY entrypoint.sh /entrypoint.sh
ENTRYPOINT ["sh", "/entrypoint.sh"]
