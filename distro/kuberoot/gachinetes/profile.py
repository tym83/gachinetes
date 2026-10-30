"""Turns kuberoot's edge profile into Gachinetes': the scheduler is
gachi-scheduler, and control planes run the tape and the referee as node
services. Usage: profile.py EDGE_PROFILE > PROFILE"""
import sys

text = open(sys.argv[1]).read()
if "\n  name: edge\n" not in text:
    sys.exit("profile of edge changed: no metadata.name to replace")
text = text.replace("\n  name: edge\n", "\n  name: gachinetes\n", 1)

# CRI-O in place of containerd: it restores containers from checkpoints,
# which containerd's CRI does not (gachi-mover).
if "\nspec:\n" not in text:
    sys.exit("profile of edge changed: no spec")
text = text.replace("\nspec:\n", "\nspec:\n  runtime: cri-o\n", 1)
containerd = """      - name: containerd
        args: [/usr/bin/containerd, --config, /etc/containerd/config.toml]
"""
if text.count(containerd) != 2:
    sys.exit("profile of edge changed: containerd is not as expected")
text = text.replace(containerd, """      - name: crio
        args: [/usr/bin/crio]
""")
podnet = "        - --pod-network={{.Net.PodNetwork}}\n"
if text.count(podnet) != 2:
    sys.exit("profile of edge changed: kuberoot-node's pod network is not as expected")
text = text.replace(podnet, podnet + "        - --cni-template=/etc/cni/kuberoot.conflist.tmpl\n")

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
        - --encryption-config={{pki "encryption.yaml"}}
      - name: gachi-referee
        after: apiserver
        args:
        - /usr/bin/gachi-referee
        - --kubeconfig={{kube "admin.kubeconfig"}}
        - --listen={{.Node.IP}}:9444
        - --webhook-url=https://{{.Node.IP}}:9444/belts
      - name: gachi-mover
        after: apiserver
        args:
        - /usr/bin/gachi-mover
        - --kubeconfig={{kube "admin.kubeconfig"}}
        - --agent={{.Node.IP}}:9600
        - --node={{.Node.Name}}
        - --controller
"""
if old not in text:
    sys.exit("profile of edge changed: kube-scheduler is not as expected")
text = text.replace(old, new, 1)

# Workers run the mover's agent with the kubelet's credentials.
worker = """      - name: kube-proxy
        after: kubelet-cert
"""
agent = """      - name: gachi-mover
        after: kubelet-cert
        args:
        - /usr/bin/gachi-mover
        - --kubeconfig=/var/lib/kubelet/kubeconfig
        - --agent={{.Node.IP}}:9600
        - --node={{.Node.Name}}
"""
if text.count(worker) != 1:
    sys.exit("profile of edge changed: the worker's kube-proxy is not as expected")
text = text.replace(worker, agent + worker, 1)
sys.stdout.write(text)
