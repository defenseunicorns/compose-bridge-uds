# compose-bridge-uds

Convert a Docker Compose application into a deployable [UDS](https://uds.defenseunicorns.com/) package using [Docker Compose Bridge](https://docs.docker.com/compose/bridge/). The transformation consumes a fully-resolved Compose model and emits a Helm chart tailored for UDS, ready for `zarf package create` and `zarf package deploy`.

> [!IMPORTANT]
> **This is not a supported product pathway.** It's an experimental transformation we're sharing to gather feedback. Please enagage with our team on the project [discussions page](https://github.com/defenseunicorns/compose-bridge-uds/discussions) with any questions or suggestions.

![Screenshot of a diagram depicting the packaging process via the conventional approach versus the Compose Bridge approach.](/docs/diagram.png)

## Prerequisites

- [Docker](https://docs.docker.com/get-docker/) with [Docker Compose](https://github.com/docker/compose) (v5.5.0 or later is recommended for build-only services)
- [k3d](https://k3d.io/stable/#releases)
- [UDS CLI](https://uds.defenseunicorns.com/reference/cli/quickstart-and-usage/)

## Quickstart

This walkthrough deploys UDS Core Slim Dev on k3d, then packages and deploys WordPress and MySQL from a Compose file.

```sh
# 1. Create a local k3d cluster with UDS Core Slim Dev
uds zarf package deploy oci://ghcr.io/defenseunicorns/dev/uds/checkpoints/k3d-core-slim-dev:1.11.1

# 2. Transform the Compose application into a UDS Package
cd examples/simple
docker compose bridge convert -t ghcr.io/defenseunicorns/compose-bridge-uds

# 3. Build and deploy the package
zarf package create out/ --flavor upstream
zarf package deploy zarf-package-wordpress-*.tar.zst
```

The transformation writes these artifacts to `out/`:

| Path | Purpose |
| --- | --- |
| `chart/` | Generated application Helm chart. |
| `docs/` | Generated package documentation, including a human-readable `conversion.md` report. |
| `values/` | Generated Helm values consumed during package deployment. |
| `conversion.json` | Machine-readable conversion report for automation. |
| `zarf.yaml` | Package metadata, components, images, variables, and documentation. |

## Documentation

> [!TIP]
> **Is Compose Bridge a good fit for your application?** Use the [Awesome Compose compatibility matrix](docs/awesome-compose-compatibility-matrix.md) as a practical benchmark. If your application depends on unsupported Compose features or needs deeper package customization, start with the [UDS reference package](https://github.com/uds-packages/reference-package) and build the package directly.

- [Compose support](docs/compose-support.md) describes supported configuration, known limitations, development-service exclusions, and local Dockerfile builds.
- [UDS package generation](docs/uds-package.md) describes generated resources, `x-uds` extensions, secrets, and policy exemptions.
- [`examples/full/compose.yaml`](examples/full/compose.yaml) demonstrates the complete supported configuration.

## Smoke test

The end-to-end smoke test builds this repository as a Compose Bridge transformation, converts `examples/simple/compose.yaml`, creates and deploys the generated Zarf package on UDS Core Slim in k3d, and runs a Playwright journey through UDS SSO and WordPress installation.

With Docker Compose, k3d, and the UDS CLI installed, run:

```sh
uds run test-install
```

The task uses the standard `uds-common` package creation, deployment, Core Slim setup, and Keycloak test-user tasks before running the Playwright journey.
