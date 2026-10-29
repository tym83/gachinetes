#!/bin/sh
# Stops the stand and waits until it is gone: an API server takes seconds to
# stop, and up.sh would take one still stopping for one running. What has
# not stopped in 20 seconds is killed.
pids() { pgrep -f "^sh /work/src/gachinetes/hack/stand/|^bash /work/src/gachinetes/hack/stand/|^bash -c until |^/work/bin/" | grep -vx "$$"; }
for p in $(pids); do kill "$p" 2>/dev/null; done
i=0
while [ -n "$(pids)" ]; do
  i=$((i + 1))
  [ $i -eq 20 ] && for p in $(pids); do kill -9 "$p" 2>/dev/null; done
  sleep 1
done
echo stopped
