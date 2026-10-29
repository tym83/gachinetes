#!/bin/sh
# Stops the stand and waits until it is gone: an API server takes seconds to
# stop, and up.sh would take one still stopping for one running.
for p in $(pgrep -f "^sh /work/src/gachinetes/hack/stand/|^bash /work/src/gachinetes/hack/stand/|^/work/bin/"); do
  [ "$p" = "$$" ] || kill "$p" 2>/dev/null
done
while pgrep -f "^/work/bin/" >/dev/null; do sleep 1; done
echo stopped
