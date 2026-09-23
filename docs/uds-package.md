# UDS package generation

The bridge maps the [Compose Specification](https://compose-spec.io/) to Kubernetes resources and synthesizes a [UDS Package CR](https://docs.defenseunicorns.com/core/reference/operator--crds/packages-v1alpha1-cr/) for network policy, monitoring, SSO, and trust-bundle distribution. It renders these resources as a Helm chart under `out/chart/`, referenced by `out/zarf.yaml` with `localPath: chart`.

The generated chart uses Helm's `{{ .Release.Namespace }}` for namespaced resources and namespace-specific identities. The chart entry in `zarf.yaml` sets the default namespace; Zarf controls namespace creation and placement.

Generated packages include consumer documentation under `out/docs/`. The top-level `documentation` map in `out/zarf.yaml` embeds the generated package readme, deploy-time configuration reference, and application dependency reference so they can be inspected from the packaged artifact.

The generated Zarf component uses one inferred package flavor. It is `registry1` when every packaged image originates from `registry1.dso.mil`; otherwise it is `upstream`. Use that flavor when running `zarf package create`; the generated package readme includes the exact command.

Generated packages default to the standard UDS development version. The Zarf package version and Helm `appVersion` are `dev`, while the Helm chart version is the semantic version `0.0.0-dev`. A supplied `x-uds.metadata.version` value is preserved so release tooling such as `uds-pk` remains the version authority. The bridge emits a warning whenever the package version does not use the conventional `<upstream>-uds.<sub-version>` form, including for the default `dev` value.

For UDS Registry publishing, generated Zarf metadata includes the standard `dev.uds.title`, `dev.uds.tagline`, and `dev.uds.icon` annotations. The title and tagline are derived from the generated package name. The SVG icon uses a deterministic color derived from a hash of that name, giving each generated package a stable visual variation.

For services with `build:`, the bridge also writes `out/build.compose.yaml`. Zarf `onCreate` actions use Buildx Bake to build those services into OCI archives under `out/image-archives/`, and the component's `imageArchives` entries add them to the package.

Generated package configuration follows these conventions:

- Package-owned secrets are rendered from chart values rather than baked into templates.
- Package-external secrets expose only non-sensitive Kubernetes Secret name and key variables; the chart neither includes their values nor creates their Secret objects.
- Inline Compose config content is exposed as both a non-sensitive Zarf variable and a Helm value that preserves the exact flat Compose key. For example, `configs.startupScript` is available as `CONFIG_STARTUP_SCRIPT`; the `startup-script` and `startup_script` key variations produce the same variable name. The Compose content supplies the default.
- External Compose configs expose non-sensitive Kubernetes ConfigMap name and key variables and do not create ConfigMaps.
- Service environment values are exposed as non-sensitive Zarf variables and rendered into per-service ConfigMaps.
- CPU and memory requests and limits are exposed independently, with Compose reservations and limits supplying deployment defaults.
- Every package exposes `SUBDOMAIN` for the first inferred public host, `DOMAIN` for generated endpoints, and `ADDITIONAL_NETWORK_ALLOW` for deploy-time UDS network rules.

The bridge writes `out/values/values.yaml` with `###ZARF_VAR_*###` placeholders for these deploy-time values, references it through `charts[].valuesFiles`, and retains their defaults, prompts, indentation, and sensitivity settings in the Zarf package's `variables:`.

## Inferred behavior

- **Expose:** Services with published `ports:` are exposed on the tenant gateway. When the first expose rule omits `host`, it uses `SUBDOMAIN` when set and otherwise the Helm release namespace; an explicit host remains literal. For multi-port services, the bridge prefers Compose `app_protocol` or `name` values indicating web traffic, then falls back to the first published port.
- **Network allow:** Intra-namespace ingress and egress rules are always included so services in the namespace can communicate. Static `x-uds.spec.network.allow` entries follow inferred rules, and deploy-time `ADDITIONAL_NETWORK_ALLOW` entries are appended last.
- **SSO:** A Keycloak client is generated for the first exposed service and omitted when no services are exposed. Its default name is `<Package Name> Login` and its client ID is `uds-compose-<release-namespace>`. Its inferred redirect URI uses the first endpoint's effective subdomain and `DOMAIN`, which defaults to `uds.dev`.
- **Policy exemptions:** Services requiring UDS policy exceptions produce `chart/templates/uds-exemption.yaml`.
- **Monitoring:** Metrics monitors are inferred from ports named `metrics` or `prometheus`, common exporter ports, and `METRICS_PORT` or `PROMETHEUS_PORT` environment variables when they match a declared TCP port. Set `x-uds.spec.monitor` to take complete control of monitoring, including `x-uds.spec.monitor: []` to disable inference.
- **Development dependencies:** Services referenced only by `depends_on` entries with `required: false` are omitted along with resources used exclusively by them.

## Extension keys

Use `x-uds` [Compose extension keys](https://docs.docker.com/reference/compose-file/extension/) only when inferred behavior needs to be overridden.

| Key | Purpose |
|---|---|
| `x-uds.metadata.name` | Package name and Zarf default namespace (default: Compose project name). |
| `x-uds.metadata.version` | Package version override (default: `dev`). A supplied value (e.g. `<upstream>-uds.<sub-version>`) is preserved. |
| `x-uds.metadata.labels` | Labels applied to generated UDS Package metadata. |
| `x-uds.metadata.annotations` | Annotations applied to generated UDS Package metadata and Zarf package metadata. |
| `x-uds.spec.network.expose[]` | Replace inferred expose rules. Missing fields are inferred from the service; set an empty list to disable exposure. |
| `x-uds.spec.network.allow[]` | Add network allow rules, deduplicated against inferred rules. |
| `x-uds.spec.monitor[]` | Add Prometheus monitoring rules, with service metadata inferred when possible. |
| `x-uds.spec.sso[]` | Replace inferred SSO clients. Set `x-uds.spec.sso: []` to disable inferred SSO. |
| `x-uds.spec.caBundle.configMap` | Customize the operator-managed trust bundle ConfigMap metadata. |

For example, override only the host for an exposed service:

```yaml
x-uds:
  spec:
    network:
      expose:
        - service: server
          host: hello-world
```

### Extension notes

- **`x-uds.spec.sso`:** Missing fields are inferred. Explicit values remain unchanged, and an empty list disables inferred SSO.
- **`x-uds.spec.monitor[]`:** When this key is absent, the bridge infers common metrics endpoints but cannot recognize every application-specific metrics configuration. When present, entries may be raw UDS `spec.monitor[]` items or use the bridge-only `service` key to infer labels and port metadata; no additional monitors are inferred. Set `portName` or `targetPort` for multi-port services, or set an empty list to disable monitoring.
- **`x-uds.spec.caBundle.configMap`:** This customizes the namespace trust-bundle ConfigMap. Trust bundle contents are configured separately in UDS Core.

See the [full Compose example](../examples/full/compose.yaml) for all supported extensions.

## Policy exemptions

UDS Core expects containers to run as non-root, avoid privileged mode, drop Linux capabilities, and use an approved seccomp profile. When Compose security settings conflict with those policies, the bridge generates an `Exemption` scoped to each matching service. No exemption is emitted when all services comply.

Prefer changing the image or Compose configuration to meet the UDS baseline. Keep an exemption when the workload cannot run correctly without the less-restricted setting.

| Compose input | Generated UDS policy exemption |
|---|---|
| `user: root`, `user: 0`, or equivalent UID/GID forms | `RequireNonRootUser` |
| `privileged: true` | `DisallowPrivileged` |
| `cap_add:` | `DropAllCapabilities`, `RestrictCapabilities` |
| `security_opt: [seccomp:unconfined]` | `RestrictSeccomp` |
| `cap_drop: [ALL]` | No exemption; rendered into the container `securityContext.capabilities.drop` instead. |

Example Compose input:

```yaml
name: homelab
services:
  gitea:
    image: docker.gitea.com/gitea:1.25.5
    user: root
  runner:
    image: gitea/act_runner:latest
    privileged: true
```

Generated exemption:

```yaml
apiVersion: uds.dev/v1alpha1
kind: Exemption
metadata:
  name: compose-{{ .Release.Namespace }}
  namespace: uds-policy-exemptions
spec:
  exemptions:
    - title: root user policy exemption for homelab gitea
      matcher:
        kind: pod
        namespace: '{{ .Release.Namespace }}'
        name: ^gitea-.*
      policies:
        - RequireNonRootUser
    - title: privileged policy exemption for homelab runner
      matcher:
        kind: pod
        namespace: '{{ .Release.Namespace }}'
        name: ^runner-.*
      policies:
        - DisallowPrivileged
```
