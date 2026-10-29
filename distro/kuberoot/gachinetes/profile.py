"""Turns kuberoot's edge profile into Gachinetes': the scheduler is
gachi-scheduler, and control planes run the tape and the referee as node
services. Usage: profile.py EDGE_PROFILE > PROFILE"""
import sys

text = open(sys.argv[1]).read()
if "\n  name: edge\n" not in text:
    sys.exit("profile of edge changed: no metadata.name to replace")
text = text.replace("\n  name: edge\n", "\n  name: gachinetes\n", 1)

old = """      - name: kube-scheduler
        after: apiserver
        args:
        - /usr/bin/kube-scheduler
        - --kubeconfig={{kube "scheduler.kubeconfig"}}
        - --authentication-kubeconfig={{kube "scheduler.kubeconfig"}}
        - --authorization-kubeconfig={{kube "scheduler.kubeconfig"}}
        - --leader-elect={{.Store.LeaderElect}}
"""
new = """      - name: kube-scheduler
        after: apiserver
        args:
        - /usr/bin/gachi-scheduler
        - --gym-kubeconfig={{kube "scheduler.kubeconfig"}}
        - --gym-leader-elect={{.Store.LeaderElect}}
        - --authentication-kubeconfig={{kube "scheduler.kubeconfig"}}
        - --authorization-kubeconfig={{kube "scheduler.kubeconfig"}}
      - name: gachi-tape
        after: apiserver
        args:
        - /usr/bin/gachi-tape
        - --kubeconfig={{kube "admin.kubeconfig"}}
        - --store=http://127.0.0.1:2382
        - --listen={{.Node.IP}}:9282
        - --register={{.Node.IP}}
      - name: gachi-referee
        after: apiserver
        args:
        - /usr/bin/gachi-referee
        - --kubeconfig={{kube "admin.kubeconfig"}}
        - --listen={{.Node.IP}}:9443
        - --webhook-url=https://{{.Node.IP}}:9443/belts
"""
if old not in text:
    sys.exit("profile of edge changed: kube-scheduler is not as expected")
text = text.replace(old, new, 1)
sys.stdout.write(text)
