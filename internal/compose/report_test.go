package compose

import (
	"os"
	"path/filepath"
	"testing"

	"defenseunicorns/uds-compose-bridge/internal/model"
)

func TestConvertCanonicalYAMLReportsStructuredInvalidUDSSetting(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
x-uds:
  metadata:
    version:
      bad: value
`))
	if err == nil {
		t.Fatal("ConvertCanonicalYAML() error = nil, want invalid x-uds.metadata.version")
	}

	rejected := requireDecision(t, conversion.Report.Rejected, "x-uds.metadata.version")
	if rejected.Code != "invalid-setting" {
		t.Fatalf("rejected code = %q, want invalid-setting", rejected.Code)
	}
	if rejected.Remediation == "" {
		t.Fatalf("rejected remediation = %q, want non-empty remediation", rejected.Remediation)
	}
	if hasDecision(conversion.Report.Translated, "x-uds.metadata.version") {
		t.Fatalf("translated decisions unexpectedly include rejected path: %#v", conversion.Report.Translated)
	}
	if hasDecision(conversion.Report.Rejected, "compose") {
		t.Fatalf("rejected decisions unexpectedly include generic compose failure: %#v", conversion.Report.Rejected)
	}
}

func TestConvertCanonicalFileReadFailureIncludesRemediation(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalFile(filepath.Join(t.TempDir(), "missing-compose.yaml"))
	if err == nil {
		t.Fatal("ConvertCanonicalFile() error = nil, want read error")
	}

	rejected := requireDecision(t, conversion.Report.Rejected, "compose")
	if rejected.Code != "read-error" || rejected.Remediation == "" {
		t.Fatalf("rejected decision = %#v, want read-error with remediation", rejected)
	}
}

func TestConvertCanonicalFileAlignsReportWithConsumedSettings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	inputPath := filepath.Join(dir, "compose.yaml")
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	if err := os.WriteFile(inputPath, []byte(`name: demo
services:
  my_api:
    build:
      context: .
    image: ghcr.io/acme/api:1.2.3
    deploy:
      resources:
        limits:
          cpus: "0.50"
          memory: 512M
          pids: 50
        reservations:
          cpus: "0.25"
          memory: 256M
        devices:
          - capabilities: ["gpu"]
x-uds:
  metadata:
    name: uds-package
    version: 1.2.3
    description: ignored
  spec:
    foo: ignored
    network:
      expose:
        - service: my_api
      foo: ignored
    caBundle:
      configMap:
        name: bundle
        key: ca.pem
`), 0o644); err != nil {
		t.Fatalf("write compose.yaml: %v", err)
	}

	conversion, err := ConvertCanonicalFile(inputPath)
	if err != nil {
		t.Fatalf("ConvertCanonicalFile() error = %v", err)
	}

	requireDecision(t, conversion.Report.Ignored, "name")
	requireDecision(t, conversion.Report.Ignored, "services.my_api.image")
	imageDecision := requireDecision(t, conversion.Report.Inferred, "services.my_api.image")
	if imageDecision.Value != "zarf.internal/uds-package-my-api:1.2.3" {
		t.Fatalf("inferred image value = %q", imageDecision.Value)
	}
	if hasDecision(conversion.Report.Inferred, "services.my-api.image") {
		t.Fatalf("inferred decisions unexpectedly use normalized raw service path: %#v", conversion.Report.Inferred)
	}
	if hasDecision(conversion.Report.Translated, "services.my_api.image") {
		t.Fatalf("translated decisions unexpectedly include overridden build image: %#v", conversion.Report.Translated)
	}

	for _, path := range []string{
		"services.my_api.deploy.resources.limits.cpus",
		"services.my_api.deploy.resources.limits.memory",
		"services.my_api.deploy.resources.reservations.cpus",
		"services.my_api.deploy.resources.reservations.memory",
		"x-uds.metadata.name",
		"x-uds.metadata.version",
		"x-uds.spec.network.expose",
		"x-uds.spec.caBundle",
	} {
		requireDecision(t, conversion.Report.Translated, path)
	}
	if hasDecision(conversion.Report.Translated, "services.my_api.deploy.resources") {
		t.Fatalf("translated decisions unexpectedly include broad deploy.resources entry: %#v", conversion.Report.Translated)
	}

	for _, path := range []string{
		"services.my_api.deploy.resources.limits.pids",
		"services.my_api.deploy.resources.devices",
		"x-uds.metadata.description",
		"x-uds.spec.foo",
		"x-uds.spec.network.foo",
	} {
		requireDecision(t, conversion.Report.Ignored, path)
	}
}

func TestConvertCanonicalYAMLCompatibilityRejectionRemovesTranslatedNetwork(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    networks:
      - custom
networks:
  custom:
    driver_opts:
      encrypted: "true"
`))
	if err == nil {
		t.Fatal("ConvertCanonicalYAML() error = nil, want unsupported network options")
	}

	requireDecision(t, conversion.Report.Rejected, "networks.custom")
	if hasDecision(conversion.Report.Translated, "networks.custom") {
		t.Fatalf("translated decisions unexpectedly include rejected network path: %#v", conversion.Report.Translated)
	}
}

func TestConvertCanonicalYAMLUnsupportedVolumeTypeIsNotTranslated(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    volumes:
      - type: tmpfs
        target: /tmp/cache
`))
	if err == nil {
		t.Fatal("ConvertCanonicalYAML() error = nil, want unsupported volume type")
	}

	requireDecision(t, conversion.Report.Rejected, "services.api.volumes")
	if hasDecision(conversion.Report.Translated, "services.api.volumes[0]") {
		t.Fatalf("translated decisions unexpectedly include unsupported volume mount: %#v", conversion.Report.Translated)
	}
}

func TestConvertCanonicalYAMLInvalidMetadataNameIsIgnoredAndInferred(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
x-uds:
  metadata:
    name:
      bad: value
`))
	if err != nil {
		t.Fatalf("ConvertCanonicalYAML() error = %v", err)
	}

	requireDecision(t, conversion.Report.Ignored, "x-uds.metadata.name")
	requireDecision(t, conversion.Report.Inferred, "x-uds.metadata.name")
	if hasDecision(conversion.Report.Translated, "x-uds.metadata.name") {
		t.Fatalf("translated decisions unexpectedly include invalid metadata.name: %#v", conversion.Report.Translated)
	}
}

func TestConvertCanonicalYAMLInferredExposeAlsoInfersSSO(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    ports:
      - "8080:8080"
`))
	if err != nil {
		t.Fatalf("ConvertCanonicalYAML() error = %v", err)
	}

	requireDecision(t, conversion.Report.Inferred, "x-uds.spec.network.expose")
	sso := requireDecision(t, conversion.Report.Inferred, "x-uds.spec.sso")
	if sso.Value != "api" {
		t.Fatalf("inferred sso value = %q, want api", sso.Value)
	}
}

func TestConvertCanonicalYAMLInfersMonitorFromMetricsPort(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    ports:
      - target: 9090
        published: "9090"
        name: metrics
`))
	if err != nil {
		t.Fatalf("ConvertCanonicalYAML() error = %v", err)
	}

	monitor := requireDecision(t, conversion.Report.Inferred, "x-uds.spec.monitor")
	if monitor.Value != "api" {
		t.Fatalf("inferred monitor value = %q, want api", monitor.Value)
	}
}

func TestConvertCanonicalYAMLExplicitExposeStillInfersSSO(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    ports:
      - target: 9090
        published: "9090"
        name: metrics
x-uds:
  spec:
    network:
      expose:
        - service: api
`))
	if err != nil {
		t.Fatalf("ConvertCanonicalYAML() error = %v", err)
	}

	sso := requireDecision(t, conversion.Report.Inferred, "x-uds.spec.sso")
	if sso.Value != "api" {
		t.Fatalf("inferred sso value = %q, want api", sso.Value)
	}
}

func TestConvertCanonicalYAMLExplicitEmptyExposeDisablesInference(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    ports:
      - "8080:8080"
x-uds:
  spec:
    network:
      expose: []
`))
	if err != nil {
		t.Fatalf("ConvertCanonicalYAML() error = %v", err)
	}

	requireDecision(t, conversion.Report.Translated, "x-uds.spec.network.expose")
	if hasDecision(conversion.Report.Inferred, "x-uds.spec.network.expose") {
		t.Fatalf("inferred decisions unexpectedly include explicitly disabled exposure: %#v", conversion.Report.Inferred)
	}
	if hasDecision(conversion.Report.Inferred, "x-uds.spec.sso") {
		t.Fatalf("inferred decisions unexpectedly include SSO without an exposure: %#v", conversion.Report.Inferred)
	}
}

func TestConvertCanonicalYAMLReportsInferredNetworkAllow(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
x-uds:
  spec:
    network:
      allow:
        - direction: Egress
          remoteNamespace: logging
`))
	if err != nil {
		t.Fatalf("ConvertCanonicalYAML() error = %v", err)
	}

	requireDecision(t, conversion.Report.Inferred, "x-uds.spec.network.allow")
	requireDecision(t, conversion.Report.Translated, "x-uds.spec.network.allow")
}

func TestConvertCanonicalYAMLDoesNotTranslateExcludedOptionalDependency(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    depends_on:
      db:
        condition: service_started
        required: false
  db:
    image: docker.io/library/postgres:18
    ports:
      - "5432"
`))
	if err != nil {
		t.Fatalf("ConvertCanonicalYAML() error = %v", err)
	}

	path := "services.api.depends_on"
	if hasDecision(conversion.Report.Translated, path) {
		t.Fatalf("translated decisions unexpectedly include excluded dependency: %#v", conversion.Report.Translated)
	}
	requireDecision(t, conversion.Report.Ignored, path)
}

func TestConvertCanonicalYAMLRejectsDependencyWithoutPort(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    depends_on:
      - db
  db:
    image: docker.io/library/postgres:18
`))
	if err == nil {
		t.Fatal("ConvertCanonicalYAML() error = nil, want dependency port rejection")
	}

	requireDecision(t, conversion.Report.Rejected, "services.api.depends_on.db")
	if hasDecision(conversion.Report.Translated, "services.api.depends_on") {
		t.Fatalf("translated decisions unexpectedly include rejected dependency: %#v", conversion.Report.Translated)
	}
}

func TestConvertCanonicalYAMLRejectsDependencyWithOnlyUDPPort(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    depends_on:
      - dns
  dns:
    image: docker.io/library/bind:9.20
    expose:
      - "53/udp"
`))
	if err == nil {
		t.Fatal("ConvertCanonicalYAML() error = nil, want dependency TCP port rejection")
	}

	rejected := requireDecision(t, conversion.Report.Rejected, "services.api.depends_on.dns")
	if rejected.Code != "dependency-port" {
		t.Fatalf("rejected code = %q, want dependency-port", rejected.Code)
	}
}

func TestConvertCanonicalYAMLReportsExcludedServiceReferenceAtExtensionPath(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    depends_on:
      db:
        condition: service_started
        required: false
  db:
    image: docker.io/library/postgres:18
x-uds:
  spec:
    network:
      expose:
        - service: db
`))
	if err == nil {
		t.Fatal("ConvertCanonicalYAML() error = nil, want excluded service reference")
	}

	rejected := requireDecision(t, conversion.Report.Rejected, "x-uds.spec.network.expose")
	if rejected.Code != "invalid-setting" || rejected.Remediation == "" {
		t.Fatalf("rejected decision = %#v, want invalid-setting with remediation", rejected)
	}
	if hasDecision(conversion.Report.Translated, "x-uds.spec.network.expose") {
		t.Fatalf("translated decisions unexpectedly include rejected extension path: %#v", conversion.Report.Translated)
	}
}

func TestConvertCanonicalYAMLReportsBuildTagsAsIgnored(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    build:
      context: .
      tags:
        - ghcr.io/acme/custom:latest
`))
	if err != nil {
		t.Fatalf("ConvertCanonicalYAML() error = %v", err)
	}

	requireDecision(t, conversion.Report.Translated, "services.api.build.context")
	requireDecision(t, conversion.Report.Ignored, "services.api.build.tags")
	if hasDecision(conversion.Report.Translated, "services.api.build") {
		t.Fatalf("translated decisions unexpectedly include broad build path: %#v", conversion.Report.Translated)
	}
}

func TestConvertCanonicalYAMLEnvFileIsIgnoredInReport(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    env_file: .env
`))
	if err != nil {
		t.Fatalf("ConvertCanonicalYAML() error = %v", err)
	}

	requireDecision(t, conversion.Report.Ignored, "services.api.env_file")
	if hasDecision(conversion.Report.Translated, "services.api.env_file") {
		t.Fatalf("translated decisions unexpectedly include env_file: %#v", conversion.Report.Translated)
	}
}

func TestConvertCanonicalYAMLReportsSecurityOptPerValue(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    security_opt:
      - seccomp:unconfined
      - no-new-privileges:true
`))
	if err != nil {
		t.Fatalf("ConvertCanonicalYAML() error = %v", err)
	}

	requireDecision(t, conversion.Report.Translated, "services.api.security_opt[0]")
	requireDecision(t, conversion.Report.Ignored, "services.api.security_opt[1]")
	if hasDecision(conversion.Report.Translated, "services.api.security_opt") {
		t.Fatalf("translated decisions unexpectedly include broad security_opt path: %#v", conversion.Report.Translated)
	}
}

func TestConvertCanonicalYAMLPortsTranslationExcludesGatewayClaim(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    ports:
      - "8080:8080"
x-uds:
  spec:
    network:
      expose: []
`))
	if err != nil {
		t.Fatalf("ConvertCanonicalYAML() error = %v", err)
	}

	decision := requireDecision(t, conversion.Report.Translated, "services.api.ports")
	if decision.Target != "Kubernetes Service ports" {
		t.Fatalf("ports target = %q, want Kubernetes Service ports", decision.Target)
	}
}

func TestConvertCanonicalYAMLNestedRejectionRemovesParentTranslation(t *testing.T) {
	t.Parallel()

	conversion, err := ConvertCanonicalYAML([]byte(`name: demo
services:
  api:
    image: ghcr.io/acme/api:1.2.3
    networks:
      custom:
        aliases:
          - backend
networks:
  custom: {}
`))
	if err == nil {
		t.Fatal("ConvertCanonicalYAML() error = nil, want network options rejection")
	}

	requireDecision(t, conversion.Report.Rejected, "services.api.networks.custom")
	if hasDecision(conversion.Report.Translated, "services.api.networks") {
		t.Fatalf("translated decisions unexpectedly include rejected parent path: %#v", conversion.Report.Translated)
	}
}

func hasDecision(decisions []model.ConversionDecision, path string) bool {
	for _, decision := range decisions {
		if decision.Path == path {
			return true
		}
	}
	return false
}

func requireDecision(t *testing.T, decisions []model.ConversionDecision, path string) model.ConversionDecision {
	t.Helper()
	for _, decision := range decisions {
		if decision.Path == path {
			return decision
		}
	}
	t.Fatalf("decision path %q not found in %#v", path, decisions)
	return model.ConversionDecision{}
}
