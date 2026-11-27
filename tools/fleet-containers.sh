#!/usr/bin/env bash
# Exercise the shipped controller and two Linux worker daemons on a private network.
# Supply the Linux coop executable and a local image with git, Docker CLI and sh.
set -euo pipefail
binary=${1:?supply an absolute path to a Linux coop executable}
image=${2:?supply a local Linux image}
scratch=$(mktemp -d)
name="coop-fleet-check-$(basename "$scratch" | tr '[:upper:].' '[:lower:]-')"
cid=""
first=""
second=""
# shellcheck disable=SC2329 # invoked by the EXIT trap
cleanup() {
  local container
  for container in "$first" "$second" "$cid"; do
    if [[ -n "$container" ]]; then docker rm -f "$container" >/dev/null || true; fi
  done
  docker network rm "$name" >/dev/null || true
  rm -f "$scratch/cert" "$scratch/key" "$scratch/a" "$scratch/b" "$scratch/job" "$scratch/prompt"
  rmdir "$scratch"
}
docker network create --internal "$name" >/dev/null
trap cleanup EXIT
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -keyout "$scratch/key" -out "$scratch/cert" \
  -subj /CN=controller -addext subjectAltName=DNS:controller >/dev/null 2>&1
common=(--pull never --network "$name" --user 0 --mount "type=bind,src=$binary,dst=/bin/coop,readonly" -e HOME=/root -e TMPDIR=/tmp)
cat >"$scratch/job" <<'JOB'
{"version":2,"job_ref":"docker-test","source":null,"companions":[],"targets":["claude@fleet-test"],"mode":"normal","repository_read_only":true,"egress":{"mode":"open","rules":[],"export_destinations":false},"environment":{},"check":{"argv":[],"environment":{}},"resources":{"cpu_millis":500,"memory_bytes":536870912,"pids":128},"limits":{"max_turns":2,"max_queued_turns":1,"max_queued_bytes":1048576,"turn_timeout_ms":60000,"warm_idle_timeout_ms":0,"max_patch_bytes":1048576}}
JOB
echo 'Reply with hello.' >"$scratch/prompt"
cid=$(docker run -d "${common[@]}" --network-alias controller --mount "type=bind,src=$scratch/key,dst=/tls/key,readonly" \
  --mount "type=bind,src=$scratch/job,dst=/job,readonly" --mount "type=bind,src=$scratch/prompt,dst=/prompt,readonly" \
  --mount "type=bind,src=$scratch/cert,dst=/tls/cert,readonly" --entrypoint /bin/coop "$image" \
  sessions fleet serve /controller :9443 /tls/cert /tls/key)
umask 077
for id in a b; do
  docker exec "$cid" /bin/coop sessions fleet invite /controller "$id" pool --slots-only >"$scratch/$id"
  worker=$(docker run -d "${common[@]}" --mount "type=bind,src=$scratch/$id,dst=/seed,readonly" \
    --mount "type=bind,src=$scratch/cert,dst=/ca,readonly" --entrypoint /bin/sh "$image" \
    -ec 'umask 077; cp /seed /tmp/invitation; exec /bin/coop sessions connect --state /worker --controller https://controller:9443 --token-file /tmp/invitation --ca-file /ca')
  if [[ "$id" == a ]]; then first=$worker; else second=$worker; fi
done
for _ in {1..60}; do
  if [[ $(docker inspect -f '{{.State.Running}}' "$first" "$second" | sort -u) != true ]]; then break; fi
  if docker exec "$cid" /bin/coop sessions fleet workers /controller | grep -c '"online":true' | grep -qx 2; then
    echo 'Two Linux workers enrolled and polled the controller.'
    docker exec "$cid" /bin/coop sessions fleet submit /controller pool /job /prompt
    # Each fixture has an empty account inventory. After both workers poll,
    # the job must still be cancellable without ever starting a session.
    status=$(docker exec "$cid" /bin/coop sessions fleet status /controller docker-test)
    [[ "$status" == *'No available worker reports all requested model accounts signed in.'* ]] || { echo "$status"; exit 1; }
    docker restart "$cid" >/dev/null
    for _ in {1..30}; do
      if status=$(docker exec "$cid" /bin/coop sessions fleet status /controller docker-test); then
        [[ "$status" == *'"stage":"queued"'* ]] || { echo "$status"; exit 1; }
        docker exec "$cid" /bin/coop sessions fleet cancel /controller docker-test
        status=$(docker exec "$cid" /bin/coop sessions fleet status /controller docker-test)
        [[ "$status" == *'"stage":"cancelled"'* ]] || { echo "$status"; exit 1; }
        echo 'An uncredentialed job survived a controller restart and cancelled while queued.'
        exit 0
      fi
      sleep 1
    done
    break
  fi
  sleep 1
done
for container in "$first" "$second" "$cid"; do docker logs --tail 8 "$container"; done
exit 1
