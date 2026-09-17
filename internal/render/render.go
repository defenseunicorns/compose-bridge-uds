package render

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"

	"defenseunicorns/uds-compose-bridge/internal/inference"
	"defenseunicorns/uds-compose-bridge/internal/model"
)

const zarfSchemaURL = "https://raw.githubusercontent.com/zarf-dev/zarf/main/zarf.schema.json"

const (
	// chartDirName is the Helm chart directory written under the package root.
	chartDirName = "chart"
	// zarfValuesRel is the Zarf-templated values file (relative to the package
	// root) referenced by the component's chart valuesFiles. Only written when
	// the package has deploy-time values.
	zarfValuesRel = "values/values.yaml"
	// buildComposeFileName is the generated Compose build definition consumed by
	// Buildx Bake during Zarf package creation.
	buildComposeFileName = "build.compose.yaml"
	imageArchiveDir      = "image-archives"
	buildxBuilderPrefix  = "compose-bridge-uds"
	// secretValuePlaceholder is the sentinel injected into a marshaled Secret's
	// stringData and then replaced with a Helm value reference, so the Secret can
	// be rendered by Helm from chart values rather than a static literal.
	secretValuePlaceholder    = "__HELM_SECRET_VALUE__"
	composeNetworkLabelPrefix = "network.compose.bridge.uds.dev/"
	podReloadLabel            = "uds.dev/pod-reload"
	// additionalNetworkAllowVariable is reserved for package-level deploy-time
	// network configuration. Environment, config, and secret variables must not
	// claim the same Zarf variable name.
	additionalNetworkAllowVariable    = "ADDITIONAL_NETWORK_ALLOW"
	additionalNetworkAllowPlaceholder = "__HELM_ADDITIONAL_NETWORK_ALLOW__"
	helmReleaseNamespace              = "{{ .Release.Namespace }}"
	zarfNetworkAllowPlaceholder       = "__ZARF_ADDITIONAL_NETWORK_ALLOW__"
	domainVariable                    = "DOMAIN"
	defaultDomain                     = "uds.dev"
	helmDomainValue                   = "{{ .Values.uds.domain }}"
	zarfDomainPlaceholder             = "__ZARF_DOMAIN__"
	resourceValuePlaceholder          = "__HELM_RESOURCE_VALUE__"
)

const externalResourceHelpers = `{{- define "composeBridge.externalResourceName" -}}
{{- $description := index . 0 -}}
{{- $value := required $description (index . 1) | toString -}}
{{- $valid := and (le (len $value) 253) (regexMatch "^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$" $value) -}}
{{- range (splitList "." $value) -}}
  {{- if gt (len .) 63 -}}
    {{- $valid = false -}}
  {{- end -}}
{{- end -}}
{{- if not $valid -}}
  {{- fail (printf "%s; got %q" $description $value) -}}
{{- end -}}
{{- $value | quote -}}
{{- end -}}

{{- define "composeBridge.externalResourceKey" -}}
{{- $description := index . 0 -}}
{{- $value := required $description (index . 1) | toString -}}
{{- if or (gt (len $value) 253) (not (regexMatch "^[-._a-zA-Z0-9]+$" $value)) -}}
  {{- fail (printf "%s; got %q" $description $value) -}}
{{- end -}}
{{- $value | quote -}}
{{- end -}}
`

func WritePackage(root string, app model.App) error {
	return writePackage(root, app, false)
}

func WriteConversion(root string, conversion model.Conversion) error {
	if err := WriteConversionReport(root, conversion.Report); err != nil {
		return err
	}
	if err := writePackage(root, conversion.App, true); err != nil {
		if decisions := renderFailureDecisions(err, conversion.Report); len(decisions) > 0 {
			conversion.Report.RemoveConflictingTranslated(decisions)
			conversion.Report.Rejected = append(conversion.Report.Rejected, decisions...)
		} else {
			conversion.Report.Rejected = append(conversion.Report.Rejected, model.ConversionDecision{
				Path:        "package",
				Code:        "package-generation",
				Message:     err.Error(),
				Remediation: "address the reported package-generation error and rerun conversion",
			})
		}
		return errors.Join(err, WriteConversionReport(root, conversion.Report))
	}
	return nil
}

var monitorRenderFailurePattern = regexp.MustCompile(`^x-uds\.spec\.monitor(?:\b| )`)

type settingError struct {
	path        string
	code        string
	message     string
	remediation string
}

func (e *settingError) Error() string {
	return e.message
}

func renderFailureDecisions(err error, report model.ConversionReport) []model.ConversionDecision {
	message := err.Error()
	var sourceErr *settingError
	if errors.As(err, &sourceErr) {
		return []model.ConversionDecision{{
			Path:        reportedSourcePath(report, sourceErr.path),
			Code:        sourceErr.code,
			Message:     sourceErr.message,
			Remediation: sourceErr.remediation,
		}}
	}
	if monitorRenderFailurePattern.MatchString(message) {
		return []model.ConversionDecision{{
			Path:        "x-uds.spec.monitor",
			Code:        "invalid-setting",
			Message:     message,
			Remediation: "ensure each monitor entry references a Compose service and valid service port settings",
		}}
	}
	return nil
}

func reportedSourcePath(report model.ConversionReport, path string) string {
	for _, decision := range report.Translated {
		if decision.Path == path || equivalentServiceSettingPath(decision.Path, path) || equivalentResourcePath(decision.Path, path) {
			return decision.Path
		}
		if servicePath, ok := equivalentServicePath(decision.Path, path); ok {
			return servicePath
		}
	}
	return path
}

func equivalentServicePath(decisionPath, sourcePath string) (string, bool) {
	const prefix = "services."
	if !strings.HasPrefix(decisionPath, prefix) || !strings.HasPrefix(sourcePath, prefix) {
		return "", false
	}
	decisionEnd := strings.LastIndex(decisionPath, ".")
	if decisionEnd <= len(prefix) {
		return "", false
	}
	rawServiceName := decisionPath[len(prefix):decisionEnd]
	sourceServiceName := strings.TrimPrefix(sourcePath, prefix)
	if normalizedSourceName(rawServiceName) != normalizedSourceName(sourceServiceName) {
		return "", false
	}
	return prefix + rawServiceName, true
}

func equivalentServiceSettingPath(left, right string) bool {
	const prefix = "services."
	if !strings.HasPrefix(left, prefix) || !strings.HasPrefix(right, prefix) {
		return false
	}
	leftEnd := strings.LastIndex(left, ".")
	rightEnd := strings.LastIndex(right, ".")
	if leftEnd <= len(prefix) || rightEnd <= len(prefix) || left[leftEnd:] != right[rightEnd:] {
		return false
	}
	return normalizedSourceName(left[len(prefix):leftEnd]) == normalizedSourceName(right[len(prefix):rightEnd])
}

func equivalentResourcePath(left, right string) bool {
	for _, prefix := range []string{"configs.", "secrets.", "volumes."} {
		if strings.HasPrefix(left, prefix) && strings.HasPrefix(right, prefix) {
			return normalizedSourceName(strings.TrimPrefix(left, prefix)) == normalizedSourceName(strings.TrimPrefix(right, prefix))
		}
	}
	return false
}

func normalizedSourceName(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "_", "-")
}

func writePackage(root string, app model.App, includeConversionReport bool) error {
	if err := validatePortNames(app.Services); err != nil {
		return err
	}

	secretVariables := buildSecretVariables(app.Secrets)
	configVariables := buildConfigVariables(app.Configs)
	environmentVariables, err := buildEnvironmentVariables(app, secretVariables, configVariables)
	if err != nil {
		return err
	}

	chartDir := filepath.Join(root, chartDirName)
	templatesDir := filepath.Join(chartDir, "templates")
	for _, dir := range []string{root, chartDir, templatesDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create directory %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(templatesDir, "_helpers.tpl"), []byte(externalResourceHelpers), 0o644); err != nil {
		return fmt.Errorf("write Helm helpers: %w", err)
	}

	preserveNetworkMembership := hasDistinctNetworkMemberships(app.Services)
	servicePorts := map[string]int{}
	for _, svc := range app.Services {
		if port, ok := primaryDependencyWaitPort(svc.Ports); ok {
			servicePorts[svc.Name] = port
		}
	}

	for _, name := range sortedVolumeNames(app.Volumes) {
		volume := app.Volumes[name]
		if volume.External {
			continue
		}
		if err := writeYAMLFile(filepath.Join(templatesDir, fmt.Sprintf("pvc-%s.yaml", volume.Name)), persistentVolumeClaimManifest{
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
			Metadata: objectMeta{
				Name:      volume.Name,
				Namespace: helmReleaseNamespace,
				Labels:    appLabels(app.Package.Name, volume.Name),
			},
			Spec: persistentVolumeClaimSpec{
				AccessModes: []string{"ReadWriteOnce"},
				Resources:   pvcResources{Requests: pvcRequests{Storage: "1Gi"}},
			},
		}); err != nil {
			return err
		}
	}

	for _, name := range sortedConfigNames(app.Configs) {
		config := app.Configs[name]
		if config.External {
			continue
		}
		if err := writeComposeConfigMapTemplate(
			filepath.Join(templatesDir, fmt.Sprintf("configmap-%s.yaml", config.Name)),
			app,
			config,
		); err != nil {
			return err
		}
	}

	for _, svc := range app.Services {
		if len(svc.Env) == 0 {
			continue
		}
		if err := writeEnvironmentConfigMapTemplate(
			filepath.Join(templatesDir, fmt.Sprintf("configmap-%s-environment.yaml", svc.Name)),
			app,
			svc,
		); err != nil {
			return err
		}
	}

	// Package-owned secrets are rendered by Helm from sensitive Zarf values.
	// External secrets are not created by this chart; workloads reference an
	// existing Kubernetes Secret selected through deploy-time name and key values.
	for _, name := range sortedSecretNames(app.Secrets) {
		secret := app.Secrets[name]
		if secret.External {
			continue
		}
		variableName := secretVariables[secret.Name].Value
		if err := writeSecretTemplate(filepath.Join(templatesDir, fmt.Sprintf("secret-%s.yaml", secret.Name)), app, secret, variableName); err != nil {
			return err
		}
	}

	for _, svc := range app.Services {
		deployment, helmValues, conditionalVolumes, checksums, err := buildDeployment(
			app.Package.Name,
			helmReleaseNamespace,
			svc,
			servicePorts,
			preserveNetworkMembership,
			app.Secrets,
			secretVariables,
			app.Configs,
			configVariables,
		)
		if err != nil {
			return err
		}
		if err := writeDeploymentTemplate(filepath.Join(templatesDir, fmt.Sprintf("deployment-%s.yaml", svc.Name)), deployment, svc.Name, helmValues, conditionalVolumes, checksums); err != nil {
			return err
		}
		if err := writeYAMLFile(filepath.Join(templatesDir, fmt.Sprintf("service-%s.yaml", svc.Name)), buildService(app.Package.Name, helmReleaseNamespace, svc)); err != nil {
			return err
		}
	}

	udsPackage, err := buildUDSPackage(app)
	if err != nil {
		return err
	}
	if err := writeUDSPackageTemplate(filepath.Join(templatesDir, "uds-package.yaml"), udsPackage); err != nil {
		return err
	}

	if exemption := buildUDSExemption(app); exemption != nil {
		if err := writeYAMLFile(filepath.Join(templatesDir, "uds-exemption.yaml"), exemption); err != nil {
			return err
		}
	}

	if err := writeChartMetadata(filepath.Join(chartDir, "Chart.yaml"), app); err != nil {
		return err
	}
	if err := writeChartValues(filepath.Join(chartDir, "values.yaml"), app, secretVariables, configVariables, environmentVariables, false); err != nil {
		return err
	}

	valuesPath := filepath.Join(root, filepath.FromSlash(zarfValuesRel))
	if err := os.MkdirAll(filepath.Dir(valuesPath), 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", filepath.Dir(valuesPath), err)
	}
	if err := writeChartValues(valuesPath, app, secretVariables, configVariables, environmentVariables, true); err != nil {
		return err
	}

	if hasBuildServices(app) {
		if err := writeBuildCompose(filepath.Join(root, buildComposeFileName), app); err != nil {
			return err
		}
	}

	images := make([]string, 0, len(app.Services))
	for _, svc := range app.Services {
		images = append(images, buildComponentImages(svc, servicePorts)...)
	}
	images = dedupeStrings(images)
	flavor := inferPackageFlavor(app, images)

	if err := writePackageDocumentation(root, app, secretVariables, configVariables, environmentVariables, flavor); err != nil {
		return err
	}

	if err := writeZarfConfig(filepath.Join(root, "zarf.yaml"), app, secretVariables, configVariables, environmentVariables, images, flavor, includeConversionReport); err != nil {
		return err
	}

	return nil
}

func writePackageDocumentation(root string, app model.App, secretVariables map[string]secretVariableNames, configVariables map[string]configVariableNames, environmentVariables map[string]map[string]string, flavor string) error {
	docsDir := filepath.Join(root, "docs")
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		return fmt.Errorf("create documentation directory %s: %w", docsDir, err)
	}

	documents := map[string]string{
		"README.md":        buildPackageReadme(app, flavor),
		"configuration.md": buildConfigurationDocumentation(app, secretVariables, configVariables, environmentVariables),
		"dependencies.md":  buildDependencyDocumentation(app),
	}
	for name, content := range documents {
		path := filepath.Join(docsDir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write package documentation %s: %w", path, err)
		}
	}
	return nil
}

func buildPackageReadme(app model.App, flavor string) string {
	return fmt.Sprintf(`# %s

This UDS package was generated from Docker Compose. Zarf deploys the %s application to the %s namespace by default and controls namespace creation and placement.

## Build

Build the generated package with its inferred flavor:

%s

## Package documentation

- [Configuration](configuration.md) describes the package's deploy-time settings.
- [Dependencies](dependencies.md) lists the application services, images, and service dependencies.

Regenerate this package after changing the source Compose project; generated files are not intended for manual editing.
`, app.Package.Name, app.Package.Name, app.Package.Namespace, "```sh\nzarf package create . --flavor "+flavor+"\n```")
}

func buildConfigurationDocumentation(app model.App, secretVariables map[string]secretVariableNames, configVariables map[string]configVariableNames, environmentVariables map[string]map[string]string) string {
	var content strings.Builder
	content.WriteString("# Configuration\n\n")
	content.WriteString("Set these values when deploying the generated Zarf package.\n\n")
	content.WriteString("| Variable | Description | Default | Sensitive |\n")
	content.WriteString("|---|---|---|---|\n")
	writeDocumentationVariable(&content, domainVariable, "Cluster domain used by generated application endpoints", defaultDomain, false)
	writeDocumentationVariable(&content, additionalNetworkAllowVariable, "Additional UDS network allow rules supplied as a YAML array", "[]", false)

	for _, secretName := range sortedSecretNames(app.Secrets) {
		secret := app.Secrets[secretName]
		variable := secretVariables[secretName]
		if secret.External {
			writeDocumentationVariable(&content, variable.SecretName, fmt.Sprintf("Kubernetes Secret name for external Compose secret %s", secretName), "", false)
			writeDocumentationVariable(&content, variable.SecretKey, fmt.Sprintf("Key in the Kubernetes Secret for external Compose secret %s", secretName), secret.Name, false)
			continue
		}
		writeDocumentationVariable(&content, variable.Value, fmt.Sprintf("Value for Compose secret %s", secretName), "", true)
	}
	for _, configName := range sortedConfigNames(app.Configs) {
		config := app.Configs[configName]
		if !config.External {
			continue
		}
		variable := configVariables[configName]
		writeDocumentationVariable(&content, variable.ConfigMapName, fmt.Sprintf("Kubernetes ConfigMap name for external Compose config %s", configName), config.ExternalName, false)
		writeDocumentationVariable(&content, variable.ConfigMapKey, fmt.Sprintf("Key in the Kubernetes ConfigMap for external Compose config %s", configName), config.Name, false)
	}

	for _, svc := range app.Services {
		resourceVariables := buildResourceVariableNames(svc.Name)
		writeDocumentationVariable(&content, resourceVariables.CPURequest, fmt.Sprintf("CPU request for Compose service %s", svc.Name), svc.Resources.Requests.CPU, false)
		writeDocumentationVariable(&content, resourceVariables.MemoryRequest, fmt.Sprintf("Memory request for Compose service %s", svc.Name), svc.Resources.Requests.Memory, false)
		writeDocumentationVariable(&content, resourceVariables.CPULimit, fmt.Sprintf("CPU limit for Compose service %s", svc.Name), svc.Resources.Limits.CPU, false)
		writeDocumentationVariable(&content, resourceVariables.MemoryLimit, fmt.Sprintf("Memory limit for Compose service %s", svc.Name), svc.Resources.Limits.Memory, false)
		for _, item := range svc.Env {
			writeDocumentationVariable(
				&content,
				environmentVariables[svc.Name][item.Name],
				fmt.Sprintf("Value for %s environment variable on Compose service %s", item.Name, svc.Name),
				item.Value,
				false,
			)
		}
	}

	bridgeValues := buildBridgeHelmValues(app.Configs)
	if len(bridgeValues) > 0 {
		content.WriteString("\n## Helm values\n\n")
		content.WriteString("These public chart values control Compose configs declared with `x-compose-bridge`.\n\n")
		content.WriteString("| Value | Description | Default |\n")
		content.WriteString("|---|---|---|\n")
		for _, value := range bridgeValues {
			fmt.Fprintf(&content, "| `%s` | %s | `%s` |\n", markdownTableValue(value.Name), markdownTableValue(value.Description), markdownTableValue(fmt.Sprint(value.Default)))
		}
	}

	return content.String()
}

func writeDocumentationVariable(content *strings.Builder, name, description, defaultValue string, sensitive bool) {
	defaultText := "—"
	if defaultValue != "" {
		defaultText = markdownTableValue(defaultValue)
	}
	fmt.Fprintf(content, "| `%s` | %s | %s | %t |\n", markdownTableValue(name), markdownTableValue(description), defaultText, sensitive)
}

func markdownTableValue(value string) string {
	value = strings.ReplaceAll(value, "|", "\\|")
	value = strings.ReplaceAll(value, "\r\n", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

func buildDependencyDocumentation(app model.App) string {
	var content strings.Builder
	content.WriteString("# Dependencies\n\n")
	content.WriteString("The generated package contains these application services and container images.\n\n")
	content.WriteString("| Service | Image | Depends on |\n")
	content.WriteString("|---|---|---|\n")
	for _, svc := range app.Services {
		dependencies := make([]string, 0, len(svc.DependsOn))
		for _, dependency := range svc.DependsOn {
			label := dependency.Service
			if dependency.Condition != "" {
				label += " (" + dependency.Condition + ")"
			}
			dependencies = append(dependencies, label)
		}
		dependencyText := "—"
		if len(dependencies) > 0 {
			dependencyText = strings.Join(dependencies, ", ")
		}
		fmt.Fprintf(&content, "| `%s` | `%s` | %s |\n", markdownTableValue(svc.Name), markdownTableValue(svc.Image), markdownTableValue(dependencyText))
	}
	return content.String()
}

// Compose build workspace and Zarf package-creation actions.

func hasBuildServices(app model.App) bool {
	for _, svc := range app.Services {
		if svc.Build != nil {
			return true
		}
	}
	return false
}

func writeBuildCompose(path string, app model.App) error {
	services := map[string]buildComposeService{}
	for _, svc := range app.Services {
		if svc.Build == nil {
			continue
		}
		services[svc.Name] = buildComposeService{
			Image: svc.Image,
			Build: svc.Build.Config,
		}
	}
	return writeYAMLFile(path, buildComposeFile{
		Name:     app.Package.Name,
		Services: services,
		Secrets:  app.BuildSecrets,
	})
}

func buildCreateActions(app model.App) *zarfComponentActions {
	if !hasBuildServices(app) {
		return nil
	}
	builderVariables := []string{
		`docker_context="$(docker context show)"`,
		`builder_context="$(printf '%s' "$docker_context" | tr -c '[:alnum:]_.-' '-')"`,
		fmt.Sprintf(`builder_name=%s"$builder_context"`, shellQuote(buildxBuilderPrefix+"-")),
	}
	ensureBuilderLines := []string{
		"set -eu",
	}
	ensureBuilderLines = append(ensureBuilderLines, builderVariables...)
	ensureBuilderLines = append(ensureBuilderLines,
		`if ! docker buildx inspect "$builder_name" >/dev/null 2>&1; then`,
		`  docker buildx create --name "$builder_name" --driver docker-container "$docker_context"`,
		"fi",
	)
	ensureBuilder := strings.Join(ensureBuilderLines, "\n")

	archives := []string{}
	arguments := []string{
		"docker buildx bake",
		`  --builder "$builder_name"`,
		"  --file " + shellQuote(buildComposeFileName),
		"  --progress plain",
	}
	readPaths := map[string]struct{}{}
	for _, svc := range app.Services {
		if svc.Build == nil {
			continue
		}
		for _, path := range svc.Build.ReadPaths {
			readPaths[path] = struct{}{}
		}
	}
	for _, path := range sortedStringSet(readPaths) {
		arguments = append(arguments, "  --allow "+shellQuote("fs.read="+path))
	}
	targets := []string{}
	for _, svc := range app.Services {
		if svc.Build == nil {
			continue
		}
		target := svc.Name
		archive := filepath.ToSlash(filepath.Join(imageArchiveDir, svc.Name+".tar"))
		archives = append(archives, shellQuote(archive))
		if !buildDeclaresPlatforms(svc.Build.Config) {
			arguments = append(arguments,
				"  --set "+shellQuote(target+".platform+=linux/amd64"),
				"  --set "+shellQuote(target+".platform+=linux/arm64"),
			)
		}
		arguments = append(arguments, "  --set "+shellQuote(target+".output=type=oci,dest="+archive))
		targets = append(targets, shellQuote(target))
	}
	arguments = append(arguments, "  "+strings.Join(targets, " "))
	for i := 0; i < len(arguments)-1; i++ {
		arguments[i] += " \\"
	}

	buildImageLines := []string{
		"set -eu",
	}
	buildImageLines = append(buildImageLines, builderVariables...)
	buildImageLines = append(buildImageLines,
		"mkdir -p "+shellQuote(imageArchiveDir),
		"rm -f "+strings.Join(archives, " "),
		strings.Join(arguments, "\n"),
	)
	buildImages := strings.Join(buildImageLines, "\n")

	return &zarfComponentActions{OnCreate: zarfComponentActionSet{Before: []zarfComponentAction{
		{Description: "Ensure the Compose Bridge Buildx builder exists", Cmd: ensureBuilder},
		{Description: "Build Compose images for the package", Cmd: buildImages},
	}}}
}

func buildDeclaresPlatforms(config map[string]any) bool {
	value, ok := config["platforms"]
	if !ok || value == nil {
		return false
	}
	switch platforms := value.(type) {
	case []any:
		return len(platforms) > 0
	case []string:
		return len(platforms) > 0
	case string:
		return strings.TrimSpace(platforms) != ""
	default:
		return true
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func sortedStringSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// writeDeploymentTemplate replaces the generated resource sentinel with a Helm
// block that emits only resource quantities supplied for this service.
func writeDeploymentTemplate(path string, manifest deploymentManifest, serviceName string, helmValues []helmValueReplacement, conditionalVolumes []conditionalVolume, checksums []configChecksum) error {
	marshaled, err := yamlv3.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal yaml for %s: %w", path, err)
	}

	lines := strings.Split(string(marshaled), "\n")
	start, end := -1, -1
	baseIndent := 0
	for i, line := range lines {
		if strings.TrimSpace(line) != "resources:" {
			continue
		}
		limit := i + 7
		if limit > len(lines) {
			limit = len(lines)
		}
		if !strings.Contains(strings.Join(lines[i:limit], "\n"), resourceValuePlaceholder) {
			continue
		}
		start = i
		baseIndent = len(line) - len(strings.TrimLeft(line, " "))
		end = len(lines)
		for j := i + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if trimmed == "" {
				continue
			}
			indent := len(lines[j]) - len(strings.TrimLeft(lines[j], " "))
			if indent <= baseIndent {
				end = j
				break
			}
		}
		break
	}
	if start < 0 {
		return fmt.Errorf("render resource template in %s: placeholder not found", path)
	}

	indent := strings.Repeat(" ", baseIndent)
	childIndent := indent + "    "
	quantityIndent := childIndent + "    "
	template := []string{
		indent + "# {{ $allResources := .Values.resources | default (dict) }}",
		indent + fmt.Sprintf("# {{ $serviceResources := (index $allResources %q) | default (dict) }}", serviceName),
		indent + "# {{ $requests := (index $serviceResources \"requests\") | default (dict) }}",
		indent + "# {{ $limits := (index $serviceResources \"limits\") | default (dict) }}",
		indent + "# {{ $cpuRequest := (index $requests \"cpu\") | default \"\" }}",
		indent + "# {{ $memoryRequest := (index $requests \"memory\") | default \"\" }}",
		indent + "# {{ $cpuLimit := (index $limits \"cpu\") | default \"\" }}",
		indent + "# {{ $memoryLimit := (index $limits \"memory\") | default \"\" }}",
		indent + "# {{ if or $cpuRequest $memoryRequest $cpuLimit $memoryLimit }}",
		indent + "resources:",
		childIndent + "# {{ if or $cpuRequest $memoryRequest }}",
		childIndent + "requests:",
		quantityIndent + "# {{ with $cpuRequest }}",
		quantityIndent + "cpu: \"{{ . }}\"",
		quantityIndent + "# {{ end }}",
		quantityIndent + "# {{ with $memoryRequest }}",
		quantityIndent + "memory: \"{{ . }}\"",
		quantityIndent + "# {{ end }}",
		childIndent + "# {{ end }}",
		childIndent + "# {{ if or $cpuLimit $memoryLimit }}",
		childIndent + "limits:",
		quantityIndent + "# {{ with $cpuLimit }}",
		quantityIndent + "cpu: \"{{ . }}\"",
		quantityIndent + "# {{ end }}",
		quantityIndent + "# {{ with $memoryLimit }}",
		quantityIndent + "memory: \"{{ . }}\"",
		quantityIndent + "# {{ end }}",
		childIndent + "# {{ end }}",
		indent + "# {{ end }}",
	}
	lines = append(lines[:start], append(template, lines[end:]...)...)
	rendered := strings.Join(lines, "\n")
	for _, value := range helmValues {
		if count := strings.Count(rendered, value.Placeholder); count != 1 {
			return fmt.Errorf("render Helm value in %s: placeholder %q occurs %d times", path, value.Placeholder, count)
		}
		rendered = strings.Replace(rendered, value.Placeholder, value.Template, 1)
	}
	for _, volume := range conditionalVolumes {
		var err error
		rendered, err = wrapNamedYAMLListItems(rendered, volume.Name, helmValueExpression(volume.EnabledValue))
		if err != nil {
			return fmt.Errorf("render conditional config volume in %s: %w", path, err)
		}
	}
	if len(conditionalVolumes) > 0 {
		rendered = wrapConditionalListHeaders(rendered, conditionalVolumes)
	}
	if len(checksums) > 0 {
		var err error
		rendered, err = renderConfigChecksums(rendered, checksums)
		if err != nil {
			return fmt.Errorf("render config checksum in %s: %w", path, err)
		}
	}
	if !strings.HasSuffix(rendered, "\n") {
		rendered += "\n"
	}
	return writeRenderedFile(path, rendered)
}

func wrapNamedYAMLListItems(rendered, name, condition string) (string, error) {
	lines := strings.Split(rendered, "\n")
	out := make([]string, 0, len(lines)+4)
	matched := 0
	for i := 0; i < len(lines); {
		line := lines[i]
		if strings.TrimSpace(line) != "- name: "+name {
			out = append(out, line)
			i++
			continue
		}
		indentSize := len(line) - len(strings.TrimLeft(line, " "))
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			lineIndent := len(lines[j]) - len(strings.TrimLeft(lines[j], " "))
			if lineIndent <= indentSize {
				end = j
				break
			}
		}
		indent := strings.Repeat(" ", indentSize)
		out = append(out, indent+"# {{- if "+condition+" }}")
		out = append(out, lines[i:end]...)
		out = append(out, indent+"# {{- end }}")
		matched++
		i = end
	}
	if matched == 0 {
		return "", fmt.Errorf("volume %q was not found", name)
	}
	return strings.Join(out, "\n"), nil
}

func wrapConditionalListHeaders(rendered string, conditionalVolumes []conditionalVolume) string {
	conditionsByName := map[string]string{}
	for _, volume := range conditionalVolumes {
		conditionsByName[volume.Name] = helmValueExpression(volume.EnabledValue)
	}
	lines := strings.Split(rendered, "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != "volumeMounts:" && trimmed != "volumes:" {
			continue
		}
		indentSize := len(lines[i]) - len(strings.TrimLeft(lines[i], " "))
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			lineIndent := len(lines[j]) - len(strings.TrimLeft(lines[j], " "))
			if lineIndent <= indentSize {
				end = j
				break
			}
		}
		conditions := map[string]struct{}{}
		hasItem := false
		allConditional := true
		for _, line := range lines[i+1 : end] {
			item := strings.TrimSpace(line)
			if !strings.HasPrefix(item, "- name: ") {
				continue
			}
			hasItem = true
			name := strings.TrimPrefix(item, "- name: ")
			condition, exists := conditionsByName[name]
			if !exists {
				allConditional = false
				break
			}
			conditions[condition] = struct{}{}
		}
		if !hasItem || !allConditional {
			continue
		}
		conditionList := sortedStringSet(conditions)
		condition := "(" + conditionList[0] + ")"
		if len(conditionList) > 1 {
			wrapped := make([]string, 0, len(conditionList))
			for _, item := range conditionList {
				wrapped = append(wrapped, "("+item+")")
			}
			condition = "or " + strings.Join(wrapped, " ")
		}
		indent := strings.Repeat(" ", indentSize)
		block := []string{indent + "# {{- if " + condition + " }}"}
		block = append(block, lines[i:end]...)
		block = append(block, indent+"# {{- end }}")
		lines = append(lines[:i], append(block, lines[end:]...)...)
		i += len(block) - 1
	}
	return strings.Join(lines, "\n")
}

func renderConfigChecksums(rendered string, checksums []configChecksum) (string, error) {
	lines := strings.Split(rendered, "\n")
	for _, checksum := range checksums {
		found := false
		for i, line := range lines {
			if !strings.Contains(line, checksum.Placeholder) {
				continue
			}
			value := fmt.Sprintf("{{ %s | sha256sum | quote }}", checksumContentExpression(checksum))
			line = strings.Replace(line, checksum.Placeholder, value, 1)
			if checksum.EnabledValue == "" {
				lines[i] = line
			} else {
				indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
				wrapped := []string{
					indent + "# {{- if " + helmValueExpression(checksum.EnabledValue) + " }}",
					line,
					indent + "# {{- end }}",
				}
				lines = append(lines[:i], append(wrapped, lines[i+1:]...)...)
			}
			found = true
			break
		}
		if !found {
			return "", fmt.Errorf("placeholder %q was not found", checksum.Placeholder)
		}
	}

	allConditional := true
	conditions := []string{}
	for _, checksum := range checksums {
		if checksum.EnabledValue == "" {
			allConditional = false
			break
		}
		conditions = append(conditions, "("+helmValueExpression(checksum.EnabledValue)+")")
	}
	if !allConditional {
		return strings.Join(lines, "\n"), nil
	}
	condition := conditions[0]
	if len(conditions) > 1 {
		condition = "or " + strings.Join(conditions, " ")
	}
	for i, line := range lines {
		if strings.TrimSpace(line) != "annotations:" {
			continue
		}
		block := strings.Join(lines[i:], "\n")
		if !strings.Contains(block, "checksum/") {
			continue
		}
		indentSize := len(line) - len(strings.TrimLeft(line, " "))
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			lineIndent := len(lines[j]) - len(strings.TrimLeft(lines[j], " "))
			if lineIndent <= indentSize {
				end = j
				break
			}
		}
		indent := strings.Repeat(" ", indentSize)
		wrapped := []string{indent + "# {{- if " + condition + " }}"}
		wrapped = append(wrapped, lines[i:end]...)
		wrapped = append(wrapped, indent+"# {{- end }}")
		lines = append(lines[:i], append(wrapped, lines[end:]...)...)
		return strings.Join(lines, "\n"), nil
	}
	return "", fmt.Errorf("annotations block was not found")
}

func checksumContentExpression(checksum configChecksum) string {
	if checksum.ContentValue != "" {
		return helmValueExpression(checksum.ContentValue) + " | toString"
	}
	return strconv.Quote(checksum.StaticContent)
}

// writeSecretTemplate writes a Helm-templated Secret whose value is sourced from
// chart values (.Values.secrets.<variableName>) rather than a static literal, so
// Zarf can inject the sensitive value via the chart values file at deploy time.
func writeSecretTemplate(path string, app model.App, secret model.Secret, variableName string) error {
	data, err := yamlv3.Marshal(secretManifest{
		APIVersion: "v1",
		Kind:       "Secret",
		Metadata: objectMeta{
			Name:      secret.Name,
			Namespace: helmReleaseNamespace,
			Labels:    appLabels(app.Package.Name, secret.Name),
		},
		Type:       "Opaque",
		StringData: map[string]string{secret.Name: secretValuePlaceholder},
	})
	if err != nil {
		return fmt.Errorf("marshal yaml for %s: %w", path, err)
	}
	rendered := strings.ReplaceAll(string(data), secretValuePlaceholder,
		fmt.Sprintf("{{ .Values.secrets.%s | quote }}", variableName))
	return writeRenderedFile(path, rendered)
}

// writeEnvironmentConfigMapTemplate writes one observable configuration object
// per service. Helm supplies each value, and the UDS pod-reload label causes the
// consuming workload to roll when deploy-time configuration changes.
func writeEnvironmentConfigMapTemplate(path string, app model.App, svc model.Service) error {
	data := make(map[string]string, len(svc.Env))
	placeholders := make(map[string]string, len(svc.Env))
	for i, item := range svc.Env {
		placeholder := fmt.Sprintf("__HELM_ENVIRONMENT_VALUE_%d__", i)
		data[item.Name] = placeholder
		placeholders[placeholder] = fmt.Sprintf(
			"{{ index .Values.environment %q %q | quote }}",
			svc.Name,
			item.Name,
		)
	}

	manifest := configMapManifest{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata: objectMeta{
			Name:      environmentConfigMapName(svc.Name),
			Namespace: helmReleaseNamespace,
			Labels:    reloadableAppLabels(app.Package.Name, svc.Name),
		},
		Data: data,
	}
	marshaled, err := yamlv3.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal yaml for %s: %w", path, err)
	}
	rendered := string(marshaled)
	for placeholder, helmValue := range placeholders {
		rendered = strings.ReplaceAll(rendered, placeholder, helmValue)
	}
	return writeRenderedFile(path, rendered)
}

// writeComposeConfigMapTemplate preserves the native Compose config content for
// ordinary configs and substitutes the declared public Helm value for configs
// carrying x-compose-bridge rendering controls.
func writeComposeConfigMapTemplate(path string, app model.App, config model.Config) error {
	data := map[string]string{}
	contentPlaceholders := map[string]string{}
	for i, key := range composeConfigDataKeys(app.Services, config) {
		if config.Bridge != nil && config.Bridge.ContentValue != nil {
			placeholder := fmt.Sprintf("__HELM_COMPOSE_CONFIG_CONTENT_%d__", i)
			data[key] = placeholder
			contentPlaceholders[placeholder] = helmStringValue(config.Bridge.ContentValue.Name) + " | quote }}"
		} else {
			data[key] = config.Content
		}
	}
	manifest := configMapManifest{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata: objectMeta{
			Name:      composeConfigMapName(app.Package.Name, config),
			Namespace: helmReleaseNamespace,
			Labels:    reloadableAppLabels(app.Package.Name, config.Name),
		},
		Data: data,
	}
	marshaled, err := yamlv3.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal yaml for %s: %w", path, err)
	}
	rendered := string(marshaled)
	if config.Bridge == nil {
		return writeRenderedFile(path, rendered)
	}
	for placeholder, templateValue := range contentPlaceholders {
		rendered = strings.Replace(rendered, placeholder, templateValue, 1)
	}
	if config.Bridge.EnabledValue != nil {
		rendered = fmt.Sprintf("{{- if %s }}\n%s{{- end }}\n", helmValueExpression(config.Bridge.EnabledValue.Name), rendered)
	}
	return writeRenderedFile(path, rendered)
}

func helmStringValue(name string) string {
	return fmt.Sprintf("{{ %s | toString", helmValueExpression(name))
}

func helmValueExpression(name string) string {
	return fmt.Sprintf("index .Values %q", name)
}

func composeConfigMapName(appName string, config model.Config) string {
	if config.Bridge == nil {
		return config.Name
	}
	return sanitizeManifestName(appName + "-" + config.Name)
}

func composeConfigDataKeys(services []model.Service, config model.Config) []string {
	keys := map[string]struct{}{}
	for _, svc := range services {
		for _, ref := range svc.Configs {
			if ref.Source == config.Name {
				keys[composeConfigDataKey(ref, config)] = struct{}{}
			}
		}
	}
	if len(keys) == 0 {
		keys[config.Name] = struct{}{}
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func composeConfigDataKey(ref model.FileRef, config model.Config) string {
	if config.Bridge == nil {
		return config.Name
	}
	target := strings.TrimSpace(ref.Target)
	if target == "" {
		return config.Name
	}
	key := filepath.Base(target)
	if key == "." || key == "/" || key == "" || !validConfigMapKey.MatchString(key) {
		return config.Name
	}
	return key
}

// writeUDSPackageTemplate preserves the statically generated network rules and
// appends deploy-time rules supplied through the chart value.
func writeUDSPackageTemplate(path string, manifest udsPackageManifest) error {
	manifest.Spec.Network.Allow = append(manifest.Spec.Network.Allow, additionalNetworkAllowPlaceholder)
	marshaled, err := yamlv3.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshal yaml for %s: %w", path, err)
	}

	placeholderLine := "            - " + additionalNetworkAllowPlaceholder
	templateBlock := strings.Join([]string{
		"            {{- with .Values.additionalNetworkAllow }}",
		"            {{ toYaml . | nindent 12 }}",
		"            {{- end }}",
	}, "\n")
	rendered := strings.Replace(string(marshaled), placeholderLine, templateBlock, 1)
	if rendered == string(marshaled) {
		return fmt.Errorf("render additional network allow template in %s: placeholder not found", path)
	}
	return writeRenderedFile(path, rendered)
}

// writeChartMetadata writes the generated chart's Chart.yaml.
func writeChartMetadata(path string, app model.App) error {
	return writeYAMLFile(path, chartMetadata{
		APIVersion:  "v2",
		Name:        app.Package.Name,
		Description: fmt.Sprintf("UDS package generated from Docker Compose for %s", app.Package.Name),
		Type:        "application",
		Version:     app.Package.Version,
		AppVersion:  app.Package.UpstreamVersion,
	})
}

// writeChartValues writes service environment, config reference, and secret
// values either as chart defaults or as Zarf variable placeholders.
func writeChartValues(
	path string,
	app model.App,
	secretVariables map[string]secretVariableNames,
	configVariables map[string]configVariableNames,
	environmentVariables map[string]map[string]string,
	placeholder bool,
) error {
	values := chartValues{
		AdditionalNetworkAllow: []any{},
		Environment:            map[string]map[string]string{},
		Resources:              map[string]resourceValues{},
		Secrets:                map[string]string{},
		ExternalSecrets:        map[string]externalResourceValues{},
		ExternalConfigs:        map[string]externalResourceValues{},
		UDS:                    udsValues{Domain: defaultDomain},
		Generated:              map[string]any{},
	}
	if placeholder {
		values.AdditionalNetworkAllow = zarfNetworkAllowPlaceholder
		values.UDS.Domain = zarfDomainPlaceholder
	}

	for _, svc := range app.Services {
		resourceVariables := buildResourceVariableNames(svc.Name)
		resourceValues := resourceValues{
			Requests: resourceQuantityValues{CPU: svc.Resources.Requests.CPU, Memory: svc.Resources.Requests.Memory},
			Limits:   resourceQuantityValues{CPU: svc.Resources.Limits.CPU, Memory: svc.Resources.Limits.Memory},
		}
		if placeholder {
			resourceValues.Requests.CPU = fmt.Sprintf("###ZARF_VAR_%s###", resourceVariables.CPURequest)
			resourceValues.Requests.Memory = fmt.Sprintf("###ZARF_VAR_%s###", resourceVariables.MemoryRequest)
			resourceValues.Limits.CPU = fmt.Sprintf("###ZARF_VAR_%s###", resourceVariables.CPULimit)
			resourceValues.Limits.Memory = fmt.Sprintf("###ZARF_VAR_%s###", resourceVariables.MemoryLimit)
		}
		values.Resources[svc.Name] = resourceValues

		if len(svc.Env) > 0 {
			serviceValues := make(map[string]string, len(svc.Env))
			for _, item := range svc.Env {
				value := item.Value
				if placeholder {
					value = fmt.Sprintf("###ZARF_VAR_%s###", environmentVariables[svc.Name][item.Name])
				}
				serviceValues[item.Name] = value
			}
			values.Environment[svc.Name] = serviceValues
		}
	}

	for _, name := range sortedSecretNames(app.Secrets) {
		secret := app.Secrets[name]
		variable := secretVariables[name]
		if secret.External {
			external := externalResourceValues{Key: plainChartString(secret.Name)}
			if placeholder {
				external.Name = zarfChartString(variable.SecretName)
				external.Key = zarfChartString(variable.SecretKey)
			}
			values.ExternalSecrets[variable.ValuesKey] = external
			continue
		}
		if placeholder {
			values.Secrets[variable.Value] = fmt.Sprintf("###ZARF_VAR_%s###", variable.Value)
		} else {
			values.Secrets[variable.Value] = ""
		}
	}
	for _, name := range sortedConfigNames(app.Configs) {
		config := app.Configs[name]
		if !config.External {
			continue
		}
		variable := configVariables[name]
		external := externalResourceValues{
			Name: plainChartString(config.ExternalName),
			Key:  plainChartString(config.Name),
		}
		if placeholder {
			external.Name = zarfChartString(variable.ConfigMapName)
			external.Key = zarfChartString(variable.ConfigMapKey)
		}
		values.ExternalConfigs[variable.ValuesKey] = external
	}
	for _, value := range buildBridgeHelmValues(app.Configs) {
		values.Generated[value.Name] = value.Default
	}

	marshaled, err := yamlv3.Marshal(values)
	if err != nil {
		return fmt.Errorf("marshal yaml for %s: %w", path, err)
	}
	rendered := string(marshaled)
	if placeholder {
		placeholderLine := "additionalNetworkAllow: " + zarfNetworkAllowPlaceholder
		variableBlock := "additionalNetworkAllow:\n  ###ZARF_VAR_" + additionalNetworkAllowVariable + "###"
		rendered = strings.Replace(rendered, placeholderLine, variableBlock, 1)
		if rendered == string(marshaled) {
			return fmt.Errorf("render additional network allow Zarf variable in %s: placeholder not found", path)
		}

		domainPlaceholderLine := "    domain: " + zarfDomainPlaceholder
		domainVariableLine := "    domain: \"###ZARF_VAR_" + domainVariable + "###\""
		withDomain := strings.Replace(rendered, domainPlaceholderLine, domainVariableLine, 1)
		if withDomain == rendered {
			return fmt.Errorf("render domain Zarf variable in %s: placeholder not found", path)
		}
		rendered = withDomain
	}
	if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}
	return nil
}

func writeZarfConfig(
	path string,
	app model.App,
	secretVariables map[string]secretVariableNames,
	configVariables map[string]configVariableNames,
	environmentVariables map[string]map[string]string,
	images []string,
	flavor string,
	includeConversionReport bool,
) error {
	variables := []zarfVariable{
		{
			Name:        domainVariable,
			Description: "The domain for accessing endpoints",
			Default:     stringPointer(defaultDomain),
		},
		{
			Name:        additionalNetworkAllowVariable,
			Description: "Additional UDS network allow rules (YAML array)",
			Default:     stringPointer("[]"),
			AutoIndent:  true,
		},
	}
	for _, secretName := range sortedSecretNames(app.Secrets) {
		secret := app.Secrets[secretName]
		variable := secretVariables[secretName]
		if secret.External {
			variables = append(variables,
				zarfVariable{
					Name:        variable.SecretName,
					Description: fmt.Sprintf("Kubernetes Secret name for external compose secret %s", secretName),
					AutoIndent:  true,
				},
				zarfVariable{
					Name:        variable.SecretKey,
					Description: fmt.Sprintf("Key in the Kubernetes Secret for external compose secret %s", secretName),
					Default:     stringPointer(secret.Name),
					AutoIndent:  true,
				},
			)
			continue
		}
		variables = append(variables, zarfVariable{
			Name:        variable.Value,
			Description: fmt.Sprintf("Value for compose secret %s", secretName),
			Prompt:      true,
			Sensitive:   true,
		})
	}
	for _, configName := range sortedConfigNames(app.Configs) {
		config := app.Configs[configName]
		if !config.External {
			continue
		}
		variable := configVariables[configName]
		variables = append(variables,
			zarfVariable{
				Name:        variable.ConfigMapName,
				Description: fmt.Sprintf("Kubernetes ConfigMap name for external compose config %s", configName),
				Default:     optionalStringPointer(config.ExternalName),
				AutoIndent:  true,
			},
			zarfVariable{
				Name:        variable.ConfigMapKey,
				Description: fmt.Sprintf("Key in the Kubernetes ConfigMap for external compose config %s", configName),
				Default:     stringPointer(config.Name),
				AutoIndent:  true,
			},
		)
	}
	for _, svc := range app.Services {
		resourceVariables := buildResourceVariableNames(svc.Name)
		variables = append(variables,
			zarfVariable{Name: resourceVariables.CPURequest, Description: fmt.Sprintf("CPU request for compose service %s", svc.Name), Default: stringPointer(svc.Resources.Requests.CPU)},
			zarfVariable{Name: resourceVariables.MemoryRequest, Description: fmt.Sprintf("Memory request for compose service %s", svc.Name), Default: stringPointer(svc.Resources.Requests.Memory)},
			zarfVariable{Name: resourceVariables.CPULimit, Description: fmt.Sprintf("CPU limit for compose service %s", svc.Name), Default: stringPointer(svc.Resources.Limits.CPU)},
			zarfVariable{Name: resourceVariables.MemoryLimit, Description: fmt.Sprintf("Memory limit for compose service %s", svc.Name), Default: stringPointer(svc.Resources.Limits.Memory)},
		)
		for _, item := range svc.Env {
			variables = append(variables, zarfVariable{
				Name:        environmentVariables[svc.Name][item.Name],
				Description: fmt.Sprintf("Value for %s environment variable on compose service %s", item.Name, svc.Name),
				Default:     stringPointer(item.Value),
			})
		}
	}

	pkg := zarfPackageConfig{
		APIVersion: "zarf.dev/v1alpha1",
		Kind:       "ZarfPackageConfig",
		Metadata: zarfMetadata{
			Name:        app.Package.Name,
			Description: fmt.Sprintf("UDS package generated from Docker Compose for %s", app.Package.Name),
			Version:     app.Package.Version,
			Annotations: buildPackageMetadataAnnotations(app),
		},
		Variables: variables,
		Documentation: map[string]string{
			"readme":        "docs/README.md",
			"configuration": "docs/configuration.md",
			"dependencies":  "docs/dependencies.md",
		},
	}
	if includeConversionReport {
		pkg.Documentation["conversion"] = "docs/conversion.md"
	}

	chart := zarfChart{
		Name:      app.Package.Name,
		Namespace: app.Package.Namespace,
		LocalPath: chartDirName,
		Version:   app.Package.Version,
		ValuesFiles: []string{
			zarfValuesRel,
		},
	}
	component := zarfComponent{
		Name:        app.Package.Name,
		Required:    true,
		Description: fmt.Sprintf("Deploy %s", app.Package.Name),
		Only:        &zarfComponentOnly{Flavor: flavor},
		Charts:      []zarfChart{chart},
		Images:      dedupeStrings(images),
		Actions:     buildCreateActions(app),
	}
	for _, svc := range app.Services {
		if svc.Build == nil {
			continue
		}
		component.ImageArchives = append(component.ImageArchives, zarfImageArchive{
			Path:   filepath.ToSlash(filepath.Join(imageArchiveDir, svc.Name+".tar")),
			Images: []string{svc.Image},
		})
	}
	pkg.Components = append(pkg.Components, component)

	data, err := yamlv3.Marshal(pkg)
	if err != nil {
		return fmt.Errorf("marshal zarf config: %w", err)
	}
	content := append([]byte("# yaml-language-server: $schema="+zarfSchemaURL+"\n"), data...)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write zarf config: %w", err)
	}
	return nil
}

func buildPackageMetadataAnnotations(app model.App) map[string]string {
	title := titleCase(app.Package.Name)
	annotations := map[string]string{
		"dev.uds.title":   title,
		"dev.uds.tagline": fmt.Sprintf("%s automatically generated by Docker Compose Bridge UDS.", title),
		"dev.uds.icon":    "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(generatedPackageIcon(app.Package.Name))),
	}
	for key, value := range app.Package.Annotations {
		annotations[key] = value
	}
	return annotations
}

func generatedPackageIcon(packageName string) string {
	digest := sha256.Sum256([]byte(packageName))
	hue := (int(digest[0])<<8 | int(digest[1])) % 360
	background := fmt.Sprintf("hsl(%d, 45%%, 18%%)", hue)
	top := fmt.Sprintf("hsl(%d, 75%%, 72%%)", hue)
	left := fmt.Sprintf("hsl(%d, 70%%, 52%%)", hue)
	right := fmt.Sprintf("hsl(%d, 75%%, 38%%)", hue)

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 200"><rect width="200" height="200" rx="32" fill="%s"/><path d="M45 62l55-30 55 30-55 30z" fill="%s"/><path d="M45 72l50 27v58l-50-28z" fill="%s"/><path d="M155 72l-50 27v58l50-28z" fill="%s"/></svg>`, background, top, left, right)
}

func inferPackageFlavor(app model.App, images []string) string {
	if len(images) == 0 {
		return "upstream"
	}
	for _, svc := range app.Services {
		if svc.Build != nil {
			return "upstream"
		}
	}
	for _, image := range images {
		if imageRegistry(image) != "registry1.dso.mil" {
			return "upstream"
		}
	}
	return "registry1"
}

func imageRegistry(image string) string {
	parts := strings.SplitN(strings.TrimSpace(image), "/", 2)
	if len(parts) < 2 {
		return "docker.io"
	}
	registry := strings.ToLower(parts[0])
	if !strings.ContainsAny(registry, ".:") && registry != "localhost" {
		return "docker.io"
	}
	return registry
}

func buildDeployment(
	appName string,
	namespace string,
	svc model.Service,
	servicePorts map[string]int,
	preserveNetworkMembership bool,
	secrets map[string]model.Secret,
	secretVariables map[string]secretVariableNames,
	configs map[string]model.Config,
	configVariables map[string]configVariableNames,
) (deploymentManifest, []helmValueReplacement, []conditionalVolume, []configChecksum, error) {
	ports := svc.Ports
	volumes, volumeMounts, helmValues, conditionalVolumes := buildVolumes(appName, svc, secrets, secretVariables, configs, configVariables)
	resources := &resourceRequirements{
		Limits:   map[string]string{"cpu": resourceValuePlaceholder, "memory": resourceValuePlaceholder},
		Requests: map[string]string{"cpu": resourceValuePlaceholder, "memory": resourceValuePlaceholder},
	}
	securityContext := buildSecurityContext(svc)
	initContainers := buildDependencyInitContainers(svc, servicePorts)

	container := containerSpec{
		Name:            svc.Name,
		Image:           svc.Image,
		ImagePullPolicy: "IfNotPresent",
		Command:         svc.Command,
		Args:            svc.Args,
		Stdin:           svc.Stdin,
		EnvFrom:         buildEnvFrom(svc),
		Ports:           buildContainerPorts(ports),
		VolumeMounts:    volumeMounts,
		LivenessProbe:   buildProbe(svc.Healthcheck),
		Resources:       resources,
		SecurityContext: securityContext,
	}
	if svc.Build != nil {
		container.ImagePullPolicy = "Always"
	}

	podLabels := serviceSelector(svc.Name)
	if preserveNetworkMembership {
		for key, value := range composeNetworkLabels(svc.Networks) {
			podLabels[key] = value
		}
	}
	checksums := buildConfigChecksums(svc, configs)
	podAnnotations := map[string]string{}
	for _, checksum := range checksums {
		podAnnotations[checksum.AnnotationKey] = checksum.Placeholder
	}

	manifest := deploymentManifest{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Metadata: objectMeta{
			Name:      svc.Name,
			Namespace: namespace,
			Labels:    appLabels(appName, svc.Name),
		},
		Spec: deploymentSpec{
			Replicas: 1,
			Selector: labelSelector{MatchLabels: serviceSelector(svc.Name)},
			Template: podTemplateSpec{
				Metadata: objectMeta{Labels: podLabels, Annotations: podAnnotations},
				Spec: podSpec{
					Hostname:       svc.Hostname,
					InitContainers: initContainers,
					Containers:     []containerSpec{container},
					Volumes:        volumes,
				},
			},
		},
	}

	return manifest, helmValues, conditionalVolumes, checksums, nil
}

func buildService(appName string, namespace string, svc model.Service) serviceManifest {
	ports := svc.Ports
	manifest := serviceManifest{
		APIVersion: "v1",
		Kind:       "Service",
		Metadata: objectMeta{
			Name:      svc.Name,
			Namespace: namespace,
			Labels:    appLabels(appName, svc.Name),
		},
		Spec: serviceSpec{
			Selector: serviceSelector(svc.Name),
		},
	}
	if len(ports) == 0 {
		manifest.Spec.ClusterIP = "None"
		return manifest
	}
	manifest.Spec.Ports = buildServicePorts(ports)
	return manifest
}

func buildUDSPackage(app model.App) (udsPackageManifest, error) {
	// --- Allow rules ---
	allow := buildNetworkAllowRules(app)
	for _, rule := range app.Package.AdditionalAllow {
		if item, ok := rule.(map[string]any); ok {
			allow = append(allow, item)
		}
	}

	// --- Expose rules ---
	var expose []any
	if app.Package.NetworkExposeConfigured {
		expose = enrichNetworkExposes(app)
	} else {
		expose = buildAutoExposes(app)
	}

	// --- SSO ---
	sso := buildSSO(app)
	monitor, err := buildMonitor(app)
	if err != nil {
		return udsPackageManifest{}, err
	}

	spec := udsPackageSpec{
		Network: udsNetwork{
			ServiceMesh: udsServiceMesh{Mode: "ambient"},
			Expose:      expose,
			Allow:       allow,
		},
	}
	if len(app.Package.CABundle) > 0 {
		spec.CABundle = app.Package.CABundle
	}
	if len(sso) > 0 {
		spec.SSO = sso
	}
	if len(monitor) > 0 {
		spec.Monitor = monitor
	}

	return udsPackageManifest{
		APIVersion: "uds.dev/v1alpha1",
		Kind:       "Package",
		Metadata: objectMeta{
			Name:        app.Package.Name,
			Namespace:   helmReleaseNamespace,
			Labels:      app.Package.Labels,
			Annotations: app.Package.Annotations,
		},
		Spec: spec,
	}, nil
}

func buildNetworkAllowRules(app model.App) []any {
	if !hasDistinctNetworkMemberships(app.Services) {
		return []any{
			map[string]any{"direction": "Ingress", "remoteGenerated": "IntraNamespace"},
			map[string]any{"direction": "Egress", "remoteGenerated": "IntraNamespace"},
		}
	}

	var allow []any
	for _, network := range sortedServiceNetworks(app.Services) {
		labelKey := composeNetworkLabelPrefix + network
		for _, direction := range []string{"Ingress", "Egress"} {
			allow = append(allow, map[string]any{
				"description":     fmt.Sprintf("compose-%s-%s", network, strings.ToLower(direction)),
				"direction":       direction,
				"selector":        map[string]string{labelKey: "true"},
				"remoteNamespace": helmReleaseNamespace,
				"remoteSelector":  map[string]string{labelKey: "true"},
			})
		}
	}
	return allow
}

func hasDistinctNetworkMemberships(services []model.Service) bool {
	if len(services) < 2 {
		return false
	}
	first := strings.Join(services[0].Networks, ",")
	for _, svc := range services[1:] {
		if strings.Join(svc.Networks, ",") != first {
			return true
		}
	}
	return false
}

func sortedServiceNetworks(services []model.Service) []string {
	seen := map[string]struct{}{}
	for _, svc := range services {
		for _, network := range svc.Networks {
			seen[network] = struct{}{}
		}
	}
	networks := make([]string, 0, len(seen))
	for network := range seen {
		networks = append(networks, network)
	}
	sort.Strings(networks)
	return networks
}

func composeNetworkLabels(networks []string) map[string]string {
	labels := make(map[string]string, len(networks))
	for _, network := range networks {
		labels[composeNetworkLabelPrefix+network] = "true"
	}
	return labels
}

// buildServiceIndex creates a map from service name to Service for O(1) lookup.
func buildServiceIndex(services []model.Service) map[string]model.Service {
	idx := make(map[string]model.Service, len(services))
	for _, svc := range services {
		idx[svc.Name] = svc
	}
	return idx
}

// setDefault sets key in m only if it is not already present.
func setDefault(m map[string]any, key string, value any) {
	if _, exists := m[key]; !exists {
		m[key] = value
	}
}

// buildAutoExposes generates tenant-gateway expose entries for services with published ports.
func buildAutoExposes(app model.App) []any {
	var expose []any
	for _, svc := range app.Services {
		port, ok := primaryPublishedPort(svc.Ports)
		if !ok {
			continue
		}
		svcSelector := map[string]string{"app.kubernetes.io/name": svc.Name}
		host := svc.Name
		if len(expose) == 0 {
			host = helmReleaseNamespace
		}
		expose = append(expose, map[string]any{
			"service":   svc.Name,
			"host":      host,
			"gateway":   "tenant",
			"port":      port.Number,
			"selector":  svcSelector,
			"podLabels": svcSelector,
		})
	}
	return expose
}

// enrichNetworkExposes fills in missing fields on user-provided x-uds.spec.network.expose entries.
func enrichNetworkExposes(app model.App) []any {
	serviceByName := buildServiceIndex(app.Services)
	enriched := make([]any, 0, len(app.Package.NetworkExpose))

	for exposeIndex, raw := range app.Package.NetworkExpose {
		item, ok := raw.(map[string]any)
		if !ok {
			enriched = append(enriched, raw)
			continue
		}

		serviceName, _ := item["service"].(string)
		setDefault(item, "gateway", "tenant")
		defaultHost := serviceName
		if exposeIndex == 0 {
			defaultHost = helmReleaseNamespace
		}
		setDefault(item, "host", defaultHost)

		if svc, found := serviceByName[serviceName]; found {
			svcSelector := map[string]string{"app.kubernetes.io/name": svc.Name}
			setDefault(item, "selector", svcSelector)
			setDefault(item, "podLabels", svcSelector)
			if _, exists := item["port"]; !exists {
				if port, ok := primaryGatewayPort(svc.Ports, false); ok {
					item["port"] = port.Number
				} else if port, err := svc.PrimaryPort(); err == nil {
					item["port"] = port.Number
				}
			}
		}

		enriched = append(enriched, item)
	}
	return enriched
}

// buildSSO generates, disables, or enriches SSO configuration.
func buildSSO(app model.App) []any {
	primaryExposure := inference.PrimaryExposure(app)
	host := primaryExposure.ResolveHost(helmReleaseNamespace)
	service := primaryExposure.Service
	if app.Package.SSOConfigured {
		if len(app.Package.SSO) == 0 {
			return nil
		}
		return enrichSSOEntries(app, host, service)
	}
	return buildInferredSSO(app, host, service)
}

func buildMonitor(app model.App) ([]any, error) {
	if !app.Package.MonitorConfigured {
		return buildInferredMonitors(app), nil
	}
	if len(app.Package.Monitor) == 0 {
		return nil, nil
	}

	serviceByName := buildServiceIndex(app.Services)
	enriched := make([]any, 0, len(app.Package.Monitor))

	for _, raw := range app.Package.Monitor {
		item, ok := raw.(map[string]any)
		if !ok {
			enriched = append(enriched, raw)
			continue
		}

		entry := cloneAnyMap(item)
		serviceRaw, hasService := entry["service"]
		serviceName := strings.TrimSpace(rawString(serviceRaw))
		if !hasService {
			enriched = append(enriched, entry)
			continue
		}
		if serviceName == "" {
			return nil, fmt.Errorf("x-uds.spec.monitor service must be a non-empty string")
		}

		svc, found := serviceByName[serviceName]
		if !found {
			return nil, fmt.Errorf("x-uds.spec.monitor service %q does not match a compose service", serviceName)
		}

		delete(entry, "service")
		selector := serviceSelector(svc.Name)
		setDefault(entry, "selector", selector)
		setDefault(entry, "podSelector", selector)
		setDefault(entry, "kind", "ServiceMonitor")
		setDefault(entry, "path", "/metrics")
		if err := enrichMonitorPortFields(entry, svc); err != nil {
			return nil, fmt.Errorf("x-uds.spec.monitor service %q: %w", serviceName, err)
		}
		defaultMonitorAuthorization(entry)

		enriched = append(enriched, entry)
	}

	return enriched, nil
}

func buildInferredMonitors(app model.App) []any {
	var monitors []any
	for _, svc := range app.Services {
		metricsPorts := inference.MetricsPorts(svc)
		for _, port := range metricsPorts {
			selector := serviceSelector(svc.Name)
			monitors = append(monitors, map[string]any{
				"description": fmt.Sprintf("Metrics for Compose service %s on port %d", svc.Name, port.Number),
				"selector":    selector,
				"podSelector": selector,
				"portName":    buildPortName(port),
				"targetPort":  port.Number,
				"path":        "/metrics",
				"kind":        "ServiceMonitor",
			})
		}
	}
	return monitors
}

func enrichMonitorPortFields(entry map[string]any, svc model.Service) error {
	ports := monitorPortsForService(svc)
	if len(ports) == 0 {
		return fmt.Errorf("service has no declared TCP ports for monitor inference")
	}

	portName, hasPortName := lookupRawString(entry, "portName")
	targetPort, hasTargetPort := lookupRawInt(entry, "targetPort")

	switch {
	case hasPortName && hasTargetPort:
		matchedByName, ok := findMonitorPortByName(ports, portName)
		if !ok {
			return fmt.Errorf("portName %q does not match any declared service port (%s)", portName, formatMonitorPorts(ports))
		}
		if matchedByName.Number != targetPort {
			return fmt.Errorf("portName %q resolves to %d, but targetPort is %d", portName, matchedByName.Number, targetPort)
		}
		entry["portName"] = matchedByName.Name
		entry["targetPort"] = matchedByName.Number
	case hasPortName:
		matchedByName, ok := findMonitorPortByName(ports, portName)
		if !ok {
			return fmt.Errorf("portName %q does not match any declared service port (%s)", portName, formatMonitorPorts(ports))
		}
		entry["portName"] = matchedByName.Name
		entry["targetPort"] = matchedByName.Number
	case hasTargetPort:
		matchedByNumber, ok := findMonitorPortByNumber(ports, targetPort)
		if !ok {
			return fmt.Errorf("targetPort %d does not match any declared service port (%s)", targetPort, formatMonitorPorts(ports))
		}
		entry["portName"] = matchedByNumber.Name
		entry["targetPort"] = matchedByNumber.Number
	default:
		if len(ports) != 1 {
			return fmt.Errorf("service has multiple declared TCP ports; set portName or targetPort (%s)", formatMonitorPorts(ports))
		}
		entry["portName"] = ports[0].Name
		entry["targetPort"] = ports[0].Number
	}

	return nil
}

func defaultMonitorAuthorization(entry map[string]any) {
	authorization, ok := entry["authorization"].(map[string]any)
	if !ok {
		return
	}
	authorizationCopy := cloneAnyMap(authorization)
	setDefault(authorizationCopy, "type", "Bearer")
	entry["authorization"] = authorizationCopy
}

func cloneAnyMap(values map[string]any) map[string]any {
	if len(values) == 0 {
		return map[string]any{}
	}
	clone := make(map[string]any, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

type monitorPort struct {
	Name   string
	Number int
}

func monitorPortsForService(svc model.Service) []monitorPort {
	ports := make([]monitorPort, 0, len(svc.Ports))
	for _, port := range svc.Ports {
		if !strings.EqualFold(port.Protocol, "TCP") {
			continue
		}
		ports = append(ports, monitorPort{
			Name:   buildPortName(port),
			Number: port.Number,
		})
	}
	return ports
}

func findMonitorPortByName(ports []monitorPort, name string) (monitorPort, bool) {
	for _, port := range ports {
		if port.Name == name {
			return port, true
		}
	}
	return monitorPort{}, false
}

func findMonitorPortByNumber(ports []monitorPort, number int) (monitorPort, bool) {
	for _, port := range ports {
		if port.Number == number {
			return port, true
		}
	}
	return monitorPort{}, false
}

func formatMonitorPorts(ports []monitorPort) string {
	values := make([]string, 0, len(ports))
	for _, port := range ports {
		values = append(values, fmt.Sprintf("%s=%d", port.Name, port.Number))
	}
	return strings.Join(values, ", ")
}

func lookupRawString(values map[string]any, key string) (string, bool) {
	raw, exists := values[key]
	if !exists {
		return "", false
	}
	text := strings.TrimSpace(rawString(raw))
	if text == "" {
		return "", false
	}
	return text, true
}

func rawString(value any) string {
	text, _ := value.(string)
	return text
}

func lookupRawInt(values map[string]any, key string) (int, bool) {
	raw, exists := values[key]
	if !exists {
		return 0, false
	}
	switch typed := raw.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case uint64:
		return int(typed), true
	case float64:
		return int(typed), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// buildInferredSSO generates a default SSO client from the app's expose rules.
func buildInferredSSO(app model.App, host, service string) []any {
	if host == "" {
		return nil
	}
	return []any{
		map[string]any{
			"clientId": inferredSSOClientID(app.Package),
			"name":     inferredSSOName(app.Package),
			"redirectUris": []any{
				inferredRedirectURI(host),
			},
			"enableAuthserviceSelector": map[string]string{
				"app.kubernetes.io/name": service,
			},
		},
	}
}

// enrichSSOEntries fills in missing fields on user-provided x-uds.spec.sso entries.
func enrichSSOEntries(app model.App, host, service string) []any {
	enriched := make([]any, 0, len(app.Package.SSO))

	for _, raw := range app.Package.SSO {
		item, ok := raw.(map[string]any)
		if !ok {
			enriched = append(enriched, raw)
			continue
		}
		setDefault(item, "clientId", inferredSSOClientID(app.Package))
		setDefault(item, "name", inferredSSOName(app.Package))
		if host != "" {
			setDefault(item, "redirectUris", []any{
				inferredRedirectURI(host),
			})
		}
		if service != "" {
			setDefault(item, "enableAuthserviceSelector", map[string]string{
				"app.kubernetes.io/name": service,
			})
		}
		enriched = append(enriched, item)
	}
	return enriched
}

func inferredSSOClientID(pkg model.Package) string {
	group := pkg.Group
	if group == "" {
		group = "compose"
	}
	return fmt.Sprintf("uds-%s-%s", group, helmReleaseNamespace)
}

func inferredSSOName(pkg model.Package) string {
	return titleCase(pkg.Name) + " Login"
}

func inferredRedirectURI(host string) string {
	return fmt.Sprintf("https://%s.%s/*", host, helmDomainValue)
}

// titleCase converts a hyphenated name to Title Case (e.g. "hello-world" → "Hello World").
func titleCase(name string) string {
	words := strings.Split(name, "-")
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

func primaryPublishedPort(ports []model.Port) (model.Port, bool) {
	return primaryGatewayPort(ports, true)
}

func primaryDependencyWaitPort(ports []model.Port) (int, bool) {
	for _, port := range ports {
		if port.Number <= 0 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(port.Protocol), "TCP") {
			return port.Number, true
		}
	}
	return 0, false
}

func primaryGatewayPort(ports []model.Port, requirePublished bool) (model.Port, bool) {
	for _, port := range ports {
		if gatewayPortCandidate(port, requirePublished) && port.HasWebHint() {
			return port, true
		}
	}
	for _, port := range ports {
		if gatewayPortCandidate(port, requirePublished) {
			return port, true
		}
	}
	return model.Port{}, false
}

func gatewayPortCandidate(port model.Port, requirePublished bool) bool {
	return !requirePublished || port.Published
}

func buildVolumes(
	appName string,
	svc model.Service,
	secrets map[string]model.Secret,
	secretVariables map[string]secretVariableNames,
	configs map[string]model.Config,
	configVariables map[string]configVariableNames,
) ([]volumeSpec, []volumeMountSpec, []helmValueReplacement, []conditionalVolume) {
	volumes := make([]volumeSpec, 0, len(svc.Volumes)+len(svc.Secrets)+len(svc.Configs))
	mounts := make([]volumeMountSpec, 0, len(svc.Volumes)+len(svc.Secrets)+len(svc.Configs))
	helmValues := []helmValueReplacement{}
	conditionalVolumes := []conditionalVolume{}
	volumeNames := map[string]string{}
	configVolumeIndexes := map[string]int{}

	for _, mount := range svc.Volumes {
		volumeKey := "volume:" + mount.Name
		volumeName, exists := volumeNames[volumeKey]
		if !exists {
			volumeName = sanitizeDNSLabelName("volume-" + mount.Name)
			volumeNames[volumeKey] = volumeName
			volumes = append(volumes, volumeSpec{
				Name: volumeName,
				PersistentVolumeClaim: &persistentVolumeClaimVolumeSource{
					ClaimName: mount.Name,
				},
			})
		}
		mounts = append(mounts, volumeMountSpec{
			Name:      volumeName,
			MountPath: mount.Target,
			ReadOnly:  mount.ReadOnly,
		})
	}

	for _, ref := range svc.Secrets {
		volumeKey := "secret:" + ref.Source
		volumeName, exists := volumeNames[volumeKey]
		if !exists {
			volumeName = sanitizeDNSLabelName("secret-" + ref.Source)
			volumeNames[volumeKey] = volumeName
			secretName := ref.Source
			secretKey := ref.Source
			if secret, external := secrets[ref.Source]; external && secret.External {
				variable := secretVariables[ref.Source]
				secretName = addExternalResourceHelmValue(
					&helmValues,
					"composeBridge.externalResourceName",
					fmt.Sprintf("external compose secret %s requires a valid Kubernetes Secret name", secret.Name),
					"externalSecrets",
					variable.ValuesKey,
					"name",
				)
				secretKey = addExternalResourceHelmValue(
					&helmValues,
					"composeBridge.externalResourceKey",
					fmt.Sprintf("external compose secret %s requires a valid Kubernetes Secret key", secret.Name),
					"externalSecrets",
					variable.ValuesKey,
					"key",
				)
			}
			volumes = append(volumes, volumeSpec{
				Name: volumeName,
				Secret: &secretVolumeSource{
					SecretName: secretName,
					Items:      []keyToPath{{Key: secretKey, Path: ref.Source}},
				},
			})
		}
		mounts = append(mounts, volumeMountSpec{
			Name:      volumeName,
			MountPath: resolveSecretTargetPath(ref.Source, ref.Target),
			SubPath:   ref.Source,
			ReadOnly:  true,
		})
	}

	for _, ref := range svc.Configs {
		volumeKey := "config:" + ref.Source
		if ref.Mode != nil {
			volumeKey += fmt.Sprintf(":mode:%o", *ref.Mode)
		}
		config := configs[ref.Source]
		itemPath := composeConfigDataKey(ref, config)
		volumeName, exists := volumeNames[volumeKey]
		if !exists {
			volumeName = sanitizeDNSLabelName("config-" + ref.Source)
			if ref.Mode != nil {
				volumeName = sanitizeDNSLabelName(fmt.Sprintf("config-%s-mode-%o", ref.Source, *ref.Mode))
			}
			volumeNames[volumeKey] = volumeName
			configMapName := composeConfigMapName(appName, config)
			configMapKey := itemPath
			if config, external := configs[ref.Source]; external && config.External {
				variable := configVariables[ref.Source]
				configMapName = addExternalResourceHelmValue(
					&helmValues,
					"composeBridge.externalResourceName",
					fmt.Sprintf("external compose config %s requires a valid Kubernetes ConfigMap name", config.Name),
					"externalConfigs",
					variable.ValuesKey,
					"name",
				)
				configMapKey = addExternalResourceHelmValue(
					&helmValues,
					"composeBridge.externalResourceKey",
					fmt.Sprintf("external compose config %s requires a valid Kubernetes ConfigMap key", config.Name),
					"externalConfigs",
					variable.ValuesKey,
					"key",
				)
			}
			volumes = append(volumes, volumeSpec{
				Name: volumeName,
				ConfigMap: &configMapVolumeSource{
					Name:        configMapName,
					Items:       []keyToPath{{Key: configMapKey, Path: itemPath}},
					DefaultMode: ref.Mode,
				},
			})
			configVolumeIndexes[volumeKey] = len(volumes) - 1
			if config.Bridge != nil && config.Bridge.EnabledValue != nil {
				conditionalVolumes = append(conditionalVolumes, conditionalVolume{
					Name:         volumeName,
					EnabledValue: config.Bridge.EnabledValue.Name,
				})
			}
		} else {
			volume := &volumes[configVolumeIndexes[volumeKey]]
			configMapKey := itemPath
			if config.External {
				configMapKey = volume.ConfigMap.Items[0].Key
			}
			item := keyToPath{Key: configMapKey, Path: itemPath}
			if !slices.Contains(volume.ConfigMap.Items, item) {
				volume.ConfigMap.Items = append(volume.ConfigMap.Items, item)
			}
		}
		configMapKey := composeConfigDataKey(ref, config)
		mounts = append(mounts, volumeMountSpec{
			Name:      volumeName,
			MountPath: resolveConfigTargetPath(ref.Source, ref.Target),
			SubPath:   configMapKey,
			ReadOnly:  true,
		})
	}

	return volumes, mounts, helmValues, conditionalVolumes
}

type conditionalVolume struct {
	Name         string
	EnabledValue string
}

type configChecksum struct {
	AnnotationKey string
	Placeholder   string
	EnabledValue  string
	ContentValue  string
	StaticContent string
}

func buildConfigChecksums(svc model.Service, configs map[string]model.Config) []configChecksum {
	checksums := []configChecksum{}
	seen := map[string]struct{}{}
	for _, ref := range svc.Configs {
		config := configs[ref.Source]
		if config.Bridge == nil || !config.Bridge.RolloutOnChange {
			continue
		}
		key := config.Name
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		checksum := configChecksum{
			AnnotationKey: "checksum/" + config.Name,
			Placeholder:   fmt.Sprintf("__HELM_CONFIG_CHECKSUM_%d__", len(checksums)),
			StaticContent: config.Content,
		}
		if config.Bridge.EnabledValue != nil {
			checksum.EnabledValue = config.Bridge.EnabledValue.Name
		}
		if config.Bridge.ContentValue != nil {
			checksum.ContentValue = config.Bridge.ContentValue.Name
		}
		checksums = append(checksums, checksum)
	}
	return checksums
}

type helmValueReplacement struct {
	Placeholder string
	Template    string
}

func addExternalResourceHelmValue(
	values *[]helmValueReplacement,
	helper string,
	description string,
	valuesRoot string,
	valuesKey string,
	field string,
) string {
	placeholder := fmt.Sprintf("__HELM_EXTERNAL_RESOURCE_VALUE_%d__", len(*values))
	templateValue := fmt.Sprintf(
		`{{ include %q (list %q (dig %q %q "" (.Values.%s | default (dict)))) }}`,
		helper,
		description,
		valuesKey,
		field,
		valuesRoot,
	)
	*values = append(*values, helmValueReplacement{Placeholder: placeholder, Template: templateValue})
	return placeholder
}

func buildDependencyInitContainers(svc model.Service, servicePorts map[string]int) []containerSpec {
	if len(svc.DependsOn) == 0 {
		return nil
	}
	containers := []containerSpec{}
	for _, dep := range svc.DependsOn {
		port, ok := servicePorts[dep.Service]
		if !ok || port <= 0 {
			continue
		}
		waitScript := fmt.Sprintf("until nc -z %s %d; do echo waiting for %s:%d; sleep 2; done", dep.Service, port, dep.Service, port)
		containers = append(containers, containerSpec{
			Name:            sanitizeManifestName("wait-" + dep.Service),
			Image:           model.DependencyInitImage,
			ImagePullPolicy: "IfNotPresent",
			Command:         []string{"sh", "-c", waitScript},
		})
	}
	return containers
}

func buildComponentImages(svc model.Service, servicePorts map[string]int) []string {
	images := []string{}
	if svc.Build == nil {
		images = append(images, svc.Image)
	}
	if len(buildDependencyInitContainers(svc, servicePorts)) > 0 {
		images = append(images, model.DependencyInitImage)
	}
	return dedupeStrings(images)
}

func buildEnvFrom(svc model.Service) []envFromSource {
	if len(svc.Env) == 0 {
		return nil
	}
	return []envFromSource{{
		ConfigMapRef: &configMapEnvSource{Name: environmentConfigMapName(svc.Name)},
	}}
}

func buildContainerPorts(ports []model.Port) []containerPort {
	if len(ports) == 0 {
		return nil
	}
	out := make([]containerPort, 0, len(ports))
	for _, port := range ports {
		out = append(out, containerPort{
			Name:          buildPortName(port),
			ContainerPort: port.Number,
			Protocol:      strings.ToUpper(port.Protocol),
		})
	}
	return out
}

func buildServicePorts(ports []model.Port) []servicePort {
	out := make([]servicePort, 0, len(ports))
	for _, port := range ports {
		out = append(out, servicePort{
			Name:        buildPortName(port),
			Port:        port.Number,
			Protocol:    strings.ToUpper(port.Protocol),
			TargetPort:  port.Number,
			AppProtocol: port.AppProtocol,
		})
	}
	return out
}

func buildSecurityContext(svc model.Service) *securityContext {
	ctx := &securityContext{}
	hasAny := false

	user := strings.TrimSpace(svc.User)
	if user != "" {
		base := user
		if strings.Contains(base, ":") {
			base = strings.SplitN(base, ":", 2)[0]
		}
		if !strings.EqualFold(base, "root") {
			if uid, err := strconv.ParseInt(base, 10, 64); err == nil {
				ctx.RunAsUser = &uid
				nonRoot := uid != 0
				ctx.RunAsNonRoot = &nonRoot
			} else {
				nonRoot := true
				ctx.RunAsNonRoot = &nonRoot
			}
			hasAny = true
		}
	}

	if svc.Privileged {
		t := true
		ctx.Privileged = &t
		hasAny = true
	}

	if len(svc.CapAdd) > 0 || len(svc.CapDrop) > 0 {
		ctx.Capabilities = &capabilitiesSpec{
			Add:  svc.CapAdd,
			Drop: svc.CapDrop,
		}
		hasAny = true
	}

	if !hasAny {
		return nil
	}
	return ctx
}

func isRootUser(user string) bool {
	trimmed := strings.TrimSpace(user)
	if trimmed == "" {
		return false
	}
	base := trimmed
	if strings.Contains(base, ":") {
		base = strings.SplitN(base, ":", 2)[0]
	}
	base = strings.TrimSpace(base)
	if strings.EqualFold(base, "root") {
		return true
	}
	uid, err := strconv.ParseInt(base, 10, 64)
	return err == nil && uid == 0
}

func hasSeccompUnconfined(securityOpts []string) bool {
	for _, opt := range securityOpts {
		normalized := strings.ToLower(strings.TrimSpace(opt))
		if normalized == "seccomp:unconfined" || normalized == "seccomp=unconfined" {
			return true
		}
	}
	return false
}

func serviceExemptionType(svc model.Service) string {
	var types []string
	if isRootUser(svc.User) {
		types = append(types, "root user")
	}
	if svc.Privileged {
		types = append(types, "privileged")
	}
	if len(svc.CapAdd) > 0 {
		types = append(types, "Linux capabilities")
	}
	if hasSeccompUnconfined(svc.SecurityOpts) {
		types = append(types, "seccomp")
	}
	return strings.Join(types, " and ")
}

func serviceExemptionPolicies(svc model.Service) []string {
	var policies []string
	if isRootUser(svc.User) {
		policies = append(policies, "RequireNonRootUser")
	}
	if svc.Privileged {
		policies = append(policies, "DisallowPrivileged")
	}
	if len(svc.CapAdd) > 0 {
		policies = append(policies, "DropAllCapabilities", "RestrictCapabilities")
	}
	if hasSeccompUnconfined(svc.SecurityOpts) {
		policies = append(policies, "RestrictSeccomp")
	}
	return policies
}

func buildUDSExemption(app model.App) *udsExemptionManifest {
	var exemptions []udsExemptionEntry
	for _, svc := range app.Services {
		policies := serviceExemptionPolicies(svc)
		if len(policies) == 0 {
			continue
		}
		exemptions = append(exemptions, udsExemptionEntry{
			Title: fmt.Sprintf("%s policy exemption for %s %s", serviceExemptionType(svc), app.Package.Name, svc.Name),
			Matcher: udsExemptionMatcher{
				Kind:      "pod",
				Namespace: helmReleaseNamespace,
				Name:      fmt.Sprintf("^%s-.*", svc.Name),
			},
			Policies: policies,
		})
	}
	if len(exemptions) == 0 {
		return nil
	}
	return &udsExemptionManifest{
		APIVersion: "uds.dev/v1alpha1",
		Kind:       "Exemption",
		Metadata: objectMeta{
			Name:      fmt.Sprintf("compose-%s", helmReleaseNamespace),
			Namespace: "uds-policy-exemptions",
		},
		Spec: udsExemptionSpec{
			Exemptions: exemptions,
		},
	}
}

func buildProbe(healthcheck *model.Healthcheck) *probe {
	if healthcheck == nil || len(healthcheck.Command) == 0 {
		return nil
	}
	return &probe{
		Exec:                &execAction{Command: healthcheck.Command},
		PeriodSeconds:       healthcheck.PeriodSeconds,
		InitialDelaySeconds: healthcheck.InitialDelaySeconds,
		TimeoutSeconds:      healthcheck.TimeoutSeconds,
		FailureThreshold:    healthcheck.FailureThreshold,
	}
}

func buildEnvironmentVariables(
	app model.App,
	secretVariables map[string]secretVariableNames,
	configVariables map[string]configVariableNames,
) (map[string]map[string]string, error) {
	usedVariables := map[string]variableOwner{
		additionalNetworkAllowVariable: {description: fmt.Sprintf("automatic package variable %q", additionalNetworkAllowVariable), path: "package"},
		domainVariable:                 {description: fmt.Sprintf("automatic package variable %q", domainVariable), path: "package"},
	}
	for _, svc := range app.Services {
		resourceVariables := buildResourceVariableNames(svc.Name)
		for _, name := range resourceVariables.all() {
			owner := variableOwner{
				description: fmt.Sprintf("automatic resource variable for service %q", svc.Name),
				path:        "services." + svc.Name,
			}
			if err := registerZarfVariable(usedVariables, name, owner); err != nil {
				return nil, err
			}
		}
	}
	for _, secretName := range sortedSecretNames(app.Secrets) {
		variable := secretVariables[secretName]
		for _, name := range []string{variable.Value, variable.SecretName, variable.SecretKey} {
			if name == "" {
				continue
			}
			if err := registerZarfVariable(
				usedVariables,
				name,
				variableOwner{description: fmt.Sprintf("compose secret %q", secretName), path: "secrets." + secretName},
			); err != nil {
				return nil, err
			}
		}
	}
	for _, configName := range sortedConfigNames(app.Configs) {
		config := app.Configs[configName]
		if !config.External {
			continue
		}
		variable := configVariables[configName]
		for _, name := range []string{variable.ConfigMapName, variable.ConfigMapKey} {
			if err := registerZarfVariable(
				usedVariables,
				name,
				variableOwner{description: fmt.Sprintf("compose config %q", configName), path: "configs." + configName},
			); err != nil {
				return nil, err
			}
		}
	}

	renderedConfigMaps := map[string]string{}
	for _, configName := range sortedConfigNames(app.Configs) {
		config := app.Configs[configName]
		if !config.External {
			renderedConfigMaps[config.Name] = configName
		}
	}

	out := map[string]map[string]string{}
	for _, svc := range app.Services {
		if len(svc.Env) == 0 {
			continue
		}
		configMapName := environmentConfigMapName(svc.Name)
		if composeConfig, exists := renderedConfigMaps[configMapName]; exists {
			message := fmt.Sprintf("environment ConfigMap %q for service %q conflicts with compose config %q", configMapName, svc.Name, composeConfig)
			return nil, &settingError{
				path:        "services." + svc.Name + ".environment",
				code:        "environment-configmap-conflict",
				message:     message,
				remediation: "rename the Compose config or service so their generated ConfigMap names are distinct",
			}
		}

		serviceVariables := map[string]string{}
		for _, item := range svc.Env {
			if !kubernetesEnvironmentName.MatchString(item.Name) {
				message := fmt.Sprintf("invalid environment variable %q on service %q: names used with generated envFrom ConfigMaps must match %s",
					item.Name,
					svc.Name,
					kubernetesEnvironmentName.String(),
				)
				return nil, &settingError{
					path:        "services." + svc.Name + ".environment",
					code:        "environment-name",
					message:     message,
					remediation: "rename the environment variable to a valid Kubernetes environment name",
				}
			}
			variableName := normalizeZarfVariableName(svc.Name + "_" + item.Name)
			owner := variableOwner{
				description: fmt.Sprintf("environment variable %q on service %q", item.Name, svc.Name),
				path:        "services." + svc.Name + ".environment",
			}
			if err := registerZarfVariable(usedVariables, variableName, owner); err != nil {
				return nil, err
			}
			serviceVariables[item.Name] = variableName
		}
		out[svc.Name] = serviceVariables
	}
	return out, nil
}

type variableOwner struct {
	description string
	path        string
}

func registerZarfVariable(used map[string]variableOwner, name string, owner variableOwner) error {
	if !validZarfVariableName.MatchString(name) {
		message := fmt.Sprintf(
			"%s generates invalid Zarf variable %q: names must match %s",
			owner.description,
			name,
			validZarfVariableName.String(),
		)
		return &settingError{
			path:        owner.path,
			code:        "zarf-variable-name",
			message:     message,
			remediation: "rename the source setting so it generates a valid and unique Zarf variable name",
		}
	}
	if existing, exists := used[name]; exists {
		message := fmt.Sprintf(
			"%s generates Zarf variable %q, which conflicts with %s",
			owner.description,
			name,
			existing.description,
		)
		return &settingError{
			path:        owner.path,
			code:        "zarf-variable-conflict",
			message:     message,
			remediation: "rename the source setting so generated Zarf variable names are unique",
		}
	}
	used[name] = owner
	return nil
}

type resourceVariableNames struct {
	CPURequest    string
	MemoryRequest string
	CPULimit      string
	MemoryLimit   string
}

func buildResourceVariableNames(serviceName string) resourceVariableNames {
	prefix := normalizeZarfVariableName(serviceName)
	return resourceVariableNames{
		CPURequest:    prefix + "_CPU_REQUEST",
		MemoryRequest: prefix + "_MEMORY_REQUEST",
		CPULimit:      prefix + "_CPU_LIMIT",
		MemoryLimit:   prefix + "_MEMORY_LIMIT",
	}
}

func (variables resourceVariableNames) all() []string {
	return []string{variables.CPURequest, variables.MemoryRequest, variables.CPULimit, variables.MemoryLimit}
}

type secretVariableNames struct {
	ValuesKey  string
	Value      string
	SecretName string
	SecretKey  string
}

func buildSecretVariables(secrets map[string]model.Secret) map[string]secretVariableNames {
	usedValuesKeys := map[string]struct{}{}
	usedVariables := map[string]struct{}{}
	out := map[string]secretVariableNames{}
	for _, name := range sortedSecretNames(secrets) {
		valuesKey := buildUniqueVariableName(name, usedValuesKeys)
		if secrets[name].External {
			out[name] = secretVariableNames{
				ValuesKey:  valuesKey,
				SecretName: buildUniqueVariableName(valuesKey+"_SECRET_NAME", usedVariables),
				SecretKey:  buildUniqueVariableName(valuesKey+"_SECRET_KEY", usedVariables),
			}
			continue
		}
		out[name] = secretVariableNames{
			ValuesKey: valuesKey,
			Value:     buildUniqueVariableName(valuesKey, usedVariables),
		}
	}
	return out
}

type configVariableNames struct {
	ValuesKey     string
	ConfigMapName string
	ConfigMapKey  string
}

func buildConfigVariables(configs map[string]model.Config) map[string]configVariableNames {
	usedValuesKeys := map[string]struct{}{}
	usedVariables := map[string]struct{}{}
	out := map[string]configVariableNames{}
	for _, name := range sortedConfigNames(configs) {
		if !configs[name].External {
			continue
		}
		valuesKey := buildUniqueVariableName(name, usedValuesKeys)
		out[name] = configVariableNames{
			ValuesKey:     valuesKey,
			ConfigMapName: buildUniqueVariableName(valuesKey+"_CONFIGMAP_NAME", usedVariables),
			ConfigMapKey:  buildUniqueVariableName(valuesKey+"_CONFIGMAP_KEY", usedVariables),
		}
	}
	return out
}

func buildUniqueVariableName(resourceName string, used map[string]struct{}) string {
	base := normalizeZarfVariableName(resourceName)
	candidate := base
	for i := 2; ; i++ {
		if _, exists := used[candidate]; !exists {
			used[candidate] = struct{}{}
			return candidate
		}
		candidate = fmt.Sprintf("%s_%d", base, i)
	}
}

func normalizeZarfVariableName(raw string) string {
	name := strings.ToUpper(strings.TrimSpace(raw))
	name = strings.ReplaceAll(name, "-", "_")
	name = strings.ReplaceAll(name, ".", "_")
	name = invalidZarfVariableRunes.ReplaceAllString(name, "_")
	name = strings.Trim(name, "_")
	if name == "" {
		return "VALUE"
	}
	return name
}

func stringPointer(value string) *string {
	return &value
}

func optionalStringPointer(value string) *string {
	if value == "" {
		return nil
	}
	return stringPointer(value)
}

func buildPortName(port model.Port) string {
	if name := sanitizePortName(port.Name); name != "" {
		return name
	}
	return fmt.Sprintf("port-%d-%s", port.Number, strings.ToLower(port.Protocol))
}

func validatePortNames(services []model.Service) error {
	for _, svc := range services {
		seen := make(map[string]model.Port, len(svc.Ports))
		for _, port := range svc.Ports {
			name := buildPortName(port)
			if previous, exists := seen[name]; exists {
				message := fmt.Sprintf(
					"service %q ports %s and %s resolve to duplicate Kubernetes port name %q",
					svc.Name,
					formatPortNameSource(previous),
					formatPortNameSource(port),
					name,
				)
				return &settingError{
					path:        "services." + svc.Name + ".ports",
					code:        "duplicate-port-name",
					message:     message,
					remediation: "set distinct Compose port names that remain unique after Kubernetes normalization",
				}
			}
			seen[name] = port
		}
	}
	return nil
}

func formatPortNameSource(port model.Port) string {
	source := "unnamed"
	if name := strings.TrimSpace(port.Name); name != "" {
		source = fmt.Sprintf("name %q", name)
	}
	return fmt.Sprintf("%d/%s (%s)", port.Number, strings.ToLower(port.Protocol), source)
}

func sanitizePortName(raw string) string {
	name := strings.ToLower(strings.TrimSpace(raw))
	name = strings.ReplaceAll(name, "_", "-")
	name = invalidPortNameRunes.ReplaceAllString(name, "-")
	name = repeatedPortNameHyphens.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name) > 15 {
		name = strings.Trim(name[:15], "-")
	}
	if !portNameLetter.MatchString(name) {
		return ""
	}
	return name
}

func resolveSecretTargetPath(source string, target string) string {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return filepath.ToSlash(filepath.Join("/run/secrets", source))
	}
	if strings.HasPrefix(trimmed, "/") {
		return trimmed
	}
	return filepath.ToSlash(filepath.Join("/run/secrets", trimmed))
}

func resolveConfigTargetPath(source string, target string) string {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return filepath.ToSlash(filepath.Join("/", source))
	}
	if strings.HasPrefix(trimmed, "/") {
		return trimmed
	}
	return filepath.ToSlash(filepath.Join("/", trimmed))
}

func appLabels(appName string, name string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":    name,
		"app.kubernetes.io/part-of": appName,
	}
}

func reloadableAppLabels(appName string, name string) map[string]string {
	labels := appLabels(appName, name)
	labels[podReloadLabel] = "true"
	return labels
}

func serviceSelector(name string) map[string]string {
	return map[string]string{"app.kubernetes.io/name": name}
}

func writeYAMLFile(path string, value any) error {
	data, err := yamlv3.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal yaml for %s: %w", path, err)
	}
	return writeRenderedFile(path, string(data))
}

func writeRenderedFile(path, rendered string) error {
	if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
		return fmt.Errorf("write file %s: %w", path, err)
	}
	return nil
}

func sortedVolumeNames(volumes map[string]model.Volume) []string {
	keys := make([]string, 0, len(volumes))
	for key := range volumes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedSecretNames(secrets map[string]model.Secret) []string {
	keys := make([]string, 0, len(secrets))
	for key := range secrets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedConfigNames(configs map[string]model.Config) []string {
	keys := make([]string, 0, len(configs))
	for key := range configs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func dedupeStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

var invalidManifestNameRunes = regexp.MustCompile(`[^a-z0-9.-]+`)
var invalidDNSLabelRunes = regexp.MustCompile(`[^a-z0-9-]+`)
var repeatedDNSLabelHyphens = regexp.MustCompile(`-+`)
var invalidPortNameRunes = regexp.MustCompile(`[^a-z0-9-]+`)
var repeatedPortNameHyphens = regexp.MustCompile(`-+`)
var portNameLetter = regexp.MustCompile(`[a-z]`)
var invalidZarfVariableRunes = regexp.MustCompile(`[^A-Z0-9_]+`)
var validZarfVariableName = regexp.MustCompile(`^[A-Z0-9_]+$`)
var kubernetesEnvironmentName = regexp.MustCompile(`^[-._a-zA-Z][-._a-zA-Z0-9]*$`)
var validConfigMapKey = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

func sanitizeManifestName(raw string) string {
	name := strings.ToLower(strings.TrimSpace(raw))
	name = strings.ReplaceAll(name, "_", "-")
	name = invalidManifestNameRunes.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-.")
	if name == "" {
		return "resource"
	}
	return name
}

func sanitizeDNSLabelName(raw string) string {
	normalizedInput := strings.ToLower(strings.TrimSpace(raw))
	name := invalidDNSLabelRunes.ReplaceAllString(normalizedInput, "-")
	name = repeatedDNSLabelHyphens.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "resource"
	}
	if name == normalizedInput && len(name) <= 63 {
		return name
	}

	digest := sha256.Sum256([]byte(normalizedInput))
	suffix := fmt.Sprintf("%x", digest[:6])
	maxPrefixLength := 63 - len(suffix) - 1
	if len(name) > maxPrefixLength {
		name = name[:maxPrefixLength]
	}
	name = strings.Trim(name, "-")
	if name == "" {
		name = "resource"
	}
	return name + "-" + suffix
}

func environmentConfigMapName(serviceName string) string {
	return sanitizeManifestName(serviceName + "-environment")
}

type objectMeta struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

type persistentVolumeClaimManifest struct {
	APIVersion string                    `yaml:"apiVersion"`
	Kind       string                    `yaml:"kind"`
	Metadata   objectMeta                `yaml:"metadata"`
	Spec       persistentVolumeClaimSpec `yaml:"spec"`
}

type persistentVolumeClaimSpec struct {
	AccessModes []string     `yaml:"accessModes"`
	Resources   pvcResources `yaml:"resources"`
}

type pvcResources struct {
	Requests pvcRequests `yaml:"requests"`
}

type pvcRequests struct {
	Storage string `yaml:"storage"`
}

type configMapManifest struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   objectMeta        `yaml:"metadata"`
	Data       map[string]string `yaml:"data"`
}

type secretManifest struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   objectMeta        `yaml:"metadata"`
	Type       string            `yaml:"type"`
	StringData map[string]string `yaml:"stringData"`
}

type deploymentManifest struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   objectMeta     `yaml:"metadata"`
	Spec       deploymentSpec `yaml:"spec"`
}

type deploymentSpec struct {
	Replicas int             `yaml:"replicas"`
	Selector labelSelector   `yaml:"selector"`
	Template podTemplateSpec `yaml:"template"`
}

type labelSelector struct {
	MatchLabels map[string]string `yaml:"matchLabels"`
}

type podTemplateSpec struct {
	Metadata objectMeta `yaml:"metadata"`
	Spec     podSpec    `yaml:"spec"`
}

type podSpec struct {
	Hostname       string          `yaml:"hostname,omitempty"`
	InitContainers []containerSpec `yaml:"initContainers,omitempty"`
	Containers     []containerSpec `yaml:"containers"`
	Volumes        []volumeSpec    `yaml:"volumes,omitempty"`
}

type containerSpec struct {
	Name            string                `yaml:"name"`
	Image           string                `yaml:"image"`
	ImagePullPolicy string                `yaml:"imagePullPolicy,omitempty"`
	Command         []string              `yaml:"command,omitempty"`
	Args            []string              `yaml:"args,omitempty"`
	Stdin           bool                  `yaml:"stdin,omitempty"`
	EnvFrom         []envFromSource       `yaml:"envFrom,omitempty"`
	Ports           []containerPort       `yaml:"ports,omitempty"`
	VolumeMounts    []volumeMountSpec     `yaml:"volumeMounts,omitempty"`
	LivenessProbe   *probe                `yaml:"livenessProbe,omitempty"`
	Resources       *resourceRequirements `yaml:"resources,omitempty"`
	SecurityContext *securityContext      `yaml:"securityContext,omitempty"`
}

type envFromSource struct {
	ConfigMapRef *configMapEnvSource `yaml:"configMapRef,omitempty"`
}

type configMapEnvSource struct {
	Name string `yaml:"name"`
}

type containerPort struct {
	Name          string `yaml:"name,omitempty"`
	ContainerPort int    `yaml:"containerPort"`
	Protocol      string `yaml:"protocol,omitempty"`
}

type volumeMountSpec struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	SubPath   string `yaml:"subPath,omitempty"`
	ReadOnly  bool   `yaml:"readOnly,omitempty"`
}

type probe struct {
	Exec                *execAction `yaml:"exec,omitempty"`
	PeriodSeconds       int         `yaml:"periodSeconds,omitempty"`
	InitialDelaySeconds int         `yaml:"initialDelaySeconds,omitempty"`
	TimeoutSeconds      int         `yaml:"timeoutSeconds,omitempty"`
	FailureThreshold    int         `yaml:"failureThreshold,omitempty"`
}

type execAction struct {
	Command []string `yaml:"command"`
}

type resourceRequirements struct {
	Limits   map[string]string `yaml:"limits,omitempty"`
	Requests map[string]string `yaml:"requests,omitempty"`
}

type securityContext struct {
	RunAsNonRoot *bool             `yaml:"runAsNonRoot,omitempty"`
	RunAsUser    *int64            `yaml:"runAsUser,omitempty"`
	Privileged   *bool             `yaml:"privileged,omitempty"`
	Capabilities *capabilitiesSpec `yaml:"capabilities,omitempty"`
}

type capabilitiesSpec struct {
	Add  []string `yaml:"add,omitempty"`
	Drop []string `yaml:"drop,omitempty"`
}

type udsExemptionManifest struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   objectMeta       `yaml:"metadata"`
	Spec       udsExemptionSpec `yaml:"spec"`
}

type udsExemptionSpec struct {
	Exemptions []udsExemptionEntry `yaml:"exemptions"`
}

type udsExemptionEntry struct {
	Title    string              `yaml:"title"`
	Matcher  udsExemptionMatcher `yaml:"matcher"`
	Policies []string            `yaml:"policies"`
}

type udsExemptionMatcher struct {
	Kind      string `yaml:"kind"`
	Namespace string `yaml:"namespace"`
	Name      string `yaml:"name"`
}

type volumeSpec struct {
	Name                  string                             `yaml:"name"`
	PersistentVolumeClaim *persistentVolumeClaimVolumeSource `yaml:"persistentVolumeClaim,omitempty"`
	Secret                *secretVolumeSource                `yaml:"secret,omitempty"`
	ConfigMap             *configMapVolumeSource             `yaml:"configMap,omitempty"`
}

type persistentVolumeClaimVolumeSource struct {
	ClaimName string `yaml:"claimName"`
}

type secretVolumeSource struct {
	SecretName string      `yaml:"secretName"`
	Items      []keyToPath `yaml:"items,omitempty"`
}

type configMapVolumeSource struct {
	Name        string      `yaml:"name"`
	Items       []keyToPath `yaml:"items,omitempty"`
	DefaultMode *int32      `yaml:"defaultMode,omitempty"`
}

type keyToPath struct {
	Key  string `yaml:"key"`
	Path string `yaml:"path"`
}

type serviceManifest struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   objectMeta  `yaml:"metadata"`
	Spec       serviceSpec `yaml:"spec"`
}

type serviceSpec struct {
	ClusterIP string            `yaml:"clusterIP,omitempty"`
	Selector  map[string]string `yaml:"selector"`
	Ports     []servicePort     `yaml:"ports,omitempty"`
}

type servicePort struct {
	Name        string `yaml:"name,omitempty"`
	Port        int    `yaml:"port"`
	TargetPort  int    `yaml:"targetPort,omitempty"`
	Protocol    string `yaml:"protocol,omitempty"`
	AppProtocol string `yaml:"appProtocol,omitempty"`
}

type udsPackageManifest struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   objectMeta     `yaml:"metadata"`
	Spec       udsPackageSpec `yaml:"spec"`
}

type udsPackageSpec struct {
	Network  udsNetwork     `yaml:"network"`
	Monitor  []any          `yaml:"monitor,omitempty"`
	SSO      []any          `yaml:"sso,omitempty"`
	CABundle map[string]any `yaml:"caBundle,omitempty"`
}

type udsNetwork struct {
	ServiceMesh udsServiceMesh `yaml:"serviceMesh"`
	Expose      []any          `yaml:"expose,omitempty"`
	Allow       []any          `yaml:"allow"`
}

type udsServiceMesh struct {
	Mode string `yaml:"mode"`
}

type zarfPackageConfig struct {
	APIVersion    string            `yaml:"apiVersion,omitempty"`
	Kind          string            `yaml:"kind"`
	Metadata      zarfMetadata      `yaml:"metadata"`
	Variables     []zarfVariable    `yaml:"variables,omitempty"`
	Components    []zarfComponent   `yaml:"components"`
	Documentation map[string]string `yaml:"documentation,omitempty"`
}

type zarfMetadata struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description,omitempty"`
	Version     string            `yaml:"version"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

type zarfVariable struct {
	Name        string  `yaml:"name"`
	Description string  `yaml:"description,omitempty"`
	Default     *string `yaml:"default,omitempty"`
	Prompt      bool    `yaml:"prompt,omitempty"`
	Sensitive   bool    `yaml:"sensitive,omitempty"`
	AutoIndent  bool    `yaml:"autoIndent,omitempty"`
}

type zarfComponent struct {
	Name          string                `yaml:"name"`
	Required      bool                  `yaml:"required"`
	Description   string                `yaml:"description,omitempty"`
	Only          *zarfComponentOnly    `yaml:"only,omitempty"`
	Charts        []zarfChart           `yaml:"charts,omitempty"`
	Images        []string              `yaml:"images,omitempty"`
	ImageArchives []zarfImageArchive    `yaml:"imageArchives,omitempty"`
	Actions       *zarfComponentActions `yaml:"actions,omitempty"`
}

type zarfComponentOnly struct {
	Flavor string `yaml:"flavor"`
}

type zarfImageArchive struct {
	Path   string   `yaml:"path"`
	Images []string `yaml:"images"`
}

type zarfComponentActions struct {
	OnCreate zarfComponentActionSet `yaml:"onCreate"`
}

type zarfComponentActionSet struct {
	Before []zarfComponentAction `yaml:"before"`
}

type zarfComponentAction struct {
	Description string `yaml:"description,omitempty"`
	Cmd         string `yaml:"cmd"`
}

type zarfChart struct {
	Name        string   `yaml:"name"`
	Namespace   string   `yaml:"namespace"`
	LocalPath   string   `yaml:"localPath"`
	Version     string   `yaml:"version"`
	ValuesFiles []string `yaml:"valuesFiles,omitempty"`
}

type buildComposeFile struct {
	Name     string                         `yaml:"name"`
	Services map[string]buildComposeService `yaml:"services"`
	Secrets  map[string]any                 `yaml:"secrets,omitempty"`
}

type buildComposeService struct {
	Image string         `yaml:"image"`
	Build map[string]any `yaml:"build"`
}

// chartMetadata is the Chart.yaml written for the generated Helm chart.
type chartMetadata struct {
	APIVersion  string `yaml:"apiVersion"`
	Name        string `yaml:"name"`
	Description string `yaml:"description,omitempty"`
	Type        string `yaml:"type,omitempty"`
	Version     string `yaml:"version"`
	AppVersion  string `yaml:"appVersion,omitempty"`
}

// chartValues is the values document written for both the chart defaults
// (chart/values.yaml) and the Zarf-templated overrides (values/values.yaml).
type chartValues struct {
	AdditionalNetworkAllow any                               `yaml:"additionalNetworkAllow"`
	Environment            map[string]map[string]string      `yaml:"environment,omitempty"`
	Resources              map[string]resourceValues         `yaml:"resources"`
	Secrets                map[string]string                 `yaml:"secrets"`
	ExternalSecrets        map[string]externalResourceValues `yaml:"externalSecrets,omitempty"`
	ExternalConfigs        map[string]externalResourceValues `yaml:"externalConfigs,omitempty"`
	UDS                    udsValues                         `yaml:"uds"`
	Generated              map[string]any                    `yaml:",inline"`
}

type bridgeHelmValue struct {
	Name        string
	Description string
	Default     any
}

func buildBridgeHelmValues(configs map[string]model.Config) []bridgeHelmValue {
	values := []bridgeHelmValue{}
	for _, name := range sortedConfigNames(configs) {
		config := configs[name]
		if config.Bridge == nil {
			continue
		}
		if config.Bridge.EnabledValue != nil {
			values = append(values, bridgeHelmValue{
				Name:        config.Bridge.EnabledValue.Name,
				Description: fmt.Sprintf("Render and mount Compose config %s", config.Name),
				Default:     config.Bridge.EnabledValue.Default,
			})
		}
		if config.Bridge.ContentValue != nil {
			values = append(values, bridgeHelmValue{
				Name:        config.Bridge.ContentValue.Name,
				Description: fmt.Sprintf("Content for Compose config %s", config.Name),
				Default:     config.Bridge.ContentValue.Default,
			})
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Name < values[j].Name })
	return values
}

type resourceValues struct {
	Requests resourceQuantityValues `yaml:"requests"`
	Limits   resourceQuantityValues `yaml:"limits"`
}

type resourceQuantityValues struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}

type udsValues struct {
	Domain string `yaml:"domain"`
}

type chartString struct {
	Value   string
	Literal bool
}

func (value chartString) MarshalYAML() (any, error) {
	node := &yamlv3.Node{Kind: yamlv3.ScalarNode, Tag: "!!str", Value: value.Value}
	if value.Literal {
		node.Style = yamlv3.LiteralStyle
	}
	return node, nil
}

func plainChartString(value string) chartString {
	return chartString{Value: value}
}

func zarfChartString(variableName string) chartString {
	return chartString{Value: fmt.Sprintf("###ZARF_VAR_%s###", variableName), Literal: true}
}

type externalResourceValues struct {
	Name chartString `yaml:"name"`
	Key  chartString `yaml:"key"`
}
