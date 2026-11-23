# syntax=docker/dockerfile:1
# Image de l'espace de travail agentkit : un Linux minimal pour l'agent.
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      bash coreutils procps util-linux findutils grep sed gawk tar gzip unzip xz-utils file less \
      ca-certificates curl git jq python3 python3-pip python3-venv iptables iproute2 \
    && rm -rf /var/lib/apt/lists/*
ENV PIP_BREAK_SYSTEM_PACKAGES=1 PIP_DISABLE_PIP_VERSION_CHECK=1 PYTHONDONTWRITEBYTECODE=1 LANG=C.UTF-8

# Superviseur : lance une commande dans sa propre session de processus,
# applique le délai, arrête tout le groupe à la fin, et encadre la sortie
# de marqueurs (début, code de sortie) pour que l'hôte sache ce qui s'est passé.
COPY --chmod=755 <<'SH' /usr/local/bin/agentkit-run
#!/bin/bash
# agentkit-run <id> <délai en secondes> <commande>
# timeout ouvre sa propre session (setsid) et, au délai, arrête tout son
# groupe ; à la fin, le reste du groupe (arrière-plan) est tué.
id=$1 t=$2 cmd=$3
mkdir -p /tmp/agentkit-run 2>/dev/null
pidfile=/tmp/agentkit-run/$id
printf '@@agentkit:%s:start@@\n' "$id"
setsid timeout -k 3 "$t" /bin/bash --noprofile --norc -c "$cmd" <&0 2>&1 &
pid=$!
echo "$pid" > "$pidfile"
wait "$pid"; code=$?
kill -KILL -- "-$pid" 2>/dev/null
if { [ "$code" = 124 ] || [ "$code" = 137 ]; } && [ "$SECONDS" -ge "$t" ]; then code=timeout; fi
rm -f "$pidfile"
printf '\n@@agentkit:%s:exit:%s@@\n' "$id" "$code"
SH

# Arrête une commande en cours (annulation par l'hôte).
COPY --chmod=755 <<'SH' /usr/local/bin/agentkit-kill
#!/bin/bash
pid=$(cat "/tmp/agentkit-run/$1" 2>/dev/null) || exit 0
kill -TERM -- "-$pid" 2>/dev/null; sleep 2; kill -KILL -- "-$pid" 2>/dev/null; exit 0
SH

# Réseau : Internet public seulement (conteneur voisin avec NET_ADMIN).
COPY --chmod=755 <<'SH' /usr/local/bin/agentkit-network
#!/bin/sh
set -eu
rm -f /tmp/network-ready
ip6tables -P OUTPUT DROP; ip6tables -A OUTPUT -o lo -j ACCEPT
iptables -A OUTPUT -o lo -j ACCEPT
for r in 0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 172.16.0.0/12 192.0.0.0/24 192.0.2.0/24 \
         192.88.99.0/24 192.168.0.0/16 198.18.0.0/15 198.51.100.0/24 203.0.113.0/24 224.0.0.0/3; do
  iptables -A OUTPUT -d "$r" -j REJECT
done
touch /tmp/network-ready
exec sleep infinity
SH

WORKDIR /workspace
CMD ["sleep", "infinity"]
