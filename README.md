# KubeDeck

KubeDeck is a lightweight, Headlamp-style Kubernetes dashboard for every cluster in your kubeconfig.

Point it at your kubeconfig and you get one web app for all your contexts. You can see live metrics, manage any resource, exec into pods, and run a real shell with `kubectl` already pointed at the cluster you're viewing.

- **One small binary** (about 8 MB) with nothing to install in the cluster. There is no Node, Python or database. The only bundled extras are xterm.js and IBM Plex fonts.
- **Works with any kubeconfig**, including:
  - kubeadm client certificates
  - EKS (`aws eks get-token`) and other `exec` plugins
  - bearer tokens
  - Rancher proxy URLs
  - `proxy-url`
  - several files merged together
- **Works with only Roles and RoleBindings.** If your user has no cluster-wide rights, KubeDeck reads just your namespaces. It detects this automatically, or you can set it per cluster.

## Features

**Every cluster at a glance:** health, versions, nodes, pods, Deployments, warnings, and CPU and memory against capacity or quota.

**Metrics:**
- **Charts for the last hour:** cluster, node, workload and pod, with usage against requests, limits and allocatable.
- **Breakdowns:** CPU and memory by namespace, and the top pods by each.
- **Health:** node pressure and pods that need attention.

**Every resource type, including CRDs.** KubeDeck asks each cluster which resource types it serves and builds the navigation from that:
- **Workloads:** Deployments, StatefulSets, DaemonSets, Pods, Jobs, CronJobs, HPAs
- **Network:** Services, Ingresses, Gateways, HTTPRoutes, NetworkPolicies, CiliumNetworkPolicies
- **Config:** ConfigMaps, Secrets, ExternalSecrets and SecretStores, quotas
- **Storage:** PVCs, PVs, StorageClasses
- **Access:** RBAC objects
- **Custom resources:** any other CRD, grouped by API group

Lists show the same columns as `kubectl get`, including CRD printer columns. A **Wide** toggle adds the extra columns from `-o wide`.

**Manage resources:**
- **View and edit YAML.** Applying replaces the object, and stale edits are rejected so you never overwrite someone else's change.
- **Delete, scale, rollout restart, cordon / uncordon.**
- **Secrets:** values stay hidden until you click Reveal, and can be copied.
- **Safety:** every action asks for confirmation and respects your RBAC.

**Terminals** in a drawer at the bottom (Ctrl+`):
- **Pod exec:** a real shell in any container, in the same way `kubectl exec -it` works.
- **Local shell:** your own login shell in a real terminal, with `kubectl` preset to the cluster you're viewing, so `kubectl get pods`, `kubectl exec -it`, `kubectl edit` and k9s all work. The context switch applies only to that shell and never changes your kubeconfig. The local shell works on macOS and Linux.
- **Tabs:** several sessions at once, and they stay open while you browse other pages.

**Namespaces that need attention** (Cluster → Namespaces): for each namespace, only what needs a human, worst first.
- **Pods:** crash-looping, pending or not-ready pods, and pods that restarted in the last hour.
- **Workloads:** Deployments, StatefulSets and DaemonSets with unavailable replicas.
- **Storage:** volume claims at 80% or more (critical at 90%), and claims that are pending or lost.
- **Events:** warning events from the last hour, grouped by reason.
- Healthy namespaces are hidden unless you ask for them.

**Pods page:** each pod shows its restart count with how long ago the last restart was, and its status with the last restart reason and exit code, for example `OOMKilled (137)`. The page reloads live from the cluster every 5s. The picker at the top right offers 1s, 5s, 10s, 30s and 1 min, and your choice is remembered. It pauses while the tab is hidden.

**Volume usage:** PVC lists and pages show real usage, not just the claimed size. KubeDeck needs no node or kubelet access: it finds a running pod that mounts the claim and runs `df` there through the exec API.
- **Light on pods:** one exec per pod covers all of its claims, at most 3 run at once, and results are cached for 5 minutes. Forced refreshes are limited to once per 10s per volume.
- **Fallbacks:** if a container has no shell or `df`, KubeDeck tries another container or pod.
- **Limits:** claims with no running pod can't be measured, `subPath` mounts show the whole volume and are marked `~`, and `-read-only` mode turns this off because it needs exec.

**Logs for a whole Deployment:**
- **Merged stream:** every pod matched by the Deployment's label selector, live, colour-coded per pod.
- **Rollouts:** new pods are picked up automatically.
- **Narrowing it down:** search, level filter, and single-pod mode.
- **Download** the merged log as a file.

**Global namespace filter**, a **cluster switcher**, and a **Stop server** button in the top bar.

## Quick start

1. Download the binary for your OS from the zip.
2. Run it:

   ```sh
   # Windows
   kubedeck.exe
   kubedeck.exe -kubeconfig C:\Users\you\.kube\config

   # Linux / macOS
   ./kubedeck
   ./kubedeck -kubeconfig ~/.kube/config:~/.kube/eks-config
   ```

3. Your browser opens at `http://127.0.0.1:7080`. You can also change the kubeconfig path at the top of the **All clusters** page. It's remembered between runs.

### macOS app

1. Open `KubeDeck-<version>-macOS-universal.dmg` and drag **KubeDeck** onto **Applications**. One app runs on both Apple Silicon and Intel Macs (macOS 11 or newer).
2. Double-click KubeDeck. It runs in the background with no Dock icon and opens the dashboard in your browser.
   - Opening it again just brings the page back.
   - Stop it with **Quit** in the bottom-left corner of the page.
   - Logs are written to `~/Library/Logs/KubeDeck.log`.
3. **The first time**, macOS says it "could not verify" KubeDeck, because the app isn't notarized by Apple. To allow it:
   - Open **System Settings → Privacy & Security** and click **Open Anyway**.
   - Or run `xattr -dr com.apple.quarantine /Applications/KubeDeck.app`.
4. **EKS and other exec logins:** the app uses your shell's `PATH`, so `aws`, `kubelogin` and `gke-gcloud-auth-plugin` installed with Homebrew are found.

To build the DMG yourself, run `python3 packaging/macos/build_dmg.py --version 1.0.0` (see the script header for what it needs on Linux or macOS).

### Flags

| Flag | Default | |
|---|---|---|
| `-kubeconfig` | `$KUBECONFIG` or `~/.kube/config` | one or more files (separate with `:` on Linux/macOS, `;` on Windows) |
| `-addr` | `127.0.0.1:7080` | listen address. On anything other than localhost, an access token is required and printed at start |
| `-token` | auto | set your own token when sharing with `-addr 0.0.0.0:7080` |
| `-interval` | `15s` | refresh interval for clusters you're viewing (minimum 5s). Others refresh every 60s |
| `-no-browser` | | don't open a browser |
| `-read-only` | off | disable every write: edit, delete, scale, restart, cordon, exec and the shell |
| `-allow-shell` | off | allow the local shell even when listening beyond localhost (only on trusted networks) |

### Requirements in the cluster

- **metrics-server** for CPU and memory usage. Everything else works without it.
- **Read access at minimum.** Either kind of access works. Write actions use whatever rights your kubeconfig user has.

### Cluster-wide or namespace-only access

Each context has an **Access** setting. Click the lock button on a cluster's page, or **Set namespaces…** on its card:

| Mode | When | What you see |
|---|---|---|
| **Automatic** (default) | anything | tries cluster-wide reads; if RBAC forbids them, switches to your namespaces |
| **Cluster-wide** | ClusterRole + ClusterRoleBinding (for example the built-in `view` plus `nodes`) | every namespace, nodes, node capacity |
| **Only my namespaces** | Role + RoleBinding only | the context's namespace plus the namespaces you list; **no cluster-scoped calls are made** |

In namespace mode:
- **Usage, workloads, pods, events and logs** come only from your namespaces.
- **Capacity line:** if every namespace you can see has a ResourceQuota, the quota is used as the capacity line. Otherwise the charts show usage against requests and limits.
- **Nodes** are hidden, because they're cluster-scoped. Each pod still shows which node it runs on.
- **Settings are saved** per context in your user config folder.

Smallest Role that gives the full namespace experience, including logs and metrics:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: { name: kubedeck-viewer, namespace: payments }
rules:
- apiGroups: [""]
  resources: [pods, pods/log, events, resourcequotas]
  verbs: [get, list]
- apiGroups: [apps]
  resources: [deployments, statefulsets, daemonsets]
  verbs: [get, list]
- apiGroups: [metrics.k8s.io]
  resources: [pods]
  verbs: [get, list]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: { name: kubedeck-viewer, namespace: payments }
subjects: [{ kind: User, name: dev-team, apiGroup: rbac.authorization.k8s.io }]
roleRef: { kind: Role, name: kubedeck-viewer, apiGroup: rbac.authorization.k8s.io }
```

Logs by Deployment labels need only `pods` (list) and `pods/log` (get) in that namespace.

## How it works

Each cluster gets a background collector. Every refresh it makes one paginated `list` call per resource, plus one call to metrics-server, and keeps the result as an in-memory snapshot. Every page is served from that snapshot, so the UI is instant and the API server load stays constant no matter how many browser tabs are open.

Usage history (1 hour) is kept in small ring buffers in memory, so nothing is written to disk except your last-used kubeconfig path, in your user config folder.

Logs are streamed straight from the kubelet through the API server, using Server-Sent Events to the browser.

Writes use the API server directly with your kubeconfig credentials, so Kubernetes RBAC decides what you can do. Pod exec bridges the API server's WebSocket exec endpoint to xterm.js in the browser.

Security:
- By default KubeDeck only accepts connections from this machine. It checks the Host header (DNS-rebinding guard) and the Origin header on writes and terminals.
- When bound to the network it requires a token, and the local shell is turned off unless you pass `-allow-shell`.
- Your kubeconfig credentials never leave the KubeDeck process.

## Build from source

You need Go 1.22 or newer. The only dependency, the YAML parser, is included in `third_party/`, so the build works offline.

```sh
go build -trimpath -ldflags "-s -w" -o kubedeck .
go test ./...
```

`tools/mockapi` is a fake Kubernetes API server for testing without a cluster. It provides realistic objects, moving metrics, streaming logs, certificate and exec auth, a Role-only user limited to two namespaces, and an unreachable context:

```sh
go run ./tools/mockapi -dir /tmp/mock &
go run . -kubeconfig /tmp/mock/kubeconfig -interval 5s
```

## Licence

MIT. The bundled `gopkg.in/yaml.v3` is MIT/Apache-2.0; its licence is included in `third_party/yaml.v3`.
