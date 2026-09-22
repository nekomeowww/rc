#!/usr/bin/env bash
set -euo pipefail

# Exercise the actual PID-1 entrypoint without a Kubernetes cluster. The probe
# leaves an orphan after its parent exits and checks both reaping and the
# parent's exit status, which a competing wait(-1) loop could consume.
image="${1:?usage: test-runner-reaping.sh RUNNER_IMAGE}"
container="$(docker run -d --tmpfs /run/rc:uid=1000,gid=1000,mode=0700 --entrypoint rc-kube "$image" serve)"
trap 'docker rm -f "$container" >/dev/null' EXIT

docker exec "$container" python3 -c '
import json
import pathlib
import subprocess
import time

def runtime(*args, **kwargs):
    return subprocess.run(["rc-kube", *args], capture_output=True, text=True, **kwargs)

for _ in range(100):
    if runtime("health").returncode == 0:
        break
    time.sleep(0.1)
else:
    raise AssertionError("supervisor did not become ready")

request = {"id": "orphan-probe", "uid": "orphan-probe",
           "command": ["sh", "-c", "sleep 0.2 </dev/null >/dev/null 2>&1 & exit 42"],
           "workingDirectory": "/tmp"}
runtime("process", "start", input=json.dumps(request), check=True)
for _ in range(100):
    state = json.loads(runtime("process", "inspect", "orphan-probe", check=True).stdout)
    if state["phase"] == "Exited":
        break
    time.sleep(0.1)
else:
    raise AssertionError("direct child did not exit")
assert state["exitCode"] == 42, state

time.sleep(0.5)
for _ in range(50):
    zombies = []
    for path in pathlib.Path("/proc").glob("[0-9]*/status"):
        try:
            status = path.read_text()
        except FileNotFoundError:
            continue
        if "State:\tZ" in status:
            zombies.append(status)
    if not zombies:
        break
    time.sleep(0.1)
assert not zombies, zombies
assert runtime("health").returncode == 0
print("Orphan reaped; direct child exit code 42 preserved; supervisor healthy")
'

docker stop --time 10 "$container" >/dev/null
test "$(docker inspect --format '{{.State.ExitCode}}' "$container")" != 137
