# yandex-cloud-fleeting-plugin

> [!note]
> This is a fork of [gitlab.com/k.mukhin/fleeting-plugin-yc](https://gitlab.com/k.mukhin/fleeting-plugin-yc), a community plugin (not an official `gitlab-org` one), detached from upstream. It fixes `preemptible` being hardcoded to `true` regardless of `template.yml`, reports human-readable instance names instead of raw UUIDs (see [VM Naming](#vm-naming)), returns proper errors from `Init()` instead of calling `log.Fatal`, adds `cloud_init_extra_file`/`cloud_init_vars` support (see [Extending cloud-init](#extending-cloud-init)), and implements `Heartbeat`/`Suspend`/`Resume` for the current `fleeting` provider interface.

This is a [fleeting plugin](https://gitlab.com/gitlab-org/fleeting/fleeting) for Yandex Cloud, used with the `docker-autoscaler` executor. It creates and destroys GitLab Runner VMs on demand via a [Yandex Compute Instance Group](https://yandex.cloud/en/docs/compute/concepts/instance-groups/).

Documentation: https://docs.gitlab.com/runner/executors/docker_autoscaler.html


## Provider Configuration

The following go under `[runners.autoscaler.plugin_config]`:

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `name` | string | Yes | Instance Group name/identity — see [Renaming a running group](#renaming-a-running-group) |
| `folder_id` | string | Yes | YC folder the Instance Group and its VMs live in |
| `config_file` | string | Yes | Path to the `template.yml` instance template — see [Instance Template](#instance-template) |
| `ssh_file` | string | No | Path to a persistent private SSH key. If unset, a fresh RSA-4096 key pair is generated in memory on every `Init()` instead — see [Connector User](#connector-user) |
| `key_file` | string | No | Path to an authorized key of a service account (the JSON written by `yc iam key create --output`). If set, the plugin authenticates to the YC API with this key instead of the metadata service — for running the plugin outside Yandex Cloud, see [Running outside Yandex Cloud](#running-outside-yandex-cloud) |

### Renaming a running group

`name` is looked up on every `Init()`: an existing Instance Group with a matching name is reused, otherwise a new one is created. Changing `name` does **not** rename the old group — it orphans it. The old group's instances keep running (and billing) but nothing manages them anymore; delete it manually (`yc compute instance-group delete`) after an intentional rename.

## Instance Template

`config_file` (commonly `template.yml`):

```yaml
platform: "standard-v3"
zone: "ru-central1-a"
preemptible: true
serviceAccount: "<plugin SA id>"      # attached to the Instance Group resource
vmServiceAccount: "<VM SA id>"        # attached to each spawned runner VM

resources:
  cpu: 4
  memory: 8589934592
  fraction: 100

disk:
  type: "network-ssd"
  size: 21474836480
  image: "<golden image id>"

network:
  network: "<network id>"    # optional if subnet already implies it - unconfirmed against the YC API, set it if instance-group creation complains
  subnet: "<subnet id>"
  nat: false
  security: "<security group id>"

monitoring:
  enabled: false

cloud_init_extra_file: "/etc/gitlab-runner/cloud-init-extra.yaml"
cloud_init_vars:
  username: "ci"
  ssh_public_key: "ssh-ed25519 AAAA..."
```

`instance_template.go`'s struct tags are `json:`, not `yaml:` — the file is parsed via `github.com/ghodss/yaml`, which converts YAML to JSON and unmarshals with `encoding/json`. That package only reads `json:` tags; without one it falls back to case-insensitive matching on the Go field name, which happens to work for single-word/camelCase keys (`platform`, `serviceAccount`) but silently drops any snake_case key, parsing it as a zero value with no error. `instance_template_test.go` guards this by unmarshalling real YAML rather than setting struct fields directly in Go.

### Multi-zone

By default the instance group lives in a single zone/subnet (`zone` +
`network.subnet` above). To span multiple zones, set the plural fields
instead — they take priority over the singular ones when non-empty:

```yaml
zones: ["ru-central1-a", "ru-central1-b"]
network:
  subnets: ["<subnet id in a>", "<subnet id in b>"]
```

YC's Instance Group service pairs each subnet to whichever zone it actually
belongs to (subnets are inherently zone-scoped) — not by matching list
position — so the order of `subnets` relative to `zones` doesn't matter, but
every zone in `zones` needs exactly one subnet in `subnets` that belongs to
it, or instance-group creation will fail for that zone. Leave `zone`/`subnet`
in place alongside the plural fields if you want a quick way to fall back to
single-zone by deleting the plural ones — the singular fields are otherwise
ignored once `zones`/`subnets` are set.

### IAM Setup

Two service accounts, different jobs:

- **`serviceAccount`** is attached to the *management VM itself* — the plugin authenticates to the YC API via `ycsdk.InstanceServiceAccount()`, which reads credentials from the VM's own attached SA through the metadata service. Attaching this SA to an already-running management VM needs a full **stop + start**, not `restart` — a soft reboot doesn't refresh the metadata service's view of the attached SA.
- **`vmServiceAccount`** is attached to each *spawned runner VM*.

Required grants (found empirically — YC's `PermissionDenied` errors here carry no detail on which permission is missing):

1. `serviceAccount` needs `compute.admin` + `vpc.admin` on the folder — but that's not enough to create an Instance Group specifically. A plain `yc compute instance create` with those two roles succeeds; `yc compute instance-group create` with the identical SA/folder/subnet still fails. Also grant `editor` on the same folder.
2. `serviceAccount` needs `iam.serviceAccounts.user` granted directly **on the `vmServiceAccount` resource** (`yc iam service-account add-access-binding --id <vm-sa> --role iam.serviceAccounts.user --service-account-id <plugin-sa>`), so it's allowed to attach that SA to instances it creates.
3. If the subnet's parent network lives in a different folder than the subnet itself (a shared/multi-folder VPC), a folder-level `vpc.user` grant is **not** sufficient for Instance Group creation — plain `compute.instance.create` tolerates it, but `instance-group create` needs `vpc.user` granted at the **cloud** level (`yc resource-manager cloud add-access-binding --id <cloud-id> --role vpc.user --service-account-id <plugin-sa>`).

#### Running outside Yandex Cloud

Off a YC VM (bare metal, on-prem, another cloud) there is no metadata service to take credentials from. Set `key_file` in `[runners.autoscaler.plugin_config]` to an authorized key of the plugin's service account instead:

```sh
yc iam key create --service-account-id <plugin-sa> --output /etc/gitlab-runner/yc-sa-key.json
chmod 600 /etc/gitlab-runner/yc-sa-key.json
```

The plugin then signs its IAM token requests with that key; everything else, including the grants above, stays the same — the key's service account takes the place of the one the management VM would have had attached. A missing or malformed key file fails `Init()` with an error naming `key_file`. The file holds a private key: keep it readable only by the user GitLab Runner runs as.

The runner VMs must also be reachable over SSH from wherever the plugin runs: outside the VPC that usually means `network.nat: true` in `template.yml`, `use_external_addr = true` in `[runners.autoscaler.connector_config]`, and a security group that lets the manager's address in on port 22.

### Connector User

`[runners.autoscaler.connector_config].username` (e.g. `ci`) must be a real Linux user on the runner VM with SSH access. On Yandex Cloud, unlike GCE, setting `ssh-keys: <username>:<key>` in instance metadata does **not** create that user — it only appends the key to an *existing* user's `authorized_keys`. A nonexistent connector user fails `Permission denied (publickey)`, indistinguishable at a glance from a wrong key.

Create the user via [`cloud_init_extra_file`](#extending-cloud-init) instead of baking it into the golden image, so it works regardless of which base image is in use:

```yaml
#cloud-config
users:
  - name: {{ .Vars.username }}
    groups: [docker]
    shell: /bin/bash
    lock_passwd: true
    ssh_authorized_keys:
      - {{ .Vars.ssh_public_key }}
```

If `ssh_file` isn't set in the provider config, the plugin generates a fresh RSA-4096 key pair in memory on every `Init()` instead of loading a persistent one. Every plugin restart then invalidates already-created VMs' authorized key, since a new random key replaces the old one in memory — set `ssh_file` for anything longer-lived than a quick trial.

### VM Naming

Instances are named `<group-name>-<uuid8>-{instance.index}`, e.g. `gitlab-autoscale-runners-cd843093-1`. Unlike the vSphere/OpenStack fleeting plugin forks, the `{instance.index}` placeholder is required here, not just cosmetic: Yandex's Instance Group creates several VMs from one shared template per scale event, and the name's `uuid8` component is computed once per `Init()` — not regenerated per VM — so without the index, simultaneous instances created in one scale-out collide on an identical name.

Names are reported to GitLab Runner/taskscaler as-is (`Update()`/`ConnectInfo()`), but YC's own `DeleteInstancesRequest` needs the real managed-instance ID — an internal `nameToID` cache bridges the two, falling back to a live `ListInstances` call on a cache miss (e.g. right after `Increase()`, before the next `Update()` poll has populated the cache).

## Extending cloud-init

`cloud_init_extra_file` layers arbitrary cloud-init on top of the plugin's own generated user-data (the monitoring-agent installer, when `monitoring.enabled` is set) instead of needing a separate golden image per tweak — the same purpose the vSphere fleeting plugin's own `cloud_init_extra_file` serves. The file is parsed as a Go [`text/template`](https://pkg.go.dev/text/template) once at `Init()`, so a broken template fails fast at startup rather than on every scale-up, and rendered per scale-up with:

| Variable | Value |
|----------|-------|
| `{{ .RunnerName }}` | The Instance Group name (`plugin_config.name`) |
| `{{ .Vars.<key> }}` | From `cloud_init_vars` — for anything the template needs that shouldn't be hardcoded into the file itself (a registry token, the connector username) |

### How the parts combine

When both the monitoring installer and `cloud_init_extra_file` are set, they're merged via real cloud-init multipart MIME rather than one clobbering the other — cloud-init appends list values (`runcmd`, `packages`, `write_files`) across parts instead of only keeping the last one.

## Differences from upstream

See the fork note at the top. In more detail:

- `SchedulingPolicy.Preemptible` was hardcoded `true` regardless of `template.yml`'s `preemptible` field; now honors it (`provider_test.go`'s `TestPreemptible_HonorsTemplateField`).
- `Init()`, on finding an existing Instance Group, now restores `g.size` from the group's real `ManagedInstancesState.TargetSize` instead of leaving it at `0` — the previous behavior meant any plugin restart while the group had running instances would compute the next scale-up's `FixedScale.Size` from `0 + delta`, causing YC to scale the group *down* to `delta` and delete live/in-use VMs.
- `Init()` returns proper errors instead of `log.Fatal`, and the plugin's logger is actually assigned (`g.logger = logger`) instead of left nil.
- Implements `Heartbeat`, `Suspend`, `Resume` for the current `gitlab.com/gitlab-org/fleeting/fleeting` interface. `Suspend`/`Resume` are stubs returning `provider.ErrCapabilityNotSupported` — YC Instance Groups have no pause/resize-to-zero primitive that maps onto fleeting's suspend/resume state machine, and `ProviderInfo.Capabilities` never advertises `CapabilitySuspendResume`, so taskscaler never calls them.

## Example Runner Config

```toml
concurrent = 10
check_interval = 0
shutdown_timeout = 0

[[runners]]
  name = "ci-runner"
  url = "https://gitlab.example.com"
  token = "glrt-xxxxxxxxxxxxxxxxxxxx"
  executor = "docker-autoscaler"

  [runners.docker]
    image = "docker.example.com/debian-ci:12-slim"
    privileged = false
    volumes = ["/var/run/docker.sock:/var/run/docker.sock", "/cache"]

  [runners.autoscaler]
    plugin = "fleeting-plugin-yc"
    capacity_per_instance = 2   # concurrent jobs per VM
    max_use_count = 2           # jobs a VM handles before it's retired - keep >= capacity_per_instance, see note below
    max_instances = 4

    [runners.autoscaler.plugin_config]
      name        = "gitlab-autoscale-runners"
      folder_id   = "b1g..."
      config_file = "/etc/gitlab-runner/template.yml"
      ssh_file    = "/etc/gitlab-runner/id_rsa"

    [runners.autoscaler.connector_config]
      username = "ci"

    [[runners.autoscaler.policy]]
      idle_count = 1
      idle_time  = "20m0s"
```

> [!note]
> `max_use_count` counts *uses* (job acquisitions), not concurrent slots. If `capacity_per_instance = 2` but `max_use_count = 1`, a VM is retired after its *first* job even though it had room for a second one running concurrently — half its capacity goes unused. Set `max_use_count >= capacity_per_instance` to actually fill a VM's declared concurrent capacity before recycling it.

## Development

### Build

```sh
make build
```

Cross-compile targets and other build details: see the [Makefile](Makefile).

### Tests

```sh
make test
```

Unit tests only — no real Yandex Cloud access needed or exercised.
